package db_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/db"
)

func TestListExtensions(t *testing.T) {
	q := db.New(dbtest.Pool(t))
	exts, err := q.ListExtensions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"citext", "pgcrypto"} {
		if !slices.Contains(exts, want) {
			t.Errorf("extension %q not installed (got %v)", want, exts)
		}
	}
}

// Migration 000001's set_updated_at() trigger function bumps updated_at.
func TestSetUpdatedAtTrigger(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)

	_, err := pool.Exec(ctx, `
		CREATE TABLE widgets (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			email citext NOT NULL,
			updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE TRIGGER set_updated_at BEFORE UPDATE ON widgets
			FOR EACH ROW EXECUTE FUNCTION set_updated_at();
		INSERT INTO widgets (email, updated_at) VALUES ('A@Farmish.gh', now() - interval '1 hour');`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE widgets SET email = email WHERE email = 'a@farmish.gh'`); err != nil {
		t.Fatal(err)
	}
	var updated time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM widgets`).Scan(&updated); err != nil {
		t.Fatal(err)
	}
	if time.Since(updated) > time.Minute {
		t.Errorf("updated_at not bumped: %v (citext match or trigger failed)", updated)
	}
}
