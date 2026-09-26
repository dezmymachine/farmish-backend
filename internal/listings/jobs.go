package listings

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
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

// CountViewArgs bumps one listing's view counter. The viewer is a one-way
// hash, never an IP address: the raw address is hashed in the request path and
// only the digest is stored, here and in River's job table.
type CountViewArgs struct {
	ListingID  uuid.UUID `json:"listingId"`
	ViewerHash string    `json:"viewerHash"`
}

// Kind implements river.JobArgs.
func (CountViewArgs) Kind() string { return "listings.count_view" }

// CountViewWorker counts a listing view. Duplicate suppression is River's
// UniqueWithin(hour) on the args, so one viewer per listing per hour is
// enough; the counter itself stays a plain increment.
type CountViewWorker struct {
	river.WorkerDefaults[CountViewArgs]
	Service *Service
}

// Work implements river.Worker.
func (w *CountViewWorker) Work(ctx context.Context, job *river.Job[CountViewArgs]) error {
	return w.Service.CountView(ctx, job.Args.ListingID)
}

// RegisterCountView adds the view-counting worker. The request path only
// enqueues; see listings.ViewCounter.
func RegisterCountView(r *jobs.Registry, svc *Service) {
	jobs.Register(r, &CountViewWorker{Service: svc})
}

// ViewCounter enqueues the view count for a listing. handlers.Server takes it
// as a seam so a read never waits on the job queue.
type ViewCounter interface {
	CountView(ctx context.Context, listingID, viewerHash uuid.UUID) error
}

// NewViewCounter returns the River-backed ViewCounter, deduplicating to one
// count per listing, viewer and hour. The client is held by pointer: it owns a
// mutex, so it must never be copied.
func NewViewCounter(client *jobs.Client) ViewCounter { return &riverViewCounter{client: client} }

type riverViewCounter struct{ client *jobs.Client }

func (c riverViewCounter) CountView(ctx context.Context, listingID, viewerHash uuid.UUID) error {
	if _, err := c.client.Insert(ctx, CountViewArgs{ListingID: listingID, ViewerHash: viewerHash.String()},
		jobs.UniqueWithin(time.Hour)); err != nil {
		return fmt.Errorf("enqueue view count: %w", err)
	}
	return nil
}
