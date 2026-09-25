// Package dbtest gives each test its own throwaway Postgres database.
//
// Set TEST_DATABASE_URL to a server the tests may create databases on (make
// test does this against docker-compose). Without it, DB tests are skipped,
// unless DBTEST_REQUIRED=1, in which case they fail instead.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/migrations"
)

// EmptyURL creates a fresh, unmigrated database and returns its URL. The
// database is dropped when the test ends.
func EmptyURL(t testing.TB) string {
	t.Helper()
	admin := adminURL(t)

	var b [8]byte
	_, _ = rand.Read(b[:])
	name := "farmish_test_" + hex.EncodeToString(b[:])

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("dbtest: connect to TEST_DATABASE_URL: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("dbtest: create database: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, admin)
		if err != nil {
			t.Errorf("dbtest: cleanup connect: %v", err)
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("dbtest: drop database %s: %v", name, err)
		}
	})

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("dbtest: parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// Pool creates a fresh database with all migrations applied and returns a
// pool connected to it. Both are cleaned up when the test ends.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dbURL := EmptyURL(t)
	if err := migrations.Up(dbURL); err != nil {
		t.Fatalf("dbtest: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("dbtest: open pool: %v", err)
	}
	// Registered after EmptyURL's cleanup, so it runs first (LIFO).
	t.Cleanup(pool.Close)
	return pool
}

func adminURL(t testing.TB) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u != "" {
		return u
	}
	if os.Getenv("DBTEST_REQUIRED") == "1" {
		t.Fatal("dbtest: TEST_DATABASE_URL is not set (DBTEST_REQUIRED=1)")
	}
	t.Skip("dbtest: TEST_DATABASE_URL not set; skipping DB test (run `make test`)")
	return ""
}
