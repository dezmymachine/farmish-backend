package main

import (
	"context"
	"testing"

	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

// TestRun_RoleChangeIsAudited proves the operator CLI leaves an audit row
// for each role change (Phase 21), in the same transaction as the change.
func TestRun_RoleChangeIsAudited(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	fb := authtest.Firebase(t)

	for k, v := range map[string]string{
		"APP_ENV":                     "test",
		"DATABASE_URL":                pool.Config().ConnString(),
		"CORS_ORIGINS":                "https://farmish.gh",
		"FIREBASE_PROJECT_ID":         authtest.ProjectID,
		"FIREBASE_AUTH_EMULATOR_HOST": authtest.Host(t),
		"TURNSTILE_SECRET":            "1x0000000000000000000000000000000AA",
		"PAYSTACK_SECRET_KEY":         "sk_test_0123456789abcdef",
		"PAYSTACK_PUBLIC_KEY":         "pk_test_0123456789abcdef",
		"PAYSTACK_CALLBACK_URL":       "https://farmish.gh/payments/status",
		"DATA_ENCRYPTION_KEY":         config.DevDataEncryptionKey,
	} {
		t.Setenv(k, v)
	}

	eu := authtest.EmailUser(t)
	id, err := fb.Verify(ctx, eu.Token)
	if err != nil {
		t.Fatal(err)
	}
	u, err := users.New(pool).Resolve(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct{ command, role, previous string }{
		{"grant-admin", users.RoleAdmin, users.RoleUser},
		{"revoke-admin", users.RoleUser, users.RoleAdmin},
	} {
		if err := run([]string{step.command, "--uid", u.FirebaseUID}); err != nil {
			t.Fatalf("%s: %v", step.command, err)
		}
		var role string
		if err := pool.QueryRow(ctx, `SELECT role FROM users WHERE id = $1`, u.ID).Scan(&role); err != nil || role != step.role {
			t.Fatalf("%s: role = %q, %v; want %q", step.command, role, err, step.role)
		}
		var rows int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM audit_events
			 WHERE action = 'user.role_change' AND target_type = 'user' AND target_id = $1
			   AND metadata->>'role' = $2 AND metadata->>'previous_role' = $3`,
			u.ID.String(), step.role, step.previous).Scan(&rows); err != nil || rows != 1 {
			t.Errorf("%s: audit rows = %d, %v; want exactly 1 for %s -> %s", step.command, rows, err, step.previous, step.role)
		}
	}
}
