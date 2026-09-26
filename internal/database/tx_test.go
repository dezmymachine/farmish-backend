package database_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
)

func countUsers(ctx context.Context, t *testing.T, q pgx.Tx) int {
	t.Helper()
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestInTx_CommitsAndRollsBack proves the helper commits on nil and rolls
// back on error or panic (re-panicking).
func TestInTx_CommitsAndRollsBack(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO users (firebase_uid, signup_method) VALUES ('intx-1', 'email')`)
		return err
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("after commit: n=%d, err=%v", n, err)
	}

	boom := errors.New("boom")
	err = database.InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO users (firebase_uid, signup_method) VALUES ('intx-2', 'email')`); err != nil {
			t.Fatal(err)
		}
		if got := countUsers(ctx, t, tx); got != 2 {
			t.Fatalf("inside tx: n=%d, want 2", got)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("after error: n=%d, err=%v", n, err)
	}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("panic was swallowed")
			}
		}()
		_ = database.InTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO users (firebase_uid, signup_method) VALUES ('intx-3', 'email')`); err != nil {
				t.Fatal(err)
			}
			panic("kaboom")
		})
	}()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("after panic: n=%d, err=%v", n, err)
	}
}
