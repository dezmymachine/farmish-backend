package listings

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/jobs"
)

// ExpireDueArgs is a periodic job: no parameters, it sweeps every listing
// past its expiry.
type ExpireDueArgs struct{}

// Kind implements river.JobArgs.
func (ExpireDueArgs) Kind() string { return "listings.expire" }

// ExpireDueWorker expires listings whose 30-day window has passed. The
// service is idempotent: only active listings past their expiry match.
type ExpireDueWorker struct {
	river.WorkerDefaults[ExpireDueArgs]
	Service *Service
	Log     *slog.Logger
}

// Work implements river.Worker.
func (w *ExpireDueWorker) Work(ctx context.Context, _ *river.Job[ExpireDueArgs]) error {
	n, err := w.Service.ExpireDue(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		w.Log.Info("expired listings", slog.Int("count", n))
	}
	return nil
}

// RegisterExpireDue adds the worker and its hourly schedule to a registry. It
// runs on start, so a deployment immediately expires what is already due.
func RegisterExpireDue(r *jobs.Registry, svc *Service, log *slog.Logger) {
	jobs.Register(r, &ExpireDueWorker{Service: svc, Log: log})
	r.Every(time.Hour, func() river.JobArgs { return ExpireDueArgs{} }, true)
}
