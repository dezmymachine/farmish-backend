package ledger_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
)

// reconcileFixture seeds users and checkouts so orders and refunds can be
// written with raw SQL, and posts balanced ledger legs for them.
type reconcileFixture struct {
	pool     *pgxpool.Pool
	books    *ledger.Ledger
	now      time.Time
	buyer    uuid.UUID
	seller   uuid.UUID
	checkout uuid.UUID
}

func newReconcileFixture(t *testing.T) *reconcileFixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Pool(t)
	f := &reconcileFixture{pool: pool, books: ledger.New(), now: time.Now().UTC().Truncate(time.Second)}
	for _, id := range []*uuid.UUID{&f.buyer, &f.seller} {
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (firebase_uid, signup_method) VALUES ($1, 'email') RETURNING id`,
			"reconcile-"+uuid.NewString()).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO checkouts (buyer_id, idempotency_key, request_hash, base_pesewas,
		                        processing_fee_pesewas, charge_pesewas, expires_at)
		 VALUES ($1, $2, 'reconcile', 100, 0, 100, now() + interval '30 minutes')
		 RETURNING id`, f.buyer, uuid.New()).Scan(&f.checkout); err != nil {
		t.Fatal(err)
	}
	return f
}

// seedOrder writes an order row with the given money and states.
func (f *reconcileFixture) seedOrder(t *testing.T, subtotal, delivery int64, status, escrow string, completedAt *time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO orders (checkout_id, buyer_id, seller_id, status, escrow_state,
		                     subtotal_pesewas, delivery_fee_pesewas, base_pesewas,
		                     commission_rate_bps, commission_pesewas, delivery_method,
		                     completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 500, $9, 'pickup', $10)
		 RETURNING id`,
		f.checkout, f.buyer, f.seller, status, escrow,
		subtotal, delivery, subtotal+delivery, (subtotal*500+5000)/10000, completedAt).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *reconcileFixture) post(t *testing.T, kind, reference string, entries ...ledger.Entry) {
	t.Helper()
	if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		return f.books.Post(context.Background(), tx, kind, reference, entries...)
	}); err != nil {
		t.Fatal(err)
	}
}

// checkoutPaid posts a balanced checkout_paid-shaped entry for one order.
func (f *reconcileFixture) checkoutPaid(t *testing.T, orderID uuid.UUID, base int64) {
	t.Helper()
	id := orderID
	f.post(t, "checkout_paid", orderID.String(),
		ledger.Entry{Account: ledger.PaystackClearing, Amount: base, Currency: ledger.CurrencyGHS},
		ledger.Entry{Account: ledger.Escrow, Amount: -base, Currency: ledger.CurrencyGHS, OrderID: &id},
	)
}

// TestReconcile_CleanAfterFullScenario runs the 15b–17b money story (a held
// order, a released one, a refunded one and a partial) and proves the report
// is clean.
func TestReconcile_CleanAfterFullScenario(t *testing.T) {
	f := newReconcileFixture(t)
	twoHoursAgo := f.now.Add(-2 * time.Hour)

	held := f.seedOrder(t, 2000, 0, "paid", "held", nil)
	f.checkoutPaid(t, held, 2000)

	released := f.seedOrder(t, 5000, 0, "completed", "released", &twoHoursAgo)
	f.checkoutPaid(t, released, 5000)
	f.post(t, "escrow_release", released.String(),
		ledger.EscrowRelease(released, f.seller, 5000, 250)...)

	refunded := f.seedOrder(t, 3000, 0, "cancelled", "refunded", nil)
	f.checkoutPaid(t, refunded, 3000)
	refundID := uuid.New()
	f.post(t, "order_refund", refundID.String(), ledger.OrderRefund(refunded, 3000)...)
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO refunds (id, order_id, amount_pesewas, reason, status) VALUES ($1, $2, 3000, 'seller_rejected', 'processed')`,
		refundID, refunded); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE orders SET refunded_pesewas = 3000 WHERE id = $1`, refunded); err != nil {
		t.Fatal(err)
	}

	partial := f.seedOrder(t, 10000, 500, "completed", "released", &twoHoursAgo)
	f.checkoutPaid(t, partial, 10500)
	partialRefundID := uuid.New()
	f.post(t, "order_refund", partialRefundID.String(), ledger.OrderRefund(partial, 2500)...)
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO refunds (id, order_id, amount_pesewas, reason, status) VALUES ($1, $2, 2500, 'dispute_partial', 'processed')`,
		partialRefundID, partial); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE orders SET refunded_pesewas = 2500 WHERE id = $1`, partial); err != nil {
		t.Fatal(err)
	}
	f.post(t, "escrow_release", partial.String(), ledger.EscrowRelease(partial, f.seller, 8000, 400)...)

	report, err := f.books.Reconcile(context.Background(), f.pool, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Clean() {
		t.Errorf("violations = %+v, want none", report.Violations)
	}
	if report.EscrowBalance != -2000 || report.ExpectedEscrow != -2000 {
		t.Errorf("escrow = %d expected %d, want -2000 and -2000", report.EscrowBalance, report.ExpectedEscrow)
	}
}

// TestReconcile_DetectsViolations inserts a deliberately broken state through
// raw SQL and proves the report lists every violated check.
func TestReconcile_DetectsViolations(t *testing.T) {
	f := newReconcileFixture(t)
	twoHoursAgo := f.now.Add(-2 * time.Hour)

	// A completed order past the grace window with no release posting.
	stale := f.seedOrder(t, 4000, 0, "completed", "held", &twoHoursAgo)
	f.checkoutPaid(t, stale, 4000)

	// A processed refund with no posting. Its missing debit also unbalances
	// the escrow account, so the escrow check fires for it too.
	ghostRefundOrder := f.seedOrder(t, 1500, 0, "cancelled", "refunded", nil)
	f.checkoutPaid(t, ghostRefundOrder, 1500)
	ghostRefund := uuid.New()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO refunds (id, order_id, amount_pesewas, reason, status) VALUES ($1, $2, 1500, 'seller_rejected', 'processed')`,
		ghostRefund, ghostRefundOrder); err != nil {
		t.Fatal(err)
	}

	// A balanced payout that leaves the seller payable negative-displayed.
	payableVictim := f.seller
	f.post(t, "payout_initiated", "reconcile-test-payout",
		ledger.PayoutInitiated(payableVictim, 1000)...)

	// A lone CRD leg: the per-currency sums stop balancing. The deferred
	// balance trigger would refuse it, so this bypasses triggers on one
	// connection: the reconciler exists for states the application can never
	// write through its own guards.
	conn, err := f.pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(context.Background(), `SET session_replication_role TO 'replica'`); err != nil {
		t.Fatalf("disable triggers: %v (the test DB role needs superuser)", err)
	}
	probeRef := uuid.NewString()
	if _, err := conn.Exec(context.Background(),
		`INSERT INTO ledger_transactions (kind, reference) VALUES ('reconcile_probe', $1)`, probeRef); err != nil {
		t.Fatal(err)
	}
	var probeTx int64
	if err := conn.QueryRow(context.Background(),
		`SELECT id FROM ledger_transactions WHERE kind = 'reconcile_probe'`).Scan(&probeTx); err != nil {
		t.Fatal(err)
	}
	var issuedID int64
	if err := conn.QueryRow(context.Background(),
		`SELECT id FROM ledger_accounts WHERE code = 'promo_credits_issued'`).Scan(&issuedID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(context.Background(),
		`INSERT INTO ledger_entries (transaction_id, account_id, amount, currency) VALUES ($1, $2, 10, 'CRD')`,
		probeTx, issuedID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(context.Background(), `SET session_replication_role TO DEFAULT`); err != nil {
		t.Fatal(err)
	}

	report, err := f.books.Reconcile(context.Background(), f.pool, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Clean() {
		t.Fatal("report is clean, want violations")
	}
	seen := map[string][]string{}
	for _, v := range report.Violations {
		seen[v.Check] = append(seen[v.Check], v.Detail)
	}
	for _, check := range []string{"completed_have_release", "processed_refunds_posted", "non_negative_balances", "entries_balance"} {
		if len(seen[check]) == 0 {
			t.Errorf("no %s violation in %+v", check, report.Violations)
		}
	}
	joined := ""
	for _, v := range report.Violations {
		joined += v.Detail
	}
	if !strings.Contains(joined, stale.String()) {
		t.Errorf("no violation names the stale order %s", stale)
	}
	if !strings.Contains(joined, ghostRefund.String()) {
		t.Errorf("no violation names the ghost refund %s", ghostRefund)
	}

	// The worker reports violations with an Error log plus an audit event,
	// and still returns nil: it detects, never fixes.
	var logs bytes.Buffer
	worker := &ledger.ReconcileWorker{
		Pool: f.pool, Ledger: f.books,
		Log: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		Now: func() time.Time { return f.now },
	}
	if err := worker.Work(context.Background(), nil); err != nil {
		t.Fatalf("worker: %v", err)
	}
	if !strings.Contains(logs.String(), `"level":"ERROR"`) ||
		!strings.Contains(logs.String(), "ledger reconciliation violation") {
		t.Errorf("no Error log for violations: %s", logs.String())
	}
	var audits int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_events WHERE action = 'ledger.reconcile'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Errorf("reconcile audits = %d, want 1", audits)
	}
}

// TestReconcile_CleanWritesNothing proves a clean ledger leaves no audit
// trail behind.
func TestReconcile_CleanWritesNothing(t *testing.T) {
	f := newReconcileFixture(t)
	held := f.seedOrder(t, 2000, 0, "paid", "held", nil)
	f.checkoutPaid(t, held, 2000)

	var logs bytes.Buffer
	worker := &ledger.ReconcileWorker{
		Pool: f.pool, Ledger: f.books,
		Log: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		Now: func() time.Time { return f.now },
	}
	if err := worker.Work(context.Background(), nil); err != nil {
		t.Fatalf("worker: %v", err)
	}
	var audits int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_events WHERE action = 'ledger.reconcile'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 0 {
		t.Errorf("reconcile audits = %d, want 0 on a clean ledger", audits)
	}
}
