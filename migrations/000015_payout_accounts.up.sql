-- Seller payout accounts (Phase 18a). One row per seller: the Paystack-
-- resolved account plus its transfer recipient. The number is stored
-- encrypted (crypto.Encrypt) and only ever leaves the database masked.
-- History is the audit trail (payout_account.set/approve with masked
-- numbers), not a table: the row holds the current account only.
CREATE TABLE seller_payout_accounts (
  seller_id           uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  type                text NOT NULL CHECK (type IN ('mobile_money','ghipss')),
  bank_code           text NOT NULL,
  bank_name           text NOT NULL,
  account_number_enc  text NOT NULL,
  account_number_mask text NOT NULL,
  account_name        text NOT NULL,
  recipient_code      text NOT NULL,
  status              text NOT NULL CHECK (status IN ('verified','needs_review')),
  verified_at         timestamptz,
  cooldown_until      timestamptz,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER seller_payout_accounts_set_updated_at BEFORE UPDATE ON seller_payout_accounts
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();
