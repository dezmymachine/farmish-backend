package audit_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dezmymachine/farmish-backend/internal/audit"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/db"
)

// TestAudit_AppendOnly proves audit_events rejects UPDATE and DELETE.
func TestAudit_AppendOnly(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	var actor uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (firebase_uid, signup_method) VALUES ('audit-actor', 'email') RETURNING id`).Scan(&actor); err != nil {
		t.Fatal(err)
	}

	err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Event{
			ActorID: &actor, Action: "seller.verify",
			TargetType: "seller", TargetID: uuid.NewString(),
			Metadata: map[string]any{"previous_status": "pending"},
		})
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d, err %v", n, err)
	}
	row, err := db.New(pool).InsertAuditEvent(ctx, db.InsertAuditEventParams{
		Action: "probe", TargetType: "t", TargetID: "1", Metadata: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("system (NULL actor) insert: %v", err)
	}
	if row.ActorID.Valid {
		t.Error("NULL actor stored as valid")
	}

	for name, stmt := range map[string]string{
		"update": "UPDATE audit_events SET action = 'rewritten'",
		"delete": "DELETE FROM audit_events",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := pool.Exec(ctx, stmt)
			if err == nil || !strings.Contains(err.Error(), "append-only") {
				t.Errorf("err = %v, want the append-only trigger", err)
			}
		})
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows after blocked writes = %d, err %v", n, err)
	}
}
