# Phase 13b: Ledger foundation

**Depends on:** 13a · **Size:** medium

## Goal

An append-only, double-entry, multi-currency (GHS, CRD) ledger, exactly as DOMAIN §5, with an idempotent `Post`, balance queries and invariant tests. Later phases post through it inside their business transactions.

## Schema: `migrations/000010_ledger.up.sql`

```sql
CREATE TABLE ledger_accounts (
  id         bigserial PRIMARY KEY,
  code       text NOT NULL UNIQUE,              -- 'escrow', 'seller_payable:<uuid>', ...
  type       text NOT NULL CHECK (type IN ('asset','liability','revenue','expense','equity')),
  currency   text NOT NULL CHECK (currency IN ('GHS','CRD')),
  owner_id   uuid REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE ledger_transactions (
  id         bigserial PRIMARY KEY,
  kind       text NOT NULL,
  reference  text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (kind, reference)
);
CREATE TABLE ledger_entries (
  id             bigserial PRIMARY KEY,
  transaction_id bigint NOT NULL REFERENCES ledger_transactions(id),
  account_id     bigint NOT NULL REFERENCES ledger_accounts(id),
  amount         bigint NOT NULL CHECK (amount <> 0),  -- + debit, - credit
  currency       text   NOT NULL CHECK (currency IN ('GHS','CRD')),
  order_id       uuid,                                  -- FK added in 15a (orders don't exist yet)
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ledger_entries_account_idx ON ledger_entries (account_id);
CREATE INDEX ledger_entries_tx_idx ON ledger_entries (transaction_id);
CREATE INDEX ledger_entries_order_idx ON ledger_entries (order_id) WHERE order_id IS NOT NULL;

-- Append-only (forbid_mutation() comes from migration 000004).
CREATE TRIGGER ledger_accounts_append_only     BEFORE UPDATE OR DELETE ON ledger_accounts     FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER ledger_transactions_append_only BEFORE UPDATE OR DELETE ON ledger_transactions FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER ledger_entries_append_only      BEFORE UPDATE OR DELETE ON ledger_entries      FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Balanced-transaction guard at commit time (defence in depth; Post also validates).
CREATE FUNCTION ledger_check_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM ledger_entries WHERE transaction_id = NEW.transaction_id
             GROUP BY currency HAVING sum(amount) <> 0) THEN
    RAISE EXCEPTION 'ledger transaction % is unbalanced', NEW.transaction_id;
  END IF;
  RETURN NULL;
END; $$;
CREATE CONSTRAINT TRIGGER ledger_entries_balanced AFTER INSERT ON ledger_entries
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_check_balanced();
```

Also add a CHECK that an entry's currency equals its account's currency. Postgres CHECKs can't reference other tables, so enforce it in `Post`: look up each account's currency. Seed the fixed accounts from DOMAIN §5.2 in this migration with `INSERT … ON CONFLICT DO NOTHING` (`paystack_clearing`, `escrow`, `payout_clearing`, `platform_commission`, `promotion_revenue`, `processing_fee_income`, `paystack_fees`, `transfer_fees`, `promo_credits_issued`).

## API (Go, `internal/ledger`)

```go
type Entry struct {
    Account  string    // account code, e.g. ledger.SellerPayable(sellerID)
    Amount   int64     // + debit, - credit
    Currency string    // "GHS" | "CRD"
    OrderID  *uuid.UUID
}
var ErrDuplicate = errors.New("ledger transaction already posted")
var ErrUnbalanced = errors.New("ledger entries do not balance per currency")

// Post writes one transaction atomically in the caller's tx. Idempotent on
// (kind, reference): returns ErrDuplicate if it was already posted.
func (l *Ledger) Post(ctx context.Context, tx pgx.Tx, kind, reference string, entries ...Entry) error
func (l *Ledger) Balance(ctx context.Context, q db.DBTX, account string) (int64, error)   // Σ amount (raw sign)
func SellerPayable(id uuid.UUID) string   // "seller_payable:<id>"
func PromoCredits(id uuid.UUID) string    // "promo_credits:<id>"
// Fixed account codes as consts: Escrow, PaystackClearing, ... (DOMAIN §5.2)
```

`Post`:
1. Validate: at least 2 entries, non-zero amounts, a known currency, and per-currency sum = 0 (else `ErrUnbalanced`, **before** any insert).
2. Resolve accounts. Dynamic accounts (`seller_payable:`, `promo_credits:`) are created lazily with `ON CONFLICT (code) DO NOTHING`, with type and currency implied by the prefix. An unknown fixed code → error.
3. Check each entry's currency matches its account.
4. Insert the transaction (`ON CONFLICT (kind, reference) DO NOTHING RETURNING id`; no row → `ErrDuplicate`), then the entries.

**Callers** treat `ErrDuplicate` as "already done": that's the idempotency contract. Document it on the function.

## Tests

| Test | Proves |
|---|---|
| `TestPost_Balanced` | A valid posting is stored; balances are correct |
| `TestPost_UnbalancedRejected` | Sum ≠ 0 → `ErrUnbalanced`, nothing written |
| `TestPost_PerCurrencyBalance` | GHS +100/−100 plus CRD +5/−5 in one tx is OK; GHS +100 / CRD −100 → rejected |
| `TestPost_CurrencyMismatchWithAccount` | A CRD entry against `escrow` → error |
| `TestPost_Idempotent` | The same (kind, reference) twice → `ErrDuplicate`, one set of entries |
| `TestPost_RollsBackWithCallerTx` | Post inside a tx that rolls back → no rows |
| `TestLedger_AppendOnly` | UPDATE/DELETE on each ledger table raises |
| `TestLedger_DeferredBalanceTrigger` | Raw SQL inserting an unbalanced entry set fails at commit |
| `TestLedger_PropertyBalancesConsistent` | Seeded random generator (fixed seed; log it): 500 random balanced postings over 20 accounts → Σ all entries per currency = 0, and each account's `Balance` equals Σ of its generated amounts |
| `TestPostingTemplates` | Helper constructors for each DOMAIN §5.3 posting (`CheckoutPaid(...)`, `EscrowRelease(...)`, …) produce balanced entries with the exact amounts of the DOMAIN examples. **Build these constructors here**, so later phases just call them |

## Manual QA

`psql`: after the tests, `select code, type, currency from ledger_accounts` shows the fixed accounts. An attempt to `update ledger_entries set amount=1` errors.

## Pitfalls

- Never compute balances by keeping a mutable `balance` column. Derive them from entries. A cached balance (materialised) is a Phase 21+ optimisation, only if needed.
- The deferred constraint trigger runs at **commit**. Tests must commit to see it fire.
