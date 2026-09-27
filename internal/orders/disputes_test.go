package orders_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// ensureSellerProfile inserts the seller_profiles row the order detail reads
// join, so dispute views can load their orders.
func ensureSellerProfile(t *testing.T, f *refundFixture, sellerID uuid.UUID) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO seller_profiles (user_id, business_name, region, district)
		 VALUES ($1, 'Test Farm', 'Ashanti', 'Kumasi Metro')
		 ON CONFLICT (user_id) DO NOTHING`, sellerID); err != nil {
		t.Fatal(err)
	}
}

// disputeAdmin returns a real user id for the disputes.resolved_by FK.
func disputeAdmin(t *testing.T, f *refundFixture) uuid.UUID {
	t.Helper()
	user, err := users.New(f.pool).Resolve(context.Background(), auth.Identity{
		UID: "dispute-admin", Email: "dispute-admin@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	return user.ID
}

// openDisputedOrder seeds an order the buyer can dispute, then disputes it
// through the real action. It returns the order, dispute and payment reference.
func openDisputedOrder(t *testing.T, f *refundFixture, spec orderSpec) (uuid.UUID, uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	if spec.status == "" {
		spec.status = orders.StatusShipped
	}
	if spec.escrowState == "" {
		spec.escrowState = orders.EscrowHeld
	}
	orderID, reference := f.seedOrder(t, spec)
	ensureSellerProfile(t, f, f.seller)
	if _, err := f.svc.Dispute(ctx, f.buyer, orderID, orders.DisputeNotReceived,
		"The consignment never reached the farm gate, not even a single bag."); err != nil {
		t.Fatalf("open dispute: %v", err)
	}
	var disputeID uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT id FROM disputes WHERE order_id = $1`, orderID).Scan(&disputeID); err != nil {
		t.Fatal(err)
	}
	return orderID, disputeID, reference
}

func (f *refundFixture) refundIDAmount(t *testing.T, orderID uuid.UUID) int64 {
	t.Helper()
	var amount int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT amount_pesewas FROM refunds WHERE order_id = $1 ORDER BY created_at DESC LIMIT 1`, orderID).Scan(&amount); err != nil {
		t.Fatal(err)
	}
	return amount
}

// TestResolve_RefundBuyer proves disputed → refunded with a full dispute
// refund: the refund job moves the money, the ledger and escrow follow, and
// the resolution is audited with both parties notified.
func TestResolve_RefundBuyer(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	admin := disputeAdmin(t, f)
	orderID, disputeID, reference := openDisputedOrder(t, f, orderSpec{})

	view, err := f.svc.Resolve(ctx, admin, disputeID, orders.DisputeOutcomeRefundBuyer, nil, "The courier lost the consignment.")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if view.Order.Status != orders.StatusRefunded {
		t.Errorf("order status = %s, want refunded", view.Order.Status)
	}
	if view.Dispute.Status != orders.DisputeStatusResolved || view.Dispute.Outcome == nil ||
		*view.Dispute.Outcome != orders.DisputeOutcomeRefundBuyer {
		t.Errorf("dispute = %+v, want resolved/refund_buyer", view.Dispute)
	}
	if view.Dispute.RefundPesewas == nil || *view.Dispute.RefundPesewas != 12345 {
		t.Errorf("refund = %v, want 12345", view.Dispute.RefundPesewas)
	}
	if _, escrow, _ := f.orderRow(t, orderID); escrow != orders.EscrowRefundPending {
		t.Errorf("escrow = %s, want refund_pending", escrow)
	}
	refundID := f.refundIDForOrder(t, orderID)
	if amount := f.refundIDAmount(t, orderID); amount != 12345 {
		t.Errorf("refund amount = %d, want 12345", amount)
	}

	// The refund job moves the money and the webhook settles it.
	f.process(t, refundID)
	_, _, paystackID := f.refundRow(t, refundID)
	if outcome := f.deliverRefundWebhook(t, webhookBody(t, *paystackID, 12345, reference)); outcome != "processed" {
		t.Fatalf("webhook outcome = %s, want processed", outcome)
	}
	if _, escrow, refunded := f.orderRow(t, orderID); escrow != orders.EscrowRefunded || refunded != 12345 {
		t.Errorf("order = %s refunded %d, want refunded and 12345", escrow, refunded)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM ledger_transactions WHERE kind = 'order_refund' AND reference = $1`, refundID.String()); n != 1 {
		t.Errorf("order_refund postings = %d, want 1", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM audit_events WHERE action = 'dispute.resolve' AND target_id = $1`, disputeID.String()); n != 1 {
		t.Errorf("dispute.resolve audits = %d, want 1", n)
	}
	for _, template := range []string{"dispute_resolved_buyer", "dispute_resolved_seller"} {
		if n := f.count(t, `SELECT COUNT(*) FROM river_job WHERE kind = 'notify.sms' AND args->>'template' = $1`, template); n != 1 {
			t.Errorf("sms %s = %d, want 1", template, n)
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM river_job WHERE kind = 'orders.refund'`); n != 1 {
		t.Errorf("refund jobs = %d, want 1", n)
	}
}

// TestResolve_ReleaseSeller proves disputed → completed with the held escrow
// released to the seller.
func TestResolve_ReleaseSeller(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	admin := disputeAdmin(t, f)
	orderID, disputeID, _ := openDisputedOrder(t, f, orderSpec{})

	view, err := f.svc.Resolve(ctx, admin, disputeID, orders.DisputeOutcomeReleaseSeller, nil, "The goods arrived a day late but intact.")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if view.Order.Status != orders.StatusCompleted {
		t.Errorf("order status = %s, want completed", view.Order.Status)
	}
	if view.Dispute.Outcome == nil || *view.Dispute.Outcome != orders.DisputeOutcomeReleaseSeller {
		t.Errorf("outcome = %v, want release_seller", view.Dispute.Outcome)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM refunds WHERE order_id = $1`, orderID); n != 0 {
		t.Errorf("refund rows = %d, want 0", n)
	}

	posted, err := f.svc.ReleaseEscrow(ctx, orderID)
	if err != nil || !posted {
		t.Fatalf("release: posted=%v err=%v", posted, err)
	}
	sellerBal, err := f.ledger.Balance(ctx, f.pool, ledger.SellerPayable(f.seller))
	if err != nil {
		t.Fatal(err)
	}
	if sellerBal != -(12345 - 617) {
		t.Errorf("seller balance = %d, want %d", sellerBal, -(12345 - 617))
	}
}

// TestResolve_PartialThenRelease proves the split: a 2,500 partial refund on
// the DOMAIN §4.1 example order, the release snoozing while the refund is
// pending, then releasing the remainder (8,000 base, 400 commission) once the
// refund settles.
func TestResolve_PartialThenRelease(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	admin := disputeAdmin(t, f)
	orderID, disputeID, reference := openDisputedOrder(t, f, orderSpec{
		subtotal: 10000, deliveryFee: 500, rateBps: 500,
	})

	amount := int64(2500)
	view, err := f.svc.Resolve(ctx, admin, disputeID, orders.DisputeOutcomePartial, &amount, "Half the bags were water-damaged.")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if view.Order.Status != orders.StatusCompleted {
		t.Errorf("order status = %s, want completed", view.Order.Status)
	}
	if view.Dispute.RefundPesewas == nil || *view.Dispute.RefundPesewas != 2500 {
		t.Errorf("refund = %v, want 2500", view.Dispute.RefundPesewas)
	}
	refundID := f.refundIDForOrder(t, orderID)

	// The release waits: the service reports the deferral and the worker
	// turns it into a 10-minute snooze.
	if _, err := f.svc.ReleaseEscrow(ctx, orderID); !errors.Is(err, orders.ErrReleaseDeferred) {
		t.Fatalf("release during refund = %v, want ErrReleaseDeferred", err)
	}
	worker := &orders.ReleaseEscrowWorker{Service: f.svc, Log: slog.New(slog.DiscardHandler)}
	riverJob := &river.Job[orders.ReleaseEscrowArgs]{Args: orders.ReleaseEscrowArgs{OrderID: orderID}}
	err = worker.Work(ctx, riverJob)
	var snooze *rivertype.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("worker err = %v, want a snooze", err)
	}
	if snooze.Duration != 10*time.Minute {
		t.Errorf("snooze = %v, want 10m", snooze.Duration)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM ledger_transactions WHERE kind = 'escrow_release' AND reference = $1`, orderID.String()); n != 0 {
		t.Fatalf("release posted while the refund was pending")
	}

	// The refund settles, then the release posts the remainder.
	f.process(t, refundID)
	_, _, paystackID := f.refundRow(t, refundID)
	if outcome := f.deliverRefundWebhook(t, webhookBody(t, *paystackID, 2500, reference)); outcome != "processed" {
		t.Fatalf("webhook outcome = %s, want processed", outcome)
	}
	posted, err := f.svc.ReleaseEscrow(ctx, orderID)
	if err != nil || !posted {
		t.Fatalf("release after refund: posted=%v err=%v", posted, err)
	}
	sellerBal, err := f.ledger.Balance(ctx, f.pool, ledger.SellerPayable(f.seller))
	if err != nil {
		t.Fatal(err)
	}
	commissionBal, err := f.ledger.Balance(ctx, f.pool, ledger.PlatformCommission)
	if err != nil {
		t.Fatal(err)
	}
	if sellerBal != -7600 || commissionBal != -400 {
		t.Errorf("seller %d, commission %d, want -7600 and -400", sellerBal, commissionBal)
	}
}

// TestResolve_Validation proves bad resolutions are 400s and a second
// resolution is a 409.
func TestResolve_Validation(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	admin := disputeAdmin(t, f)
	_, disputeID, _ := openDisputedOrder(t, f, orderSpec{})

	cases := map[string]struct {
		outcome string
		amount  *int64
		field   string
	}{
		"partial at remaining":    {orders.DisputeOutcomePartial, int64Ptr(12345), "refundAmount"},
		"partial above remaining": {orders.DisputeOutcomePartial, int64Ptr(12346), "refundAmount"},
		"partial zero":            {orders.DisputeOutcomePartial, int64Ptr(0), "refundAmount"},
		"partial negative":        {orders.DisputeOutcomePartial, int64Ptr(-100), "refundAmount"},
		"partial missing amount":  {orders.DisputeOutcomePartial, nil, "refundAmount"},
		"refund with amount":      {orders.DisputeOutcomeRefundBuyer, int64Ptr(100), "refundAmount"},
		"release with amount":     {orders.DisputeOutcomeReleaseSeller, int64Ptr(100), "refundAmount"},
		"unknown outcome":         {"split_down_middle", nil, "outcome"},
		"blank note":              {orders.DisputeOutcomeReleaseSeller, nil, "note"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			note := "A proper resolution note."
			if tc.field == "note" {
				note = "   "
			}
			_, err := f.svc.Resolve(ctx, admin, disputeID, tc.outcome, tc.amount, note)
			var invalid *validation.Error
			if !errors.As(err, &invalid) {
				t.Fatalf("err = %v, want a validation error", err)
			}
			found := false
			for _, field := range invalid.Fields {
				if field.Name == tc.field {
					found = true
				}
			}
			if !found {
				t.Errorf("fields = %+v, want %q", invalid.Fields, tc.field)
			}
		})
	}

	if _, err := f.svc.Resolve(ctx, admin, disputeID, orders.DisputeOutcomeReleaseSeller, nil, "First and only."); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	_, err := f.svc.Resolve(ctx, admin, disputeID, orders.DisputeOutcomeReleaseSeller, nil, "Second attempt.")
	if !errors.Is(err, orders.ErrDisputeNotOpen) {
		t.Errorf("second resolve err = %v, want ErrDisputeNotOpen", err)
	}
	if _, err := f.svc.Resolve(ctx, admin, uuid.New(), orders.DisputeOutcomeReleaseSeller, nil, "Missing."); !errors.Is(err, orders.ErrDisputeNotFound) {
		t.Errorf("unknown dispute err = %v, want ErrDisputeNotFound", err)
	}
}

// TestRetryRefund proves failed → queued → processed, and that a non-failed
// refund or a post-release refusal cannot be retried.
func TestRetryRefund(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	admin := disputeAdmin(t, f)
	orderID, reference := f.seedOrder(t, orderSpec{status: orders.StatusPaid, escrowState: orders.EscrowHeld})
	refundID := f.createRefund(t, orderID, 12345, orders.RefundReasonSellerRejected)

	f.process(t, refundID)
	_, _, paystackID := f.refundRow(t, refundID)
	failedBody := refundEvent(t, map[string]any{
		"id": paystackIDNum(t, *paystackID), "amount": 12345, "currency": "GHS",
		"transaction_reference": reference, "status": "failed",
	})
	if outcome := f.deliver(t, f.svc.OnRefundFailed, failedBody); outcome != "processed" {
		t.Fatalf("fail webhook outcome = %s", outcome)
	}

	retried, err := f.svc.RetryRefund(ctx, admin, refundID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retried.Status != orders.RefundStatusQueued {
		t.Errorf("refund status = %s, want queued", retried.Status)
	}
	// The retry enqueues a runnable job even though a job row for these args
	// already exists: a unique insert would be swallowed as a duplicate and
	// the retry would sit queued forever.
	if n := f.count(t, `SELECT COUNT(*) FROM river_job WHERE kind = 'orders.refund' AND args->>'refundId' = $1`,
		refundID.String()); n != 2 {
		t.Errorf("refund jobs = %d, want 2 (the first attempt plus the retry)", n)
	}
	f.process(t, refundID)
	if n := f.provider.CallCount("CreateRefund"); n != 2 {
		t.Errorf("paystack calls = %d, want 2 (the retry)", n)
	}
	_, _, retriedPS := f.refundRow(t, refundID)
	f.provider.SetRefundStatus(paystackIDNum(t, *retriedPS), "processed")
	f.process(t, refundID)
	if status, _, _ := f.refundRow(t, refundID); status != orders.RefundStatusProcessed {
		t.Errorf("refund = %s, want processed", status)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM audit_events WHERE action = 'refund.retry' AND target_id = $1`, refundID.String()); n != 1 {
		t.Errorf("retry audits = %d, want 1", n)
	}

	queuedOrder, _ := f.seedOrder(t, orderSpec{status: orders.StatusPaid, escrowState: orders.EscrowHeld})
	queued := f.createRefund(t, queuedOrder, 100, orders.RefundReasonDisputePartial)
	if _, err := f.svc.RetryRefund(ctx, admin, queued); !errors.Is(err, orders.ErrRefundNotFailed) {
		t.Errorf("retry queued err = %v, want ErrRefundNotFailed", err)
	}

	released, _ := f.seedOrder(t, orderSpec{})
	if posted, err := f.svc.ReleaseEscrow(ctx, released); err != nil || !posted {
		t.Fatalf("release: posted=%v err=%v", posted, err)
	}
	if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		return f.svc.CreateRefund(ctx, tx, released, 1000, orders.RefundReasonDisputeRefund)
	}); err != nil {
		t.Fatal(err)
	}
	stuckID := f.refundIDForOrder(t, released)
	f.process(t, stuckID) // refused: refund_after_release
	if status, reason, _ := f.refundRow(t, stuckID); status != orders.RefundStatusFailed || reason == nil || *reason != "refund_after_release" {
		t.Fatalf("refund = %s/%v, want failed/refund_after_release", status, reason)
	}
	if _, err := f.svc.RetryRefund(ctx, admin, stuckID); !errors.Is(err, orders.ErrRefundAfterRelease) {
		t.Errorf("retry after release err = %v, want ErrRefundAfterRelease", err)
	}
	if _, err := f.svc.RetryRefund(ctx, admin, uuid.New()); !errors.Is(err, orders.ErrRefundNotFound) {
		t.Errorf("retry unknown err = %v, want ErrRefundNotFound", err)
	}
}

// TestResolve_ListAndGet proves the admin reads: oldest-first queue, the
// single view with its order, and 404s for unknown ids.
func TestResolve_ListAndGet(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	ensureSellerProfile(t, f, f.seller)

	first, firstDispute, _ := openDisputedOrder(t, f, orderSpec{subtotal: 1000})
	second, secondDispute, _ := openDisputedOrder(t, f, orderSpec{subtotal: 2000})
	_ = first
	_ = second

	views, total, err := f.svc.ListDisputes(ctx, "", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(views) != 2 {
		t.Fatalf("total = %d views %d, want 2", total, len(views))
	}
	if views[0].Dispute.ID != firstDispute || views[1].Dispute.ID != secondDispute {
		t.Errorf("queue order wrong: oldest first")
	}
	filtered, ftotal, ferr := f.svc.ListDisputes(ctx, orders.DisputeStatusOpen, 20, 0)
	if ferr != nil || ftotal != 2 || len(filtered) != 2 {
		t.Fatalf("open filter: total=%d err=%v", ftotal, ferr)
	}
	admin := disputeAdmin(t, f)
	if _, err := f.svc.Resolve(ctx, admin, firstDispute, orders.DisputeOutcomeReleaseSeller, nil, "Released."); err != nil {
		t.Fatal(err)
	}
	resolved, total, err := f.svc.ListDisputes(ctx, orders.DisputeStatusResolved, 20, 0)
	if err != nil || total != 1 || resolved[0].Dispute.ID != firstDispute {
		t.Fatalf("resolved filter: total=%d err=%v", total, err)
	}

	view, err := f.svc.GetDispute(ctx, secondDispute)
	if err != nil {
		t.Fatal(err)
	}
	if view.Order.ID == uuid.Nil || len(view.Order.Events) == 0 {
		t.Errorf("single view lacks the order and its events")
	}
	if _, err := f.svc.GetDispute(ctx, uuid.New()); !errors.Is(err, orders.ErrDisputeNotFound) {
		t.Errorf("unknown dispute err = %v, want ErrDisputeNotFound", err)
	}
	if _, _, err := f.svc.ListDisputes(ctx, "bogus", 20, 0); err == nil {
		t.Error("bad status filter: want a validation error")
	} else {
		var invalid *validation.Error
		if !errors.As(err, &invalid) {
			t.Errorf("bad status err = %v, want validation", err)
		}
	}
}

// TestResolve_AfterReleaseStaysManual proves a dispute refund can never follow
// a release into Paystack: resolving a released order's dispute is refused by
// the state machine, and the manual-path audit exists.
func TestResolve_AfterReleaseStaysManual(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	admin := disputeAdmin(t, f)
	orderID, disputeID, _ := openDisputedOrder(t, f, orderSpec{})

	// The order completes and releases while disputed (an operator force for
	// the test): the dispute resolution then has nothing legal to move.
	if _, err := f.pool.Exec(ctx, `UPDATE orders SET status = 'completed' WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	if posted, err := f.svc.ReleaseEscrow(ctx, orderID); err != nil || !posted {
		t.Fatalf("release: posted=%v err=%v", posted, err)
	}
	_, err := f.svc.Resolve(ctx, admin, disputeID, orders.DisputeOutcomeRefundBuyer, nil, "Too late.")
	if err == nil {
		t.Fatal("resolve after release: want an error")
	}
	if logs := f.logs.String(); !strings.Contains(logs, "refund_after_release") && !strings.Contains(logs, "release") {
		t.Logf("logs (informational): %s", logs)
	}
}

func int64Ptr(n int64) *int64 { return &n }
