package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
	"github.com/dezmymachine/farmish-backend/internal/http/middleware"
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
)

// adminFixture wires the real dispute endpoints with live workers, like the
// fulfilment fixture: settling a checkout waits for its payments.succeeded
// job, and refund and release jobs run on their own.
type adminFixture struct {
	router   *gin.Engine
	pool     *pgxpool.Pool
	store    *media.R2
	provider *fake.Provider
	orders   *orders.Service
	events   <-chan *river.Event
}

func newAdminFixture(t *testing.T) *adminFixture {
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
	orders.RegisterRefundEvents(paymentsSvc, ordersSvc)

	reg := jobs.NewRegistry()
	payments.RegisterSucceeded(reg, paymentsSvc, log)
	checkout.RegisterJobs(reg, checkoutSvc, log)
	orders.RegisterJobs(reg, ordersSvc, log)
	ledger.RegisterReconcile(reg, pool, ledger.New(), log)
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
	limits := middleware.DefaultRateLimits()
	limits.Operation["sensitive"] = ratelimit.Rule{Name: "sensitive", Limit: 100, Period: time.Minute, Burst: 50}
	router := newTestRouterWithConfig(t, Deps{
		DB: fakePinger{}, Verifier: fb, Users: users.New(pool),
		Sellers: sellersSvc, Media: media.New(pool, store),
		Listings: listingsSvc, PublicListings: listingsSvc,
		Payments: paymentsSvc, Checkout: checkoutSvc,
		Orders: ordersSvc, OrderActions: ordersSvc,
		// The validation test resolves the same dispute eight times; the
		// default sensitive budget would 429 it before the assertions run.
		RateLimits: &limits, SharedLimiter: ratelimit.NewMemory(),
	}, config.Config{
		Env: config.EnvTest, CORSOrigins: []string{"https://farmish.gh"},
		Paystack: config.Paystack{SecretKey: paystackSecret},
	})
	return &adminFixture{router: router, pool: pool, store: store, provider: provider, orders: ordersSvc, events: events}
}

// paidOrder buys one listing and settles its charge through the real webhook,
// waiting for the settle job like the fulfilment tests do.
func (f *adminFixture) paidOrder(t *testing.T, sellerTok, buyerTok string, sellerID uuid.UUID) uuid.UUID {
	t.Helper()
	listingID := publishQuoteListing(t, f.router, f.pool, f.store, sellerTok, "Dispute Cowpea")
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
	deadline := time.After(30 * time.Second)
	for {
		select {
		case event := <-f.events:
			if event.Kind == river.EventKindJobCompleted && event.Job.Kind == "payments.succeeded" {
				goto settled
			}
		case <-deadline:
			t.Fatalf("timed out waiting for payments.succeeded of %s", created.Reference)
		}
	}
settled:
	var orderID uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id FROM orders WHERE checkout_id = $1`, created.CheckoutId).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	return orderID
}

// disputedOrder walks an order to disputed through the real endpoints.
func (f *adminFixture) disputedOrder(t *testing.T, sellerTok, buyerTok string, sellerID uuid.UUID) uuid.UUID {
	t.Helper()
	orderID := f.paidOrder(t, sellerTok, buyerTok, sellerID)
	for _, path := range []string{"/accept", "/ship"} {
		req := jsonRequest(http.MethodPost, "/v1/seller/orders/"+orderID.String()+path, sellerTok, `{}`)
		if w := serve(t, f.router, req); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	req := jsonRequest(http.MethodPost, "/v1/orders/"+orderID.String()+"/dispute", buyerTok,
		`{"reason": "damaged", "description": "Half the bags arrived soaked and torn."}`)
	if w := serve(t, f.router, req); w.Code != http.StatusOK {
		t.Fatalf("dispute: %d %s", w.Code, w.Body.String())
	}
	return orderID
}

// TestAdminDisputes_HappyPath walks list → get → refund_buyer through the
// real endpoints, every response contract-valid.
func TestAdminDisputes_HappyPath(t *testing.T) {
	f := newAdminFixture(t)
	fb := authtest.Firebase(t)
	_, adminTok := authAdmin(t, f.pool, fb)
	sellerTok, sellerID := f.userWithProfile(t, "admin-dispute-seller@example.com")
	buyerTok, _ := f.userWithProfile(t, "admin-dispute-buyer@example.com")
	orderID := f.disputedOrder(t, sellerTok, buyerTok, sellerID)

	// List: the open queue holds the dispute with its order.
	req := jsonRequest(http.MethodGet, "/v1/admin/disputes?status=open", adminTok, "")
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var list api.DisputeAdminList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].OrderId != orderID || list.Meta.Total != 1 {
		t.Fatalf("list = %+v, want the one dispute", list)
	}
	if list.Items[0].Order.Status != api.OrderStatusDisputed {
		t.Errorf("embedded order status = %s, want disputed", list.Items[0].Order.Status)
	}

	// Get: the single view carries the order and its events.
	disputeID := list.Items[0].Id
	req = jsonRequest(http.MethodGet, "/v1/admin/disputes/"+disputeID.String(), adminTok, "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var single api.DisputeAdmin
	if err := json.Unmarshal(w.Body.Bytes(), &single); err != nil {
		t.Fatal(err)
	}
	if len(single.Order.Events) == 0 {
		t.Error("single view has no order events")
	}

	// Resolve a full refund.
	req = jsonRequest(http.MethodPost, "/v1/admin/disputes/"+disputeID.String()+"/resolve", adminTok,
		`{"outcome": "refund_buyer", "note": "The courier confirmed the loss."}`)
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var resolved api.DisputeAdmin
	if err := json.Unmarshal(w.Body.Bytes(), &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved.Outcome == nil || *resolved.Outcome != api.DisputeOutcomeRefundBuyer {
		t.Errorf("outcome = %v, want refund_buyer", resolved.Outcome)
	}
	if resolved.Order.Status != api.OrderStatusRefunded {
		t.Errorf("order status = %s, want refunded", resolved.Order.Status)
	}
	if resolved.RefundAmount == nil || resolved.RefundAmount.Currency != api.MoneyCurrencyGHS {
		t.Errorf("refundAmount = %v, want a GHS money", resolved.RefundAmount)
	}
}

// TestAdminDisputes_PartialAndRelease resolves a split through the endpoints.
func TestAdminDisputes_PartialAndRelease(t *testing.T) {
	f := newAdminFixture(t)
	fb := authtest.Firebase(t)
	_, adminTok := authAdmin(t, f.pool, fb)
	sellerTok, sellerID := f.userWithProfile(t, "admin-partial-seller@example.com")
	buyerTok, _ := f.userWithProfile(t, "admin-partial-buyer@example.com")
	orderID := f.disputedOrder(t, sellerTok, buyerTok, sellerID)

	var disputeID uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id FROM disputes WHERE order_id = $1`, orderID).Scan(&disputeID); err != nil {
		t.Fatal(err)
	}
	var base int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT base_pesewas FROM orders WHERE id = $1`, orderID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	half := base / 2
	req := jsonRequest(http.MethodPost, "/v1/admin/disputes/"+disputeID.String()+"/resolve", adminTok,
		`{"outcome": "partial", "refundAmount": {"amount": `+strconv.FormatInt(half, 10)+`, "currency": "GHS"}, "note": "Half the order was damaged."}`)
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("resolve partial: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var resolved api.DisputeAdmin
	if err := json.Unmarshal(w.Body.Bytes(), &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved.Order.Status != api.OrderStatusCompleted {
		t.Errorf("order status = %s, want completed", resolved.Order.Status)
	}
	if resolved.RefundAmount == nil || resolved.RefundAmount.Amount != half {
		t.Errorf("refundAmount = %v, want %d", resolved.RefundAmount, half)
	}
}

// TestAdminDisputes_Validation proves the failure paths: bad amounts are
// 400, resolving twice and retrying a live refund are 409, unknown ids are
// 404, non-admins are 403 and anonymous callers are 401.
func TestAdminDisputes_Validation(t *testing.T) {
	f := newAdminFixture(t)
	fb := authtest.Firebase(t)
	_, adminTok := authAdmin(t, f.pool, fb)
	sellerTok, sellerID := f.userWithProfile(t, "admin-valid-seller@example.com")
	buyerTok, _ := f.userWithProfile(t, "admin-valid-buyer@example.com")
	strangerTok, _ := f.userWithProfile(t, "admin-valid-stranger@example.com")
	orderID := f.disputedOrder(t, sellerTok, buyerTok, sellerID)

	var disputeID uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id FROM disputes WHERE order_id = $1`, orderID).Scan(&disputeID); err != nil {
		t.Fatal(err)
	}
	var base int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT base_pesewas FROM orders WHERE id = $1`, orderID).Scan(&base); err != nil {
		t.Fatal(err)
	}

	resolve := func(token, body string) (*httptest.ResponseRecorder, *http.Request) {
		req := jsonRequest(http.MethodPost, "/v1/admin/disputes/"+disputeID.String()+"/resolve", token, body)
		return serve(t, f.router, req), req
	}

	// A partial at or above the remaining escrow is a 400.
	for _, amount := range []int64{base, base + 1, 0, -50} {
		w, req := resolve(adminTok,
			`{"outcome": "partial", "refundAmount": {"amount": `+strconv.FormatInt(amount, 10)+`, "currency": "GHS"}, "note": "Bad split."}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("partial %d = %d, want 400", amount, w.Code)
		} else {
			assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeValidationFailed)
			assertContract(t, req, w)
		}
	}
	// A non-GHS refund amount is a 400.
	if w, req := resolve(adminTok,
		`{"outcome": "partial", "refundAmount": {"amount": 100, "currency": "NGN"}, "note": "Wrong money."}`); w.Code != http.StatusBadRequest {
		t.Errorf("non-GHS = %d, want 400", w.Code)
	} else {
		assertContract(t, req, w)
	}
	// A refund amount on a full release is a 400.
	if w, req := resolve(adminTok,
		`{"outcome": "release_seller", "refundAmount": {"amount": 100, "currency": "GHS"}, "note": "Extra."}`); w.Code != http.StatusBadRequest {
		t.Errorf("release with amount = %d, want 400", w.Code)
	} else {
		assertContract(t, req, w)
	}

	// Resolving works once, then 409s.
	req := jsonRequest(http.MethodPost, "/v1/admin/disputes/"+disputeID.String()+"/resolve", adminTok,
		`{"outcome": "release_seller", "note": "Goods intact."}`)
	if w := serve(t, f.router, req); w.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", w.Code, w.Body.String())
	}
	req = jsonRequest(http.MethodPost, "/v1/admin/disputes/"+disputeID.String()+"/resolve", adminTok,
		`{"outcome": "release_seller", "note": "Again."}`)
	w := serve(t, f.router, req)
	if w.Code != http.StatusConflict {
		t.Errorf("second resolve = %d, want 409", w.Code)
	}
	assertContract(t, req, w)
	assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeConflict)

	// Unknown dispute and refund ids are 404.
	missing := uuid.NewString()
	for _, path := range []string{"/v1/admin/disputes/" + missing, "/v1/admin/disputes/" + missing + "/resolve"} {
		req := jsonRequest(http.MethodPost, path, adminTok, `{"outcome": "release_seller", "note": "Missing."}`)
		if path == "/v1/admin/disputes/"+missing {
			req = jsonRequest(http.MethodGet, path, adminTok, "")
		}
		w := serve(t, f.router, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, w.Code)
		}
		assertContract(t, req, w)
	}
	req = jsonRequest(http.MethodPost, "/v1/admin/refunds/"+missing+"/retry", adminTok, "")
	if w := serve(t, f.router, req); w.Code != http.StatusNotFound {
		t.Errorf("retry missing = %d, want 404", w.Code)
	} else {
		assertContract(t, req, w)
	}

	// Non-admins are forbidden; anonymous callers are unauthorized.
	req = jsonRequest(http.MethodGet, "/v1/admin/disputes?status=open", buyerTok, "")
	if w := serve(t, f.router, req); w.Code != http.StatusForbidden {
		t.Errorf("buyer list = %d, want 403", w.Code)
	} else {
		assertContract(t, req, w)
	}
	req = jsonRequest(http.MethodPost, "/v1/admin/disputes/"+disputeID.String()+"/resolve", strangerTok,
		`{"outcome": "release_seller", "note": "Stranger."}`)
	if w := serve(t, f.router, req); w.Code != http.StatusForbidden {
		t.Errorf("stranger resolve = %d, want 403", w.Code)
	} else {
		assertContract(t, req, w)
	}
	req = jsonRequest(http.MethodGet, "/v1/admin/disputes?status=open", "", "")
	if w := serve(t, f.router, req); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous list = %d, want 401", w.Code)
	} else {
		assertContract(t, req, w)
	}
}

// TestAdminOrder_Get proves the admin order view: both parties' contacts and
// the order's ledger entries, visible to an admin but not to a stranger.
func TestAdminOrder_Get(t *testing.T) {
	f := newAdminFixture(t)
	fb := authtest.Firebase(t)
	_, adminTok := authAdmin(t, f.pool, fb)
	sellerTok, sellerID := f.userWithProfile(t, "admin-order-seller@example.com")
	buyerTok, _ := f.userWithProfile(t, "admin-order-buyer@example.com")
	strangerTok, _ := f.userWithProfile(t, "admin-order-stranger@example.com")
	orderID := f.disputedOrder(t, sellerTok, buyerTok, sellerID)

	req := jsonRequest(http.MethodGet, "/v1/admin/orders/"+orderID.String(), adminTok, "")
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("admin order: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var view api.AdminOrderDetail
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Buyer.Email == nil || view.Seller.Email == nil {
		t.Errorf("contacts missing: buyer %v seller %v", view.Buyer.Email, view.Seller.Email)
	}
	if len(view.LedgerEntries) == 0 {
		t.Error("no ledger entries for a paid order")
	}
	foundEscrow := false
	for _, entry := range view.LedgerEntries {
		if entry.Account == "escrow" {
			foundEscrow = true
		}
		if entry.TransactionKind == "" || entry.TransactionReference == "" {
			t.Errorf("entry %+v lacks its transaction", entry)
		}
	}
	if !foundEscrow {
		t.Error("no escrow leg among the entries")
	}

	// A stranger cannot see the order on the buyer surface, but the admin can.
	strangerReq := jsonRequest(http.MethodGet, "/v1/orders/"+orderID.String(), strangerTok, "")
	if w := serve(t, f.router, strangerReq); w.Code != http.StatusNotFound {
		t.Errorf("stranger order = %d, want 404", w.Code)
	}
	req = jsonRequest(http.MethodGet, "/v1/admin/orders/"+uuid.NewString(), adminTok, "")
	if w := serve(t, f.router, req); w.Code != http.StatusNotFound {
		t.Errorf("missing order = %d, want 404", w.Code)
	} else {
		assertContract(t, req, w)
	}
}

// TestAdminRefund_Retry drives failed → queued → processed through the real
// endpoint, then proves a live refund cannot be retried.
func TestAdminRefund_Retry(t *testing.T) {
	f := newAdminFixture(t)
	fb := authtest.Firebase(t)
	_, adminTok := authAdmin(t, f.pool, fb)
	sellerTok, sellerID := f.userWithProfile(t, "admin-retry-seller@example.com")
	buyerTok, _ := f.userWithProfile(t, "admin-retry-buyer@example.com")
	orderID := f.paidOrder(t, sellerTok, buyerTok, sellerID)

	req := jsonRequest(http.MethodPost, "/v1/seller/orders/"+orderID.String()+"/reject", sellerTok,
		`{"reason": "The harvest failed before pickup."}`)
	if w := serve(t, f.router, req); w.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", w.Code, w.Body.String())
	}
	var refundID uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id FROM refunds WHERE order_id = $1`, orderID).Scan(&refundID); err != nil {
		t.Fatal(err)
	}

	// Fail it deterministically: reject while Paystack refuses, then wait
	// for the worker to record the failure.
	f.provider.RefundErr = fmt.Errorf("%w: POST /refund: transaction has been fully reversed", payments.ErrRejected)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var status string
		if err := f.pool.QueryRow(context.Background(),
			`SELECT status FROM refunds WHERE order_id = $1`, orderID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("refund never failed, status = %s", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.provider.RefundErr = nil

	req = jsonRequest(http.MethodPost, "/v1/admin/refunds/"+refundID.String()+"/retry", adminTok, "")
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var retried api.Refund
	if err := json.Unmarshal(w.Body.Bytes(), &retried); err != nil {
		t.Fatal(err)
	}
	if retried.Status != api.RefundStatusQueued {
		t.Errorf("refund status = %s, want queued", retried.Status)
	}

	// The re-queued call reaches Paystack again: wait for the worker to
	// take the retry past queued.
	deadline = time.Now().Add(10 * time.Second)
	for {
		var status string
		if err := f.pool.QueryRow(context.Background(),
			`SELECT status FROM refunds WHERE id = $1`, refundID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "queued" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retry never left queued")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := f.provider.CallCount("CreateRefund"); n != 2 {
		t.Errorf("paystack calls = %d, want 2", n)
	}

	// A live refund is not retryable.
	req = jsonRequest(http.MethodPost, "/v1/admin/refunds/"+refundID.String()+"/retry", adminTok, "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusConflict {
		t.Errorf("retry live = %d, want 409", w.Code)
	} else {
		assertContract(t, req, w)
	}
}

// userWithProfile creates a seller profile for a fresh emulator user.
func (f *adminFixture) userWithProfile(t *testing.T, uid string) (string, uuid.UUID) {
	t.Helper()
	token := withProfile(t, f.pool, uid)
	return token, userIDFor(t, f.pool, token)
}
