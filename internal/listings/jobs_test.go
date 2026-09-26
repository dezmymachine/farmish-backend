package listings_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/listings"
)

// fastRetry keeps a failing sweep from retrying for minutes.
type fastRetry struct{}

func (fastRetry) NextRetry(*rivertype.JobRow) time.Time { return time.Now().Add(10 * time.Millisecond) }

// TestExpireListings_Job runs the expiry sweep through River: a listing past
// its window is expired, everything else is untouched.
func TestExpireListings_Job(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	f.svc.Now = func() time.Time { return now }
	overdue := f.published(t, f.seller)
	fresh := f.published(t, f.seller)
	if _, err := f.pool.Exec(ctx, `UPDATE listings SET expires_at = $2 WHERE id = $1`,
		overdue.ID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	// The clock moves past both windows, so the service would expire both; the
	// job runs with the fixture's clock.
	log := slog.New(slog.DiscardHandler)
	reg := jobs.NewRegistry()
	listings.RegisterExpireDue(reg, f.svc, log)
	client, err := jobs.NewClient(f.pool, reg, log, jobs.Options{
		Work: true, RetryPolicy: fastRetry{}, FetchPollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := client.Subscribe(river.EventKindJobCompleted)
	defer cancel()
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := jobs.Stop(client, 5*time.Second, 2*time.Second, log); err != nil {
			t.Errorf("stop: %v", err)
		}
	})

	if _, err := client.Insert(ctx, listings.ExpireDueArgs{}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, events, "listings.expire", 30*time.Second)

	// The overdue one is expired; the fresh one is not.
	if got := statusOf(t, f, overdue.ID); got != listings.StatusExpired {
		t.Errorf("overdue = %q, want expired", got)
	}
	if got := statusOf(t, f, fresh.ID); got != listings.StatusActive {
		t.Errorf("fresh = %q, want active", got)
	}
}

// waitFor waits for a completed job of the given kind.
func waitFor(t *testing.T, events <-chan *river.Event, kind string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case e := <-events:
			if e.Kind == river.EventKindJobCompleted && e.Job.Kind == kind {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a completed %q job", kind)
		}
	}
}

func statusOf(t *testing.T, f *fixture, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status FROM listings WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestCountView_Job runs a view count through River, and checks the dedupe
// that keeps one viewer from inflating a listing's counter.
func TestCountView_Job(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	view := f.published(t, f.seller)
	other := f.published(t, f.seller)

	log := slog.New(slog.DiscardHandler)
	reg := jobs.NewRegistry()
	listings.RegisterCountView(reg, f.svc)
	client, err := jobs.NewClient(f.pool, reg, log, jobs.Options{
		Work: true, FetchPollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := client.Subscribe(river.EventKindJobCompleted)
	defer cancel()
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := jobs.Stop(client, 5*time.Second, 2*time.Second, log); err != nil {
			t.Errorf("stop: %v", err)
		}
	})

	counter := listings.NewViewCounter(client)
	viewer := uuid.New()
	// The same viewer twice inside the window counts once.
	for range 2 {
		if err := counter.CountView(ctx, view.ID, viewer); err != nil {
			t.Fatal(err)
		}
	}
	// A different viewer for the same listing is a separate count.
	if err := counter.CountView(ctx, view.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, events, "listings.count_view", 30*time.Second)
	waitFor(t, events, "listings.count_view", 30*time.Second)

	if got := viewCount(t, f, view.ID); got != 2 {
		t.Errorf("view_count = %d, want 2 (one per viewer in the window)", got)
	}
	if got := viewCount(t, f, other.ID); got != 0 {
		t.Errorf("other listing view_count = %d, want 0", got)
	}
}

func viewCount(t *testing.T, f *fixture, id uuid.UUID) int32 {
	t.Helper()
	var n int32
	if err := f.pool.QueryRow(context.Background(),
		`SELECT view_count FROM listings WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
