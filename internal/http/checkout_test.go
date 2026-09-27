package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/checkout"
	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/delivery"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/middleware"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/media"
	"github.com/dezmymachine/farmish-backend/internal/media/mediatest"
	"github.com/dezmymachine/farmish-backend/internal/money"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/payments/fake"
	"github.com/dezmymachine/farmish-backend/internal/payments/paystacktest"
	"github.com/dezmymachine/farmish-backend/internal/ratelimit"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// checkoutFixture wires the real router with a full checkout stack and a fake
// Paystack, mirroring the production registration.
type checkoutFixture struct {
	router   *gin.Engine
	pool     *pgxpool.Pool
	store    *media.R2
	provider *fake.Provider
	svc      *checkout.Service
	events   <-chan *river.Event
}

func newCheckoutFixture(t *testing.T) *checkoutFixture {
	t.Helper()
	return newCheckoutFixtureWithLimits(t, nil)
}

func newCheckoutFixtureWithLimits(t *testing.T, limits *middleware.RateLimits) *checkoutFixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Pool(t)
	if err := catalog.Seed(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := mediatest.R2(t)
	sellersSvc := sellers.New(pool, nil, nil)
	listingsSvc := listings.New(pool, catalog.New(pool), media.New(pool, store), sellersSvc)
	log := slog.New(slog.DiscardHandler)
	provider := fake.New()
	paymentsSvc := payments.New(pool, provider, log, paystackFeeBps, "https://farmish.gh/payments/status")
	ordersSvc := orders.NewService(pool, 48*time.Hour, 3*24*time.Hour)
	svc := checkout.New(pool, paymentsSvc, provider, delivery.Manual{}, ledger.New(), ordersSvc, log, paystackFeeBps, 30*time.Minute)
	paymentsSvc.RegisterPurpose(payments.PurposeCheckout, svc.HandleCheckoutPaid)

	reg := jobs.NewRegistry()
	payments.RegisterSucceeded(reg, paymentsSvc, log)
	checkout.RegisterJobs(reg, svc, log)
	orders.RegisterJobs(reg, ordersSvc, log)
	notify.Register(reg, notify.NewWorker(notify.LogOnly{Log: log}, log, pool))
	client, err := jobs.NewClient(pool, reg, log, jobs.Options{Work: true, FetchPollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := client.Subscribe(river.EventKindJobCompleted)
	t.Cleanup(cancel)
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	paymentsSvc.AttachJobClient(client)
	svc.AttachJobClient(client)
	ordersSvc.AttachJobClient(client)
	t.Cleanup(func() {
		if err := jobs.Stop(client, 5*time.Second, 2*time.Second, log); err != nil {
			t.Errorf("stop jobs: %v", err)
		}
	})

	fb := authtest.Firebase(t)
	deps := Deps{
		DB: fakePinger{}, Verifier: fb, Users: users.New(pool),
		Sellers: sellersSvc, Media: media.New(pool, store),
		Listings: listingsSvc, PublicListings: listingsSvc,
		Payments: paymentsSvc, Checkout: svc, Orders: ordersSvc,
	}
	if limits != nil {
		deps.RateLimits = limits
		deps.SharedLimiter = ratelimit.NewMemory()
	}
	return &checkoutFixture{
		router: newTestRouterWithConfig(t, deps, config.Config{
			Env: config.EnvTest, CORSOrigins: []string{"https://farmish.gh"},
			Paystack: config.Paystack{SecretKey: paystackSecret},
		}),
		pool: pool, store: store, provider: provider, svc: svc, events: events,
	}
}

func (f *checkoutFixture) userWithProfile(t *testing.T, uid string) (string, uuid.UUID) {
	t.Helper()
	token := withProfile(t, f.pool, uid)
	return token, userIDFor(t, f.pool, token)
}

// publishQuoteListing creates and publishes a listing through the real API.
func publishQuoteListing(t *testing.T, r *gin.Engine, pool *pgxpool.Pool, store *media.R2, token, title string) uuid.UUID {
	t.Helper()
	id := createListing(t, r, token)
	mediaID := uploadImage(t, pool, store, token)
	req := jsonRequest(http.MethodPatch, "/v1/me/listings/"+id.String(), token,
		`{"title":`+strconv.Quote(title)+`,"imageMediaIds":["`+mediaID+`"]}`)
	w := serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("attach image: %d %s", w.Code, w.Body.String())
	}
	req = jsonRequest(http.MethodPost, "/v1/me/listings/"+id.String()+"/publish", token, "")
	w = serve(t, r, req)
	if w.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	return id
}

func checkoutBody(lines string, delivery string) string {
	return `{"lines": [` + lines + `], "delivery": [` + delivery + `]}`
}

func TestCheckout_TamperedClientPriceIgnored(t *testing.T) {
	f := newCheckoutFixture(t)
	sellerTok, sellerID := f.userWithProfile(t, "checkout-tamper-seller")
	buyerTok, _ := f.userWithProfile(t, "checkout-tamper-buyer")
	listingID := publishQuoteListing(t, f.router, f.pool, f.store, sellerTok, "Tamper-Proof Heifer")

	// The tampered body carries prices the server never asked for.
	tampered := `{"lines": [{"listingId": "` + listingID.String() +
		`", "quantity": 2, "unitPrice": {"amount": 1, "currency": "GHS"}}],
		"delivery": [{"sellerId": "` + sellerID.String() + `", "method": "pickup"}]}`
	req := jsonRequest(http.MethodPost, "/v1/checkout", buyerTok, tampered)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	w := serve(t, f.router, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("tampered body = %d %s, want 400", w.Code, w.Body.String())
	}
	assertContract(t, req, w)

	// The honest body prices from the database only.
	honest := checkoutBody(
		`{"listingId": "`+listingID.String()+`", "quantity": 2}`,
		`{"sellerId": "`+sellerID.String()+`", "method": "pickup"}`,
	)
	req = jsonRequest(http.MethodPost, "/v1/checkout", buyerTok, honest)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	w = serve(t, f.router, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var created api.CheckoutCreated
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	charge, _, err := money.GrossUp(2*850000, paystackFeeBps)
	if err != nil {
		t.Fatal(err)
	}
	if created.Quote.Base.Amount != 1700000 || created.Quote.Charge.Amount != charge {
		t.Errorf("quote = base %+v charge %+v, want 1700000 and the grossed-up %d",
			created.Quote.Base, created.Quote.Charge, charge)
	}
}

func TestCheckoutEndpoint_FullContract(t *testing.T) {
	limits := middleware.DefaultRateLimits()
	limits.Operation["sensitive"] = ratelimit.Rule{Name: "sensitive", Limit: 100, Period: time.Minute, Burst: 50}
	f := newCheckoutFixtureWithLimits(t, &limits)
	sellerTok, sellerID := f.userWithProfile(t, "checkout-contract-seller")
	buyerTok, _ := f.userWithProfile(t, "checkout-contract-buyer")
	otherTok, _ := f.userWithProfile(t, "checkout-contract-stranger")
	listingID := publishQuoteListing(t, f.router, f.pool, f.store, sellerTok, "Contract Heifer")

	body := checkoutBody(
		`{"listingId": "`+listingID.String()+`", "quantity": 2}`,
		`{"sellerId": "`+sellerID.String()+`", "method": "pickup"}`,
	)
	create := func(token, key string, want int) *httptest.ResponseRecorder {
		req := jsonRequest(http.MethodPost, "/v1/checkout", token, body)
		req.Header.Set("Idempotency-Key", key)
		w := serve(t, f.router, req)
		if w.Code != want {
			t.Fatalf("checkout = %d %s, want %d", w.Code, w.Body.String(), want)
		}
		assertContract(t, req, w)
		return w
	}

	// Missing header: 400 before anything is priced.
	req := jsonRequest(http.MethodPost, "/v1/checkout", buyerTok, body)
	if w := serve(t, f.router, req); w.Code != http.StatusBadRequest {
		t.Fatalf("missing idempotency key = %d %s", w.Code, w.Body.String())
	}
	// Anonymous: 401.
	req = jsonRequest(http.MethodPost, "/v1/checkout", "", body)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	if w := serve(t, f.router, req); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d %s", w.Code, w.Body.String())
	}
	// Malformed key: 400.
	req = jsonRequest(http.MethodPost, "/v1/checkout", buyerTok, body)
	req.Header.Set("Idempotency-Key", "not-a-uuid")
	if w := serve(t, f.router, req); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed key = %d %s", w.Code, w.Body.String())
	}

	key := uuid.NewString()
	w := create(buyerTok, key, http.StatusCreated)
	var created api.CheckoutCreated
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// The replay is 200 with the same body, and no second provider call.
	before := f.provider.CallCount("InitializeTransaction")
	w = create(buyerTok, key, http.StatusOK)
	var replayed api.CheckoutCreated
	if err := json.Unmarshal(w.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.CheckoutId != created.CheckoutId {
		t.Errorf("replay id = %s, want %s", replayed.CheckoutId, created.CheckoutId)
	}
	if got := f.provider.CallCount("InitializeTransaction"); got != before {
		t.Errorf("replay called the provider again: %d -> %d", before, got)
	}

	// Same key, different payload: 409 idempotency_key_reused.
	otherBody := checkoutBody(
		`{"listingId": "`+listingID.String()+`", "quantity": 1}`,
		`{"sellerId": "`+sellerID.String()+`", "method": "pickup"}`,
	)
	req = jsonRequest(http.MethodPost, "/v1/checkout", buyerTok, otherBody)
	req.Header.Set("Idempotency-Key", key)
	if w := serve(t, f.router, req); w.Code != http.StatusConflict {
		t.Fatalf("reused key = %d %s, want 409", w.Code, w.Body.String())
	}
	assertContract(t, req, w)

	// The buyer polls; a stranger gets a 404.
	req = jsonRequest(http.MethodGet, "/v1/checkouts/"+created.CheckoutId.String(), buyerTok, "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("poll = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var status api.CheckoutStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Status != "pending_payment" || len(status.Orders) != 1 {
		t.Errorf("poll = %+v", status)
	}
	req = jsonRequest(http.MethodGet, "/v1/checkouts/"+created.CheckoutId.String(), otherTok, "")
	if w := serve(t, f.router, req); w.Code != http.StatusNotFound {
		t.Fatalf("stranger poll = %d %s, want 404", w.Code, w.Body.String())
	}

	// Settle the charge and confirm the poll flips to paid.
	settleBody := paystacktest.ChargeSuccessBody(created.Reference, created.Quote.Charge.Amount, 424242)
	w = postWebhook(f.router, settleBody, paystacktest.Sign(paystackSecret, settleBody))
	if w.Code != http.StatusOK {
		// Surface the swallowed cause: the strict handler logged to Discard.
		hookErr := f.pool.QueryRow(context.Background(),
			`SELECT outcome FROM webhook_events ORDER BY id DESC LIMIT 1`).Scan(new(string))
		t.Fatalf("webhook = %d %s (outcome scan: %v)", w.Code, w.Body.String(), hookErr)
	}
	waitForCheckoutJob(t, f.events)

	req = jsonRequest(http.MethodGet, "/v1/checkouts/"+created.CheckoutId.String(), buyerTok, "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("poll after pay = %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Status != "paid" {
		t.Errorf("poll after pay = %+v", status)
	}

	// Both parties see the order; the commission shows to the seller only.
	var orderID uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id FROM orders WHERE checkout_id = $1`, created.CheckoutId).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	req = jsonRequest(http.MethodGet, "/v1/orders/"+orderID.String(), buyerTok, "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("buyer order = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var buyerView api.OrderDetail
	if err := json.Unmarshal(w.Body.Bytes(), &buyerView); err != nil {
		t.Fatal(err)
	}
	if buyerView.Commission != nil || buyerView.SellerNet != nil {
		t.Errorf("buyer sees the commission: %+v", buyerView.Commission)
	}
	if buyerView.Seller == nil || buyerView.Seller.UserId != sellerID {
		t.Errorf("buyer seller projection = %+v", buyerView.Seller)
	}
	if buyerView.Delivery.RecipientPhone != nil {
		t.Errorf("buyer sees the recipient phone: %+v", buyerView.Delivery.RecipientPhone)
	}

	req = jsonRequest(http.MethodGet, "/v1/orders/"+orderID.String(), sellerTok, "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("seller order = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var sellerView api.OrderDetail
	if err := json.Unmarshal(w.Body.Bytes(), &sellerView); err != nil {
		t.Fatal(err)
	}
	if sellerView.Commission == nil || sellerView.Commission.Amount <= 0 {
		t.Errorf("seller commission = %+v, want a positive snapshot", sellerView.Commission)
	}
	if sellerView.SellerNet == nil || sellerView.SellerNet.Amount != sellerView.Base.Amount-sellerView.Commission.Amount {
		t.Errorf("seller net = %+v, want base minus commission", sellerView.SellerNet)
	}
	req = jsonRequest(http.MethodGet, "/v1/orders/"+orderID.String(), otherTok, "")
	if w := serve(t, f.router, req); w.Code != http.StatusNotFound {
		t.Fatalf("stranger order = %d, want 404", w.Code)
	}

	// The lists see the order from each side.
	req = jsonRequest(http.MethodGet, "/v1/orders?page=1&limit=20", buyerTok, "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), orderID.String()) {
		t.Fatalf("buyer list = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	req = jsonRequest(http.MethodGet, "/v1/seller/orders?status=paid&page=1&limit=20", sellerTok, "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), orderID.String()) {
		t.Fatalf("seller list = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	// Anonymous and stranger checks.
	req = jsonRequest(http.MethodGet, "/v1/orders", "", "")
	if w := serve(t, f.router, req); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous orders = %d", w.Code)
	}
	req = jsonRequest(http.MethodGet, "/v1/orders/"+uuid.NewString(), buyerTok, "")
	if w := serve(t, f.router, req); w.Code != http.StatusNotFound {
		t.Fatalf("unknown order = %d", w.Code)
	}
}

func TestCheckoutEndpoint_InsufficientStock409(t *testing.T) {
	f := newCheckoutFixture(t)
	sellerTok, sellerID := f.userWithProfile(t, "checkout-stock-seller")
	buyerTok, _ := f.userWithProfile(t, "checkout-stock-buyer")
	listingID := publishQuoteListing(t, f.router, f.pool, f.store, sellerTok, "Single Unit Heifer")
	// Only one unit exists: the published default is twelve.
	shrink := jsonRequest(http.MethodPatch, "/v1/me/listings/"+listingID.String(), sellerTok,
		`{"quantityAvailable": 1}`)
	if w := serve(t, f.router, shrink); w.Code != http.StatusOK {
		t.Fatalf("shrink stock: %d %s", w.Code, w.Body.String())
	}

	// Exhaust the stock with one checkout.
	first := checkoutBody(
		`{"listingId": "`+listingID.String()+`", "quantity": 1}`,
		`{"sellerId": "`+sellerID.String()+`", "method": "pickup"}`,
	)
	req := jsonRequest(http.MethodPost, "/v1/checkout", buyerTok, first)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	if w := serve(t, f.router, req); w.Code != http.StatusCreated {
		t.Fatalf("first checkout = %d %s", w.Code, w.Body.String())
	}

	// A second buyer cannot have the same unit.
	otherTok, _ := f.userWithProfile(t, "checkout-stock-other")
	req = jsonRequest(http.MethodPost, "/v1/checkout", otherTok, first)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	w := serve(t, f.router, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("second checkout = %d %s, want 409", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	if !strings.Contains(w.Body.String(), "insufficient_stock") {
		t.Errorf("body = %s, want the insufficient_stock code", w.Body.String())
	}
}

func waitForCheckoutJob(t *testing.T, events <-chan *river.Event) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Kind == river.EventKindJobCompleted && event.Job.Kind == "payments.succeeded" {
				return
			}
			if event.Kind == river.EventKindJobFailed && event.Job.Kind == "payments.succeeded" {
				t.Fatalf("payments.succeeded failed: %s", event.Job.Errors[0].Error)
			}
		case <-deadline:
			t.Fatal("timed out waiting for payments.succeeded")
		}
	}
}
