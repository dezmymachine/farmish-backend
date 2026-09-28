-- name: InsertSupplyRequest :one
-- Records a supply request with its generated number. A number collision
-- returns no row (the UNIQUE is the retry signal); anything else errors.
INSERT INTO supply_requests (request_number, user_id, delivery_name, delivery_phone,
  delivery_address, expected_date, notes)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (request_number) DO NOTHING
RETURNING *;

-- name: InsertSupplyRequestItem :exec
INSERT INTO supply_request_items (supply_request_id, category_id, product_name, quantity, unit, sort_order)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: InsertSupplyRequestEvent :exec
-- The append-only transition trail: every move writes exactly one row.
INSERT INTO supply_request_events (supply_request_id, from_status, to_status, actor_id, note)
VALUES ($1, $2, $3, $4, $5);

-- name: GetSupplyRequestByID :one
SELECT * FROM supply_requests WHERE id = $1;

-- name: GetSupplyRequestForUpdate :one
SELECT * FROM supply_requests WHERE id = $1 FOR UPDATE;

-- name: SetSupplyRequestStatus :one
-- Moves a request guarded by its current status: no row means the move is
-- illegal from where the request actually is.
UPDATE supply_requests SET status = $2 WHERE id = $1 AND status = $3
RETURNING *;

-- name: ListSupplyRequestItems :many
SELECT * FROM supply_request_items WHERE supply_request_id = $1 ORDER BY sort_order;

-- name: ListSupplyRequestEvents :many
SELECT * FROM supply_request_events WHERE supply_request_id = $1 ORDER BY id;

-- name: ListSupplyRequestsByUser :many
SELECT * FROM supply_requests
WHERE user_id = $1 AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
ORDER BY created_at DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountSupplyRequestsByUser :one
SELECT count(*) FROM supply_requests
WHERE user_id = $1 AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'));

-- name: ListAllSupplyRequests :many
SELECT * FROM supply_requests
WHERE (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
ORDER BY created_at DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountAllSupplyRequests :one
SELECT count(*) FROM supply_requests
WHERE (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'));

-- name: GetSupplyCategoryBySlug :one
-- Items must name a parent category: children are rejected by the caller.
SELECT * FROM categories WHERE slug = $1;
