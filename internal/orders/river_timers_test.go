package orders_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/orders"
)

// riverTimerFixture runs the sweep workers for real: jobs are inserted through
// the client and executed by River, never by calling the service directly.
type riverTimerFixture struct {
	*tableFixture
	client *jobs.Client
	events <-chan *river.Event
}

func newRiverTimerFixture(t *testing.T, now time.Time) *riverTimerFixture {
	t.Helper()
	base := newTableFixture(t)
	base.svc.Now = func() time.Time { return now }
	log := slog.New(slog.DiscardHandler)
	reg := jobs.NewRegistry()
	orders.RegisterJobs(reg, base.svc, log)
	notify.Register(reg, notify.NewWorker(notify.LogOnly{Log: log}, log, base.pool))
	client, err := jobs.NewClient(base.pool, reg, log, jobs.Options{Work: true, FetchPollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := client.Subscribe(river.EventKindJobCompleted)
	t.Cleanup(cancel)
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	base.svc.AttachJobClient(client)
	t.Cleanup(func() {
		if err := jobs.Stop(client, 5*time.Second, 2*time.Second, log); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	return &riverTimerFixture{tableFixture: base, client: client, events: events}
}

func (f *riverTimerFixture) runSweep(t *testing.T, args river.JobArgs, kind string) {
	t.Helper()
	if _, err := f.client.Insert(context.Background(), args, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(20 * time.Second)
	for {
		select {
		case event := <-f.events:
			if event.Kind == river.EventKindJobCompleted && event.Job.Kind == kind {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", kind)
		}
	}
}

// TestSweeps_RiverExecution inserts both periodic sweeps through River and
// proves the workers move overdue orders: an unaccepted paid order is
// system-cancelled with its stock back, a due delivered order completes.
func TestSweeps_RiverExecution(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	f := newRiverTimerFixture(t, now)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx,
		`UPDATE orders SET paid_at = $2 WHERE id = $1`, f.orderID, now.Add(-49*time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.runSweep(t, orders.AutoCancelArgs{}, "orders.auto_cancel_unaccepted")
	var status, escrow, actor, note string
	if err := f.pool.QueryRow(ctx,
		`SELECT o.status, o.escrow_state, e.actor_type, e.note FROM orders o
		 JOIN order_events e ON e.order_id = o.id AND e.to_status = 'cancelled'
		 WHERE o.id = $1`, f.orderID).Scan(&status, &escrow, &actor, &note); err != nil {
		t.Fatal(err)
	}
	if status != orders.StatusCancelled || escrow != orders.EscrowRefundPending {
		t.Errorf("auto-cancelled = %s/%s, want cancelled/refund_pending", status, escrow)
	}
	if actor != orders.ActorSystem || note != "seller_timeout" {
		t.Errorf("event = %s/%q, want the system blaming the seller's clock", actor, note)
	}

	// Back to delivered and due: the second sweep completes it through River.
	if _, err := f.pool.Exec(ctx,
		`UPDATE orders SET status = 'delivered', delivered_at = $2, auto_complete_at = $3,
		   cancelled_at = NULL WHERE id = $1`,
		f.orderID, now.Add(-4*24*time.Hour), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.runSweep(t, orders.AutoCompleteArgs{}, "orders.auto_complete")
	var completed, released string
	if err := f.pool.QueryRow(ctx,
		`SELECT status, escrow_state FROM orders WHERE id = $1`, f.orderID).Scan(&completed, &released); err != nil {
		t.Fatal(err)
	}
	if completed != orders.StatusCompleted || released != orders.EscrowReleased {
		t.Errorf("auto-completed = %s/%s, want completed/released", completed, released)
	}
}
