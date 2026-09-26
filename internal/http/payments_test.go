package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/payments/fake"
	"github.com/dezmymachine/farmish-backend/internal/payments/paystacktest"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// paystackSecret is the webhook signing key for these tests.
const paystackSecret = "sk_test_webhook_secret"

// feeBps mirrors PAYSTACK_FEE_BPS (DOMAIN §2.2).
const paystackFeeBps = 195

type paymentsFixture struct {
	router  *gin.Engine
	pool    *pgxpool.Pool
	ps      *fake.Provider
	svc     *payments.Service
	seller  string
	buyer   string
	other   string
	mu      sync.Mutex
	applied int
}

// newPaymentsFixture wires the real router with a payments service over real
// Postgres, a fake provider, and a test purpose handler (Phase 14 registers a
// real one).
func newPaymentsFixture(t *testing.T, withSecret bool) *paymentsFixture {
	t.Helper()
	pool := dbtest.Pool(t)
	fb := authtest.Firebase(t)
	ps := fake.New()
	f := &paymentsFixture{pool: pool, ps: ps}
	f.svc = payments.New(pool, ps, slog.New(slog.DiscardHandler), paystackFeeBps, "https://farmish.gh/payments/status",
		payments.WithPurposeHandler(payments.PurposePromotion, f.applyPromotion))

	secret := ""
	if withSecret {
		secret = paystackSecret
	}
	deps := Deps{
		DB: fakePinger{}, Verifier: fb, Users: users.New(pool), Payments: f.svc,
	}
	f.router = newTestRouterWithConfig(t, deps, config.Config{
		Env: config.EnvTest, CORSOrigins: []string{"https://farmish.gh"},
		Paystack: config.Paystack{SecretKey: secret},
	})
	f.seller = userToken(t, pool, fb, "payments-seller")
	f.buyer = userToken(t, pool, fb, "payments-buyer")
	f.other = userToken(t, pool, fb, "payments-stranger")
	return f
}

func (f *paymentsFixture) applyPromotion(_ context.Context, _ pgx.Tx, p payments.Payment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied++
	return nil
}

func (f *paymentsFixture) appliedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applied
}

// userToken signs in a fresh emulator user and returns its token, resolving the
// user row the way a real sign-in does.
func userToken(t *testing.T, pool *pgxpool.Pool, fb *auth.Firebase, uid string) string {
	t.Helper()
	eu := authtest.EmailUser(t)
	id, err := fb.Verify(context.Background(), eu.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.New(pool).Resolve(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return eu.Token
}

// create makes a pending promotion payment for the fixture's buyer.
func (f *paymentsFixture) create(t *testing.T, base int64) payments.Payment {
	t.Helper()
	p, err := f.svc.Initialize(context.Background(), payments.CreateInput{
		UserID: userIDFor(t, f.pool, f.buyer), Purpose: payments.PurposePromotion,
		PurposeRef: "vip", BasePesewas: base,
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return p
}

// postWebhook posts a body to the webhook route, signed when a signature is
// given.
func postWebhook(r http.Handler, body []byte, signature string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/paystack", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set(paystacktest.SignatureHeader, signature)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func (f *paymentsFixture) eventRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM webhook_events WHERE provider = 'paystack'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *paymentsFixture) status(t *testing.T, reference string) string {
	t.Helper()
	var status string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status FROM payments WHERE reference = $1`, reference).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestWebhook_BadSignature401(t *testing.T) {
	f := newPaymentsFixture(t, true)
	p := f.create(t, 10_000)
	body := paystacktest.ChargeSuccessBody(p.Reference, p.Charge, 1234)
	good := paystacktest.Sign(paystackSecret, body)

	for _, tt := range []struct {
		name      string
		signature string
	}{
		{"no signature at all", ""},
		{"wrong secret", paystacktest.Sign("sk_test_wrong_secret", body)},
		{"signature of a different body", paystacktest.Sign(paystackSecret, []byte(`{"event":"charge.success"}`))},
		{"not hex", "zzzz"},
		{"truncated digest", good[:len(good)-2]},
		// Paystack sends lowercase hex. Uppercase decodes to the same bytes, so
		// it verifies: the comparison is over the digest, not the encoding.
		{"prefixed with W/", "W/" + good},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := postWebhook(f.router, body, tt.signature)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d %s, want 401", w.Code, w.Body.String())
			}
			var env api.Error
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Error.Code != "unauthorized" {
				t.Errorf("body = %s, want an unauthorized envelope", w.Body.String())
			}
		})
	}

	// Nothing was recorded and no payment moved.
	if n := f.eventRows(t); n != 0 {
		t.Errorf("webhook_events = %d, want 0: a rejected signature stores nothing", n)
	}
	if got := f.status(t, p.Reference); got != payments.StatusPending {
		t.Errorf("payment status = %q, want pending", got)
	}
	if f.appliedCount() != 0 {
		t.Error("the purpose handler ran for an unverified webhook")
	}

	// The correctly signed body still works afterwards, so the refusals were
	// about the signature and nothing else. Uppercase hex is the same digest.
	w := postWebhook(f.router, body, strings.ToUpper(good))
	if w.Code != http.StatusOK {
		t.Fatalf("signed body = %d %s, want 200", w.Code, w.Body.String())
	}
	if got := f.status(t, p.Reference); got != payments.StatusSuccess {
		t.Errorf("payment status = %q, want success", got)
	}
}

// TestWebhook_AmountMismatchIsRejectedNotRetried pins the rule that a business
// rejection answers 200: asking Paystack to retry a mismatch would not help.
func TestWebhook_AmountMismatchIsRejectedNotRetried(t *testing.T) {
	f := newPaymentsFixture(t, true)
	p := f.create(t, 10_000)
	body := paystacktest.ChargeSuccessBody(p.Reference, p.Charge+1, 4321)
	w := postWebhook(f.router, body, paystacktest.Sign(paystackSecret, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200 for a rejection", w.Code, w.Body.String())
	}
	if got := f.status(t, p.Reference); got != payments.StatusPending {
		t.Errorf("payment status = %q, want it left pending", got)
	}
	if f.appliedCount() != 0 {
		t.Error("the purpose handler ran for a mismatched payment")
	}
}

func TestWebhook_MalformedBodyIs400(t *testing.T) {
	f := newPaymentsFixture(t, true)
	body := []byte("not json at all")
	w := postWebhook(f.router, body, paystacktest.Sign(paystackSecret, body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d %s, want 400", w.Code, w.Body.String())
	}
	if n := f.eventRows(t); n != 0 {
		t.Errorf("webhook_events = %d, want 0", n)
	}
}

// TestWebhook_NoSecretFailsClosed is the misconfiguration case: with no secret
// nothing can be verified, so nothing is acted on.
func TestWebhook_NoSecretFailsClosed(t *testing.T) {
	f := newPaymentsFixture(t, false)
	p := f.create(t, 1000)
	body := paystacktest.ChargeSuccessBody(p.Reference, p.Charge, 7)
	w := postWebhook(f.router, body, paystacktest.Sign("anything", body))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d %s, want 503 when no secret is configured", w.Code, w.Body.String())
	}
	if got := f.status(t, p.Reference); got != payments.StatusPending {
		t.Errorf("payment status = %q, want pending", got)
	}
}

func TestWebhook_ChargeSuccessProcessesOnce(t *testing.T) {
	f := newPaymentsFixture(t, true)
	f.startJobs(t)
	p := f.create(t, 10_000)
	body := paystacktest.ChargeSuccessBody(p.Reference, p.Charge, 20260926)
	signature := paystacktest.Sign(paystackSecret, body)

	w := postWebhook(f.router, body, signature)
	if w.Code != http.StatusOK {
		t.Fatalf("first delivery = %d %s, want 200", w.Code, w.Body.String())
	}
	if got := f.status(t, p.Reference); got != payments.StatusSuccess {
		t.Fatalf("payment status = %q, want success", got)
	}
	waitForAppliedHTTP(t, f, 1)

	// Replaying the identical body is a no-op.
	for i := range 2 {
		w = postWebhook(f.router, body, signature)
		if w.Code != http.StatusOK {
			t.Fatalf("replay %d = %d %s, want 200", i, w.Code, w.Body.String())
		}
	}
	if n := f.eventRows(t); n != 1 {
		t.Errorf("webhook_events = %d, want 1 after three deliveries", n)
	}
	if got := f.appliedCount(); got != 1 {
		t.Errorf("purpose handler ran %d times, want 1", got)
	}
	if got := f.status(t, p.Reference); got != payments.StatusSuccess {
		t.Errorf("payment status = %q, want it still settled exactly once", got)
	}
}

func TestGetPaymentStatus_OwnerOnly(t *testing.T) {
	f := newPaymentsFixture(t, true)
	p := f.create(t, 12_345)

	t.Run("anonymous is 401", func(t *testing.T) {
		req := jsonRequest(http.MethodGet, "/v1/payments/"+p.Reference, "", "")
		w := serve(t, f.router, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d %s, want 401", w.Code, w.Body.String())
		}
		assertContract(t, req, w)
	})

	t.Run("another user is 404", func(t *testing.T) {
		req := jsonRequest(http.MethodGet, "/v1/payments/"+p.Reference, f.other, "")
		w := serve(t, f.router, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d %s, want 404", w.Code, w.Body.String())
		}
		assertContract(t, req, w)
	})

	t.Run("an unknown reference is the same 404", func(t *testing.T) {
		req := jsonRequest(http.MethodGet, "/v1/payments/FMS-NOT-A-REAL-REFERENCE", f.other, "")
		w := serve(t, f.router, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d %s, want 404", w.Code, w.Body.String())
		}
	})

	t.Run("the owner sees the amounts", func(t *testing.T) {
		req := jsonRequest(http.MethodGet, "/v1/payments/"+p.Reference, f.buyer, "")
		w := serve(t, f.router, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d %s", w.Code, w.Body.String())
		}
		assertContract(t, req, w)
		var got api.PaymentStatus
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Reference != p.Reference || got.Status != api.PaymentStatusStatusPending {
			t.Errorf("status = %+v", got)
		}
		// base 12345 at 195bps grosses up to 12591/246.
		if got.Base.Amount != 12_345 || got.Charge.Amount != 12_591 || got.ProcessingFee.Amount != 246 {
			t.Errorf("amounts = base %d, charge %d, fee %d; want 12345/12591/246",
				got.Base.Amount, got.Charge.Amount, got.ProcessingFee.Amount)
		}
		if got.Charge.Currency != api.MoneyCurrencyGHS {
			t.Errorf("currency = %v", got.Charge.Currency)
		}
		if got.PaidAt != nil {
			t.Errorf("paidAt = %v, want it absent while pending", got.PaidAt)
		}
		// Nothing internal leaks: no provider id, no authorization URL.
		if strings.Contains(w.Body.String(), "authorization") ||
			strings.Contains(w.Body.String(), "provider") {
			t.Errorf("body leaks internals: %s", w.Body.String())
		}
	})

	t.Run("a settled payment reports paidAt", func(t *testing.T) {
		body := paystacktest.ChargeSuccessBody(p.Reference, p.Charge, 999)
		if w := postWebhook(f.router, body, paystacktest.Sign(paystackSecret, body)); w.Code != http.StatusOK {
			t.Fatalf("webhook = %d %s", w.Code, w.Body.String())
		}
		req := jsonRequest(http.MethodGet, "/v1/payments/"+p.Reference, f.buyer, "")
		w := serve(t, f.router, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d %s", w.Code, w.Body.String())
		}
		var got api.PaymentStatus
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Status != api.PaymentStatusStatusSuccess || got.PaidAt == nil {
			t.Errorf("status = %+v, want success with paidAt", got)
		}
	})
}

// TestGetPaymentStatus_ProviderDownIs502 covers the verify fallback failing: the
// buyer is told to retry rather than shown a wrong status.
func TestGetPaymentStatus_ProviderDownIs502(t *testing.T) {
	f := newPaymentsFixture(t, true)
	p := f.create(t, 1000)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE payments SET created_at = now() - interval '1 minute' WHERE reference = $1`,
		p.Reference); err != nil {
		t.Fatal(err)
	}
	f.ps.VerifyErr = errProviderDown{}

	req := jsonRequest(http.MethodGet, "/v1/payments/"+p.Reference, f.buyer, "")
	w := serve(t, f.router, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d %s, want 502", w.Code, w.Body.String())
	}
	assertContract(t, req, w)
	var env api.Error
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Error.Code != "payment_provider_error" {
		t.Errorf("body = %s, want a payment_provider_error envelope", w.Body.String())
	}
	if got := f.status(t, p.Reference); got != payments.StatusPending {
		t.Errorf("payment status = %q, want it left pending", got)
	}
}

// errProviderDown is a provider transport failure.
type errProviderDown struct{}

func (errProviderDown) Error() string { return "paystack: unavailable: dial tcp: connection refused" }

func (errProviderDown) Is(target error) bool {
	return target == payments.ErrProviderUnavailable
}

// startJobs runs payments.succeeded through a real River client, so the test
// proves the enqueue and the worker rather than a stub.
func (f *paymentsFixture) startJobs(t *testing.T) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	reg := jobs.NewRegistry()
	payments.RegisterSucceeded(reg, f.svc, log)
	client, err := jobs.NewClient(f.pool, reg, log, jobs.Options{
		Work: true, FetchPollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.svc.AttachJobClient(client)
	t.Cleanup(func() {
		if err := jobs.Stop(client, 5*time.Second, 2*time.Second, log); err != nil {
			t.Errorf("stop jobs: %v", err)
		}
	})
}

func waitForAppliedHTTP(t *testing.T, f *paymentsFixture, want int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if f.appliedCount() == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("purpose handler ran %d times, want %d", f.appliedCount(), want)
}
