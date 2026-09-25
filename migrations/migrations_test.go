package migrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"

	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/migrations"
)

// Every migration must apply, fully roll back, and re-apply cleanly.
func TestUpDownUp(t *testing.T) {
	dbURL := dbtest.EmptyURL(t)
	m, err := migrations.New(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}
	upVersion := assertClean(t, m)

	if err := m.Down(); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, _, err := m.Version(); !errors.Is(err, migrate.ErrNilVersion) {
		t.Fatalf("after down: version err = %v, want ErrNilVersion", err)
	}
	assertNoLeftovers(t, dbURL)

	if err := m.Up(); err != nil {
		t.Fatalf("second up: %v", err)
	}
	if v := assertClean(t, m); v != upVersion {
		t.Errorf("second up version %d != first %d", v, upVersion)
	}

	if err := migrations.Up(dbURL); err != nil {
		t.Errorf("Up with nothing pending: %v", err)
	}
}

func assertClean(t *testing.T, m *migrate.Migrate) uint {
	t.Helper()
	v, dirty, err := m.Version()
	if err != nil || dirty {
		t.Fatalf("version %d dirty=%t err=%v", v, dirty, err)
	}
	return v
}

// After a full down, only golang-migrate's own table may remain.
func assertNoLeftovers(t *testing.T, dbURL string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var n int
	err = conn.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM pg_extension WHERE extname <> 'plpgsql')
		     + (SELECT count(*) FROM pg_proc p JOIN pg_namespace ns ON ns.oid = p.pronamespace WHERE ns.nspname = 'public')
		     + (SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename <> 'schema_migrations')`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d objects left after down", n)
	}
}
