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
