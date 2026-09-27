package checkout

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/jobs"
)

// RefundNeededArgs marks an order whose buyer must be refunded. Phase 17a
// replaces this log-only worker with the real Paystack refund; until then the
// audit row written beside the enqueue plus this job's log line is the
// operator's trail.
type RefundNeededArgs struct {
	OrderID       uuid.UUID `json:"orderId"`
	AmountPesewas int64     `json:"amountPesewas"`
}

// Kind implements river.JobArgs.
func (RefundNeededArgs) Kind() string { return "orders.refund_needed" }

// RefundNeededWorker records that a refund is owed. It is deliberately a
// no-op: Phase 17a wires the actual refund, and this worker must never be the
// thing that silently pays money twice.
type RefundNeededWorker struct {
	river.WorkerDefaults[RefundNeededArgs]
	Log  *slog.Logger
	Pool *pgxpool.Pool
}

// Work implements river.Worker.
func (w *RefundNeededWorker) Work(_ context.Context, job *river.Job[RefundNeededArgs]) error {
	w.Log.Warn("refund needed for a late paid-after-expiry order (Phase 17a will process it)",
		slog.String("order_id", job.Args.OrderID.String()),
		slog.Int64("pesewas", job.Args.AmountPesewas))
	return nil
}

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
	jobs.Register(r, &RefundNeededWorker{Log: log})
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
