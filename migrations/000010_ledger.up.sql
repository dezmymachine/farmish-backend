-- Ledger foundation (Phase 13b): append-only, double-entry accounts,
-- transactions and entries. Balances are always derived from entries; there
-- is no mutable balance column.
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

-- Fixed accounts from DOMAIN §5.2. The fixed accounts have no owner; dynamic
-- seller and promotion-credit accounts are created lazily by ledger.Post.
INSERT INTO ledger_accounts (code, type, currency, owner_id) VALUES
  ('paystack_clearing',    'asset',     'GHS', NULL),
  ('escrow',               'liability', 'GHS', NULL),
  ('payout_clearing',      'liability', 'GHS', NULL),
  ('platform_commission',  'revenue',   'GHS', NULL),
  ('promotion_revenue',    'revenue',   'GHS', NULL),
  ('processing_fee_income','revenue',   'GHS', NULL),
  ('paystack_fees',        'expense',   'GHS', NULL),
  ('transfer_fees',        'expense',   'GHS', NULL),
  ('promo_credits_issued', 'equity',    'CRD', NULL)
ON CONFLICT (code) DO NOTHING;
