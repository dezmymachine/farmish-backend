package orders_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/orders"
)

// TestAutoCancel_FakeClock proves the 48-hour acceptance timer: nothing at
// 47h59m, a system cancel with the stock restored and the refund queued at 48h.
func TestAutoCancel_FakeClock(t *testing.T) {
	f := newTimerFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	f.svc.Now = func() time.Time { return now }

	// The order was paid a minute before the window closes: nothing happens.
	if _, err := f.pool.Exec(ctx,
		`UPDATE orders SET paid_at = $2 WHERE id = $1`, f.orderID,
		now.Add(-47*time.Hour-59*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// One reserved unit: the listing is empty until the order resolves.
	f.seedStock(t, 0)
	cancelled, err := f.svc.AutoCancelUnaccepted(ctx)
	if err != nil || cancelled != 0 {
		t.Fatalf("at 47h59m: cancelled = %d, err = %v", cancelled, err)
	}
	if got := f.stock(t); got != 0 {
		t.Errorf("stock = %d, want 0 (still reserved)", got)
	}

	// One more minute: the system cancels, restores stock and queues refunds.
	if _, err := f.pool.Exec(ctx,
		`UPDATE orders SET paid_at = $2 WHERE id = $1`, f.orderID,
		now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	cancelled, err = f.svc.AutoCancelUnaccepted(ctx)
	if err != nil || cancelled != 1 {
		t.Fatalf("at 48h: cancelled = %d, err = %v", cancelled, err)
	}
	if got := f.stock(t); got != 1 {
		t.Errorf("stock = %d, want 1 restored", got)
	}
	var status, escrowState, note, actorType string
	if err := f.pool.QueryRow(ctx,
		`SELECT o.status, o.escrow_state, e.note, e.actor_type
		 FROM orders o JOIN order_events e ON e.order_id = o.id AND e.to_status = 'cancelled'
		 WHERE o.id = $1`, f.orderID).Scan(&status, &escrowState, &note, &actorType); err != nil {
		t.Fatal(err)
	}
	if status != orders.StatusCancelled || escrowState != orders.EscrowRefundPending ||
		note != "seller_timeout" || actorType != orders.ActorSystem {
		t.Errorf("cancel = %s/%s note %q by %s, want cancelled/refund_pending seller_timeout by system",
			status, escrowState, note, actorType)
	}
	var refundJobs int64
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM river_job WHERE kind = 'orders.refund'`).Scan(&refundJobs); err != nil {
		t.Fatal(err)
	}
	if refundJobs != 1 {
		t.Errorf("refund jobs = %d, want 1", refundJobs)
	}
	var refundRows int64
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM refunds WHERE order_id = $1 AND reason = 'seller_timeout'`, f.orderID).Scan(&refundRows); err != nil {
		t.Fatal(err)
	}
	if refundRows != 1 {
		t.Errorf("refund rows = %d, want 1 seller_timeout", refundRows)
	}
	var notifyJobs int64
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM river_job WHERE kind = 'notify.sms'`).Scan(&notifyJobs); err != nil {
		t.Fatal(err)
	}
	if notifyJobs != 2 {
		t.Errorf("notify jobs = %d, want 2 (buyer and seller)", notifyJobs)
	}
}

// TestAutoComplete_FakeClock proves the 3-day completion timer, and that an
// open dispute holds it.
func TestAutoComplete_FakeClock(t *testing.T) {
	f := newTimerFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	f.svc.Now = func() time.Time { return now }

	// Delivered two days ago: the deadline is delivered_at + 3 days, so
	// nothing is due yet.
	if _, err := f.pool.Exec(ctx,
		`UPDATE orders SET status = 'delivered', delivered_at = $2, auto_complete_at = $3
		 WHERE id = $1`, f.orderID, now.Add(-48*time.Hour), now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	completed, err := f.svc.AutoCompleteDelivered(ctx)
	if err != nil || completed != 0 {
		t.Fatalf("at day 2: completed = %d, err = %v", completed, err)
	}

	// Due now: completes and queues the release.
	completed, err = f.svc.AutoCompleteDelivered(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if completed != 0 {
		t.Fatalf("due order completed without advancing the clock: %d", completed)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE orders SET auto_complete_at = $2 WHERE id = $1`, f.orderID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	completed, err = f.svc.AutoCompleteDelivered(ctx)
	if err != nil || completed != 1 {
		t.Fatalf("at deadline: completed = %d, err = %v", completed, err)
	}
	var status, escrowState string
	if err := f.pool.QueryRow(ctx,
		`SELECT status, escrow_state FROM orders WHERE id = $1`, f.orderID).Scan(&status, &escrowState); err != nil {
		t.Fatal(err)
	}
	// Escrow stays held until the release job (Phase 17a) actually posts the
	// ledger entries; the sweep only completes the order and queues it.
	if status != orders.StatusCompleted || escrowState != orders.EscrowHeld {
		t.Errorf("order = %s/%s, want completed/held", status, escrowState)
	}
	var releaseJobs int64
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM river_job WHERE kind = 'orders.release_escrow'`).Scan(&releaseJobs); err != nil {
		t.Fatal(err)
	}
	if releaseJobs != 1 {
		t.Errorf("release jobs = %d, want 1", releaseJobs)
	}
}

// TestDispute_CreatesRowAndStopsTimer proves the dispute row stops the
// auto-complete sweep.
func TestDispute_CreatesRowAndStopsTimer(t *testing.T) {
	f := newTimerFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	f.svc.Now = func() time.Time { return now }
	if _, err := f.pool.Exec(ctx,
		`UPDATE orders SET status = 'delivered', delivered_at = $2, auto_complete_at = $3
		 WHERE id = $1`, f.orderID, now.Add(-48*time.Hour), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.Dispute(ctx, f.buyer, f.orderID, orders.DisputeNotReceived, "The parcel never arrived at the farm."); err != nil {
		t.Fatal(err)
	}
	var reason, status string
	if err := f.pool.QueryRow(ctx,
		`SELECT reason, status FROM disputes WHERE order_id = $1`, f.orderID).Scan(&reason, &status); err != nil {
		t.Fatal(err)
	}
	if reason != orders.DisputeNotReceived || status != "open" {
		t.Errorf("dispute = %s/%s, want not_received/open", reason, status)
	}
	var orderStatus string
	if err := f.pool.QueryRow(ctx,
		`SELECT status FROM orders WHERE id = $1`, f.orderID).Scan(&orderStatus); err != nil {
		t.Fatal(err)
	}
	if orderStatus != orders.StatusDisputed {
		t.Errorf("order status = %s, want disputed", orderStatus)
	}

	// The open dispute holds the sweep even past the deadline.
	completed, err := f.svc.AutoCompleteDelivered(ctx)
	if err != nil || completed != 0 {
		t.Fatalf("with an open dispute: completed = %d, err = %v", completed, err)
	}

	// A second dispute on the same order is refused by the unique row, even
	// when the status would allow the move again.
	if _, err := f.pool.Exec(ctx,
		`UPDATE orders SET status = 'delivered' WHERE id = $1`, f.orderID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Dispute(ctx, f.buyer, f.orderID, orders.DisputeDamaged, "A second case on the same order."); !isDisputeExists(err) {
		t.Errorf("second dispute = %v, want ErrDisputeExists", err)
	}
}

func isDisputeExists(err error) bool { return errors.Is(err, orders.ErrDisputeExists) }

// timerFixture adds a River client to the seeded order, so the timer tests
// exercise the real job enqueues.
type timerFixture struct {
	*tableFixture
}

func newTimerFixture(t *testing.T) *timerFixture {
	t.Helper()
	base := newTableFixture(t)
	f := &timerFixture{base}
	log := slog.New(slog.DiscardHandler)
	reg := jobs.NewRegistry()
	orders.RegisterJobs(reg, f.svc, log)
	notify.Register(reg, notify.NewWorker(notify.LogOnly{Log: log}, log, f.pool))
	client, err := jobs.NewClient(f.pool, reg, log, jobs.Options{Work: false, FetchPollInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	f.svc.AttachJobClient(client)
	t.Cleanup(func() {
		if err := jobs.Stop(client, time.Second, time.Second, log); err != nil {
			t.Errorf("stop jobs: %v", err)
		}
	})
	return f
}

// seedStock gives the order one reserved unit by writing a listing and an
// order item, the way checkout would have.
func (f *timerFixture) seedStock(t *testing.T, available int32) {
	t.Helper()
	ctx := context.Background()
	var listingID uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO listings (seller_id, category_id, title, slug, description,
		                       price_pesewas, unit, quantity_available, min_order_qty,
		                       item_state, region, district, offers_pickup, offers_seller_delivery)
		 SELECT $1, c.id, 'Timer Maize', 'timer-maize', 'Maize stored in a silo for pickup.',
		        1000, 'kg', $2, 1, 'grade_a', 'Ashanti', 'Kumasi Metro', true, false
		 FROM categories c WHERE c.slug = 'fresh-produce-grains-cereals'
		 RETURNING id`, f.seller, available).Scan(&listingID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO order_items (order_id, listing_id, title, unit, unit_price_pesewas, quantity, line_total_pesewas)
		 VALUES ($1, $2, 'Timer Maize', 'kg', 1000, 1, 1000)`, f.orderID, listingID); err != nil {
		t.Fatal(err)
	}
}

func (f *timerFixture) stock(t *testing.T) int32 {
	t.Helper()
	var stock int32
	if err := f.pool.QueryRow(context.Background(),
		`SELECT quantity_available FROM listings
		 WHERE id = (SELECT listing_id FROM order_items WHERE order_id = $1 LIMIT 1)`,
		f.orderID).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	return stock
}
