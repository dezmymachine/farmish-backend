-- Public search (Phase 12). `now` is always passed from the service clock,
-- never SQL now(), so tests are deterministic.
--
-- Notes:
--   - websearch_to_tsquery never errors on user input (to_tsquery does), so
--     it is safe with raw search text.
--   - The free-text filter coalesces: with no q the condition is TRUE, and
--     Postgres does not guarantee OR short-circuits, so a NULL tsquery must
--     not turn the predicate into NULL.
--   - Promoted listings rank first (highest active tier rank), then the
--     requested sort. An expired promotion is not active, so it ranks as
--     unpromoted (DOMAIN §6).
--
-- name: SearchListings :many
WITH active_promo AS (
  SELECT DISTINCT ON (listing_id) listing_id, tier, tier_rank
  FROM listing_promotions
  WHERE starts_at <= sqlc.arg('now') AND ends_at > sqlc.arg('now')
  ORDER BY listing_id, tier_rank DESC
)
SELECT l.id, l.slug, l.title, l.price_pesewas, l.unit, l.region, l.district, l.item_state,
       l.published_at, c.slug AS category_slug, c.name AS category_name,
       p.tier AS promo_tier, coalesce(p.tier_rank, 0)::int AS promo_rank,
       (SELECT m.key FROM listing_images li JOIN media_objects m ON m.id = li.media_id
         WHERE li.listing_id = l.id ORDER BY li.sort_order LIMIT 1) AS cover_key,
       sp.business_name AS seller_name, (sp.verification_status = 'verified') AS seller_verified
FROM listings l
JOIN categories c ON c.id = l.category_id
JOIN seller_profiles sp ON sp.user_id = l.seller_id
LEFT JOIN active_promo p ON p.listing_id = l.id
WHERE l.status = 'active' AND l.expires_at > sqlc.arg('now')
  AND (coalesce(sqlc.narg('q')::text, '') = ''
       OR l.search_vector @@ websearch_to_tsquery('simple', sqlc.narg('q'))
       OR l.title % sqlc.narg('q'))
  AND (sqlc.narg('category_ids')::uuid[] IS NULL OR l.category_id = ANY(sqlc.narg('category_ids')))
  AND (sqlc.narg('region')::text IS NULL OR l.region = sqlc.narg('region'))
  AND (sqlc.narg('district')::text IS NULL OR l.district ILIKE sqlc.narg('district'))
  AND (sqlc.narg('min_price')::bigint IS NULL OR l.price_pesewas >= sqlc.narg('min_price'))
  AND (sqlc.narg('max_price')::bigint IS NULL OR l.price_pesewas <= sqlc.narg('max_price'))
  AND (sqlc.narg('item_state')::text IS NULL OR l.item_state = sqlc.narg('item_state'))
ORDER BY promo_rank DESC,
  CASE WHEN sqlc.arg('sort') = 'price_asc' THEN l.price_pesewas END ASC,
  CASE WHEN sqlc.arg('sort') = 'price_desc' THEN l.price_pesewas END DESC,
  CASE WHEN sqlc.arg('sort') = 'relevance' AND sqlc.narg('q')::text IS NOT NULL
       THEN ts_rank(l.search_vector, websearch_to_tsquery('simple', sqlc.narg('q'))) END DESC,
  l.published_at DESC, l.id
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountSearchListings :one
WITH active_promo AS (
  SELECT DISTINCT ON (listing_id) listing_id, tier, tier_rank
  FROM listing_promotions
  WHERE starts_at <= sqlc.arg('now') AND ends_at > sqlc.arg('now')
  ORDER BY listing_id, tier_rank DESC
)
SELECT count(*)
FROM listings l
JOIN categories c ON c.id = l.category_id
JOIN seller_profiles sp ON sp.user_id = l.seller_id
LEFT JOIN active_promo p ON p.listing_id = l.id
WHERE l.status = 'active' AND l.expires_at > sqlc.arg('now')
  AND (coalesce(sqlc.narg('q')::text, '') = ''
       OR l.search_vector @@ websearch_to_tsquery('simple', sqlc.narg('q'))
       OR l.title % sqlc.narg('q'))
  AND (sqlc.narg('category_ids')::uuid[] IS NULL OR l.category_id = ANY(sqlc.narg('category_ids')))
  AND (sqlc.narg('region')::text IS NULL OR l.region = sqlc.narg('region'))
  AND (sqlc.narg('district')::text IS NULL OR l.district ILIKE sqlc.narg('district'))
  AND (sqlc.narg('min_price')::bigint IS NULL OR l.price_pesewas >= sqlc.narg('min_price'))
  AND (sqlc.narg('max_price')::bigint IS NULL OR l.price_pesewas <= sqlc.narg('max_price'))
  AND (sqlc.narg('item_state')::text IS NULL OR l.item_state = sqlc.narg('item_state'));

-- name: GetPublicListingBySlug :one
-- One active, unexpired listing with its category and the seller's safe
-- profile fields. Never selects contact or identity data.
SELECT l.*, c.slug AS category_slug, c.name AS category_name,
       c.listing_group AS category_group,
       sp.business_name, sp.region AS seller_region, sp.district AS seller_district,
       sp.bio AS seller_bio, (sp.verification_status = 'verified') AS seller_verified,
       u.created_at AS seller_created_at
FROM listings l
JOIN categories c ON c.id = l.category_id
JOIN seller_profiles sp ON sp.user_id = l.seller_id
JOIN users u ON u.id = l.seller_id
WHERE l.slug = $1;

-- name: IncrementListingViewCount :exec
UPDATE listings SET view_count = view_count + 1 WHERE id = $1;

-- name: IncrementListingContactCount :exec
UPDATE listings SET contact_count = contact_count + 1 WHERE id = $1;

-- name: GetListingContactDetails :one
-- The contact reveal. Carries the listing's seller and state as well as the
-- two opt-in flags, so the handler can tell "your own listing" (403) from
-- "not browsable" (404) and answer in one round trip. phone_e164 and
-- whatsapp_e164 are the seller's own values: they leave the building only
-- when the matching show_* flag is true.
SELECT l.seller_id, l.status, l.expires_at,
       u.phone_e164, sp.whatsapp_e164, sp.show_phone, sp.show_whatsapp
FROM listings l
JOIN users u ON u.id = l.seller_id
JOIN seller_profiles sp ON sp.user_id = l.seller_id
WHERE l.id = $1;
