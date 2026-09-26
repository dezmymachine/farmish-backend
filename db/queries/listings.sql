-- name: InsertListing :one
INSERT INTO listings (
  seller_id, category_id, title, slug, description, price_pesewas, unit,
  quantity_available, min_order_qty, is_negotiable, item_state, region, district, area,
  offers_pickup, offers_seller_delivery, seller_delivery_fee_pesewas
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
)
RETURNING *;

-- name: GetListingByID :one
SELECT * FROM listings WHERE id = $1;

-- name: GetListingByIDForUpdate :one
SELECT * FROM listings WHERE id = $1 FOR UPDATE;

-- name: GetListingBySlug :one
SELECT * FROM listings WHERE slug = $1;

-- name: SlugExists :one
SELECT EXISTS (SELECT 1 FROM listings WHERE slug = $1) AS exists;

-- name: UpdateListing :one
-- Every editable field, COALESCE-style: a nil parameter keeps the column.
UPDATE listings SET
  category_id = COALESCE(sqlc.narg('category_id'), category_id),
  title = COALESCE(sqlc.narg('title'), title),
  description = COALESCE(sqlc.narg('description'), description),
  price_pesewas = COALESCE(sqlc.narg('price_pesewas'), price_pesewas),
  unit = COALESCE(sqlc.narg('unit'), unit),
  quantity_available = COALESCE(sqlc.narg('quantity_available'), quantity_available),
  min_order_qty = COALESCE(sqlc.narg('min_order_qty'), min_order_qty),
  is_negotiable = COALESCE(sqlc.narg('is_negotiable'), is_negotiable),
  item_state = COALESCE(sqlc.narg('item_state'), item_state),
  region = COALESCE(sqlc.narg('region'), region),
  district = COALESCE(sqlc.narg('district'), district),
  area = COALESCE(sqlc.narg('area'), area),
  offers_pickup = COALESCE(sqlc.narg('offers_pickup'), offers_pickup),
  offers_seller_delivery = COALESCE(sqlc.narg('offers_seller_delivery'), offers_seller_delivery),
  seller_delivery_fee_pesewas = COALESCE(sqlc.narg('seller_delivery_fee_pesewas'), seller_delivery_fee_pesewas)
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: SetListingStatus :one
-- Only published_at / expires_at are set when they are given (publish sets
-- both; mark-sold and archive leave them alone).
UPDATE listings SET
  status = sqlc.arg('status'),
  published_at = COALESCE(sqlc.narg('published_at'), published_at),
  expires_at = COALESCE(sqlc.narg('expires_at'), expires_at)
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: ListSellerListings :many
SELECT * FROM listings
WHERE seller_id = $1 AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
ORDER BY created_at DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountSellerListings :one
SELECT count(*) FROM listings
WHERE seller_id = $1 AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'));

-- name: DeleteListingImages :exec
DELETE FROM listing_images WHERE listing_id = $1;

-- name: InsertListingImage :exec
INSERT INTO listing_images (listing_id, media_id, sort_order) VALUES ($1, $2, $3);

-- name: ListListingImages :many
-- Read-only join so the owner's view can carry each object's key (and hence
-- its public URL).
SELECT li.listing_id, li.media_id, li.sort_order, mo.key, mo.content_type, mo.size_bytes
FROM listing_images li
JOIN media_objects mo ON mo.id = li.media_id
WHERE li.listing_id = $1
ORDER BY li.sort_order;

-- name: DeleteListingAttributes :exec
DELETE FROM listing_attribute_values WHERE listing_id = $1;

-- name: InsertListingAttribute :exec
INSERT INTO listing_attribute_values (listing_id, attribute_id, value) VALUES ($1, $2, $3);

-- name: ListListingAttributes :many
SELECT la.listing_id, la.attribute_id, la.value, ca.key, ca.type, ca.required
FROM listing_attribute_values la
JOIN category_attributes ca ON ca.id = la.attribute_id
WHERE la.listing_id = $1
ORDER BY ca.sort_order, ca.key;

-- name: ExpireDueListings :many
-- The expiry sweep: active listings past their expiry become expired.
UPDATE listings SET status = 'expired'
WHERE status = 'active' AND expires_at <= $1
RETURNING id;

-- name: DeleteListing :one
DELETE FROM listings WHERE id = $1 RETURNING id;

-- name: CountListingImages :one
SELECT count(*) FROM listing_images WHERE listing_id = $1;
