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
