package media_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// newOwner creates a users row for a media owner (no Firebase needed: media
// never touches custom claims).
func newOwner(t *testing.T, pool *pgxpool.Pool, uid string) uuid.UUID {
	t.Helper()
	u, err := users.New(pool).Resolve(context.Background(), auth.Identity{
		UID: uid, Email: uid + "@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// ageRows backdates created_at so the orphan sweep sees them as old.
func ageRows(t *testing.T, pool *pgxpool.Pool, at time.Time, ids ...uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE media_objects SET created_at = $1 WHERE id = ANY($2)`, at, ids); err != nil {
		t.Fatal(err)
	}
}
