-- Disputes (Phase 16). Opening a dispute stops the auto-complete timer: the
-- orders.auto_complete sweep skips delivered orders with an open dispute row.
-- Resolution (outcome, refund amounts) is Phase 17b.
CREATE TABLE disputes (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  -- One dispute per order: UNIQUE means a buyer cannot open a second case.
  order_id        uuid NOT NULL UNIQUE REFERENCES orders(id),
  opened_by       uuid NOT NULL REFERENCES users(id),
  reason          text NOT NULL CHECK (reason IN ('not_received','not_as_described','damaged','wrong_item','other')),
  description     text NOT NULL CHECK (char_length(description) BETWEEN 10 AND 2000),
  status          text NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved')),
  outcome         text CHECK (outcome IN ('refund_buyer','release_seller','partial')),
  refund_pesewas  bigint CHECK (refund_pesewas >= 0),
  resolution_note text CHECK (char_length(resolution_note) <= 2000),
  resolved_by     uuid REFERENCES users(id),
  resolved_at     timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER disputes_set_updated_at BEFORE UPDATE ON disputes
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();
