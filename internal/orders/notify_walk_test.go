package orders_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/orders"

	"github.com/google/uuid"
)

// TestNotify_EveryTransitionEnqueuesSMS walks the real endpoint transitions and
// inspects the River table for the expected template and recipient.
func TestNotify_EveryTransitionEnqueuesSMS(t *testing.T) {
	f := newTableFixture(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	reg := jobs.NewRegistry()
	orders.RegisterJobs(reg, f.svc, log)
	notify.Register(reg, notify.NewWorker(notify.LogOnly{Log: log}, log, f.pool))
	client, err := jobs.NewClient(f.pool, reg, log, jobs.Options{Work: false})
	if err != nil {
		t.Fatal(err)
	}
	f.svc.AttachJobClient(client)
	t.Cleanup(func() {
		if err := jobs.Stop(client, time.Second, time.Second, log); err != nil {
			t.Errorf("stop: %v", err)
		}
	})

	notifyJobs := func() []struct {
		template, recipient string
	} {
		rows, err := f.pool.Query(ctx,
			`SELECT args->>'template', args->>'userId' FROM river_job WHERE kind = 'notify.sms'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []struct{ template, recipient string }
		for rows.Next() {
			var template, recipient string
			if err := rows.Scan(&template, &recipient); err != nil {
				t.Fatal(err)
			}
			out = append(out, struct{ template, recipient string }{template, recipient})
		}
		return out
	}

	type step struct {
		name      string
		run       func() error
		template  string
		recipient uuid.UUID
	}
	// A paid order walks the happy path to completion.
	steps := []step{
		{"paid notifies the seller", func() error {
			return f.seedPaid(ctx)
		}, "", uuid.Nil},
		{"accept", func() error {
			_, err := f.svc.Accept(ctx, f.seller, f.orderID)
			return err
		}, notify.TemplateOrderAcceptedBuyer, f.buyer},
		{"ship", func() error {
			_, err := f.svc.Ship(ctx, f.seller, f.orderID, "TRK-1")
			return err
		}, notify.TemplateOrderShippedBuyer, f.buyer},
		{"mark delivered", func() error {
			_, err := f.svc.MarkDelivered(ctx, f.seller, f.orderID)
			return err
		}, notify.TemplateOrderDeliveredBuyer, f.buyer},
		{"confirm receipt", func() error {
			_, err := f.svc.ConfirmReceipt(ctx, f.buyer, f.orderID)
			return err
		}, notify.TemplateOrderCompletedSeller, f.seller},
	}
	for _, expected := range steps {
		if err := expected.run(); err != nil {
			t.Fatalf("%s: %v", expected.name, err)
		}
	}
	jobs := notifyJobs()
	want := map[string]string{
		notify.TemplateOrderAcceptedBuyer:   f.buyer.String(),
		notify.TemplateOrderShippedBuyer:    f.buyer.String(),
		notify.TemplateOrderDeliveredBuyer:  f.buyer.String(),
		notify.TemplateOrderCompletedSeller: f.seller.String(),
	}
	found := map[string]string{}
	for _, job := range jobs {
		if wantRecipient, ok := want[job.template]; ok {
			if job.recipient != wantRecipient {
				t.Errorf("%s went to %s, want %s", job.template, job.recipient, wantRecipient)
			}
			found[job.template] = job.recipient
		}
	}
	for template, recipient := range want {
		if _, ok := found[template]; !ok {
			t.Errorf("no %s job for %s", template, recipient)
		}
	}

	// The seller rejecting a fresh paid order notifies the buyer with the
	// rejected template, and restores the stock and queues the refund.
	second := seedSecondOrder(t, f)
	if _, err := f.svc.Reject(ctx, f.seller, second, "Out of stock this week."); err != nil {
		t.Fatal(err)
	}
	after := notifyJobs()
	rejected := false
	for _, job := range after {
		if job.template == notify.TemplateOrderRejectedBuyer && job.recipient == f.buyer.String() {
			rejected = true
		}
	}
	if !rejected {
		t.Error("the seller reject did not notify the buyer with order_rejected_buyer")
	}
}

// seedPaid brings the fixture's order back to paid for the walk.
func (f *tableFixture) seedPaid(ctx context.Context) error {
	_, err := f.pool.Exec(ctx,
		`UPDATE orders SET status = 'paid', escrow_state = 'held', paid_at = now(),
		   accepted_at = NULL, shipped_at = NULL, delivered_at = NULL,
		   completed_at = NULL, cancelled_at = NULL, auto_complete_at = NULL
		 WHERE id = $1`, f.orderID)
	return err
}

// seedSecondOrder creates a second paid order to drive the reject path.
func seedSecondOrder(t *testing.T, f *tableFixture) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var checkoutID, orderID uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO checkouts (buyer_id, idempotency_key, request_hash, base_pesewas,
		                        processing_fee_pesewas, charge_pesewas, expires_at)
		 VALUES ($1, $2, 'table-test-2', 1000, 20, 1020, now() + interval '30 minutes')
		 RETURNING id`, f.buyer, uuid.New()).Scan(&checkoutID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO orders (checkout_id, buyer_id, seller_id, status, escrow_state,
		                     subtotal_pesewas, delivery_fee_pesewas, base_pesewas,
		                     commission_rate_bps, commission_pesewas, delivery_method)
		 VALUES ($1, $2, $3, 'paid', 'held', 1000, 0, 1000, 500, 50, 'pickup')
		 RETURNING id`, checkoutID, f.buyer, f.seller).Scan(&orderID); err != nil {
		t.Fatal(err)
	}
	return orderID
}
