package orders_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/payments/fake"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// refundFixture is a minimal stack for the release and refund jobs: a real
// service over a real database, a fake Paystack, a controllable clock, a
// captured log, and an insert-only job client (each test drives
// ReleaseEscrow/ProcessRefund/the webhook handlers directly, so it can assert
// on each step without a River worker racing it).
type refundFixture struct {
	pool     *pgxpool.Pool
	svc      *orders.Service
	ledger   *ledger.Ledger
	provider *fake.Provider
	logs     *bytes.Buffer
	now      time.Time
	buyer    uuid.UUID
	seller   uuid.UUID
	seq      int
}

func newRefundFixture(t *testing.T) *refundFixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Pool(t)
	mk := func(uid string) uuid.UUID {
		user, err := users.New(pool).Resolve(ctx, auth.Identity{
			UID: uid, Email: uid + "@farmish.test", Provider: "password",
		})
		if err != nil {
			t.Fatal(err)
		}
		return user.ID
	}
	f := &refundFixture{
		pool: pool, ledger: ledger.New(), provider: fake.New(), logs: &bytes.Buffer{},
		now:   time.Now().UTC().Truncate(time.Second),
		buyer: mk("refund-buyer"), seller: mk("refund-seller"),
	}
	f.provider.Now = func() time.Time { return f.now }
	log := slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	f.svc = orders.NewService(pool, 48*time.Hour, 3*24*time.Hour)
	f.svc.Now = func() time.Time { return f.now }
	f.svc.AttachLedger(f.ledger)
	f.svc.AttachPaystack(f.provider)
	f.svc.AttachLogger(log)
	reg := jobs.NewRegistry()
	orders.RegisterJobs(reg, f.svc, log)
	notify.Register(reg, notify.NewWorker(notify.LogOnly{Log: log}, log, pool))
	client, err := jobs.NewClient(pool, reg, log, jobs.Options{Work: false, FetchPollInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	f.svc.AttachJobClient(client)
	return f
}

// advance moves the service's and the fake Paystack's clock.
func (f *refundFixture) advance(d time.Duration) { f.now = f.now.Add(d) }

// process runs one refund job step and fails the test on a database error.
func (f *refundFixture) process(t *testing.T, refundID uuid.UUID) time.Duration {
	t.Helper()
	next, err := f.svc.ProcessRefund(context.Background(), refundID)
	if err != nil {
		t.Fatalf("process refund: %v", err)
	}
	return next
}

// createRefund records a refund in its own transaction.
func (f *refundFixture) createRefund(t *testing.T, orderID uuid.UUID, amount int64, reason string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		return f.svc.CreateRefund(ctx, tx, orderID, amount, reason)
	}); err != nil {
		t.Fatal(err)
	}
	return f.refundIDForOrder(t, orderID)
}

func (f *refundFixture) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// orderSpec describes one seeded order; zero values pick the DOMAIN example
// numbers (subtotal 12,345, no delivery fee, 500 bps).
type orderSpec struct {
	subtotal, deliveryFee, rateBps, refunded int64
	status, escrowState                      string
	// checkoutID puts the order in an existing checkout (a multi-seller
	// checkout: one payment reference for several orders).
	checkoutID uuid.UUID
}

// seedOrder writes a checkout, a successful payment and an order, and
// (unless the caller wants an unpaid escrow) posts a checkout_paid-shaped
// ledger entry so the escrow invariant (DOMAIN §5.4) is meaningful to check.
func (f *refundFixture) seedOrder(t *testing.T, spec orderSpec) (orderID uuid.UUID, reference string) {
	t.Helper()
	ctx := context.Background()
	f.seq++
	subtotal, deliveryFee, rateBps := spec.subtotal, spec.deliveryFee, spec.rateBps
	if subtotal == 0 {
		subtotal = 12345
	}
	if rateBps == 0 {
		rateBps = 500
	}
	base := subtotal + deliveryFee
	commission := (subtotal*rateBps + 5000) / 10000

	var checkoutID uuid.UUID
	if spec.checkoutID != uuid.Nil {
		checkoutID = spec.checkoutID
		if err := f.pool.QueryRow(ctx,
			`SELECT p.reference FROM checkouts c JOIN payments p ON p.id = c.payment_id WHERE c.id = $1`,
			checkoutID).Scan(&reference); err != nil {
			t.Fatal(err)
		}
	} else {
		reference = fmt.Sprintf("FMS-REFUND-TEST-%d", f.seq)
		var paymentID uuid.UUID
		if err := f.pool.QueryRow(ctx,
			`INSERT INTO payments (reference, user_id, purpose, purpose_ref, base_pesewas,
			                       processing_fee_pesewas, charge_pesewas, currency, status, paid_at)
			 VALUES ($1, $2, 'checkout', 'refund-test', $3, 0, $3, 'GHS', 'success', now())
			 RETURNING id`, reference, f.buyer, base).Scan(&paymentID); err != nil {
			t.Fatal(err)
		}
		if err := f.pool.QueryRow(ctx,
			`INSERT INTO checkouts (buyer_id, idempotency_key, request_hash, base_pesewas,
			                        processing_fee_pesewas, charge_pesewas, payment_id, status, expires_at)
			 VALUES ($1, $2, 'refund-test', $3, 0, $3, $4, 'paid', now() + interval '30 minutes')
			 RETURNING id`, f.buyer, uuid.New(), base, paymentID).Scan(&checkoutID); err != nil {
			t.Fatal(err)
		}
	}
	status, escrowState := spec.status, spec.escrowState
	if status == "" {
		status = orders.StatusCompleted
	}
	if escrowState == "" {
		escrowState = orders.EscrowHeld
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO orders (checkout_id, buyer_id, seller_id, status, escrow_state,
		                     subtotal_pesewas, delivery_fee_pesewas, base_pesewas,
		                     commission_rate_bps, commission_pesewas, refunded_pesewas, delivery_method)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'pickup')
		 RETURNING id`,
		checkoutID, f.buyer, f.seller, status, escrowState,
		subtotal, deliveryFee, base, rateBps, commission, spec.refunded).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		id := orderID
		return f.ledger.Post(ctx, tx, "checkout_paid", orderID.String(),
			ledger.Entry{Account: ledger.PaystackClearing, Amount: base, Currency: ledger.CurrencyGHS},
			ledger.Entry{Account: ledger.Escrow, Amount: -base, Currency: ledger.CurrencyGHS, OrderID: &id},
		)
	}); err != nil {
		t.Fatal(err)
	}
	return orderID, reference
}

func (f *refundFixture) refundIDForOrder(t *testing.T, orderID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id FROM refunds WHERE order_id = $1 ORDER BY created_at DESC LIMIT 1`, orderID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *refundFixture) refundRow(t *testing.T, refundID uuid.UUID) (status string, failureReason *string, paystackID *string) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, failure_reason, paystack_refund_id FROM refunds WHERE id = $1`, refundID).
		Scan(&status, &failureReason, &paystackID); err != nil {
		t.Fatal(err)
	}
	return status, failureReason, paystackID
}

func (f *refundFixture) orderRow(t *testing.T, orderID uuid.UUID) (status, escrowState string, refunded int64) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, escrow_state, refunded_pesewas FROM orders WHERE id = $1`, orderID).
		Scan(&status, &escrowState, &refunded); err != nil {
		t.Fatal(err)
	}
	return status, escrowState, refunded
}

// TestReleaseEscrow_PostsAndIsIdempotent proves a completed order releases the
// seller's earnings minus DOMAIN's commission example (12,345 at 500 bps ->
// 617), and that a second run posts nothing new.
func TestReleaseEscrow_PostsAndIsIdempotent(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	orderID, _ := f.seedOrder(t, orderSpec{})

	posted, err := f.svc.ReleaseEscrow(ctx, orderID)
	if err != nil || !posted {
		t.Fatalf("release: posted=%v err=%v", posted, err)
	}
	status, escrowState, _ := f.orderRow(t, orderID)
	if status != orders.StatusCompleted || escrowState != orders.EscrowReleased {
		t.Errorf("order = %s/%s, want completed/released", status, escrowState)
	}
	sellerBal, err := f.ledger.Balance(ctx, f.pool, ledger.SellerPayable(f.seller))
	if err != nil {
		t.Fatal(err)
	}
	commissionBal, err := f.ledger.Balance(ctx, f.pool, ledger.PlatformCommission)
	if err != nil {
		t.Fatal(err)
	}
	if sellerBal != -(12345-617) || commissionBal != -617 {
		t.Errorf("seller balance %d, commission balance %d, want %d and %d",
			sellerBal, commissionBal, -(12345 - 617), -617)
	}

	posted, err = f.svc.ReleaseEscrow(ctx, orderID)
	if err != nil || posted {
		t.Fatalf("second release: posted=%v err=%v, want a no-op", posted, err)
	}
	var count int64
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ledger_transactions WHERE kind = 'escrow_release' AND reference = $1`,
		orderID.String()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("escrow_release transactions = %d, want 1", count)
	}
}

// TestReleaseEscrow_WrongStateNoop proves an order that is not completed, or
// whose escrow is not held or partially refunded, posts nothing.
func TestReleaseEscrow_WrongStateNoop(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()

	notCompleted, _ := f.seedOrder(t, orderSpec{status: orders.StatusPaid, escrowState: orders.EscrowHeld})
	if posted, err := f.svc.ReleaseEscrow(ctx, notCompleted); err != nil || posted {
		t.Errorf("not completed: posted=%v err=%v, want a no-op", posted, err)
	}

	alreadyReleased, _ := f.seedOrder(t, orderSpec{status: orders.StatusCompleted, escrowState: orders.EscrowReleased})
	if posted, err := f.svc.ReleaseEscrow(ctx, alreadyReleased); err != nil || posted {
		t.Errorf("already released: posted=%v err=%v, want a no-op", posted, err)
	}
	var count int64
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ledger_transactions WHERE kind = 'escrow_release'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("escrow_release transactions = %d, want 0", count)
	}
}

// TestPartialRefund_CommissionOnRemaining proves DOMAIN §4.1's example: base
// 10,500 (subtotal 10,000 + delivery 500), a 2,500 partial refund already
// applied -> remaining base 8,000, commission 400, seller gets 7,600.
func TestPartialRefund_CommissionOnRemaining(t *testing.T) {
	order := orders.Order{
		SubtotalPesewas: 10000, DeliveryFeePesewas: 500, BasePesewas: 10500,
		CommissionRateBps: 500, RefundedPesewas: 2500,
	}
	remainingBase, commission, err := orders.RemainingRelease(order)
	if err != nil {
		t.Fatal(err)
	}
	if remainingBase != 8000 || commission != 400 {
		t.Errorf("remaining = %d, commission = %d, want 8000 and 400", remainingBase, commission)
	}
	if sellerNet := remainingBase - commission; sellerNet != 7600 {
		t.Errorf("seller net = %d, want 7600", sellerNet)
	}

	// The same numbers, posted for real: a partially-refunded order releases
	// its remainder correctly.
	f := newRefundFixture(t)
	ctx := context.Background()
	orderID, _ := f.seedOrder(t, orderSpec{
		subtotal: 10000, deliveryFee: 500, rateBps: 500, refunded: 2500,
		status: orders.StatusCompleted, escrowState: orders.EscrowPartiallyRefunded,
	})
	posted, err := f.svc.ReleaseEscrow(ctx, orderID)
	if err != nil || !posted {
		t.Fatalf("release: posted=%v err=%v", posted, err)
	}
	sellerBal, err := f.ledger.Balance(ctx, f.pool, ledger.SellerPayable(f.seller))
	if err != nil {
		t.Fatal(err)
	}
	if sellerBal != -7600 {
		t.Errorf("seller balance = %d, want -7600", sellerBal)
	}
}

// TestRefund_FullFlow drives a seller reject through the real job body and
// the real webhook handler: queued -> the fake Paystack call -> processed ->
// the ledger and escrow_state, with a webhook replay posting nothing twice.
func TestRefund_FullFlow(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	orderID, reference := f.seedOrder(t, orderSpec{status: orders.StatusPaid, escrowState: orders.EscrowHeld})

	if _, err := f.svc.Reject(ctx, f.seller, orderID, "The stock spoiled before pickup."); err != nil {
		t.Fatal(err)
	}
	refundID := f.refundIDForOrder(t, orderID)
	status, _, _ := f.refundRow(t, refundID)
	if status != orders.RefundStatusQueued {
		t.Fatalf("refund status = %s, want queued", status)
	}

	f.process(t, refundID)
	if f.provider.CallCount("CreateRefund") != 1 {
		t.Fatalf("paystack calls = %d, want 1", f.provider.CallCount("CreateRefund"))
	}
	status, _, paystackID := f.refundRow(t, refundID)
	if status != orders.RefundStatusPending || paystackID == nil {
		t.Fatalf("refund = %s, paystack id %v, want pending with an id", status, paystackID)
	}

	body := webhookBody(t, *paystackID, 12345, reference)
	if outcome := f.deliverRefundWebhook(t, body); outcome != payments.OutcomeProcessed {
		t.Fatalf("webhook outcome = %s, want processed", outcome)
	}
	orderStatus, escrowState, refunded := f.orderRow(t, orderID)
	if orderStatus != orders.StatusCancelled || escrowState != orders.EscrowRefunded || refunded != 12345 {
		t.Errorf("order = %s/%s refunded %d, want cancelled/refunded and 12345", orderStatus, escrowState, refunded)
	}
	var ledgerCount int64
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ledger_transactions WHERE kind = 'order_refund' AND reference = $1`,
		refundID.String()).Scan(&ledgerCount); err != nil {
		t.Fatal(err)
	}
	if ledgerCount != 1 {
		t.Errorf("order_refund transactions = %d, want 1", ledgerCount)
	}

	// A webhook replay settles nothing twice.
	if outcome := f.deliverRefundWebhook(t, body); outcome != payments.OutcomeIgnored {
		t.Errorf("replay outcome = %s, want ignored", outcome)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ledger_transactions WHERE kind = 'order_refund' AND reference = $1`,
		refundID.String()).Scan(&ledgerCount); err != nil {
		t.Fatal(err)
	}
	if ledgerCount != 1 {
		t.Errorf("after replay, order_refund transactions = %d, want still 1", ledgerCount)
	}
}

// TestRefund_Idempotent proves the job's own queued check: running
// ProcessRefund twice calls Paystack only once, and the partial unique index
// stops two full refunds for the same order.
func TestRefund_Idempotent(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	orderID, _ := f.seedOrder(t, orderSpec{status: orders.StatusPaid, escrowState: orders.EscrowHeld})

	f.createRefund(t, orderID, 12345, orders.RefundReasonSellerRejected)
	// A second full refund for the same order is refused at the row: the
	// partial unique index, not application logic, is the guard.
	if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		return f.svc.CreateRefund(ctx, tx, orderID, 12345, orders.RefundReasonBuyerCancelled)
	}); err != nil {
		t.Fatal(err)
	}
	var refundRows int64
	if err := f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM refunds WHERE order_id = $1`, orderID).Scan(&refundRows); err != nil {
		t.Fatal(err)
	}
	if refundRows != 1 {
		t.Fatalf("refund rows = %d, want 1", refundRows)
	}

	refundID := f.refundIDForOrder(t, orderID)
	f.process(t, refundID)
	f.process(t, refundID) // pending now: reconciles, never calls again
	if f.provider.CallCount("CreateRefund") != 1 {
		t.Errorf("paystack calls = %d, want 1", f.provider.CallCount("CreateRefund"))
	}
}

// TestRefund_AfterReleaseGoesManual proves the manual path: once escrow is
// released, a refund never reaches Paystack.
func TestRefund_AfterReleaseGoesManual(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	orderID, _ := f.seedOrder(t, orderSpec{})
	if posted, err := f.svc.ReleaseEscrow(ctx, orderID); err != nil || !posted {
		t.Fatalf("release: posted=%v err=%v", posted, err)
	}

	// A forced refund: nothing in the normal state machine reaches here after
	// release, but the job must refuse it defensively regardless.
	if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		return f.svc.CreateRefund(ctx, tx, orderID, 1000, orders.RefundReasonDisputeRefund)
	}); err != nil {
		t.Fatal(err)
	}
	refundID := f.refundIDForOrder(t, orderID)
	f.process(t, refundID)
	if f.provider.CallCount("CreateRefund") != 0 {
		t.Errorf("paystack calls = %d, want 0", f.provider.CallCount("CreateRefund"))
	}
	status, failureReason, _ := f.refundRow(t, refundID)
	if status != orders.RefundStatusFailed || failureReason == nil || *failureReason != "refund_after_release" {
		t.Errorf("refund = %s/%v, want failed/refund_after_release", status, failureReason)
	}
	var audits int64
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_events WHERE action = 'refund.after_release' AND target_id = $1`,
		refundID.String()).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Errorf("audit rows = %d, want 1", audits)
	}
}

// TestRefund_ProviderErrorRetries proves the only automatic resend: an
// ambiguous failure where reconciliation shows Paystack holds nothing. Before
// the grace period the refund stays pending and nothing is resent; after it,
// the refund is requeued and the call is made again.
func TestRefund_ProviderErrorRetries(t *testing.T) {
	f := newRefundFixture(t)
	orderID, _ := f.seedOrder(t, orderSpec{status: orders.StatusPaid, escrowState: orders.EscrowHeld})
	refundID := f.createRefund(t, orderID, 12345, orders.RefundReasonSellerRejected)

	f.provider.RefundErr = fmt.Errorf("paystack: %w", context.DeadlineExceeded) // ambiguous, nothing created
	if next := f.process(t, refundID); next <= 0 {
		t.Fatalf("ambiguous failure: next = %v, want a re-check", next)
	}
	if status, _, _ := f.refundRow(t, refundID); status != orders.RefundStatusPending {
		t.Fatalf("after the ambiguous failure = %s, want pending", status)
	}

	f.provider.RefundErr = nil
	f.advance(orders.RefundRecheckSoon)
	f.process(t, refundID) // inside the grace period: reconciles, never resends
	if n := f.provider.CallCount("CreateRefund"); n != 1 {
		t.Fatalf("paystack calls inside the grace period = %d, want 1", n)
	}

	f.advance(orders.RefundRetryGrace)
	f.process(t, refundID) // nothing at Paystack after the grace: requeued
	if status, _, _ := f.refundRow(t, refundID); status != orders.RefundStatusQueued {
		t.Fatalf("after the grace period = %s, want queued", status)
	}
	f.process(t, refundID) // the retry
	status, _, paystackID := f.refundRow(t, refundID)
	if status != orders.RefundStatusPending || paystackID == nil {
		t.Errorf("refund after retry = %s, paystack id %v, want pending with an id", status, paystackID)
	}
	if n := f.provider.CallCount("CreateRefund"); n != 2 {
		t.Errorf("paystack calls = %d, want 2 (the retry was proven safe)", n)
	}
}

// TestRefund_TimeoutNeverDoubleRefunds is Blocker 1: Paystack creates the
// refund but the response is lost. The job must adopt the existing refund,
// never send a second one, whatever the clock does.
func TestRefund_TimeoutNeverDoubleRefunds(t *testing.T) {
	f := newRefundFixture(t)
	orderID, _ := f.seedOrder(t, orderSpec{status: orders.StatusPaid, escrowState: orders.EscrowHeld})
	refundID := f.createRefund(t, orderID, 12345, orders.RefundReasonSellerRejected)

	f.provider.RefundErrAfterRecord = fmt.Errorf("paystack: %w", context.DeadlineExceeded)
	f.process(t, refundID)
	f.provider.RefundErrAfterRecord = nil

	for i := 0; i < 5; i++ {
		f.advance(orders.RefundRetryGrace) // well past the grace each time
		f.process(t, refundID)
	}
	if n := f.provider.CallCount("CreateRefund"); n != 1 {
		t.Fatalf("paystack CreateRefund calls = %d, want exactly 1", n)
	}
	status, _, paystackID := f.refundRow(t, refundID)
	if status != orders.RefundStatusPending || paystackID == nil || *paystackID != "1" {
		t.Fatalf("refund = %s, paystack id %v, want pending with the adopted id 1", status, paystackID)
	}

	// Paystack settles it; the next poll books it once.
	f.provider.SetRefundStatus(1, "processed")
	if next := f.process(t, refundID); next != 0 {
		t.Errorf("after settlement next = %v, want 0", next)
	}
	if status, _, _ := f.refundRow(t, refundID); status != orders.RefundStatusProcessed {
		t.Errorf("refund = %s, want processed", status)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM ledger_transactions WHERE kind = 'order_refund' AND reference = $1`, refundID.String()); n != 1 {
		t.Errorf("order_refund transactions = %d, want 1", n)
	}
}

// TestRefund_RejectedMarksFailed proves a definite rejection fails the refund
// with an audit row and an Error log, and is never retried.
func TestRefund_RejectedMarksFailed(t *testing.T) {
	f := newRefundFixture(t)
	orderID, _ := f.seedOrder(t, orderSpec{status: orders.StatusPaid, escrowState: orders.EscrowHeld})
	refundID := f.createRefund(t, orderID, 12345, orders.RefundReasonSellerRejected)

	f.provider.RefundErr = fmt.Errorf("%w: POST /refund: Transaction has been fully reversed", payments.ErrRejected)
	if next := f.process(t, refundID); next != 0 {
		t.Errorf("next = %v, want 0 (no retry)", next)
	}
	status, reason, _ := f.refundRow(t, refundID)
	if status != orders.RefundStatusFailed || reason == nil || !strings.HasPrefix(*reason, "paystack_rejected:") {
		t.Fatalf("refund = %s/%v, want failed/paystack_rejected", status, reason)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM audit_events WHERE action = 'refund.failed' AND target_id = $1`, refundID.String()); n != 1 {
		t.Errorf("audit rows = %d, want 1", n)
	}
	if !strings.Contains(f.logs.String(), `"level":"ERROR"`) || !strings.Contains(f.logs.String(), refundID.String()) {
		t.Errorf("no Error log for the failed refund: %s", f.logs.String())
	}
	f.advance(orders.RefundRetryGrace * 2)
	f.process(t, refundID)
	if n := f.provider.CallCount("CreateRefund"); n != 1 {
		t.Errorf("paystack calls = %d, want 1 (a rejection is final)", n)
	}
	// A failed refund moves no money: escrow is exactly as it was (seeded held).
	if _, escrow, refunded := f.orderRow(t, orderID); escrow != orders.EscrowHeld || refunded != 0 {
		t.Errorf("escrow = %s refunded %d, want unchanged held and 0", escrow, refunded)
	}
}

// TestRefund_ReconcileFetchSettlesOrFails proves the job settles or fails a
// refund from Paystack's own record, so settlement never depends on webhooks.
func TestRefund_ReconcileFetchSettlesOrFails(t *testing.T) {
	f := newRefundFixture(t)
	settled, _ := f.seedOrder(t, orderSpec{status: orders.StatusPaid, escrowState: orders.EscrowHeld})
	failed, _ := f.seedOrder(t, orderSpec{status: orders.StatusPaid, escrowState: orders.EscrowHeld})
	settledID := f.createRefund(t, settled, 12345, orders.RefundReasonSellerRejected)
	failedID := f.createRefund(t, failed, 12345, orders.RefundReasonBuyerCancelled)

	f.process(t, settledID)
	f.process(t, failedID)
	_, _, settledPS := f.refundRow(t, settledID)
	_, _, failedPS := f.refundRow(t, failedID)

	f.process(t, settledID) // still pending at Paystack: keeps polling
	if status, _, _ := f.refundRow(t, settledID); status != orders.RefundStatusPending {
		t.Fatalf("refund = %s, want pending while Paystack is pending", status)
	}

	f.provider.SetRefundStatus(paystackIDNum(t, *settledPS), "processed")
	f.provider.SetRefundStatus(paystackIDNum(t, *failedPS), "failed")
	f.process(t, settledID)
	f.process(t, failedID)

	if _, escrow, refunded := f.orderRow(t, settled); escrow != orders.EscrowRefunded || refunded != 12345 {
		t.Errorf("settled order escrow = %s refunded %d", escrow, refunded)
	}
	if status, reason, _ := f.refundRow(t, failedID); status != orders.RefundStatusFailed || *reason != "paystack_refund_failed" {
		t.Errorf("failed refund = %s/%v", status, reason)
	}
}

// TestRefund_GiveUpAfterWindow proves polling stops, with an audit row, when
// Paystack never settles a refund.
func TestRefund_GiveUpAfterWindow(t *testing.T) {
	f := newRefundFixture(t)
	orderID, _ := f.seedOrder(t, orderSpec{status: orders.StatusPaid, escrowState: orders.EscrowHeld})
	refundID := f.createRefund(t, orderID, 12345, orders.RefundReasonSellerRejected)
	f.process(t, refundID)
	f.advance(orders.RefundGiveUpAfter)
	if next := f.process(t, refundID); next != 0 {
		t.Errorf("next = %v, want 0 (stop polling)", next)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM audit_events WHERE action = 'refund.stuck' AND target_id = $1`, refundID.String()); n != 1 {
		t.Errorf("stuck audit rows = %d, want 1", n)
	}
}

// TestCreateRefund_CannotExceedBase is Blocker 3's creation guard.
func TestCreateRefund_CannotExceedBase(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	orderID, _ := f.seedOrder(t, orderSpec{status: orders.StatusDisputed, escrowState: orders.EscrowHeld})
	f.createRefund(t, orderID, 10000, orders.RefundReasonDisputePartial)
	err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		return f.svc.CreateRefund(ctx, tx, orderID, 2346, orders.RefundReasonDisputePartial) // 10000 + 2346 > 12345
	})
	if !errors.Is(err, orders.ErrRefundExceedsBase) {
		t.Fatalf("err = %v, want ErrRefundExceedsBase", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM refunds WHERE order_id = $1`, orderID); n != 1 {
		t.Errorf("refund rows = %d, want 1 (the excess was rolled back)", n)
	}
	// Exactly up to the base is fine.
	f.createRefund(t, orderID, 2345, orders.RefundReasonDisputePartial)
	// The database refuses refunded_pesewas above base even if code slipped.
	if _, err := f.pool.Exec(ctx, `UPDATE orders SET refunded_pesewas = base_pesewas + 1 WHERE id = $1`, orderID); err == nil {
		t.Error("orders_refunded_within_base CHECK did not fire")
	}
}

// TestCreateRefund_RequiresJobClient is Minor 7: a refund row is never
// recorded without the job that moves the money.
func TestCreateRefund_RequiresJobClient(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	orderID, _ := f.seedOrder(t, orderSpec{status: orders.StatusPaid, escrowState: orders.EscrowHeld})
	bare := orders.NewService(f.pool, 48*time.Hour, 3*24*time.Hour)
	err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		return bare.CreateRefund(ctx, tx, orderID, 100, orders.RefundReasonSellerRejected)
	})
	if err == nil {
		t.Fatal("want an error without a job client")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM refunds WHERE order_id = $1`, orderID); n != 0 {
		t.Errorf("refund rows = %d, want 0", n)
	}
}

// TestRefundWebhook_AmountMismatchRejected and the currency variant are
// Blocker 3's settlement guard: nothing is settled, an Error log and audit row
// record why.
func TestRefundWebhook_AmountMismatchRejected(t *testing.T) {
	for name, tc := range map[string]struct {
		amount   int64
		currency string
		outcome  string
	}{
		"amount":   {12000, "GHS", payments.OutcomeAmountMismatch},
		"currency": {12345, "NGN", payments.OutcomeCurrencyMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRefundFixture(t)
			orderID, reference := f.seedOrder(t, orderSpec{status: orders.StatusPaid, escrowState: orders.EscrowHeld})
			refundID := f.createRefund(t, orderID, 12345, orders.RefundReasonSellerRejected)
			f.process(t, refundID)
			_, _, paystackID := f.refundRow(t, refundID)

			body := refundEvent(t, map[string]any{
				"id": paystackIDNum(t, *paystackID), "amount": tc.amount,
				"currency": tc.currency, "transaction_reference": reference, "status": "processed",
			})
			if outcome := f.deliver(t, f.svc.OnRefundProcessed, body); outcome != tc.outcome {
				t.Fatalf("outcome = %s, want %s", outcome, tc.outcome)
			}
			if status, _, _ := f.refundRow(t, refundID); status != orders.RefundStatusPending {
				t.Errorf("refund = %s, want still pending", status)
			}
			if _, escrow, refunded := f.orderRow(t, orderID); refunded != 0 || escrow == orders.EscrowRefunded {
				t.Errorf("order escrow = %s refunded %d, want untouched", escrow, refunded)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM ledger_transactions WHERE kind = 'order_refund'`); n != 0 {
				t.Errorf("order_refund postings = %d, want 0", n)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM audit_events WHERE action = 'refund.settlement_rejected'`); n != 1 {
				t.Errorf("audit rows = %d, want 1", n)
			}
			if !strings.Contains(f.logs.String(), `"msg":"refund settlement rejected"`) {
				t.Error("no Error log for the rejected settlement")
			}
		})
	}
}

// TestRefundWebhook_AmbiguousFallbackDeferred is Blocker 2: two same-amount
// refunds on one multi-seller checkout reference. A webhook that can only be
// matched by (reference, amount) is refused (a transient error: Paystack
// retries); once the job has stored each id, each webhook settles its own.
func TestRefundWebhook_AmbiguousFallbackDeferred(t *testing.T) {
	f := newRefundFixture(t)
	first, reference := f.seedOrder(t, orderSpec{subtotal: 5000, status: orders.StatusPaid, escrowState: orders.EscrowHeld})
	var checkoutID uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT checkout_id FROM orders WHERE id = $1`, first).Scan(&checkoutID); err != nil {
		t.Fatal(err)
	}
	second, _ := f.seedOrder(t, orderSpec{subtotal: 5000, status: orders.StatusPaid, escrowState: orders.EscrowHeld, checkoutID: checkoutID})
	firstRefund := f.createRefund(t, first, 5000, orders.RefundReasonSellerRejected)
	secondRefund := f.createRefund(t, second, 5000, orders.RefundReasonSellerRejected)

	noID := refundEvent(t, map[string]any{"amount": 5000, "currency": "GHS", "transaction_reference": reference, "status": "processed"})
	var err error
	_ = database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		_, err = f.svc.OnRefundProcessed(context.Background(), tx, noID)
		return err
	})
	if !errors.Is(err, orders.ErrAmbiguousRefundMatch) {
		t.Fatalf("err = %v, want ErrAmbiguousRefundMatch", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM refunds WHERE status = 'processed'`); n != 0 {
		t.Fatalf("processed refunds = %d, want 0", n)
	}

	f.process(t, firstRefund)
	f.process(t, secondRefund)
	_, _, firstPS := f.refundRow(t, firstRefund)
	_, _, secondPS := f.refundRow(t, secondRefund)
	secondEvent := refundEvent(t, map[string]any{
		"id": paystackIDNum(t, *secondPS), "amount": 5000, "currency": "GHS",
		"transaction_reference": reference, "status": "processed",
	})
	if outcome := f.deliver(t, f.svc.OnRefundProcessed, secondEvent); outcome != payments.OutcomeProcessed {
		t.Fatalf("second outcome = %s", outcome)
	}
	if status, _, _ := f.refundRow(t, secondRefund); status != orders.RefundStatusProcessed {
		t.Errorf("second refund = %s, want processed", status)
	}
	if status, _, _ := f.refundRow(t, firstRefund); status != orders.RefundStatusPending {
		t.Errorf("first refund = %s, want still pending (not settled by the other's event)", status)
	}
	_ = firstPS
}

// TestRefundWebhook_FallbackMatchSingleCandidate proves the fallback works
// when it is unambiguous, and remembers Paystack's id.
func TestRefundWebhook_FallbackMatchSingleCandidate(t *testing.T) {
	f := newRefundFixture(t)
	orderID, reference := f.seedOrder(t, orderSpec{status: orders.StatusPaid, escrowState: orders.EscrowHeld})
	refundID := f.createRefund(t, orderID, 12345, orders.RefundReasonSellerRejected)
	body := refundEvent(t, map[string]any{"id": 777, "amount": 12345, "currency": "GHS", "transaction_reference": reference, "status": "pending"})
	if outcome := f.deliver(t, f.svc.OnRefundPending, body); outcome != payments.OutcomeProcessed {
		t.Fatalf("outcome = %s", outcome)
	}
	if _, _, paystackID := f.refundRow(t, refundID); paystackID == nil || *paystackID != "777" {
		t.Errorf("paystack id = %v, want 777 remembered", paystackID)
	}
}

// TestRefundWebhook_FailedAuditsLogsAndKeepsEscrow is Major 4.
func TestRefundWebhook_FailedAuditsLogsAndKeepsEscrow(t *testing.T) {
	f := newRefundFixture(t)
	orderID, reference := f.seedOrder(t, orderSpec{status: orders.StatusPaid, escrowState: orders.EscrowHeld})
	if _, err := f.svc.Reject(context.Background(), f.seller, orderID, "Could not fulfil this order."); err != nil {
		t.Fatal(err)
	}
	refundID := f.refundIDForOrder(t, orderID)
	f.process(t, refundID)
	_, _, paystackID := f.refundRow(t, refundID)
	body := refundEvent(t, map[string]any{
		"id": paystackIDNum(t, *paystackID), "amount": 12345, "currency": "GHS",
		"transaction_reference": reference, "status": "failed",
	})

	if outcome := f.deliver(t, f.svc.OnRefundFailed, body); outcome != payments.OutcomeProcessed {
		t.Fatalf("outcome = %s, want %s", outcome, payments.OutcomeProcessed)
	}
	if status, reason, _ := f.refundRow(t, refundID); status != orders.RefundStatusFailed || *reason != "paystack_refund_failed" {
		t.Errorf("refund = %s/%v", status, reason)
	}
	if _, escrow, refunded := f.orderRow(t, orderID); escrow != orders.EscrowRefundPending || refunded != 0 {
		t.Errorf("escrow = %s refunded %d, want refund_pending and 0", escrow, refunded)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM audit_events WHERE action = 'refund.failed' AND target_id = $1`, refundID.String()); n != 1 {
		t.Errorf("audit rows = %d, want 1", n)
	}
	if !strings.Contains(f.logs.String(), `"level":"ERROR","msg":"refund failed; escrow stays held for an admin"`) {
		t.Errorf("no Error log: %s", f.logs.String())
	}
	if outcome := f.deliver(t, f.svc.OnRefundFailed, body); outcome != payments.OutcomeIgnored {
		t.Errorf("replay outcome = %s, want ignored", outcome)
	}
}

// TestRefundWebhook_PendingIsInformational proves refund.pending changes no
// money state.
func TestRefundWebhook_PendingIsInformational(t *testing.T) {
	f := newRefundFixture(t)
	orderID, reference := f.seedOrder(t, orderSpec{status: orders.StatusPaid, escrowState: orders.EscrowHeld})
	refundID := f.createRefund(t, orderID, 12345, orders.RefundReasonSellerRejected)
	f.process(t, refundID)
	_, _, paystackID := f.refundRow(t, refundID)
	body := refundEvent(t, map[string]any{
		"id": paystackIDNum(t, *paystackID), "amount": 12345, "currency": "GHS",
		"transaction_reference": reference, "status": "processing",
	})
	if outcome := f.deliver(t, f.svc.OnRefundPending, body); outcome != payments.OutcomeProcessed {
		t.Fatalf("outcome = %s", outcome)
	}
	if status, _, _ := f.refundRow(t, refundID); status != orders.RefundStatusPending {
		t.Errorf("refund = %s, want pending", status)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM ledger_transactions WHERE kind = 'order_refund'`); n != 0 {
		t.Errorf("postings = %d, want 0", n)
	}
	if outcome, err := f.deliverErr(f.svc.OnRefundPending, []byte(`{not json`)); !errors.Is(err, payments.ErrMalformedEvent) {
		t.Errorf("malformed body: outcome %q err %v, want ErrMalformedEvent (ADR-0021)", outcome, err)
	}
}

// TestLedger_ReconcileAfterMixedFlows proves DOMAIN §5.4: the escrow balance
// equals the sum of the held remainder over orders still held, refund-pending
// or partially refunded. Released and fully-refunded orders contribute 0.
func TestLedger_ReconcileAfterMixedFlows(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()

	released, _ := f.seedOrder(t, orderSpec{subtotal: 5000})
	if posted, err := f.svc.ReleaseEscrow(ctx, released); err != nil || !posted {
		t.Fatalf("release: posted=%v err=%v", posted, err)
	}

	refundedFull, reference := f.seedOrder(t, orderSpec{
		subtotal: 3000, status: orders.StatusPaid, escrowState: orders.EscrowHeld,
	})
	if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		return f.svc.CreateRefund(ctx, tx, refundedFull, 3000, orders.RefundReasonSellerRejected)
	}); err != nil {
		t.Fatal(err)
	}
	refundID := f.refundIDForOrder(t, refundedFull)
	f.process(t, refundID)
	_, _, paystackID := f.refundRow(t, refundID)
	body := webhookBody(t, *paystackID, 3000, reference)
	if outcome := f.deliverRefundWebhook(t, body); outcome != payments.OutcomeProcessed {
		t.Fatalf("webhook outcome = %s, want processed", outcome)
	}

	// The partial order's refund is posted for real, so the ledger and the
	// order row agree: checkout_paid -4,000, order_refund +1,000, then
	// escrow_release +3,000 nets to 0 for this order.
	partial, partialRef := f.seedOrder(t, orderSpec{
		subtotal: 4000, status: orders.StatusPaid, escrowState: orders.EscrowHeld,
	})
	if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		return f.svc.CreateRefund(ctx, tx, partial, 1000, orders.RefundReasonDisputePartial)
	}); err != nil {
		t.Fatal(err)
	}
	partialRefundID := f.refundIDForOrder(t, partial)
	f.process(t, partialRefundID)
	_, _, partialPaystackID := f.refundRow(t, partialRefundID)
	partialBody := webhookBody(t, *partialPaystackID, 1000, partialRef)
	if outcome := f.deliverRefundWebhook(t, partialBody); outcome != payments.OutcomeProcessed {
		t.Fatalf("partial webhook outcome = %s, want processed", outcome)
	}
	// Phase 17b resolves the dispute and moves the order to completed; that
	// transition is out of this phase's scope, so the test sets it directly.
	if _, err := f.pool.Exec(ctx, `UPDATE orders SET status = 'completed' WHERE id = $1`, partial); err != nil {
		t.Fatal(err)
	}
	if posted, err := f.svc.ReleaseEscrow(ctx, partial); err != nil || !posted {
		t.Fatalf("release partial: posted=%v err=%v", posted, err)
	}

	held, _ := f.seedOrder(t, orderSpec{subtotal: 2000, status: orders.StatusPaid, escrowState: orders.EscrowHeld})
	_ = held

	balance, err := f.ledger.Balance(ctx, f.pool, ledger.Escrow)
	if err != nil {
		t.Fatal(err)
	}
	// Released and fully-refunded orders leave escrow untouched; the partial
	// order's remaining 3,000 (4,000 - 1,000) is released, so escrow keeps
	// only the plain held order's 2,000.
	if balance != -2000 {
		t.Errorf("escrow balance = %d, want -2000", balance)
	}
}

// webhookBody builds a refund webhook body from the refund's stored
// paystack_refund_id (a decimal string).
func webhookBody(t *testing.T, paystackRefundID string, amountPesewas int64, transactionReference string) []byte {
	t.Helper()
	var numericID int64
	if _, err := fmt.Sscanf(paystackRefundID, "%d", &numericID); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"id": numericID, "amount": amountPesewas, "status": "success",
		"currency": "GHS", "transaction_reference": transactionReference,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// deliverRefundWebhook runs OnRefundProcessed in its own transaction, the way
// payments.Service.HandleWebhook would, and returns the outcome.
func (f *refundFixture) deliverRefundWebhook(t *testing.T, body []byte) string {
	t.Helper()
	var outcome string
	if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		out, err := f.svc.OnRefundProcessed(context.Background(), tx, json.RawMessage(body))
		outcome = out
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return outcome
}

type eventHandler func(context.Context, pgx.Tx, json.RawMessage) (string, error)

// deliver runs a refund webhook handler in its own transaction, the way
// payments.Service.HandleWebhook does, and returns the outcome.
func (f *refundFixture) deliver(t *testing.T, h eventHandler, body []byte) string {
	t.Helper()
	outcome, err := f.deliverErr(h, body)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func (f *refundFixture) deliverErr(h eventHandler, body []byte) (string, error) {
	var outcome string
	err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		out, err := h(context.Background(), tx, json.RawMessage(body))
		outcome = out
		return err
	})
	return outcome, err
}

func refundEvent(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func paystackIDNum(t *testing.T, id string) int64 {
	t.Helper()
	var n int64
	if _, err := fmt.Sscanf(id, "%d", &n); err != nil {
		t.Fatal(err)
	}
	return n
}
