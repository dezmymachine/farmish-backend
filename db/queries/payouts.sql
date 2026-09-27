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
