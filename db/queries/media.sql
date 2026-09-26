-- name: InsertMediaObject :one
INSERT INTO media_objects (owner_id, key, purpose, content_type, size_bytes)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetMediaObject :one
SELECT * FROM media_objects WHERE id = $1;

-- name: AttachMediaObject :one
-- Marks the object attached, but only while it is still pending.
UPDATE media_objects SET status = 'attached', attached_at = now()
WHERE id = $1 AND status = 'pending'
RETURNING *;

-- name: ListPendingMediaBefore :many
-- Cleanup sweep: pending rows older than the cutoff, oldest first.
SELECT * FROM media_objects
WHERE status = 'pending' AND created_at < $1
ORDER BY created_at
LIMIT sqlc.arg('limit');

-- name: DeleteMediaObject :one
DELETE FROM media_objects WHERE id = $1 RETURNING id;
