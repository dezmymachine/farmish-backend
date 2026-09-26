-- name: ListActivePromotionConfigs :many
-- The public package list, in display order.
SELECT * FROM promotion_configs WHERE is_active ORDER BY sort_order, tier;

-- name: GetPromotionConfig :one
SELECT * FROM promotion_configs WHERE tier = $1;

-- name: UpsertPromotionConfig :one
-- Seed upsert: updates only when something changed (the WHERE guard keeps a
-- repeat run from touching updated_at). Returns no row when unchanged.
INSERT INTO promotion_configs (tier, name, price_pesewas, credits, duration_days,
                               tier_rank, featured, description, features, sort_order, is_active)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (tier) DO UPDATE SET
  name = EXCLUDED.name, price_pesewas = EXCLUDED.price_pesewas, credits = EXCLUDED.credits,
  duration_days = EXCLUDED.duration_days, tier_rank = EXCLUDED.tier_rank, featured = EXCLUDED.featured,
  description = EXCLUDED.description, features = EXCLUDED.features, sort_order = EXCLUDED.sort_order,
  is_active = EXCLUDED.is_active, updated_at = now()
WHERE promotion_configs.name IS DISTINCT FROM EXCLUDED.name
   OR promotion_configs.price_pesewas IS DISTINCT FROM EXCLUDED.price_pesewas
   OR promotion_configs.credits IS DISTINCT FROM EXCLUDED.credits
   OR promotion_configs.duration_days IS DISTINCT FROM EXCLUDED.duration_days
   OR promotion_configs.tier_rank IS DISTINCT FROM EXCLUDED.tier_rank
   OR promotion_configs.featured IS DISTINCT FROM EXCLUDED.featured
   OR promotion_configs.description IS DISTINCT FROM EXCLUDED.description
   OR promotion_configs.features IS DISTINCT FROM EXCLUDED.features
   OR promotion_configs.sort_order IS DISTINCT FROM EXCLUDED.sort_order
   OR promotion_configs.is_active IS DISTINCT FROM EXCLUDED.is_active
RETURNING *;

-- name: InsertListingPromotion :one
-- A promotion application always inserts a row. A higher tier replaces an
-- active promotion by ending that row first; the replacement row is the only
-- one active from the replacement time onward.
INSERT INTO listing_promotions (listing_id, seller_id, tier, tier_rank, starts_at, ends_at, credits_spent)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: LockPromotionCredits :one
-- Serializes one buyer's credit balance changes for the transaction. The
-- ledger keeps no mutable balance row to lock, so the advisory lock is the
-- concurrency control DOMAIN §5.3.5 requires.
SELECT pg_advisory_xact_lock(hashtext('promo:' || $1::text)) AS locked;

-- name: GetActiveListingPromotionForUpdate :one
-- The currently effective promotion, locked so two applications cannot both
-- conclude that the same row is active. When rankings overlap after a
-- replacement bug, the highest rank wins and the latest expiry breaks the tie.
SELECT * FROM listing_promotions
WHERE listing_id = $1 AND starts_at <= $2 AND ends_at > $2
ORDER BY tier_rank DESC, ends_at DESC
LIMIT 1
FOR UPDATE;

-- name: EndActiveListingPromotions :exec
-- Forfeit the remaining time on every currently active promotion when a higher
-- tier replaces it. The table requires ends_at > starts_at, even for a row
-- being replaced at the instant it started, so an immediate replacement ends
-- one microsecond after its start rather than exactly at the replacement time.
UPDATE listing_promotions
SET ends_at = GREATEST($2, starts_at + INTERVAL '1 microsecond')
WHERE listing_id = $1 AND starts_at <= $2 AND ends_at > $2;

-- name: ListListingPromotions :many
-- One listing's promotion history, newest effective window first.
SELECT * FROM listing_promotions
WHERE listing_id = $1
ORDER BY starts_at DESC, created_at DESC;
