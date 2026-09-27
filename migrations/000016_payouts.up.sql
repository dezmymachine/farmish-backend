-- Payouts (Phase 18b). One row per Paystack Transfer: the ledger's
-- payout_initiated/succeeded/failed postings key on the reference, and the
-- row's status is the idempotency guard for the webhooks. At most one
-- in-flight payout per seller, so a seller is never paid twice for the same
-- balance.
CREATE TABLE payouts (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  seller_id      uuid NOT NULL REFERENCES users(id),
  amount_pesewas bigint NOT NULL CHECK (amount_pesewas > 0),
  reference      text NOT NULL UNIQUE,
  recipient_code text NOT NULL,
  transfer_code  text UNIQUE,
  status         text NOT NULL DEFAULT 'queued'
                 CHECK (status IN ('queued','pending','success','failed','reversed')),
  failure_reason text,
  sent_at        timestamptz,
  completed_at   timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payouts_seller_idx ON payouts (seller_id, created_at DESC);
CREATE INDEX payouts_status_idx ON payouts (status, sent_at);
CREATE UNIQUE INDEX payouts_one_inflight ON payouts (seller_id) WHERE status IN ('queued','pending');
CREATE TRIGGER payouts_set_updated_at BEFORE UPDATE ON payouts
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();
