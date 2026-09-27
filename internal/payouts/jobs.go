package payouts

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/jobs"
)

// ExecuteAllArgs is the daily sweep that queues one payout per payable
// seller.
type ExecuteAllArgs struct{}

// Kind implements river.JobArgs.
func (ExecuteAllArgs) Kind() string { return "payouts.execute_all" }

// ExecuteAllWorker queues the day's payouts.
type ExecuteAllWorker struct {
	river.WorkerDefaults[ExecuteAllArgs]
	Service *Service
	Log     *slog.Logger
}

// Work implements river.Worker.
func (w *ExecuteAllWorker) Work(ctx context.Context, _ *river.Job[ExecuteAllArgs]) error {
	n, err := w.Service.ExecuteAll(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		w.Log.Info("queued seller payouts", slog.Int("count", n))
	}
	return nil
}

// SendArgs sends one queued payout through Paystack Transfers.
type SendArgs struct {
	PayoutID uuid.UUID `json:"payoutId"`
}

// Kind implements river.JobArgs.
func (SendArgs) Kind() string { return "payouts.send" }

// SendWorker runs one step of a payout's send. While the transfer's outcome
// is ambiguous it snoozes and reconciles with Paystack (snoozes do not use
// up attempts); only database errors return an error for River's retry.
type SendWorker struct {
	river.WorkerDefaults[SendArgs]
	Service *Service
	Log     *slog.Logger
}

// Work implements river.Worker.
func (w *SendWorker) Work(ctx context.Context, job *river.Job[SendArgs]) error {
	next, err := w.Service.SendPayout(ctx, job.Args.PayoutID)
	if err != nil {
		return err
	}
	if next > 0 {
		return river.JobSnooze(next)
	}
	return nil
}

// ReconcileArgs settles transfers whose webhooks never arrived.
type ReconcileArgs struct{}

// Kind implements river.JobArgs.
func (ReconcileArgs) Kind() string { return "payouts.reconcile" }

// ReconcileWorker settles stuck pending payouts through VerifyTransfer.
type ReconcileWorker struct {
	river.WorkerDefaults[ReconcileArgs]
	Service *Service
	Log     *slog.Logger
}

// Work implements river.Worker.
func (w *ReconcileWorker) Work(ctx context.Context, _ *river.Job[ReconcileArgs]) error {
	n, err := w.Service.ReconcileStuck(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		w.Log.Info("reconciled stuck payouts", slog.Int("count", n))
	}
	return nil
}

// RegisterJobs adds the workers and the two daily schedules (10:00 execute,
// 11:00 reconcile, Africa/Accra). Neither runs at deploy: payouts start on
// the next scheduled wall-clock time.
func RegisterJobs(r *jobs.Registry, svc *Service, log *slog.Logger) {
	jobs.Register(r, &ExecuteAllWorker{Service: svc, Log: log})
	jobs.Register(r, &SendWorker{Service: svc, Log: log})
	jobs.Register(r, &ReconcileWorker{Service: svc, Log: log})
	r.Schedule(jobs.DailyAt{Hour: 10, Min: 0, Loc: jobs.AccraLocation()}, func() river.JobArgs { return ExecuteAllArgs{} }, false)
	r.Schedule(jobs.DailyAt{Hour: 11, Min: 0, Loc: jobs.AccraLocation()}, func() river.JobArgs { return ReconcileArgs{} }, false)
}
