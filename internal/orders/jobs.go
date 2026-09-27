package orders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
)

// SweepBatch is how many orders one timer pass may move (the spec's batch of
// 100, taken with FOR UPDATE SKIP LOCKED).
const SweepBatch = 100

// RefundNeededWorker records that a refund is owed. Phase 17a replaces this
// body with the real Paystack refund; until then the audit trail plus this
// log line is the operator's queue.
type RefundNeededWorker struct {
	river.WorkerDefaults[RefundNeededArgs]
	Log *slog.Logger
}

// Work implements river.Worker.
func (w *RefundNeededWorker) Work(_ context.Context, job *river.Job[RefundNeededArgs]) error {
	w.Log.Warn("refund needed (Phase 17a will process it)",
		slog.String("order_id", job.Args.OrderID.String()),
		slog.Int64("pesewas", job.Args.AmountPesewas))
	return nil
}

// ReleaseNeededWorker records that escrow should be released. Phase 17a
// replaces this body with the real release posting.
type ReleaseNeededWorker struct {
	river.WorkerDefaults[ReleaseNeededArgs]
	Log *slog.Logger
}

// Work implements river.Worker.
func (w *ReleaseNeededWorker) Work(_ context.Context, job *river.Job[ReleaseNeededArgs]) error {
	w.Log.Warn("escrow release needed (Phase 17a will process it)",
		slog.String("order_id", job.Args.OrderID.String()),
		slog.Int64("pesewas", job.Args.AmountPesewas))
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
	jobs.Register(r, &RefundNeededWorker{Log: log})
	jobs.Register(r, &ReleaseNeededWorker{Log: log})
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
			if err := s.SetEscrowState(ctx, tx, order.ID, EscrowReleased); err != nil {
				return err
			}
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
