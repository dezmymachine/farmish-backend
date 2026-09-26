-- name: InsertPayment :one
-- A pending payment, before Paystack is called. The gross-up is computed by
-- the caller from internal/money and the CHECK on charge_pesewas enforces it.
INSERT INTO payments (reference, user_id, purpose, purpose_ref, base_pesewas,
                      processing_fee_pesewas, charge_pesewas, currency, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'GHS', 'pending')
RETURNING *;

-- name: GetPaymentByID :one
SELECT * FROM payments WHERE id = $1;

-- name: GetPaymentByReference :one
SELECT * FROM payments WHERE reference = $1;

-- name: GetPaymentByReferenceForUpdate :one
-- Row-locked. Two webhook deliveries for the same reference must not both
-- settle the payment, so the handler settles it under this lock.
SELECT * FROM payments WHERE reference = $1 FOR UPDATE;

-- name: GetPaymentsByPurposeRef :many
-- A buyer retrying after a failed attempt needs to find the earlier rows.
SELECT * FROM payments WHERE purpose = $1 AND purpose_ref = $2
ORDER BY created_at DESC;

-- name: SetPaymentAuthorizationURL :one
-- Stored after InitializeTransaction, which happens outside the insert's
-- transaction: the provider call must never hold a database transaction open.
UPDATE payments SET authorization_url = $2 WHERE id = $1
RETURNING *;

-- name: MarkPaymentFailed :one
UPDATE payments
SET status = 'failed', failure_reason = $2
WHERE id = $1 AND status = 'pending'
RETURNING *;

-- name: MarkPaymentAbandoned :one
UPDATE payments
SET status = 'abandoned', failure_reason = $2
WHERE id = $1 AND status = 'pending'
RETURNING *;

-- name: SettlePaymentSuccess :one
-- The single writer of success. It only fires from pending, so a replay (or a
-- webhook racing the verify fallback) is a no-op that returns no row.
UPDATE payments
SET status = 'success',
    paid_at = $2,
    channel = $3,
    paystack_fee_pesewas = $4,
    failure_reason = NULL
WHERE id = $1 AND status = 'pending'
RETURNING *;

-- name: InsertWebhookEvent :one
-- Returns no row when (provider, event_key) already exists: that is a replay,
-- and the caller answers 200 without dispatching anything.
INSERT INTO webhook_events (provider, event_key, event_type, payload)
VALUES ($1, $2, $3, $4)
ON CONFLICT (provider, event_key) DO NOTHING
RETURNING *;

-- name: CompleteWebhookEvent :exec
UPDATE webhook_events SET processed_at = now(), outcome = $2 WHERE id = $1;
