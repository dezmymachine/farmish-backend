-- name: ListCommissionConfigs :many
-- Every category override plus the default row, for server-side commission
-- resolution. DOMAIN §2.1 snapshots the resolved rate on each order.
SELECT id, category_id, rate_bps FROM commission_configs;

-- name: ListQuoteListings :many
-- The server-side snapshot a quote may use. Prices come only from this query:
-- cart lines carry quantities, never prices. Active is evaluated against the
-- caller's clock, which tests control; the pricing function itself stays pure.
SELECT l.id, l.seller_id, sp.business_name AS seller_name, l.title, l.unit,
       l.price_pesewas AS unit_price, l.quantity_available, l.min_order_qty,
       l.category_id, c.parent_id AS category_parent_id,
       (l.status = 'active' AND l.expires_at > sqlc.arg('now')::timestamptz)::bool AS active,
       l.offers_pickup, l.offers_seller_delivery, l.seller_delivery_fee_pesewas
FROM listings l
JOIN categories c ON c.id = l.category_id
JOIN seller_profiles sp ON sp.user_id = l.seller_id
WHERE l.id = ANY(sqlc.arg('listing_ids')::uuid[]);
