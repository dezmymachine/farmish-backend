package orders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
)

// SweepBatch is how many orders one timer pass may move (the spec's batch of
// 100, taken with FOR UPDATE SKIP LOCKED).
const SweepBatch = 100

// ReleaseEscrowArgs queues an order's escrow release to its seller (DOMAIN
// §5.3.2, §4.1). The job recomputes the remaining base and commission from
// the order row itself, so no amount travels in the args.
type ReleaseEscrowArgs struct {
	OrderID uuid.UUID `json:"orderId"`
}

// Kind implements river.JobArgs.
func (ReleaseEscrowArgs) Kind() string { return "orders.release_escrow" }

// ReleaseEscrowWorker posts the escrow release. It is idempotent: a wrong
// state is a logged no-op, and the ledger's UNIQUE(kind, reference) stops a
// second posting for the same order.
type ReleaseEscrowWorker struct {
	river.WorkerDefaults[ReleaseEscrowArgs]
	Service *Service
	Log     *slog.Logger
}

// Work implements river.Worker.
func (w *ReleaseEscrowWorker) Work(ctx context.Context, job *river.Job[ReleaseEscrowArgs]) error {
	posted, err := w.Service.ReleaseEscrow(ctx, job.Args.OrderID)
	if err != nil {
		return err
	}
	if !posted {
		w.Log.Info("escrow release skipped: order is not completed with escrow held",
			slog.String("order_id", job.Args.OrderID.String()))
	}
	return nil
}

// RefundArgs queues one attempt of a queued refund's Paystack call. The
// refund row is created in the transition's own transaction, with
// status=queued; this job only makes the external call and reacts to it.
type RefundArgs struct {
	RefundID uuid.UUID `json:"refundId"`
}

// Kind implements river.JobArgs.
func (RefundArgs) Kind() string { return "orders.refund" }

// RefundWorker runs one step of a refund (ADR-0027). While the refund is
// pending it snoozes and re-checks with Paystack (snoozes do not use up
// attempts); only database errors return an error for River's retry.
type RefundWorker struct {
	river.WorkerDefaults[RefundArgs]
	Service *Service
	Log     *slog.Logger
}

// Work implements river.Worker.
func (w *RefundWorker) Work(ctx context.Context, job *river.Job[RefundArgs]) error {
	next, err := w.Service.ProcessRefund(ctx, job.Args.RefundID)
	if err != nil {
		return err
	}
	if next > 0 {
		return river.JobSnooze(next)
	}
	return nil
}

// AutoCancelArgs is the periodic sweep that cancels paid orders their seller
// never accepted.
type AutoCancelArgs struct{}

// Kind implements river.JobArgs.
func (AutoCancelArgs) Kind() string { return "orders.auto_cancel_unaccepted" }

// AutoCancelWorker cancels timed-out unaccepted orders.
type AutoCancelWorker struct {
	river.WorkerDefaults[AutoCancelArgs]
	Service *Service
	Log     *slog.Logger
}

// Work implements river.Worker.
func (w *AutoCancelWorker) Work(ctx context.Context, _ *river.Job[AutoCancelArgs]) error {
	n, err := w.Service.AutoCancelUnaccepted(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		w.Log.Info("auto-cancelled unaccepted orders", slog.Int("count", n))
	}
	return nil
}

// AutoCompleteArgs is the periodic sweep that completes delivered orders whose
// 3-day window closed without a dispute.
type AutoCompleteArgs struct{}

// Kind implements river.JobArgs.
func (AutoCompleteArgs) Kind() string { return "orders.auto_complete" }

// AutoCompleteWorker completes due delivered orders.
type AutoCompleteWorker struct {
	river.WorkerDefaults[AutoCompleteArgs]
	Service *Service
	Log     *slog.Logger
}

// Work implements river.Worker.
func (w *AutoCompleteWorker) Work(ctx context.Context, _ *river.Job[AutoCompleteArgs]) error {
	n, err := w.Service.AutoCompleteDelivered(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		w.Log.Info("auto-completed delivered orders", slog.Int("count", n))
	}
	return nil
}

// RegisterJobs adds the workers and the two hourly schedules. Both run on
// start, so a deployment immediately clears anything already overdue.
func RegisterJobs(r *jobs.Registry, svc *Service, log *slog.Logger) {
	jobs.Register(r, &ReleaseEscrowWorker{Service: svc, Log: log})
	jobs.Register(r, &RefundWorker{Service: svc, Log: log})
	jobs.Register(r, &AutoCancelWorker{Service: svc, Log: log})
	jobs.Register(r, &AutoCompleteWorker{Service: svc, Log: log})
	r.Every(time.Hour, func() river.JobArgs { return AutoCancelArgs{} }, true)
	r.Every(time.Hour, func() river.JobArgs { return AutoCompleteArgs{} }, true)
}

// AutoCancelUnaccepted cancels paid orders whose seller let the acceptance
// window (DOMAIN §3: 48h) lapse. The state is re-checked under the row lock,
// because the seller may have accepted since selection.
func (s *Service) AutoCancelUnaccepted(ctx context.Context) (int, error) {
	deadline := s.Now().Add(-s.sellerAcceptTimeout)
	rows, err := db.New(s.pool).ListUnacceptedPaidOrders(ctx, db.ListUnacceptedPaidOrdersParams{
		PaidBefore: deadline, Limit: SweepBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("list unaccepted orders: %w", err)
	}
	cancelled := 0
	for _, row := range rows {
		err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			order, effects, err := s.Transition(ctx, tx, row.ID, StatusCancelled, System(), "seller_timeout")
			if errors.Is(err, ErrInvalidTransition) || errors.Is(err, ErrForbidden) {
				return nil // the seller accepted, or another sweep won the row
			}
			if err != nil {
				return err
			}
			if err := s.SetEscrowState(ctx, tx, order.ID, EscrowRefundPending); err != nil {
				return err
			}
			if err := s.ApplyEffects(ctx, tx, order, effects); err != nil {
				return err
			}
			cancelled++
			return nil
		})
		if err != nil {
			return cancelled, err
		}
	}
	return cancelled, nil
}

// AutoCompleteDelivered completes delivered orders past their 3-day window
// with no open dispute, which queues the escrow release. The dispute check
// runs under the row lock, so a case opened mid-sweep still stops the timer.
func (s *Service) AutoCompleteDelivered(ctx context.Context) (int, error) {
	rows, err := db.New(s.pool).ListDueDeliveredOrders(ctx, db.ListDueDeliveredOrdersParams{
		DueAt: s.Now(), Limit: SweepBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("list due delivered orders: %w", err)
	}
	completed := 0
	for _, row := range rows {
		err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			if _, err := db.New(tx).GetOpenDisputeByOrder(ctx, row.ID); err == nil {
				return nil // an open dispute holds this order
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("check dispute: %w", err)
			}
			order, effects, err := s.Transition(ctx, tx, row.ID, StatusCompleted, System(), "auto_complete")
			if errors.Is(err, ErrInvalidTransition) || errors.Is(err, ErrForbidden) {
				return nil
			}
			if err != nil {
				return err
			}
			// Escrow stays held: the release job (Phase 17a) sets it to released
			// only after it actually posts the ledger entries.
			if err := s.ApplyEffects(ctx, tx, order, effects); err != nil {
				return err
			}
			completed++
			return nil
		})
		if err != nil {
			return completed, err
		}
	}
	return completed, nil
}
