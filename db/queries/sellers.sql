-- name: GetSellerProfile :one
SELECT * FROM seller_profiles WHERE user_id = $1;

-- name: GetSellerProfileForUpdate :one
SELECT * FROM seller_profiles WHERE user_id = $1 FOR UPDATE;

-- name: UpsertSellerProfile :one
-- Creates the profile or edits its non-identity fields. Identity
-- (id_type/id_number) and verification status are managed separately, so an
-- edit never changes them.
INSERT INTO seller_profiles (user_id, business_name, region, district, bio, show_phone, show_whatsapp, whatsapp_e164)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (user_id) DO UPDATE SET
  business_name = EXCLUDED.business_name,
  region = EXCLUDED.region,
  district = EXCLUDED.district,
  bio = EXCLUDED.bio,
  show_phone = EXCLUDED.show_phone,
  show_whatsapp = EXCLUDED.show_whatsapp,
  whatsapp_e164 = EXCLUDED.whatsapp_e164
RETURNING *;

-- name: SetSellerIdentity :one
-- Stores a (new) encrypted ID and (re)submits the profile for verification.
UPDATE seller_profiles
SET id_type = $2, id_number_enc = $3, id_number_last4 = $4,
    verification_status = 'pending', submitted_at = now()
WHERE user_id = $1
RETURNING *;

-- name: SetSellerVerification :one
-- Records an admin verification decision (reviewed_at = now()).
UPDATE seller_profiles
SET verification_status = $2, reviewed_at = now(), reviewed_by = $3, rejection_reason = $4
WHERE user_id = $1
RETURNING *;

-- name: ListSellerProfilesByStatus :many
-- Admin review queue: oldest submission first.
SELECT sp.*, u.display_name, u.email, u.phone_e164
FROM seller_profiles sp JOIN users u ON u.id = sp.user_id
WHERE sp.verification_status = $1
ORDER BY sp.submitted_at ASC NULLS LAST, sp.created_at ASC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountSellerProfilesByStatus :one
SELECT count(*) FROM seller_profiles WHERE verification_status = $1;

-- name: GetPublicSeller :one
-- Narrow projection for the public endpoint: never selects id_number_enc.
SELECT sp.business_name, sp.region, sp.district, sp.bio, sp.verification_status, u.created_at
FROM seller_profiles sp JOIN users u ON u.id = sp.user_id
WHERE sp.user_id = $1;
