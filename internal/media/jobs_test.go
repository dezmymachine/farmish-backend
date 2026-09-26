package media_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/media"
	"github.com/dezmymachine/farmish-backend/internal/media/mediatest"
)

// fastRetry keeps a failing sweep from retrying for minutes.
type fastRetry struct{}

func (fastRetry) NextRetry(*rivertype.JobRow) time.Time { return time.Now().Add(10 * time.Millisecond) }

// TestCleanupOrphans_JobEndToEnd enqueues the sweep through River and waits
// for it to complete, proving the worker is registered and effective.
func TestCleanupOrphans_JobEndToEnd(t *testing.T) {
	store := mediatest.R2(t)
	pool := dbtest.Pool(t)
	ctx := context.Background()
	svc := media.New(pool, store)

	// A stale pending upload: the job must remove it.
	up, err := svc.CreateUpload(ctx, newOwner(t, pool, "media-job"), media.PurposeListingImage, "image/jpeg", 32)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("a", 32)
	req, err := http.NewRequest(http.MethodPut, up.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	for k, v := range up.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	ageRows(t, pool, time.Now().Add(-25*time.Hour), up.ID)

	reg := jobs.NewRegistry()
	media.RegisterCleanupOrphans(reg, svc, slog.New(slog.DiscardHandler))
	client, err := jobs.NewClient(pool, reg, slog.New(slog.DiscardHandler), jobs.Options{
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
		if err := jobs.Stop(client, 5*time.Second, 2*time.Second, slog.New(slog.DiscardHandler)); err != nil {
			t.Errorf("stop: %v", err)
		}
	})

	if _, err := client.Insert(ctx, media.CleanupOrphansArgs{}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, events, "media.cleanup_orphans", 30*time.Second)

	// The row and the object are gone.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_objects WHERE id = $1`, up.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sweep did not delete the orphan row")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := store.Head(ctx, up.Key); !errors.Is(err, media.ErrObjectNotFound) {
		t.Errorf("orphan object still in storage: %v", err)
	}
}

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
