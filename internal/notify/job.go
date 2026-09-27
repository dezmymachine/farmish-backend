package notify

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
)

// SMSArgs sends one templated message to one user. The phone number is
// resolved at send time, not enqueue time: a user who adds a phone between
// the transition and the job run still gets the message.
type SMSArgs struct {
	UserID   uuid.UUID         `json:"userId"`
	Template string            `json:"template"`
	Params   map[string]string `json:"params"`
}

// Kind implements river.JobArgs.
func (SMSArgs) Kind() string { return "notify.sms" }

// Worker renders and sends one SMS. A user without a phone is skipped at info
// level: most email-first accounts have no phone, and that is not an error.
type Worker struct {
	river.WorkerDefaults[SMSArgs]
	SMS SMS
	Log *slog.Logger
	// pool resolves the recipient's phone at send time.
	pool *pgxpool.Pool
}

// NewWorker builds the worker over a sender and a pool.
func NewWorker(sender SMS, log *slog.Logger, pool *pgxpool.Pool) *Worker {
	return &Worker{SMS: sender, Log: log, pool: pool}
}

// Work implements river.Worker.
func (w *Worker) Work(ctx context.Context, job *river.Job[SMSArgs]) error {
	if w.SMS == nil || w.pool == nil {
		// Wiring mistake: a nil sender must fail loudly, not silently skip.
		return fmt.Errorf("notify.sms worker is not wired")
	}
	row, err := db.New(w.pool).GetUserByID(ctx, job.Args.UserID)
	if err != nil {
		return fmt.Errorf("load user: %w", err)
	}
	if row.PhoneE164 == nil || *row.PhoneE164 == "" {
		w.Log.Info("notify.sms skipped: user has no phone",
			slog.String("user_id", job.Args.UserID.String()),
			slog.String("template", job.Args.Template))
		return nil
	}
	message, err := Render(job.Args.Template, job.Args.Params)
	if err != nil {
		return fmt.Errorf("render %s: %w", job.Args.Template, err)
	}
	if err := w.SMS.Send(ctx, *row.PhoneE164, message); err != nil {
		return fmt.Errorf("send %s: %w", job.Args.Template, err)
	}
	return nil
}

// Register adds the worker.
func Register(r *jobs.Registry, w *Worker) { jobs.Register(r, w) }
