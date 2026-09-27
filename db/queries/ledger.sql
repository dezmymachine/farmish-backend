-- name: GetLedgerAccountByCode :one
-- An account row by its unique code.
SELECT * FROM ledger_accounts WHERE code = $1;

-- name: CreateLedgerAccount :one
-- Creates an account, or returns no row when code already exists. Dynamic
-- seller and promotion-credit accounts use this path; the caller selects the
-- existing row after losing the race.
INSERT INTO ledger_accounts (code, type, currency, owner_id)
VALUES ($1, $2, $3, $4)
ON CONFLICT (code) DO NOTHING
RETURNING *;

-- name: CreateLedgerTransaction :one
-- Creates a ledger transaction, or returns no row when (kind, reference) was
-- already posted. That missing row is the caller's idempotency signal.
INSERT INTO ledger_transactions (kind, reference)
VALUES ($1, $2)
ON CONFLICT (kind, reference) DO NOTHING
RETURNING *;

-- name: CreateLedgerEntry :one
INSERT INTO ledger_entries (transaction_id, account_id, amount, currency, order_id)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: SumLedgerAccountBalance :one
-- The account balance is derived from its entries. A code with no entries has
-- a zero balance, so this always returns a row for an existing account.
SELECT COALESCE(SUM(le.amount), 0)::bigint AS balance
FROM ledger_accounts la
LEFT JOIN ledger_entries le ON le.account_id = la.id
WHERE la.code = $1;

-- name: SumLedgerEntriesByCurrency :many
-- Every currency's signed total. A clean ledger sums to zero per currency
-- (DOMAIN §5.4); the reconciler reports any remainder.
SELECT le.currency, COALESCE(SUM(le.amount), 0)::bigint AS total
FROM ledger_entries le
GROUP BY le.currency;

-- name: SumHeldOrdersRemainder :one
-- The escrow the orders say is still held: base minus refunded over every
-- order whose escrow is not yet released or refunded away (DOMAIN §5.4).
SELECT COALESCE(SUM(base_pesewas - refunded_pesewas), 0)::bigint AS remainder
FROM orders
WHERE escrow_state IN ('held', 'refund_pending', 'partially_refunded');

-- name: ListPositiveDynamicBalances :many
-- Seller-payable and promotion-credit accounts whose raw entry sum is
-- positive: displayed as negative balances, which DOMAIN §5.4 forbids.
SELECT la.code, COALESCE(SUM(le.amount), 0)::bigint AS balance
FROM ledger_accounts la
LEFT JOIN ledger_entries le ON le.account_id = la.id
WHERE la.code LIKE 'seller_payable:%' OR la.code LIKE 'promo_credits:%'
GROUP BY la.code
HAVING COALESCE(SUM(le.amount), 0) > 0;

-- name: ListCompletedWithoutRelease :many
-- Completed orders past the grace window with no escrow_release posting.
SELECT o.id, o.completed_at FROM orders o
WHERE o.status = 'completed' AND o.completed_at <= $1
  AND NOT EXISTS (SELECT 1 FROM ledger_transactions t
                  WHERE t.kind = 'escrow_release' AND t.reference = o.id::text);

-- name: ListProcessedRefundsWithoutPosting :many
-- Processed refunds with no order_refund posting.
SELECT r.id FROM refunds r
WHERE r.status = 'processed'
  AND NOT EXISTS (SELECT 1 FROM ledger_transactions t
                  WHERE t.kind = 'order_refund' AND t.reference = r.id::text);

-- name: ListLedgerEntriesForOrder :many
-- Every entry tagged with the order, with its transaction, in posting order.
SELECT la.code AS account, le.amount, le.currency,
       t.kind AS transaction_kind, t.reference AS transaction_reference, le.created_at
FROM ledger_entries le
JOIN ledger_accounts la ON la.id = le.account_id
JOIN ledger_transactions t ON t.id = le.transaction_id
WHERE le.order_id = $1
ORDER BY le.created_at, le.id;
