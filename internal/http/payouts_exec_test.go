package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/crypto"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/money"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/payments/fake"
	"github.com/dezmymachine/farmish-backend/internal/payouts"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"

	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/http/apierror"
)

// execPayoutFixture wires the payout execution endpoints with a stub
// verifier, a scripted Paystack and the real signed webhook route.
type execPayoutFixture struct {
	router   *gin.Engine
	pool     *pgxpool.Pool
	verifier *stubVerifier
	provider *fake.Provider
	pay      *payouts.Service
	sellers  *sellers.Service
	books    *ledger.Ledger
	now      time.Time
}

func newExecPayoutFixture(t *testing.T) *execPayoutFixture {
	t.Helper()
	pool := dbtest.Pool(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	crypter, err := crypto.New(key)
	if err != nil {
		t.Fatal(err)
	}
	sellersSvc := sellers.New(pool, crypter, nil)
	provider := fake.New()
	provider.BanksResult = []payments.Bank{{Name: "MTN Mobile Money", Code: "MTN", Type: "mobile_money"}}
	provider.ResolveResult = payments.Account{AccountName: "AKOSUA MENSAH"}
	provider.RecipientResult = payments.TransferRecipient{RecipientCode: "RCP_HTTP"}
	f := &execPayoutFixture{
		pool: pool, verifier: &stubVerifier{ids: map[string]auth.Identity{}},
		provider: provider, books: ledger.New(), now: time.Now().UTC().Truncate(time.Second),
		sellers: sellersSvc,
	}
	log := slog.New(slog.DiscardHandler)
	pay := payouts.New(pool, crypter, provider, sellersSvc)
	pay.Now = func() time.Time { return f.now }
	pay.AttachLedger(f.books)
	pay.AttachLogger(log)
	pay.Configure(2000, 0)
	paymentsSvc := payments.New(pool, provider, log, paystackFeeBps, "https://farmish.gh/payments/status")
	payouts.RegisterTransferEvents(paymentsSvc, pay)
	reg := jobs.NewRegistry()
	payouts.RegisterJobs(reg, pay, log)
	notify.Register(reg, notify.NewWorker(notify.LogOnly{Log: log}, log, pool))
	client, err := jobs.NewClient(pool, reg, log, jobs.Options{Work: false})
	if err != nil {
		t.Fatal(err)
	}
	pay.AttachJobClient(client)
	paymentsSvc.AttachJobClient(client)
	f.pay = pay
	f.router = newTestRouterWithConfig(t, Deps{
		DB: fakePinger{}, Verifier: f.verifier, Users: users.New(pool),
		Sellers: sellersSvc, Payouts: pay, Payments: paymentsSvc,
	}, config.Config{
		Env: config.EnvTest, CORSOrigins: []string{"https://farmish.gh"},
		Paystack: config.Paystack{SecretKey: paystackSecret},
	})
	return f
}

// sellerWithAccount resolves a stub identity, profiles them and sets a
// verified payout account, returning the seller id.
func (f *execPayoutFixture) sellerWithAccount(t *testing.T, token, uid string) uuid.UUID {
	t.Helper()
	f.verifier.mu.Lock()
	f.verifier.ids[token] = stubIdentity(uid, uid+"@farmish.test", time.Now())
	f.verifier.mu.Unlock()
	u, err := users.New(f.pool).Resolve(context.Background(), auth.Identity{
		UID: uid, Email: uid + "@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.sellers.UpsertMine(context.Background(), u.ID, sellers.ProfileInput{
		BusinessName: "Akosua Farms", Region: "Ashanti", District: "Kumasi Metro",
	}); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if _, err := f.pay.SetAccount(context.Background(), u.ID, "", payouts.Input{
		Type: "mobile_money", BankCode: "MTN", AccountNumber: "0241234567",
	}); err != nil {
		t.Fatalf("set account: %v", err)
	}
	return u.ID
}

// releaseOrder posts a completed order's legs for the seller, growing the
// payable by base minus commission.
func (f *execPayoutFixture) releaseOrder(t *testing.T, sellerID uuid.UUID, subtotal int64) {
	t.Helper()
	ctx := context.Background()
	buyer, err := users.New(f.pool).Resolve(ctx, auth.Identity{
		UID: "exec-buyer-" + uuid.NewString()[:8], Email: uuid.NewString() + "@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := subtotal
	commission, err := money.Commission(subtotal, 500)
	if err != nil {
		t.Fatal(err)
	}
	var checkoutID, orderID uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO checkouts (buyer_id, idempotency_key, request_hash, base_pesewas,
		                        processing_fee_pesewas, charge_pesewas, status, expires_at)
		 VALUES ($1, $2, 'exec-http', $3, 0, $3, 'paid', now() + interval '30 minutes')
		 RETURNING id`, buyer.ID, uuid.New(), base).Scan(&checkoutID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO orders (checkout_id, buyer_id, seller_id, status, escrow_state,
		                     subtotal_pesewas, delivery_fee_pesewas, base_pesewas,
		                     commission_rate_bps, commission_pesewas, delivery_method)
		 VALUES ($1, $2, $3, 'completed', 'released', $4, 0, $4, 500, $5, 'pickup')
		 RETURNING id`,
		checkoutID, buyer.ID, sellerID, subtotal, commission).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		if err := f.books.Post(ctx, tx, "checkout_paid", orderID.String(),
			ledger.CheckoutPaid(base+150, base, 100, ledger.CheckoutOrder{OrderID: orderID, Base: base})...); err != nil {
			return err
		}
		return f.books.Post(ctx, tx, "escrow_release", orderID.String(),
			ledger.EscrowRelease(orderID, sellerID, base, commission)...)
	}); err != nil {
		t.Fatal(err)
	}
}

// signTransfer builds a signed transfer webhook body, like paystacktest does
// for charges.
func signTransfer(secret string, event string, data map[string]any) (body []byte, sig string) {
	envelope := map[string]any{"event": event, "data": data}
	raw, err := json.Marshal(envelope)
	if err != nil {
		panic(err)
	}
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write(raw)
	return raw, hex.EncodeToString(mac.Sum(nil))
}

// TestPayoutEndpoints_Balance proves the money split and that no account
// number appears in it.
func TestPayoutEndpoints_Balance(t *testing.T) {
	f := newExecPayoutFixture(t)
	sellerID := f.sellerWithAccount(t, "seller", "exec-bal-seller")
	f.releaseOrder(t, sellerID, 5000) // payable 4750

	req := jsonRequest(http.MethodGet, "/v1/seller/balance", "seller", "")
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("balance = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var balance api.SellerBalance
	if err := json.Unmarshal(w.Body.Bytes(), &balance); err != nil {
		t.Fatal(err)
	}
	if balance.Available.Amount != 4750 || balance.PaidOut.Amount != 0 || balance.InFlight.Amount != 0 {
		t.Errorf("balance = %+v", balance)
	}
	if strings.Contains(w.Body.String(), "0241234567") {
		t.Error("balance body contains the account number")
	}

	if _, err := f.pay.ExecuteSeller(context.Background(), sellerID); err != nil {
		t.Fatal(err)
	}
	req = jsonRequest(http.MethodGet, "/v1/seller/balance", "seller", "")
	w = serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("balance = %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &balance); err != nil {
		t.Fatal(err)
	}
	if balance.Available.Amount != 0 || balance.InFlight.Amount != 4750 {
		t.Errorf("after execute: %+v", balance)
	}

	req = jsonRequest(http.MethodGet, "/v1/seller/balance", "", "")
	if w := serve(t, f.router, req); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", w.Code)
	} else {
		assertContract(t, req, w)
	}
}

// TestPayoutEndpoints_HistoryAndAdminList proves the seller history, the
// admin list with filters, and the never-unmasked rule across them.
func TestPayoutEndpoints_HistoryAndAdminList(t *testing.T) {
	f := newExecPayoutFixture(t)
	sellerID := f.sellerWithAccount(t, "seller", "exec-hist-seller")
	otherID := f.sellerWithAccount(t, "other", "exec-hist-other")
	f.releaseOrder(t, sellerID, 5000)
	f.releaseOrder(t, otherID, 3000)

	mine, err := f.pay.ExecuteSeller(context.Background(), sellerID)
	if err != nil || mine == nil {
		t.Fatalf("execute = %v, %v", mine, err)
	}
	if _, err := f.pay.SendPayout(context.Background(), mine.ID); err != nil {
		t.Fatal(err)
	}

	bodies := []string{}
	req := jsonRequest(http.MethodGet, "/v1/seller/payouts", "seller", "")
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("history = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	bodies = append(bodies, w.Body.String())
	var history api.PayoutList
	if err := json.Unmarshal(w.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Items) != 1 || history.Meta.Total != 1 || history.Items[0].Status != api.PayoutStatusPending {
		t.Errorf("history = %+v", history)
	}
	otherReq := jsonRequest(http.MethodGet, "/v1/seller/payouts", "other", "")
	otherW := serve(t, f.router, otherReq)
	if otherW.Code != http.StatusOK {
		t.Fatalf("other history = %d", otherW.Code)
	}
	var otherHistory api.PayoutList
	if err := json.Unmarshal(otherW.Body.Bytes(), &otherHistory); err != nil {
		t.Fatal(err)
	}
	if len(otherHistory.Items) != 0 {
		t.Errorf("other seller sees %d payouts: %s", len(otherHistory.Items), otherW.Body.String())
	}

	f.verifier.mu.Lock()
	f.verifier.ids["admin"] = stubIdentity("exec-admin", "exec-admin@farmish.test", time.Now())
	f.verifier.mu.Unlock()
	admin, err := users.New(f.pool).Resolve(context.Background(), auth.Identity{
		UID: "exec-admin", Email: "exec-admin@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE users SET role = 'admin' WHERE id = $1`, admin.ID); err != nil {
		t.Fatal(err)
	}
	get := func(query string) api.PayoutList {
		req := jsonRequest(http.MethodGet, "/v1/admin/payouts"+query, "admin", "")
		w := serve(t, f.router, req)
		if w.Code != http.StatusOK {
			t.Fatalf("admin list%s = %d %s", query, w.Code, w.Body.String())
		}
		assertContract(t, req, w)
		bodies = append(bodies, w.Body.String())
		var list api.PayoutList
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		return list
	}
	if all := get(""); len(all.Items) != 1 {
		t.Errorf("admin list = %d items, want 1", len(all.Items))
	}
	if filtered := get("?status=pending"); len(filtered.Items) != 1 {
		t.Errorf("pending filter = %d items, want 1", len(filtered.Items))
	}
	if filtered := get("?status=success"); len(filtered.Items) != 0 {
		t.Errorf("success filter = %d items, want 0", len(filtered.Items))
	}
	if filtered := get("?sellerId=" + otherID.String()); len(filtered.Items) != 0 {
		t.Errorf("other seller filter = %d items, want 0", len(filtered.Items))
	}
	req = jsonRequest(http.MethodGet, "/v1/admin/payouts?status=bogus", "admin", "")
	if w := serve(t, f.router, req); w.Code != http.StatusBadRequest {
		t.Errorf("bad status = %d, want 400", w.Code)
	} else {
		assertContract(t, req, w)
	}
	req = jsonRequest(http.MethodGet, "/v1/admin/payouts", "seller", "")
	if w := serve(t, f.router, req); w.Code != http.StatusForbidden {
		t.Errorf("seller admin list = %d, want 403", w.Code)
	} else {
		assertContract(t, req, w)
	}
	for _, body := range bodies {
		if strings.Contains(body, "0241234567") {
			t.Errorf("payout body contains the account number: %s", body)
		}
	}
}

// TestPayoutEndpoints_Retry proves failed → fresh queued, live → 409,
// missing → 404, stranger → 403.
func TestPayoutEndpoints_Retry(t *testing.T) {
	f := newExecPayoutFixture(t)
	sellerID := f.sellerWithAccount(t, "seller", "exec-retry-seller")
	f.releaseOrder(t, sellerID, 5000)
	mine, err := f.pay.ExecuteSeller(context.Background(), sellerID)
	if err != nil || mine == nil {
		t.Fatalf("execute = %v, %v", mine, err)
	}
	if _, err := f.pay.SendPayout(context.Background(), mine.ID); err != nil {
		t.Fatal(err)
	}
	f.provider.SetTransferStatus(mine.Reference, "failed")
	if _, err := f.pay.SendPayout(context.Background(), mine.ID); err != nil {
		t.Fatal(err)
	}

	f.verifier.mu.Lock()
	f.verifier.ids["admin"] = stubIdentity("exec-retry-admin", "exec-retry-admin@farmish.test", time.Now())
	f.verifier.mu.Unlock()
	admin, err := users.New(f.pool).Resolve(context.Background(), auth.Identity{
		UID: "exec-retry-admin", Email: "exec-retry-admin@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE users SET role = 'admin' WHERE id = $1`, admin.ID); err != nil {
		t.Fatal(err)
	}
	req := jsonRequest(http.MethodPost, "/v1/admin/payouts/"+mine.ID.String()+"/retry", "admin", "")
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("retry = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var fresh api.Payout
	if err := json.Unmarshal(w.Body.Bytes(), &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.Id == mine.ID || fresh.Reference == mine.Reference || fresh.Status != api.PayoutStatusQueued {
		t.Errorf("retry = %+v, want a fresh queued row", fresh)
	}
	if strings.Contains(w.Body.String(), "0241234567") {
		t.Error("retry body contains the account number")
	}

	req = jsonRequest(http.MethodPost, "/v1/admin/payouts/"+fresh.Id.String()+"/retry", "admin", "")
	if w := serve(t, f.router, req); w.Code != http.StatusConflict {
		t.Errorf("retry live = %d, want 409", w.Code)
	} else {
		assertContract(t, req, w)
		assertErrorEnvelope(t, w.Body.Bytes(), apierror.CodeConflict)
	}
	req = jsonRequest(http.MethodPost, "/v1/admin/payouts/"+uuid.NewString()+"/retry", "admin", "")
	if w := serve(t, f.router, req); w.Code != http.StatusNotFound {
		t.Errorf("retry missing = %d, want 404", w.Code)
	} else {
		assertContract(t, req, w)
	}
	req = jsonRequest(http.MethodPost, "/v1/admin/payouts/"+mine.ID.String()+"/retry", "seller", "")
	if w := serve(t, f.router, req); w.Code != http.StatusForbidden {
		t.Errorf("seller retry = %d, want 403", w.Code)
	} else {
		assertContract(t, req, w)
	}
}

// TestPayoutEndpoints_WebhookRouted proves transfer.success settles through
// the real signed webhook route (the production registration), and a replay
// is ignored.
func TestPayoutEndpoints_WebhookRouted(t *testing.T) {
	f := newExecPayoutFixture(t)
	sellerID := f.sellerWithAccount(t, "seller", "exec-wh-seller")
	f.releaseOrder(t, sellerID, 5000)
	mine, err := f.pay.ExecuteSeller(context.Background(), sellerID)
	if err != nil || mine == nil {
		t.Fatalf("execute = %v, %v", mine, err)
	}
	if _, err := f.pay.SendPayout(context.Background(), mine.ID); err != nil {
		t.Fatal(err)
	}

	raw, sig := signTransfer(paystackSecret, "transfer.success", map[string]any{
		"id": 424242, "reference": mine.Reference, "amount": 4750, "currency": "GHS", "status": "success",
	})
	req := jsonRequest(http.MethodPost, "/v1/webhooks/paystack", "", string(raw))
	req.Header.Set("x-paystack-signature", sig)
	w := serve(t, f.router, req)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook = %d %s", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var status string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status FROM payouts WHERE id = $1`, mine.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != payouts.PayoutSuccess {
		t.Errorf("payout = %s, want success", status)
	}
	// Replay: same id, same answer, no second posting.
	req = jsonRequest(http.MethodPost, "/v1/webhooks/paystack", "", string(raw))
	req.Header.Set("x-paystack-signature", sig)
	if w := serve(t, f.router, req); w.Code != http.StatusOK {
		t.Fatalf("replay = %d", w.Code)
	}
	var postings int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM ledger_transactions WHERE kind = 'payout_succeeded'`).Scan(&postings); err != nil {
		t.Fatal(err)
	}
	if postings != 1 {
		t.Errorf("success postings = %d, want 1", postings)
	}
}
