package jobs

import (
	"context"
	"log/slog"

	"github.com/riverqueue/river"
)

// NoopArgs is a demo job proving the queue end to end: enqueue it inside a
// transaction and it runs once after commit, never after rollback.
type NoopArgs struct {
	Note string `json:"note"`
}

// Kind implements river.JobArgs.
func (NoopArgs) Kind() string { return "noop" }

// NoopWorker logs and succeeds.
type NoopWorker struct {
	river.WorkerDefaults[NoopArgs]
	Log *slog.Logger
}

// Work implements river.Worker.
func (w *NoopWorker) Work(_ context.Context, job *river.Job[NoopArgs]) error {
	w.Log.Info("noop job ran", slog.Int64("job_id", job.ID), slog.String("note", job.Args.Note))
	return nil
}
