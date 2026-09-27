-- name: InsertCheckout :one
-- A checkout holds reserved stock and one payment while the buyer pays. The
-- unique (buyer_id, idempotency_key) is what makes POST /v1/checkout replayable.
INSERT INTO checkouts (buyer_id, idempotency_key, request_hash, base_pesewas,
                      processing_fee_pesewas, charge_pesewas, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetCheckoutByIdempotencyKey :one
SELECT * FROM checkouts WHERE buyer_id = $1 AND idempotency_key = $2;

-- name: SetCheckoutPayment :exec
UPDATE checkouts SET payment_id = $2 WHERE id = $1;

-- name: GetCheckoutByID :one
SELECT * FROM checkouts WHERE id = $1;

-- name: GetCheckoutForUpdate :one
SELECT * FROM checkouts WHERE id = $1 FOR UPDATE;

-- name: SetCheckoutStatus :exec
UPDATE checkouts SET status = $2 WHERE id = $1 AND status = $3;

-- name: ListExpiredPendingCheckouts :many
-- Unpaid checkouts past their window, in a deterministic order so the sweep
-- behaves the same on every run. SKIP LOCKED keeps concurrent sweeps from
-- fighting over the same checkout.
SELECT * FROM checkouts
WHERE status = 'pending_payment' AND expires_at <= $1
ORDER BY expires_at
LIMIT $2
FOR UPDATE SKIP LOCKED;

-- name: InsertOrder :one
-- The commission rate and amount are the resolved, snapshotted values from
-- pricing: later config edits never touch an existing order.
INSERT INTO orders (checkout_id, buyer_id, seller_id, status, subtotal_pesewas,
                    delivery_fee_pesewas, base_pesewas, commission_rate_bps,
                    commission_pesewas, delivery_method, delivery_address,
                    delivery_region, delivery_district, recipient_name, recipient_phone)
VALUES ($1, $2, $3, 'pending_payment', $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
RETURNING *;

-- name: GetOrderByID :one
SELECT * FROM orders WHERE id = $1;

-- name: GetOrderForUpdate :one
SELECT * FROM orders WHERE id = $1 FOR UPDATE;

-- name: SetOrderStatus :one
-- Transition's status write. Only the target status's timestamp column moves;
-- escrow_state is the caller's to set, because it depends on why the order
-- moved. Returns no row when the current status differs, which Transition
-- treats as a lost race rather than a silent no-op.
UPDATE orders SET status = $2 WHERE id = $1 AND status = $3
RETURNING *;

-- name: SetOrderEscrowState :exec
UPDATE orders SET escrow_state = $2 WHERE id = $1;

-- name: SetOrderPaid :exec
UPDATE orders SET escrow_state = 'held', paid_at = $2 WHERE id = $1 AND status = 'paid';

-- name: SetOrderCancelledAt :exec
UPDATE orders SET cancelled_at = $2 WHERE id = $1 AND status = 'cancelled';

-- name: ListOrdersByBuyer :many
SELECT o.*, sp.business_name AS seller_name
FROM orders o
JOIN seller_profiles sp ON sp.user_id = o.seller_id
WHERE o.buyer_id = $1 AND (sqlc.narg('status')::text IS NULL OR o.status = sqlc.narg('status'))
ORDER BY o.created_at DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountOrdersByBuyer :one
SELECT count(*) FROM orders
WHERE buyer_id = $1 AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'));

-- name: ListOrdersBySeller :many
SELECT o.*, sp.business_name AS seller_name
FROM orders o
JOIN seller_profiles sp ON sp.user_id = o.seller_id
WHERE o.seller_id = $1 AND (sqlc.narg('status')::text IS NULL OR o.status = sqlc.narg('status'))
ORDER BY o.created_at DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountOrdersBySeller :one
SELECT count(*) FROM orders
WHERE seller_id = $1 AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'));

-- name: ListOrdersByCheckout :many
-- Deterministic seller order, matching how the checkout created them.
SELECT * FROM orders WHERE checkout_id = $1 ORDER BY seller_id;

-- name: InsertOrderItem :exec
INSERT INTO order_items (order_id, listing_id, title, unit, unit_price_pesewas, quantity, line_total_pesewas)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: ListOrderItemsByOrder :many
SELECT * FROM order_items WHERE order_id = $1 ORDER BY listing_id;

-- name: ListOrderItemsByCheckout :many
-- The stock a checkout reserved, for restoring it exactly on expiry or
-- provider failure. Ordered by listing id, the same order used to reserve.
SELECT oi.* FROM order_items oi
JOIN orders o ON o.id = oi.order_id
WHERE o.checkout_id = $1
ORDER BY oi.listing_id;

-- name: InsertOrderEvent :exec
-- The append-only event trail orders.Transition writes.
INSERT INTO order_events (order_id, from_status, to_status, actor_type, actor_id, note)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListOrderEvents :many
SELECT * FROM order_events WHERE order_id = $1 ORDER BY id;

-- name: ReserveListingStock :execrows
-- Atomically decrements stock only while enough remains. Zero affected rows is
-- ErrInsufficientStock; the caller's transaction rolls back.
UPDATE listings SET quantity_available = quantity_available - $2
WHERE id = $1 AND quantity_available >= $2;

-- name: RestoreListingStock :exec
-- The exact inverse of a reservation, from the stored order items rather than
-- the request, so a restored checkout can never drift.
UPDATE listings SET quantity_available = quantity_available + $2 WHERE id = $1;

-- name: GetQuoteListingForUpdate :many
-- Snapshot locks for pricing, taken in ascending listing-id order by the
-- caller to avoid deadlocks between concurrent checkouts.
SELECT l.id, l.seller_id, sp.business_name AS seller_name, l.title, l.unit,
       l.price_pesewas AS unit_price, l.quantity_available, l.min_order_qty,
       l.category_id, c.parent_id AS category_parent_id,
       (l.status = 'active' AND l.expires_at > sqlc.arg('now')::timestamptz)::bool AS active,
       l.offers_pickup, l.offers_seller_delivery, l.seller_delivery_fee_pesewas
FROM listings l
JOIN categories c ON c.id = l.category_id
JOIN seller_profiles sp ON sp.user_id = l.seller_id
WHERE l.id = ANY(sqlc.arg('listing_ids')::uuid[])
ORDER BY l.id
FOR UPDATE;

-- name: ListCheckoutOrders :many
-- One row per seller order of a checkout, with the seller's public name for
-- summaries and replays.
SELECT o.*, sp.business_name AS seller_name
FROM orders o
JOIN seller_profiles sp ON sp.user_id = o.seller_id
WHERE o.checkout_id = $1
ORDER BY o.seller_id;

-- name: GetOrderDetail :one
-- Everything the order detail endpoint needs for either role, in one read.
SELECT o.*, sp.business_name AS seller_name, sp.bio AS seller_bio,
       sp.verification_status AS seller_verification, u.created_at AS seller_member_since
FROM orders o
JOIN seller_profiles sp ON sp.user_id = o.seller_id
JOIN users u ON u.id = o.seller_id
WHERE o.id = $1;

-- name: InsertDispute :one
-- Returns no row when the order already has a dispute: the buyer cannot open
-- a second case (the unique order_id enforces it).
INSERT INTO disputes (order_id, opened_by, reason, description)
VALUES ($1, $2, $3, $4)
ON CONFLICT (order_id) DO NOTHING
RETURNING *;

-- name: GetOpenDisputeByOrder :one
-- The auto-complete sweep's timer stop: an open dispute holds the order.
SELECT * FROM disputes WHERE order_id = $1 AND status = 'open';

-- name: ListUnacceptedPaidOrders :many
-- Orders whose seller has not accepted within the timeout window, in a
-- deterministic order. SKIP LOCKED keeps concurrent sweeps from fighting.
SELECT * FROM orders
WHERE status = 'paid' AND paid_at <= sqlc.arg('paid_before')::timestamptz
ORDER BY paid_at
LIMIT sqlc.arg('limit')
FOR UPDATE SKIP LOCKED;

-- name: ListDueDeliveredOrders :many
-- Delivered orders past their auto-complete deadline, for the sweep to finish.
-- The open-dispute exclusion lives in the sweep's per-order check under the
-- row lock, so a dispute opened mid-sweep is still honoured.
SELECT * FROM orders
WHERE status = 'delivered' AND auto_complete_at <= sqlc.arg('due_at')::timestamptz
ORDER BY auto_complete_at
LIMIT sqlc.arg('limit')
FOR UPDATE SKIP LOCKED;

-- name: SetOrderAcceptedAt :exec
UPDATE orders SET accepted_at = $2 WHERE id = $1 AND status = 'accepted';

-- name: SetOrderShipped :exec
UPDATE orders SET shipped_at = $2, tracking_ref = COALESCE($3, tracking_ref)
WHERE id = $1 AND status = 'shipped';

-- name: SetOrderDeliveredAt :exec
-- The auto-complete clock starts at delivery (DOMAIN §3: 3 days).
UPDATE orders SET delivered_at = $2, auto_complete_at = $3
WHERE id = $1 AND status = 'delivered';

-- name: SetOrderCompletedAt :exec
UPDATE orders SET completed_at = $2 WHERE id = $1 AND status = 'completed';

-- name: SetOrderTrackingRef :exec
-- The seller sets it at ship time, optionally; an empty ref keeps the stored one.
UPDATE orders SET tracking_ref = COALESCE($2, tracking_ref) WHERE id = $1;
