-- name: GetPayoutAccountBySeller :one
-- The seller's current payout account, if they set one.
SELECT * FROM seller_payout_accounts WHERE seller_id = $1;

-- name: UpsertPayoutAccount :one
-- Records the resolved account. On a repeat setup the row already exists:
-- the caller sets cooldown_until (now + 48h) itself, and passes NULL for a
-- first setup.
INSERT INTO seller_payout_accounts (seller_id, type, bank_code, bank_name,
  account_number_enc, account_number_mask, account_name, recipient_code,
  status, verified_at, cooldown_until)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (seller_id) DO UPDATE SET
  type = EXCLUDED.type, bank_code = EXCLUDED.bank_code, bank_name = EXCLUDED.bank_name,
  account_number_enc = EXCLUDED.account_number_enc, account_number_mask = EXCLUDED.account_number_mask,
  account_name = EXCLUDED.account_name, recipient_code = EXCLUDED.recipient_code,
  status = EXCLUDED.status, verified_at = EXCLUDED.verified_at, cooldown_until = EXCLUDED.cooldown_until
RETURNING *;

-- name: SetPayoutAccountVerified :one
-- An admin approves a needs_review account. Returns no row unless the
-- account is waiting for review: the caller treats that as "cannot approve".
UPDATE seller_payout_accounts
SET status = 'verified', verified_at = $2
WHERE seller_id = $1 AND status = 'needs_review'
RETURNING *;

-- name: InsertPayout :one
-- Records a queued payout for the seller's full payable balance, inside the
-- execution's own transaction. Returns no row when the seller already has a
-- payout in flight: the caller treats that as "already running".
INSERT INTO payouts (seller_id, amount_pesewas, reference, recipient_code)
VALUES ($1, $2, $3, $4)
ON CONFLICT (seller_id) WHERE status IN ('queued','pending') DO NOTHING
RETURNING *;

-- name: GetPayoutByID :one
SELECT * FROM payouts WHERE id = $1;

-- name: GetPayoutForUpdate :one
SELECT * FROM payouts WHERE id = $1 FOR UPDATE;

-- name: GetPayoutForUpdateByReference :one
-- Matches a transfer webhook to its payout by Paystack's reference.
SELECT * FROM payouts WHERE reference = $1 FOR UPDATE;

-- name: SetPayoutPending :exec
-- The transfer exists at Paystack: committed before anything that settles
-- it, so a crash mid-send leaves a pending payout the reconciler finds.
UPDATE payouts SET status = 'pending', transfer_code = $2, sent_at = $3
WHERE id = $1 AND status = 'queued';

-- name: SetPayoutSuccess :one
-- Returns no row when the payout already settled: the caller treats that as
-- a webhook replay.
UPDATE payouts SET status = 'success', completed_at = $2
WHERE id = $1 AND status IN ('queued', 'pending')
RETURNING *;

-- name: SetPayoutFailed :one
-- A failed or reversed transfer. Returns no row once successful: money that
-- already left can never be failed by a later event.
UPDATE payouts SET status = $2, failure_reason = $3, completed_at = $4
WHERE id = $1 AND status <> 'success'
RETURNING *;

-- name: SetPayoutQueuedForRetry :exec
-- Only from pending with no transfer at Paystack: reconciliation proved the
-- send never took effect, so sending again cannot pay twice.
UPDATE payouts SET status = 'queued', transfer_code = NULL, sent_at = NULL
WHERE id = $1 AND status = 'pending' AND transfer_code IS NULL;

-- name: CountInFlightPayoutsForSeller :one
SELECT count(*) FROM payouts WHERE seller_id = $1 AND status IN ('queued', 'pending');

-- name: SumPayoutsBySellerStatus :one
-- The seller's totals for one terminal status (success) or the in-flight
-- pair. Callers pass the statuses they need.
SELECT coalesce(sum(amount_pesewas), 0)::bigint FROM payouts
WHERE seller_id = $1 AND status = ANY(sqlc.arg('statuses')::text[]);

-- name: ListPayoutsBySeller :many
SELECT * FROM payouts WHERE seller_id = $1
ORDER BY created_at DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountPayoutsBySeller :one
SELECT count(*) FROM payouts WHERE seller_id = $1;

-- name: ListAdminPayouts :many
-- Every payout, newest first, with optional status and seller filters.
SELECT * FROM payouts
WHERE (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
  AND (sqlc.narg('seller_id')::uuid IS NULL OR seller_id = sqlc.narg('seller_id'))
ORDER BY created_at DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountAdminPayouts :one
SELECT count(*) FROM payouts
WHERE (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
  AND (sqlc.narg('seller_id')::uuid IS NULL OR seller_id = sqlc.narg('seller_id'));

-- name: ListPayoutCandidates :many
-- Sellers whose account is verified and past its change cooldown, oldest
-- change first so long-waiting sellers go first.
SELECT seller_id FROM seller_payout_accounts
WHERE status = 'verified' AND (cooldown_until IS NULL OR cooldown_until <= $1)
ORDER BY cooldown_until NULLS FIRST, seller_id;

-- name: ListStuckPendingPayouts :many
-- Pending payouts sent longer ago than the cutoff, for the reconciler.
SELECT * FROM payouts
WHERE status = 'pending' AND sent_at <= $1
ORDER BY sent_at
LIMIT $2;

-- name: SumHeldRemainderBySeller :one
-- What the seller's orders still hold in escrow: base minus refunded over
-- every order whose escrow is not yet released or refunded away.
SELECT coalesce(sum(base_pesewas - refunded_pesewas), 0)::bigint AS remainder
FROM orders
WHERE seller_id = $1 AND escrow_state IN ('held', 'refund_pending', 'partially_refunded');

-- name: LockPayoutSeller :one
-- Serialises one seller's payout execution against concurrent sweeps.
SELECT pg_advisory_xact_lock(hashtext('payout:' || $1::text)) AS locked;
