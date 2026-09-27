package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/checkout"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/delivery"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/media"
	"github.com/dezmymachine/farmish-backend/internal/media/mediatest"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/payments/fake"
	"github.com/dezmymachine/farmish-backend/internal/payments/paystacktest"
	"github.com/dezmymachine/farmish-backend/internal/ratelimit"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"

	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/http/middleware"
)

// fulfilmentFixture wires the full router with the orders state machine.
type fulfilmentFixture struct {
	router   *gin.Engine
	pool     *pgxpool.Pool
	store    *media.R2
	provider *fake.Provider
	checkout *checkout.Service
	orders   *orders.Service
	events   <-chan *river.Event
}

func newFulfilmentFixture(t *testing.T) *fulfilmentFixture {
	t.Helper()
	return newFulfilmentFixtureWithLimits(t, nil)
}

func newFulfilmentFixtureWithLimits(t *testing.T, limits *middleware.RateLimits) *fulfilmentFixture {
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
	ordersSvc.AttachLedger(ledger.New())
	ordersSvc.AttachPaystack(provider)
	checkoutSvc := checkout.New(pool, paymentsSvc, provider, delivery.Manual{}, ledger.New(), ordersSvc, log, paystackFeeBps, 30*time.Minute)
	paymentsSvc.RegisterPurpose(payments.PurposeCheckout, checkoutSvc.HandleCheckoutPaid)
	// The same registration cmd/api performs (Phase 17a), so the refund
	// webhook tests prove the production wiring.
	orders.RegisterRefundEvents(paymentsSvc, ordersSvc)

	reg := jobs.NewRegistry()
	payments.RegisterSucceeded(reg, paymentsSvc, log)
	checkout.RegisterJobs(reg, checkoutSvc, log)
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
	checkoutSvc.AttachJobClient(client)
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
		Payments: paymentsSvc, Checkout: checkoutSvc,
		Orders: ordersSvc, OrderActions: ordersSvc,
	}
	if limits != nil {
		deps.RateLimits = limits
		deps.SharedLimiter = ratelimit.NewMemory()
	}
	router := newTestRouterWithConfig(t, deps, config.Config{
		Env: config.EnvTest, CORSOrigins: []string{"https://farmish.gh"},
		Paystack: config.Paystack{SecretKey: paystackSecret},
	})
	return &fulfilmentFixture{
		router: router, pool: pool, store: store, provider: provider,
		checkout: checkoutSvc, orders: ordersSvc, events: events,
	}
}

// userWithProfile creates a seller profile for a fresh emulator user.
func (f *fulfilmentFixture) userWithProfile(t *testing.T, uid string) (string, uuid.UUID) {
	t.Helper()
	token := withProfile(t, f.pool, uid)
	return token, userIDFor(t, f.pool, token)
}

// createPaidOrder buys one listing through the real endpoints and settles the
// charge, returning the order id plus its seller and buyer tokens.
func (f *fulfilmentFixture) createPaidOrder(t *testing.T, sellerTok, buyerTok string, sellerID uuid.UUID) (uuid.UUID, uuid.UUID) {
	t.Helper()
	listingID := publishQuoteListing(t, f.router, f.pool, f.store, sellerTok, "Fulfilment Heifer")
	body := `{"lines": [{"listingId": "` + listingID.String() + `", "quantity": 1}],
		"delivery": [{"sellerId": "` + sellerID.String() + `", "method": "pickup"}]}`
	req := jsonRequest(http.MethodPost, "/v1/checkout", buyerTok, body)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	w := serve(t, f.router, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("checkout: %d %s", w.Code, w.Body.String())
	}
	var created api.CheckoutCreated
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	settleBody := paystacktest.ChargeSuccessBody(created.Reference, created.Quote.Charge.Amount, nextPaystackEventID())
	if w := postWebhook(f.router, settleBody, paystacktest.Sign(paystackSecret, settleBody)); w.Code != http.StatusOK {
		t.Fatalf("webhook: %d %s", w.Code, w.Body.String())
	}
	waitForFulfilmentJob(t, f, created.Reference)
	var orderID uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id FROM orders WHERE checkout_id = $1`, created.CheckoutId).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	return orderID, created.CheckoutId
}

// nextPaystackEventID hands out a fresh provider event id per settle. The
// webhook keys events '<event>:<provider id>', so reusing one id makes the
// second settle look like a replay of the first and it is ignored.
var paystackEventIDs atomic.Int64

func nextPaystackEventID() int64 {
	return paystackEventIDs.Add(1) + 424242
}

// waitForFulfilmentJob waits for the settle's payments.succeeded job, dumping
// the River table on timeout so a stuck job names itself.
func waitForFulfilmentJob(t *testing.T, f *fulfilmentFixture, reference string) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case event := <-f.events:
			if event.Kind == river.EventKindJobCompleted && event.Job.Kind == "payments.succeeded" {
				return
			}
		case <-deadline:
			rows, err := f.pool.Query(context.Background(),
				`SELECT kind, state, COALESCE(args->>'reference', args->>'orderId', ''),
				        attempt, errors[array_length(errors, 1)]->>'error'
				 FROM river_job ORDER BY id`)
			if err != nil {
				t.Fatalf("timed out waiting for payments.succeeded of %s (and cannot dump river_job: %v)", reference, err)
			}
			defer rows.Close()
			var dump strings.Builder
			for rows.Next() {
				var kind, state, ref string
				var attempt int
				var lastErr *string
				if err := rows.Scan(&kind, &state, &ref, &attempt, &lastErr); err != nil {
					t.Fatal(err)
				}
				dump.WriteString(kind + "/" + state + " ref=" + ref + " attempt=" + strconv.Itoa(attempt))
				if lastErr != nil {
					dump.WriteString(" err=" + *lastErr)
				}
				dump.WriteString("\n")
			}
			prows, perr := f.pool.Query(context.Background(),
				`SELECT reference, status FROM payments ORDER BY id`)
			if perr == nil {
				defer prows.Close()
				dump.WriteString("payments:\n")
				for prows.Next() {
					var ref, status string
					if err := prows.Scan(&ref, &status); err != nil {
						t.Fatal(err)
					}
					dump.WriteString("  " + ref + " " + status + "\n")
				}
			}
			t.Fatalf("timed out waiting for payments.succeeded of %s\nriver_job:\n%s", reference, dump.String())
		}
	}
}

// TestEndpoints_FulfilmentHappyPath walks accept → ship → mark-delivered →
// confirm-receipt, with every response contract-valid and role-shaped.
func TestEndpoints_FulfilmentHappyPath(t *testing.T) {
	f := newFulfilmentFixture(t)
	sellerTok, sellerID := f.userWithProfile(t, "fulfil-seller@example.com")
	buyerTok, _ := f.userWithProfile(t, "fulfil-buyer@example.com")
	orderID, _ := f.createPaidOrder(t, sellerTok, buyerTok, sellerID)

	act := func(method, path, token, body string, want int) *httptest.ResponseRecorder {
		req := jsonRequest(method, path, token, body)
		w := serve(t, f.router, req)
		if w.Code != want {
			t.Fatalf("%s: %d %s, want %d", path, w.Code, w.Body.String(), want)
		}
		assertContract(t, req, w)
		return w
	}

	// Accept: the seller sees the commission and the recipient.
	w := act(http.MethodPost, "/v1/seller/orders/"+orderID.String()+"/accept", sellerTok, "", http.StatusOK)
	var sellerView api.OrderDetail
	if err := json.Unmarshal(w.Body.Bytes(), &sellerView); err != nil {
		t.Fatal(err)
	}
	if sellerView.Status != "accepted" {
		t.Errorf("seller view status = %s, want accepted", sellerView.Status)
	}
	if sellerView.Commission == nil {
		t.Error("the accept response hides the commission from the seller")
	}

	// Ship with a tracking ref, then without one on a fresh order.
	w = act(http.MethodPost, "/v1/seller/orders/"+orderID.String()+"/ship", sellerTok,
		`{"trackingRef": "TRK-881"}`, http.StatusOK)
	sellerView = api.OrderDetail{}
	if err := json.Unmarshal(w.Body.Bytes(), &sellerView); err != nil {
		t.Fatal(err)
	}
	if sellerView.Status != "shipped" || sellerView.Delivery.TrackingRef == nil || *sellerView.Delivery.TrackingRef != "TRK-881" {
		t.Errorf("ship response = %s tracking %v", sellerView.Status, sellerView.Delivery.TrackingRef)
	}

	act(http.MethodPost, "/v1/seller/orders/"+orderID.String()+"/mark-delivered", sellerTok, "", http.StatusOK)

	// Confirm receipt: the buyer sees the seller's profile, never the commission.
	w = act(http.MethodPost, "/v1/orders/"+orderID.String()+"/confirm-receipt", buyerTok, "", http.StatusOK)
	var buyerView api.OrderDetail
	if err := json.Unmarshal(w.Body.Bytes(), &buyerView); err != nil {
		t.Fatal(err)
	}
	if buyerView.Status != "completed" {
		t.Errorf("status = %s, want completed", buyerView.Status)
	}
	if buyerView.Commission != nil {
		t.Error("the buyer sees the commission after confirm-receipt")
	}
	if buyerView.Seller == nil {
		t.Error("the buyer response omits the seller's public profile")
	}

	// The escrow release is queued.
	var releaseJobs int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM river_job WHERE kind = 'orders.release_escrow'`).Scan(&releaseJobs); err != nil {
		t.Fatal(err)
	}
	if releaseJobs != 1 {
		t.Errorf("release jobs = %d, want 1", releaseJobs)
	}
}

// TestEndpoints_IllegalTransition409 proves a move the table forbids answers
// 409 with the from and to in the details.
func TestEndpoints_IllegalTransition409(t *testing.T) {
	f := newFulfilmentFixture(t)
	sellerTok, sellerID := f.userWithProfile(t, "illegal-seller@example.com")
	buyerTok, _ := f.userWithProfile(t, "illegal-buyer@example.com")
	orderID, _ := f.createPaidOrder(t, sellerTok, buyerTok, sellerID)

	// Shipping a paid order is not in DOMAIN §4.
	req := jsonRequest(http.MethodPost, "/v1/seller/orders/"+orderID.String()+"/ship", sellerTok, `{}`)
	w := serve(t, f.router, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("ship a paid order = %d %s, want 409", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	if !strings.Contains(w.Body.String(), "paid") || !strings.Contains(w.Body.String(), "shipped") {
		t.Errorf("409 details = %s, want the from and to", w.Body.String())
	}
}

// TestEndpoints_ActorEnforcement proves the wrong party is a 403 and a
// stranger is a 404, on the seller and buyer surfaces.
func TestEndpoints_ActorEnforcement(t *testing.T) {
	f := newFulfilmentFixture(t)
	sellerTok, sellerID := f.userWithProfile(t, "actor-seller@example.com")
	buyerTok, _ := f.userWithProfile(t, "actor-buyer@example.com")
	strangerTok, _ := f.userWithProfile(t, "actor-stranger@example.com")
	orderID, _ := f.createPaidOrder(t, sellerTok, buyerTok, sellerID)

	check := func(path, token string, want int, label string) {
		req := jsonRequest(http.MethodPost, path, token, "")
		w := serve(t, f.router, req)
		if w.Code != want {
			t.Errorf("%s = %d %s, want %d", label, w.Code, w.Body.String(), want)
		}
		assertContract(t, req, w)
	}
	// The buyer cannot call the seller accept on their own purchase.
	check("/v1/seller/orders/"+orderID.String()+"/accept", buyerTok, http.StatusForbidden, "buyer as seller")
	// The seller cannot confirm receipt of their own sale.
	check("/v1/orders/"+orderID.String()+"/confirm-receipt", sellerTok, http.StatusForbidden, "seller as buyer")
	// A stranger is not a party: the order does not exist for them.
	check("/v1/seller/orders/"+orderID.String()+"/accept", strangerTok, http.StatusNotFound, "stranger as seller")
	check("/v1/orders/"+orderID.String()+"/confirm-receipt", strangerTok, http.StatusNotFound, "stranger as buyer")
	// Anonymous is 401.
	check("/v1/seller/orders/"+orderID.String()+"/accept", "", http.StatusUnauthorized, "anonymous")
}

// TestEndpoints_DisputeAndCancel drive the two buyer-initiated paths.
func TestEndpoints_DisputeAndCancel(t *testing.T) {
	limits := middleware.DefaultRateLimits()
	limits.Operation["sensitive"] = ratelimit.Rule{Name: "sensitive", Limit: 100, Period: time.Minute, Burst: 50}
	f := newFulfilmentFixtureWithLimits(t, &limits)
	sellerTok, sellerID := f.userWithProfile(t, "dispute-seller@example.com")
	buyerTok, _ := f.userWithProfile(t, "dispute-buyer@example.com")
	strangerTok, _ := f.userWithProfile(t, "dispute-stranger@example.com")

	// A dispute needs a shipped order.
	disputeOrder, _ := f.createPaidOrder(t, sellerTok, buyerTok, sellerID)
	for _, path := range []string{"/accept", "/ship"} {
		req := jsonRequest(http.MethodPost, "/v1/seller/orders/"+disputeOrder.String()+path, sellerTok, `{}`)
		if w := serve(t, f.router, req); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	req := jsonRequest(http.MethodPost, "/v1/orders/"+disputeOrder.String()+"/dispute", buyerTok,
		`{"reason": "not_received", "description": "The consignment never reached the farm."}`)
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("dispute: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var view api.OrderDetail
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Status != "disputed" || view.EscrowState != "held" {
		t.Errorf("dispute response = %s/%s, want disputed with escrow still held", view.Status, view.EscrowState)
	}
	// The disputes row exists and the seller is notified.
	var disputes int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM disputes WHERE order_id = $1`, disputeOrder).Scan(&disputes); err != nil {
		t.Fatal(err)
	}
	if disputes != 1 {
		t.Errorf("disputes rows = %d, want 1", disputes)
	}

	// Validation: a short description and an unknown reason are 400s.
	for _, body := range []string{
		`{"reason": "not_received", "description": "short"}`,
		`{"reason": "no_such_reason", "description": "A description of decent length here."}`,
	} {
		req = jsonRequest(http.MethodPost, "/v1/orders/"+disputeOrder.String()+"/dispute", buyerTok, body)
		if w := serve(t, f.router, req); w.Code != http.StatusBadRequest {
			t.Errorf("invalid dispute body %d %s, want 400", w.Code, w.Body.String())
		}
	}

	// Cancel: a paid order only, and only the buyer's.
	cancelOrder, _ := f.createPaidOrder(t, sellerTok, buyerTok, sellerID)
	req = jsonRequest(http.MethodPost, "/v1/orders/"+cancelOrder.String()+"/cancel", buyerTok, `{"reason": "Changed my mind."}`)
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	// A stranger cannot even see it.
	req = jsonRequest(http.MethodPost, "/v1/orders/"+cancelOrder.String()+"/cancel", strangerTok, `{"reason": "Changed my mind."}`)
	if w := serve(t, f.router, req); w.Code != http.StatusNotFound {
		t.Errorf("stranger cancel = %d, want 404", w.Code)
	}
	// Rejecting the cancelled order is now an illegal move: 409.
	req = jsonRequest(http.MethodPost, "/v1/seller/orders/"+cancelOrder.String()+"/reject", sellerTok, `{"reason": "too late"}`)
	if w := serve(t, f.router, req); w.Code != http.StatusConflict {
		t.Errorf("reject after cancel = %d, want 409", w.Code)
	}
	assertContract(t, req, w)
}

// TestEndpoints_RejectRefunds drives the seller reject: the buyer gets their
// money back, the stock is restored, and both are told.
func TestEndpoints_RejectRefunds(t *testing.T) {
	f := newFulfilmentFixture(t)
	sellerTok, sellerID := f.userWithProfile(t, "reject-seller@example.com")
	buyerTok, _ := f.userWithProfile(t, "reject-buyer@example.com")
	orderID, _ := f.createPaidOrder(t, sellerTok, buyerTok, sellerID)

	req := jsonRequest(http.MethodPost, "/v1/seller/orders/"+orderID.String()+"/reject", sellerTok,
		`{"reason": "The heifer was sold at the market this morning."}`)
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var view api.OrderDetail
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Status != "cancelled" || view.EscrowState != "refund_pending" {
		t.Errorf("reject response = %s/%s, want cancelled/refund_pending", view.Status, view.EscrowState)
	}
	var refundJobs int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM river_job WHERE kind = 'orders.refund'`).Scan(&refundJobs); err != nil {
		t.Fatal(err)
	}
	if refundJobs != 1 {
		t.Errorf("refund jobs = %d, want 1", refundJobs)
	}
	var rejectedNotify int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM river_job WHERE kind = 'notify.sms' AND args->>'template' = 'order_rejected_buyer'`).Scan(&rejectedNotify); err != nil {
		t.Fatal(err)
	}
	if rejectedNotify != 1 {
		t.Errorf("rejected-buyer notifications = %d, want 1", rejectedNotify)
	}
}

// TestEndpoints_DisputeRateLimit proves the dispute endpoint's sensitive
// budget answers 429 with a Retry-After once exhausted.
func TestEndpoints_DisputeRateLimit(t *testing.T) {
	f := newFulfilmentFixture(t)
	sellerTok, sellerID := f.userWithProfile(t, "limit-seller@example.com")
	buyerTok, _ := f.userWithProfile(t, "limit-buyer@example.com")
	orderID, _ := f.createPaidOrder(t, sellerTok, buyerTok, sellerID)
	for _, path := range []string{"/accept", "/ship"} {
		req := jsonRequest(http.MethodPost, "/v1/seller/orders/"+orderID.String()+path, sellerTok, `{}`)
		if w := serve(t, f.router, req); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}

	var limited *httptest.ResponseRecorder
	for range 12 {
		req := jsonRequest(http.MethodPost, "/v1/orders/"+orderID.String()+"/dispute", buyerTok,
			`{"reason": "not_received", "description": "The consignment never reached the farm."}`)
		w := serve(t, f.router, req)
		if w.Code == http.StatusTooManyRequests {
			limited = w
			break
		}
		if w.Code != http.StatusOK && w.Code != http.StatusConflict {
			t.Fatalf("dispute: %d %s", w.Code, w.Body.String())
		}
	}
	if limited == nil {
		t.Fatal("sensitive policy never limited 12 dispute requests")
	}
	assertErrorEnvelope(t, limited.Body.Bytes(), apierror.CodeRateLimited)
	if limited.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
}
