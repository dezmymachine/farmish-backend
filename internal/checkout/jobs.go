package checkout

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/jobs"
)

// ExpireUnpaidArgs is the periodic sweep's only argument-free payload.
type ExpireUnpaidArgs struct{}

// Kind implements river.JobArgs.
func (ExpireUnpaidArgs) Kind() string { return "checkout.expire_unpaid" }

// ExpireUnpaidWorker expires checkouts whose payment never arrived.
type ExpireUnpaidWorker struct {
	river.WorkerDefaults[ExpireUnpaidArgs]
	Service *Service
	Log     *slog.Logger
}

// Work implements river.Worker.
func (w *ExpireUnpaidWorker) Work(ctx context.Context, _ *river.Job[ExpireUnpaidArgs]) error {
	n, err := w.Service.ExpireUnpaid(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		w.Log.Info("expired unpaid checkouts", slog.Int("count", n))
	}
	return nil
}

// RegisterJobs adds the checkout workers. The sweep runs every five minutes and
// on start, so a deployment immediately reaps anything already overdue.
func RegisterJobs(r *jobs.Registry, svc *Service, log *slog.Logger) {
	jobs.Register(r, &ExpireUnpaidWorker{Service: svc, Log: log})
	r.Every(5*time.Minute, func() river.JobArgs { return ExpireUnpaidArgs{} }, true)
}

// jobsUnique is River's by-args uniqueness: at most one refund-needed job per
// order, because the args carry the order id.
func jobsUnique() *river.InsertOpts { return jobs.Unique() }

func sortedUUIDs(ids []uuid.UUID) []uuid.UUID {
	sorted := append([]uuid.UUID(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].String() < sorted[j].String() })
	return sorted
}
