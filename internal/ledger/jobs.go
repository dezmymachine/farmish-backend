package ledger

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/audit"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
)

// accraLocation is Africa/Accra for the daily reconciliation run. Accra stays
// on UTC+0 with no daylight saving, so the fixed zone is exact even where the
// tz database is missing; LoadLocation wins when it exists.
func accraLocation() *time.Location {
	if loc, err := time.LoadLocation("Africa/Accra"); err == nil {
		return loc
	}
	return time.FixedZone("Africa/Accra", 0)
}

// ReconcileArgs is the daily detector that checks the DOMAIN §5.4 invariants.
type ReconcileArgs struct{}

// Kind implements river.JobArgs.
func (ReconcileArgs) Kind() string { return "ledger.reconcile" }

// ReconcileWorker runs the detector. It returns nil either way: a violation
// is an Error log plus an audit event, never a retry, because there is
// nothing a retry would fix.
type ReconcileWorker struct {
	river.WorkerDefaults[ReconcileArgs]
	Pool   *pgxpool.Pool
	Ledger *Ledger
	Log    *slog.Logger
	Now    func() time.Time
}

// Work implements river.Worker.
func (w *ReconcileWorker) Work(ctx context.Context, _ *river.Job[ReconcileArgs]) error {
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	report, err := w.Ledger.Reconcile(ctx, w.Pool, now())
	if err != nil {
		return err
	}
	if report.Clean() {
		return nil
	}
	log := w.Log
	if log == nil {
		log = slog.Default()
	}
	for _, violation := range report.Violations {
		log.Error("ledger reconciliation violation",
			slog.String("check", violation.Check), slog.String("detail", violation.Detail))
	}
	details := make([]any, 0, len(report.Violations))
	for _, violation := range report.Violations {
		details = append(details, map[string]any{"check": violation.Check, "detail": violation.Detail})
	}
	return database.InTx(ctx, w.Pool, func(tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Event{
			Action: "ledger.reconcile", TargetType: "ledger", TargetID: "reconcile",
			Metadata: map[string]any{
				"violations": details, "escrow_balance": report.EscrowBalance,
				"expected_escrow": report.ExpectedEscrow,
			},
		})
	})
}

// RegisterReconcile adds the worker and its daily 03:00 Africa/Accra schedule.
// The first run waits for 03:00 rather than firing at deploy: a detector has
// no backlog to clear.
func RegisterReconcile(r *jobs.Registry, pool *pgxpool.Pool, l *Ledger, log *slog.Logger) {
	jobs.Register(r, &ReconcileWorker{Pool: pool, Ledger: l, Log: log})
	r.Schedule(jobs.DailyAt{Hour: 3, Min: 0, Loc: accraLocation()}, func() river.JobArgs { return ReconcileArgs{} }, false)
}
