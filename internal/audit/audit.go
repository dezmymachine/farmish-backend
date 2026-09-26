// Package audit records an append-only trail of sensitive actions (seller
// verification, disputes, payouts) in audit_events.
package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/dezmymachine/farmish-backend/internal/db"
)

// Event is one audit trail entry. ActorID is nil for system actions.
type Event struct {
	ActorID    *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	Metadata   map[string]any
}

// Record inserts e in the caller's transaction, so the event commits or
// rolls back with the state change it describes.
func Record(ctx context.Context, tx pgx.Tx, e Event) error {
	meta := []byte("{}")
	if e.Metadata != nil {
		b, err := json.Marshal(e.Metadata)
		if err != nil {
			return fmt.Errorf("marshal audit metadata: %w", err)
		}
		meta = b
	}
	var actor pgtype.UUID
	if e.ActorID != nil {
		actor = pgtype.UUID{Bytes: *e.ActorID, Valid: true}
	}
	if _, err := db.New(tx).InsertAuditEvent(ctx, db.InsertAuditEventParams{
		ActorID: actor, Action: e.Action, TargetType: e.TargetType, TargetID: e.TargetID, Metadata: meta,
	}); err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}
