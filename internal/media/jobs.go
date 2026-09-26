package media

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/jobs"
)

// CleanupOrphansArgs is a periodic job: no parameters, it sweeps every stale
// pending upload.
type CleanupOrphansArgs struct{}

// Kind implements river.JobArgs.
func (CleanupOrphansArgs) Kind() string { return "media.cleanup_orphans" }

// CleanupOrphansWorker deletes orphaned uploads. The service is idempotent:
// a row already gone (or an object already deleted) is success.
type CleanupOrphansWorker struct {
	river.WorkerDefaults[CleanupOrphansArgs]
	Service *Service
	Log     *slog.Logger
}

// Work implements river.Worker.
func (w *CleanupOrphansWorker) Work(ctx context.Context, _ *river.Job[CleanupOrphansArgs]) error {
	removed, err := w.Service.CleanupOrphans(ctx)
	if err != nil {
		return err
	}
	if removed > 0 {
		w.Log.Info("media orphan sweep", slog.Int("removed", removed))
	}
	return nil
}

// RegisterCleanupOrphans adds the worker and its hourly schedule to a
// registry. Not run on start: a fresh deployment has nothing to sweep.
func RegisterCleanupOrphans(r *jobs.Registry, svc *Service, log *slog.Logger) {
	jobs.Register(r, &CleanupOrphansWorker{Service: svc, Log: log})
	r.Every(time.Hour, func() river.JobArgs { return CleanupOrphansArgs{} }, false)
}
