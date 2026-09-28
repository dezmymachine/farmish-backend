package supply

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// TestNumberCollisionRetries forces the retry loop: the generator first
// returns a taken number, then a fresh one, and creation succeeds with the
// fresh number after exactly one collision.
func TestNumberCollisionRetries(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	if err := catalog.Seed(ctx, pool); err != nil {
		t.Fatal(err)
	}
	user, err := users.New(pool).Resolve(ctx, auth.Identity{
		UID: "supply-collision", Email: "supply-collision@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := New(pool, users.New(pool))
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	log := slog.New(slog.DiscardHandler)
	reg := jobs.NewRegistry()
	notify.Register(reg, notify.NewWorker(notify.LogOnly{Log: log}, log, pool))
	client, err := jobs.NewClient(pool, reg, log, jobs.Options{Work: false})
	if err != nil {
		t.Fatal(err)
	}
	svc.AttachJobClient(client)

	taken := "SUP-20260928-AAAAAA"
	if _, err := pool.Exec(ctx,
		`INSERT INTO supply_requests (request_number, user_id) VALUES ($1, $2)`,
		taken, user.ID); err != nil {
		t.Fatal(err)
	}
	calls := 0
	svc.number = func(time.Time) (string, error) {
		calls++
		if calls == 1 {
			return taken, nil
		}
		return "SUP-20260928-BBBBBB", nil
	}
	created, err := svc.Create(ctx, user.ID, Input{
		Items: []ItemInput{
			{CategorySlug: "fresh-produce", ProductName: "Tomatoes", Quantity: 1, Unit: "kg"},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.RequestNumber != "SUP-20260928-BBBBBB" {
		t.Errorf("number = %q, want the post-collision one", created.RequestNumber)
	}
	if calls != 2 {
		t.Errorf("generator calls = %d, want 2", calls)
	}
}
