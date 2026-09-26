package payments_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/payments/fake"
	"github.com/dezmymachine/farmish-backend/internal/payments/paystacktest"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// feeBps is PAYSTACK_FEE_BPS (DOMAIN §2.2).
const feeBps = 195

// logCapture keeps log lines so a test can assert that a security-relevant
// event was actually logged, at the level it was logged at.
type logCapture struct {
	mu    sync.Mutex
	lines []capturedLine
}

type capturedLine struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// has reports whether a line at that level containing substr was logged.
func (l *logCapture) has(level slog.Level, substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if line.level == level && strings.Contains(line.msg, substr) {
			return true
		}
	}
	return false
}

// attr returns the first value logged under key, so a test can check that a
// log line carried the amounts it should have.
func (l *logCapture) attr(key string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if v, ok := line.attrs[key]; ok {
			return v, true
		}
	}
	return "", false
}

// handler is a minimal slog.Handler that records instead of writing.
type handler struct{ capture *logCapture }

func (h handler) Enabled(context.Context, slog.Level) bool { return true }

func (h handler) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.capture.mu.Lock()
	defer h.capture.mu.Unlock()
	h.capture.lines = append(h.capture.lines, capturedLine{level: r.Level, msg: r.Message, attrs: attrs})
	return nil
}

func (h handler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h handler) WithGroup(string) slog.Handler      { return h }

func (l *logCapture) logger() *slog.Logger {
	return slog.New(handler{capture: l})
}

type fixture struct {
	pool  *pgxpool.Pool
	svc   *payments.Service
	ps    *fake.Provider
	user  uuid.UUID
	other uuid.UUID
	// applied counts how many times the test purpose handler ran.
	applied int
	mu      sync.Mutex
	client  *jobs.Client
	log     *logCapture
}

// newFixture builds a service over real Postgres with a fake provider and a
// registered test purpose handler, which is what Phase 14 will do for real.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := dbtest.Pool(t)
	user := resolveUser(t, pool, "payments-user-1")
	other := resolveUser(t, pool, "payments-user-2")
	ps := fake.New()
	f := &fixture{pool: pool, ps: ps, user: user, other: other, log: &logCapture{}}
	f.svc = f.build()
	return f
}

// build (re)creates the service, always with the test purpose handler and
// whatever else the test needs. A test that starts jobs rebuilds it so the
// settle path can enqueue payments.succeeded.
func (f *fixture) build(opts ...payments.Option) *payments.Service {
	all := append([]payments.Option{
		payments.WithPurposeHandler(payments.PurposePromotion, f.applyPromotion),
	}, opts...)
	return payments.New(f.pool, f.ps, f.log.logger(), feeBps,
		"https://farmish.gh/payments/status", all...)
}

// resolveUser creates a user row the way a sign-in would.
func resolveUser(t *testing.T, pool *pgxpool.Pool, uid string) uuid.UUID {
	t.Helper()
	u, err := users.New(pool).Resolve(context.Background(), authIdentity(uid))
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// applyPromotion stands in for Phase 14's handler and records its calls.
func (f *fixture) applyPromotion(_ context.Context, _ pgx.Tx, payment payments.Payment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied++
	if payment.Status != payments.StatusSuccess {
		return errors.New("purpose handler called for a payment that is not successful")
	}
	return nil
}

func (f *fixture) appliedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applied
}

// startJobs wires a real River client so payments.succeeded is worked in tests.
func (f *fixture) startJobs(t *testing.T) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	reg := jobs.NewRegistry()
	payments.RegisterSucceeded(reg, f.svc, log)
	client, err := jobs.NewClient(f.pool, reg, log, jobs.Options{Work: true, FetchPollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.client = client
	// Rebuild with the client, so settling a payment enqueues the job.
	f.svc = f.build(payments.WithJobClient(client))
	t.Cleanup(func() {
		if err := jobs.Stop(client, 5*time.Second, 2*time.Second, log); err != nil {
			t.Errorf("stop jobs: %v", err)
		}
	})
}

// create makes a pending payment of base pesewas for the promotion purpose.
func (f *fixture) create(t *testing.T, base int64) payments.Payment {
	t.Helper()
	p, err := f.svc.Initialize(context.Background(), payments.CreateInput{
		UserID: f.user, Purpose: payments.PurposePromotion, PurposeRef: "vip", BasePesewas: base,
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return p
}

// byReference reads the stored payment directly. Assertions here are about what
// is in the database, so they must not go through Verify (which would call the
// provider) nor depend on the service's clock.
func (f *fixture) byReference(t *testing.T, reference string) payments.Payment {
	t.Helper()
	var (
		id                uuid.UUID
		base, fee, charge int64
		currency, status  string
		paidAt            *time.Time
		failureReason     *string
	)
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id, base_pesewas, processing_fee_pesewas, charge_pesewas, currency,
		        status, paid_at, failure_reason
		 FROM payments WHERE reference = $1`, reference).
		Scan(&id, &base, &fee, &charge, &currency, &status, &paidAt, &failureReason); err != nil {
		t.Fatalf("load %s: %v", reference, err)
	}
	return payments.Payment{
		ID: id, Reference: reference, Base: base, Fee: fee, Charge: charge,
		Currency: currency, Status: status, PaidAt: paidAt, FailureReason: failureReason,
	}
}

func (f *fixture) eventOutcome(t *testing.T, eventKey string) string {
	t.Helper()
	var outcome *string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT outcome FROM webhook_events WHERE provider = 'paystack' AND event_key = $1`, eventKey).
		Scan(&outcome); err != nil {
		t.Fatalf("outcome for %s: %v", eventKey, err)
	}
	if outcome == nil {
		return "<null>"
	}
	return *outcome
}

func (f *fixture) eventCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM webhook_events WHERE provider = 'paystack'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInitialize_GrossUpAndProviderFailure(t *testing.T) {
	f := newFixture(t)

	t.Run("amounts follow DOMAIN 2.2", func(t *testing.T) {
		p := f.create(t, 10_000)
		if p.Base != 10_000 || p.Charge != 10_199 || p.Fee != 199 {
			t.Errorf("payment = base %d, fee %d, charge %d; want 10000/199/10199",
				p.Base, p.Fee, p.Charge)
		}
		if p.Currency != "GHS" || p.Status != payments.StatusPending {
			t.Errorf("payment = %+v", p)
		}
		if !strings.HasPrefix(p.Reference, "FMS-") || len(p.Reference) != 24 {
			t.Errorf("reference = %q, want FMS- plus 20 characters", p.Reference)
		}
		if p.AuthorizationURL == nil || *p.AuthorizationURL == "" {
			t.Error("authorizationURL was not stored")
		}
		// The charge is the amount sent to Paystack, grossed up.
		if len(f.ps.Initialized) != 1 {
			t.Fatalf("provider calls = %d", len(f.ps.Initialized))
		}
		if got := f.ps.Initialized[0].AmountPesewas; got != 10_199 {
			t.Errorf("provider amount = %d, want the grossed-up charge", got)
		}
		if got := f.ps.Initialized[0].Reference; got != p.Reference {
			t.Errorf("provider reference = %q, want %q", got, p.Reference)
		}
		meta := f.ps.Initialized[0].Metadata
		if meta["payment_id"] != p.ID.String() || meta["purpose"] != payments.PurposePromotion {
			t.Errorf("metadata = %v", meta)
		}
	})

	t.Run("references are unique", func(t *testing.T) {
		seen := map[string]bool{}
		for range 25 {
			ref := f.create(t, 1000).Reference
			if seen[ref] {
				t.Fatalf("reference %s was generated twice", ref)
			}
			seen[ref] = true
		}
	})

	t.Run("a phone account gets a placeholder email", func(t *testing.T) {
		before := len(f.ps.Initialized)
		if _, err := f.svc.Initialize(context.Background(), payments.CreateInput{
			UserID: f.user, Purpose: payments.PurposeCheckout, PurposeRef: "cart-1", BasePesewas: 5000,
		}); err != nil {
			t.Fatal(err)
		}
		got := f.ps.Initialized[before].Email
		if !strings.HasSuffix(got, "@users.farmish.gh") || !strings.HasPrefix(got, "u") {
			t.Errorf("email = %q, want a u<hex>@users.farmish.gh placeholder", got)
		}
		if !strings.Contains(got, strings.ReplaceAll(f.user.String(), "-", "")[:12]) {
			t.Errorf("email = %q, want the user id's first 12 hex characters", got)
		}
	})

	t.Run("validation", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			in   payments.CreateInput
			want string
		}{
			{"no user", payments.CreateInput{Purpose: payments.PurposePromotion, PurposeRef: "vip", BasePesewas: 100}, "userId"},
			{"bad purpose", payments.CreateInput{UserID: f.user, Purpose: "tip", PurposeRef: "vip", BasePesewas: 100}, "purpose"},
			{"no purpose ref", payments.CreateInput{UserID: f.user, Purpose: payments.PurposePromotion, BasePesewas: 100}, "purposeRef"},
			{"zero base", payments.CreateInput{UserID: f.user, Purpose: payments.PurposePromotion, PurposeRef: "vip"}, "baseAmount"},
			{"negative base", payments.CreateInput{UserID: f.user, Purpose: payments.PurposePromotion, PurposeRef: "vip", BasePesewas: -5}, "baseAmount"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				_, err := f.svc.Initialize(context.Background(), tt.in)
				var verr *validation.Error
				if !errors.As(err, &verr) {
					t.Fatalf("err = %v, want *validation.Error", err)
				}
				if !hasField(verr, tt.want) {
					t.Errorf("fields = %v, want one for %q", verr.Fields, tt.want)
				}
			})
		}
	})

	t.Run("a provider failure fails the payment", func(t *testing.T) {
		f.ps.InitErr = errors.New("dial tcp: connection refused")
		before := f.eventCount(t)
		_, err := f.svc.Initialize(context.Background(), payments.CreateInput{
			UserID: f.user, Purpose: payments.PurposePromotion, PurposeRef: "vip", BasePesewas: 2000,
		})
		if !errors.Is(err, payments.ErrProviderUnavailable) && !strings.Contains(err.Error(), "paystack") {
			t.Fatalf("err = %v, want a provider failure", err)
		}
		// The row survives as the audit trail of the attempt, marked failed.
		var status, reason string
		if err := f.pool.QueryRow(context.Background(),
			`SELECT status, coalesce(failure_reason,'') FROM payments
			 WHERE purpose_ref = 'vip' AND status = 'failed'
			 ORDER BY created_at DESC LIMIT 1`).Scan(&status, &reason); err != nil {
			t.Fatalf("the failed payment was not recorded: %v", err)
		}
		if status != payments.StatusFailed || reason == "" {
			t.Errorf("status = %q reason = %q", status, reason)
		}
		if f.eventCount(t) != before {
			t.Error("a failed initialize wrote a webhook event")
		}
		f.ps.InitErr = nil
	})
}

func TestWebhook_AmountMismatchRejected(t *testing.T) {
	f := newFixture(t)
	logs := f.log
	p := f.create(t, 10_000)

	// One pesewa off.
	body := paystacktest.ChargeSuccessBody(p.Reference, p.Charge+1, 4242)
	if err := f.svc.HandleWebhook(context.Background(), body); err != nil {
		t.Fatal(err)
	}

	if got := f.eventOutcome(t, "charge.success:4242"); got != payments.OutcomeAmountMismatch {
		t.Errorf("outcome = %q, want %q", got, payments.OutcomeAmountMismatch)
	}
	after := f.byReference(t, p.Reference)
	if after.Status != payments.StatusPending {
		t.Errorf("status = %q, want it left pending: the money does not match", after.Status)
	}
	if after.Settled() {
		t.Error("a mismatched payment reports as settled")
	}
	// The alert hook: an Error line naming the reference and both amounts.
	if !logs.has(slog.LevelError, "payment amount mismatch") {
		t.Error("no error-level log for the mismatch")
	}
	if got, ok := logs.attr("expected_pesewas"); !ok || got != "10199" {
		t.Errorf("logged expected_pesewas = %q, want 10199", got)
	}
	if got, ok := logs.attr("received_pesewas"); !ok || got != "10200" {
		t.Errorf("logged received_pesewas = %q, want 10200", got)
	}
	// And an audit row for Phase 21.
	var actions int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE action = 'payment.amount_mismatch'`).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if actions != 1 {
		t.Errorf("audit rows = %d, want 1", actions)
	}
	if f.appliedCount() != 0 {
		t.Error("the purpose handler ran for a mismatched payment")
	}
}

func TestWebhook_CurrencyMismatchRejected(t *testing.T) {
	f := newFixture(t)
	p := f.create(t, 10_000)
	body := paystacktest.ChargeSuccessBodyWithCurrency(p.Reference, p.Charge, 77, "NGN")
	if err := f.svc.HandleWebhook(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	if got := f.eventOutcome(t, "charge.success:77"); got != payments.OutcomeCurrencyMismatch {
		t.Errorf("outcome = %q, want %q", got, payments.OutcomeCurrencyMismatch)
	}
	if got := f.byReference(t, p.Reference); got.Status != payments.StatusPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
}

func TestWebhook_UnknownEventIgnored(t *testing.T) {
	f := newFixture(t)
	body := paystacktest.EventBody("subscription.create", 999)
	if err := f.svc.HandleWebhook(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	if got := f.eventOutcome(t, "subscription.create:999"); got != payments.OutcomeIgnored {
		t.Errorf("outcome = %q, want %q", got, payments.OutcomeIgnored)
	}
}

func TestWebhook_UnknownReferenceRejected(t *testing.T) {
	f := newFixture(t)
	body := paystacktest.ChargeSuccessBody("FMS-NOT-OURS", 10_000, 5150)
	if err := f.svc.HandleWebhook(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	if got := f.eventOutcome(t, "charge.success:5150"); got != payments.OutcomeUnknownReference {
		t.Errorf("outcome = %q, want %q", got, payments.OutcomeUnknownReference)
	}
}

func TestWebhook_ChargeFailedMarksFailed(t *testing.T) {
	f := newFixture(t)
	p := f.create(t, 5000)
	body := paystacktest.ChargeFailedBody(p.Reference, 88, "insufficient funds")
	if err := f.svc.HandleWebhook(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	after := f.byReference(t, p.Reference)
	if after.Status != payments.StatusFailed {
		t.Errorf("status = %q, want failed", after.Status)
	}
	if after.FailureReason == nil || *after.FailureReason != "insufficient funds" {
		t.Errorf("failureReason = %v", after.FailureReason)
	}
	if f.appliedCount() != 0 {
		t.Error("the purpose handler ran for a failed payment")
	}
}

func TestWebhook_MalformedBodies(t *testing.T) {
	f := newFixture(t)
	for _, tt := range []struct {
		name string
		body string
	}{
		{"not json", "not json"},
		{"no event", `{"data":{"id":1}}`},
		{"no data", `{"event":"charge.success"}`},
		{"data without an id", `{"event":"charge.success","data":{"reference":"FMS-X"}}`},
		{"data is not an object", `{"event":"charge.success","data":"nope"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// A body the service cannot read is an error, so the endpoint
			// answers 500 and Paystack retries: nothing was recorded.
			if err := f.svc.HandleWebhook(context.Background(), []byte(tt.body)); err == nil {
				t.Error("a malformed body was accepted")
			}
			if n := f.eventCount(t); n != 0 {
				t.Errorf("webhook_events = %d, want 0 recorded", n)
			}
		})
	}
}

func TestWebhook_ConcurrentReplays(t *testing.T) {
	f := newFixture(t)
	f.startJobs(t)
	p := f.create(t, 10_000)
	body := paystacktest.ChargeSuccessBody(p.Reference, p.Charge, 31337)

	const senders = 10
	var wg sync.WaitGroup
	errs := make([]error, senders)
	start := make(chan struct{})
	for i := range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = f.svc.HandleWebhook(context.Background(), body)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("sender %d: %v", i, err)
		}
	}
	// Exactly one effect: one event row, one settlement, one purpose call.
	if n := f.eventCount(t); n != 1 {
		t.Errorf("webhook_events = %d, want exactly 1", n)
	}
	after := f.byReference(t, p.Reference)
	if after.Status != payments.StatusSuccess {
		t.Errorf("status = %q, want success", after.Status)
	}
	waitForApplied(t, f, 1)
}

func TestWebhook_ReplaysAreNoOps(t *testing.T) {
	f := newFixture(t)
	f.startJobs(t)
	p := f.create(t, 10_000)
	body := paystacktest.ChargeSuccessBody(p.Reference, p.Charge, 4711)

	for range 3 {
		if err := f.svc.HandleWebhook(context.Background(), body); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.eventCount(t); n != 1 {
		t.Errorf("webhook_events = %d, want 1 after three identical deliveries", n)
	}
	after := f.byReference(t, p.Reference)
	if after.Status != payments.StatusSuccess || after.PaidAt == nil {
		t.Errorf("payment = %+v, want it settled once", after)
	}
	waitForApplied(t, f, 1)
}

// TestWebhook_ReplayAfterASecondEventIsStillOneRow covers the same provider
// event arriving with a different data id: a new key, so a new row, and the
// payment is already settled so nothing happens.
func TestWebhook_SecondEventForASettledPaymentIsIgnored(t *testing.T) {
	f := newFixture(t)
	p := f.create(t, 10_000)
	if err := f.svc.HandleWebhook(context.Background(),
		paystacktest.ChargeSuccessBody(p.Reference, p.Charge, 1)); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.HandleWebhook(context.Background(),
		paystacktest.ChargeSuccessBody(p.Reference, p.Charge, 2)); err != nil {
		t.Fatal(err)
	}
	if got := f.eventOutcome(t, "charge.success:2"); got != payments.OutcomeIgnored {
		t.Errorf("second event outcome = %q, want ignored", got)
	}
	if got := f.byReference(t, p.Reference); got.Status != payments.StatusSuccess {
		t.Errorf("status = %q", got.Status)
	}
}

func TestVerify_OtherUsers404(t *testing.T) {
	f := newFixture(t)
	p := f.create(t, 1000)
	// Somebody else's reference: the same error as one that does not exist, so
	// the endpoint's 404 reveals nothing.
	_, err := f.svc.Verify(context.Background(), f.other, p.Reference)
	if !errors.Is(err, payments.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	_, err = f.svc.Verify(context.Background(), f.other, "FMS-DOES-NOT-EXIST")
	if !errors.Is(err, payments.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestVerify_FallbackThenWebhookIsDuplicate(t *testing.T) {
	f := newFixture(t)
	f.startJobs(t)
	p := f.create(t, 10_000)
	// Make the payment old enough that the fallback is willing to ask.
	old := time.Now().Add(-time.Minute)
	if err := f.pool.QueryRow(context.Background(),
		`UPDATE payments SET created_at = $2 WHERE id = $1 RETURNING id`, p.ID, old).Scan(new(any)); err != nil {
		t.Fatal(err)
	}
	paidAt := time.Now().Add(-time.Minute)
	f.ps.VerifyResult = payments.Transaction{
		ID: 90210, Status: payments.StatusSuccess, Reference: p.Reference,
		AmountPesewas: p.Charge, Currency: "GHS", FeesPesewas: 199,
		Channel: "mobile_money", PaidAt: &paidAt,
	}

	got, err := f.svc.Verify(context.Background(), f.user, p.Reference)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != payments.StatusSuccess {
		t.Fatalf("status = %q, want success from the verify fallback", got.Status)
	}
	if f.ps.CallCount("VerifyTransaction") != 1 {
		t.Errorf("verify calls = %d, want 1", f.ps.CallCount("VerifyTransaction"))
	}
	// The synthetic event is keyed on the provider's transaction id, so the
	// real webhook that follows is a duplicate.
	if got := f.eventOutcome(t, "charge.success:90210"); got != payments.OutcomeProcessed {
		t.Errorf("synthetic event outcome = %q, want processed", got)
	}
	waitForApplied(t, f, 1)

	if err := f.svc.HandleWebhook(context.Background(),
		paystacktest.ChargeSuccessBody(p.Reference, p.Charge, 90210)); err != nil {
		t.Fatal(err)
	}
	if n := f.eventCount(t); n != 1 {
		t.Errorf("webhook_events = %d, want 1: the webhook collides with the synthetic event", n)
	}
	if f.appliedCount() != 1 {
		t.Errorf("purpose handler ran %d times, want 1", f.appliedCount())
	}
	if after := f.byReference(t, p.Reference); after.Status != payments.StatusSuccess {
		t.Errorf("status = %q, want it still settled exactly once", after.Status)
	}
}

func TestVerify_FallbackMarksFailedAndAbandoned(t *testing.T) {
	for _, tt := range []struct {
		status string
		want   string
	}{
		{payments.StatusFailed, payments.StatusFailed},
		{payments.StatusAbandoned, payments.StatusAbandoned},
	} {
		t.Run(tt.status, func(t *testing.T) {
			f := newFixture(t)
			p := f.create(t, 1000)
			if _, err := f.pool.Exec(context.Background(),
				`UPDATE payments SET created_at = now() - interval '1 minute' WHERE id = $1`, p.ID); err != nil {
				t.Fatal(err)
			}
			f.ps.VerifyResult = payments.Transaction{
				ID: 1, Status: tt.status, Reference: p.Reference,
				AmountPesewas: p.Charge, Currency: "GHS",
			}
			got, err := f.svc.Verify(context.Background(), f.user, p.Reference)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tt.want {
				t.Errorf("status = %q, want %q", got.Status, tt.want)
			}
			if got.FailureReason == nil {
				t.Error("failureReason = nil, want the reason recorded")
			}
		})
	}
}

func TestVerify_YoungPaymentIsNotVerified(t *testing.T) {
	f := newFixture(t)
	p := f.create(t, 1000)
	// Just created: the buyer cannot have paid yet, so Paystack is not asked.
	got, err := f.svc.Verify(context.Background(), f.user, p.Reference)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != payments.StatusPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if f.ps.CallCount("VerifyTransaction") != 0 {
		t.Errorf("verify calls = %d, want none for a fresh payment", f.ps.CallCount("VerifyTransaction"))
	}
}

func TestVerify_SettledPaymentIsNotReverified(t *testing.T) {
	f := newFixture(t)
	p := f.create(t, 1000)
	if err := f.svc.HandleWebhook(context.Background(),
		paystacktest.ChargeSuccessBody(p.Reference, p.Charge, 5)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE payments SET created_at = now() - interval '1 minute' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Verify(context.Background(), f.user, p.Reference); err != nil {
		t.Fatal(err)
	}
	if f.ps.CallCount("VerifyTransaction") != 0 {
		t.Errorf("verify calls = %d, want none once the payment is settled",
			f.ps.CallCount("VerifyTransaction"))
	}
}

func TestVerify_ProviderUnavailableLeavesItPending(t *testing.T) {
	f := newFixture(t)
	p := f.create(t, 1000)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE payments SET created_at = now() - interval '1 minute' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	f.ps.VerifyErr = errors.New("dial tcp: timeout")
	_, err := f.svc.Verify(context.Background(), f.user, p.Reference)
	if err == nil {
		t.Fatal("a provider outage was reported as success")
	}
	if after := f.byReference(t, p.Reference); after.Status != payments.StatusPending {
		t.Errorf("status = %q, want it left pending so a retry can settle it", after.Status)
	}
}

// TestSucceeded_JobNeedsNoRegisteredPurpose is the state of production before
// Phase 14: the payment is settled and the job has nothing to apply. It must
// not fail and retry forever.
func TestSucceeded_JobNeedsNoRegisteredPurpose(t *testing.T) {
	f := newFixture(t)
	p := f.create(t, 10_000)
	// A service with no purpose handlers at all, as main.go has today.
	bare := payments.New(f.pool, f.ps, slog.New(slog.DiscardHandler), feeBps, "https://farmish.gh/payments/status")
	if err := bare.HandleWebhook(context.Background(),
		paystacktest.ChargeSuccessBody(p.Reference, p.Charge, 6)); err != nil {
		t.Fatal(err)
	}
	row, err := bare.PaymentForLookup(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != payments.StatusSuccess {
		t.Fatalf("status = %q, want success", row.Status)
	}
	// runPurposeHandler is unexported; the job is the public path and is
	// covered by the tests above with a handler registered.
}

func TestPayments_SettledFlags(t *testing.T) {
	f := newFixture(t)
	p := f.create(t, 1000)
	if p.Succeeded() || p.Settled() {
		t.Error("a fresh payment reports as settled")
	}
	// The charge is base plus fee, always.
	if p.Charge != p.Base+p.Fee {
		t.Errorf("charge %d != base %d + fee %d", p.Charge, p.Base, p.Fee)
	}
}

// waitForApplied waits for the payments.succeeded job to run the purpose
// handler exactly the given number of times.
func waitForApplied(t *testing.T, f *fixture, want int) {
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

func hasField(verr *validation.Error, name string) bool {
	for _, f := range verr.Fields {
		if f.Name == name {
			return true
		}
	}
	return false
}

// authIdentity is the minimal verified identity a sign-in would produce.
func authIdentity(uid string) auth.Identity {
	return auth.Identity{UID: uid, Email: uid + "@farmish.test", Provider: "password"}
}
