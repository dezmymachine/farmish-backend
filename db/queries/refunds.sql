-- name: InsertRefund :one
-- Records money owed for an order, inside the transition's own transaction
-- (the side effect of a cancellation, or the checkout expiry recovery path).
-- Returns no row when a full refund already exists for this order: the
-- partial unique index is the idempotency guard, and the caller treats a
-- missing row as "already recorded".
INSERT INTO refunds (order_id, amount_pesewas, reason)
VALUES ($1, $2, $3)
ON CONFLICT (order_id) WHERE reason IN ('seller_rejected','buyer_cancelled','seller_timeout','stock_unavailable','dispute_refund')
DO NOTHING
RETURNING *;

-- name: SumOutstandingRefundsForOrder :one
-- Every refund that has or may still move money (all but failed ones). New
-- refunds must keep this within the order's base.
SELECT coalesce(sum(amount_pesewas), 0)::bigint FROM refunds
WHERE order_id = $1 AND status <> 'failed';

-- name: GetRefundForUpdate :one
SELECT * FROM refunds WHERE id = $1 FOR UPDATE;

-- name: GetRefundByID :one
SELECT * FROM refunds WHERE id = $1;

-- name: SetRefundQueuedFromFailed :one
-- An admin retry: a failed refund goes back to queued with a clean attempt
-- slate. Returns no row unless the refund is failed: the caller treats that
-- as "cannot retry". The Paystack id stays linked, so reconciliation never
-- adopts the failed Paystack record for another row.
UPDATE refunds SET status = 'queued', failure_reason = NULL, attempted_at = NULL
WHERE id = $1 AND status = 'failed'
RETURNING *;

-- name: CountInFlightRefundsForOrder :one
-- Refunds that may still move money for an order. The escrow release waits
-- while any exists (Phase 17b partial resolutions).
SELECT count(*) FROM refunds WHERE order_id = $1 AND status IN ('queued', 'pending');

-- name: GetRefundForUpdateByPaystackRefundID :one
-- Matches a refund webhook once Paystack's id has been stored by the job.
SELECT * FROM refunds WHERE paystack_refund_id = $1 FOR UPDATE;

-- name: PaystackRefundIDLinked :one
-- Whether a Paystack refund id already belongs to one of our rows, so
-- reconciliation never adopts the same Paystack refund twice.
SELECT EXISTS (SELECT 1 FROM refunds WHERE paystack_refund_id = $1)::bool;

-- name: ListInFlightRefundsByReference :many
-- The webhook fallback match, before a refund's paystack_refund_id is known:
-- the order's payment reference plus the refunded amount, restricted to
-- refunds still in flight. A multi-seller checkout shares one reference, so
-- the caller must only act when exactly one row matches (ADR-0027).
SELECT r.* FROM refunds r
JOIN orders o ON o.id = r.order_id
JOIN checkouts c ON c.id = o.checkout_id
JOIN payments p ON p.id = c.payment_id
WHERE p.reference = $1 AND r.amount_pesewas = $2 AND r.status IN ('queued','pending')
ORDER BY r.created_at
FOR UPDATE OF r;

-- name: SetRefundPending :exec
-- The job is about to call Paystack: committed before the call, so a crash
-- mid-call leaves a pending refund that reconciliation finds, never a queued
-- one that a retry would send twice.
UPDATE refunds SET status = 'pending', attempted_at = $2 WHERE id = $1 AND status = 'queued';

-- name: SetRefundStatus :exec
-- Any status except processed may still move; processed is final, so a
-- replayed webhook or a job retry can never reopen a settled refund.
UPDATE refunds SET status = $2 WHERE id = $1 AND status <> 'processed';

-- name: SetRefundQueuedForRetry :exec
-- Only from pending with no Paystack refund: reconciliation proved Paystack
-- holds nothing for this attempt, so sending again cannot double-refund.
UPDATE refunds SET status = 'queued' WHERE id = $1 AND status = 'pending' AND paystack_refund_id IS NULL;

-- name: SetRefundFailed :exec
UPDATE refunds SET status = 'failed', failure_reason = $2 WHERE id = $1 AND status <> 'processed';

-- name: SetRefundProcessed :one
-- Returns no row when the refund was already processed: the caller treats
-- that as a replay.
UPDATE refunds SET status = 'processed' WHERE id = $1 AND status <> 'processed'
RETURNING *;

-- name: SetRefundPaystackID :exec
UPDATE refunds SET paystack_refund_id = $2 WHERE id = $1;

-- name: GetPaymentReferenceForOrder :one
-- The reference Paystack refunds against: the order's checkout payment.
SELECT p.reference FROM payments p
JOIN checkouts c ON c.payment_id = p.id
JOIN orders o ON o.checkout_id = c.id
WHERE o.id = $1;

-- name: AddOrderRefundedPesewas :one
UPDATE orders SET refunded_pesewas = refunded_pesewas + $2 WHERE id = $1
RETURNING *;

-- name: ListRefundsByOrder :many
SELECT * FROM refunds WHERE order_id = $1 ORDER BY created_at;
