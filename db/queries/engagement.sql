-- name: InsertReview :one
-- Records a review. Returns no row when this reviewer already reviewed this
-- listing in this order: the caller treats that as already_reviewed.
INSERT INTO reviews (order_id, listing_id, seller_id, reviewer_id, rating, comment)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (order_id, listing_id, reviewer_id) DO NOTHING
RETURNING *;

-- name: GetReviewByID :one
SELECT * FROM reviews WHERE id = $1;

-- name: ListVisibleReviewsByListing :many
SELECT * FROM reviews
WHERE listing_id = $1 AND hidden_at IS NULL
ORDER BY created_at DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountVisibleReviewsByListing :one
SELECT count(*) FROM reviews WHERE listing_id = $1 AND hidden_at IS NULL;

-- name: SellerRating :one
-- The seller's aggregate over visible reviews: the average to one decimal
-- and the count. Average is 0 when there are no visible reviews.
SELECT coalesce(round(avg(rating)::numeric, 1), 0)::float AS average,
       count(*)::bigint AS count
FROM reviews WHERE seller_id = $1 AND hidden_at IS NULL;

-- name: SetReviewHidden :one
-- Hides a review for moderation. Returns no row when already hidden.
UPDATE reviews SET hidden_at = $2 WHERE id = $1 AND hidden_at IS NULL
RETURNING *;

-- name: InsertFavorite :one
-- Idempotent add: returns no row when already favourited. The caller bumps
-- the counter only on a returned row, in the same transaction.
INSERT INTO favorites (user_id, listing_id)
VALUES ($1, $2)
ON CONFLICT (user_id, listing_id) DO NOTHING
RETURNING *;

-- name: DeleteFavorite :execrows
-- Idempotent remove: the caller drops the counter only when a row went away.
DELETE FROM favorites WHERE user_id = $1 AND listing_id = $2;

-- name: BumpFavoriteCount :exec
UPDATE listings SET favorite_count = favorite_count + $2 WHERE id = $1;

-- name: ListFavoriteListings :many
-- The caller's favourited listings, newest favourited first, including
-- inactive ones: the available flag tells the client which are browsable.
-- The projection mirrors SearchListings so summaries map the same way.
WITH active_promo AS (
  SELECT DISTINCT ON (listing_id) listing_id, tier, tier_rank
  FROM listing_promotions
  WHERE starts_at <= sqlc.arg('now') AND ends_at > sqlc.arg('now')
  ORDER BY listing_id, tier_rank DESC
)
SELECT l.id, l.slug, l.title, l.price_pesewas, l.unit, l.region, l.district, l.item_state,
       l.published_at, c.slug AS category_slug, c.name AS category_name,
       p.tier AS promo_tier,
       COALESCE((SELECT m.key FROM listing_images li JOIN media_objects m ON m.id = li.media_id
         WHERE li.listing_id = l.id ORDER BY li.sort_order LIMIT 1), '') AS cover_key,
       sp.business_name AS seller_name, (sp.verification_status = 'verified') AS seller_verified,
       (l.status = 'active' AND l.expires_at > sqlc.arg('now'))::bool AS available
FROM listings l
JOIN favorites f ON f.listing_id = l.id
JOIN categories c ON c.id = l.category_id
JOIN seller_profiles sp ON sp.user_id = l.seller_id
LEFT JOIN active_promo p ON p.listing_id = l.id
WHERE f.user_id = $1
ORDER BY f.created_at DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountFavoritesByUser :one
SELECT count(*) FROM favorites WHERE user_id = $1;

-- name: GetFavorite :one
SELECT * FROM favorites WHERE user_id = $1 AND listing_id = $2;

-- name: GetListingForFavorite :one
-- Existence gate for favorites: any listing may be favourited, including
-- inactive ones (they show flagged).
SELECT id FROM listings WHERE id = $1;
