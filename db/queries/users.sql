-- name: GetUserByFirebaseUID :one
SELECT * FROM users WHERE firebase_uid = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: ListUsersByEmail :many
-- Several accounts may share an email (no account linking in v1).
SELECT * FROM users WHERE email = $1 ORDER BY created_at;

-- name: InsertUser :one
-- Returns no row if a concurrent request created the user first.
INSERT INTO users (firebase_uid, signup_method, email, email_verified, phone_e164, display_name)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (firebase_uid) DO NOTHING
RETURNING *;

-- name: SyncUserIdentity :one
-- Mirror Firebase-owned identity fields; only writes when something changed.
UPDATE users
SET email = $2, email_verified = $3, phone_e164 = $4
WHERE id = $1
  AND (email IS DISTINCT FROM $2 OR email_verified IS DISTINCT FROM $3 OR phone_e164 IS DISTINCT FROM $4)
RETURNING *;

-- name: UpdateUserDisplayName :one
UPDATE users SET display_name = $2 WHERE id = $1 RETURNING *;

-- name: SetUserRole :one
UPDATE users SET role = $2 WHERE id = $1 RETURNING *;

-- name: SetUserSellerVerified :one
UPDATE users SET seller_verified = $2 WHERE id = $1 RETURNING *;
