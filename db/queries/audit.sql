-- name: InsertAuditEvent :one
INSERT INTO audit_events (actor_id, action, target_type, target_id, metadata)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;
