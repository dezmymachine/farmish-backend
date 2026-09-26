-- Payments (Phase 13a): a Paystack charge, and the webhook events that
-- settle it. Amounts are integer pesewas (DOMAIN §1) and the currency is
-- always GHS.

CREATE TABLE payments (
  id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  -- Server-generated: 'FMS-' || 20 base32 chars. Never a client-supplied
  -- value, and the only thing Paystack knows about this payment.
  reference               text NOT NULL UNIQUE,
  user_id                 uuid NOT NULL REFERENCES users(id),
  -- What the money is for. 'promotion' (Phase 14) and 'checkout' (Phase 15b)
  -- register handlers; purpose_ref is the tier or the cart id.
  purpose                 text NOT NULL CHECK (purpose IN ('promotion','checkout')),
  purpose_ref             text NOT NULL,
  -- base is what the platform must net; charge is what the buyer pays, and
  -- the check keeps the gross-up honest at the storage layer too.
  base_pesewas            bigint NOT NULL CHECK (base_pesewas > 0),
  processing_fee_pesewas  bigint NOT NULL CHECK (processing_fee_pesewas >= 0),
  charge_pesewas          bigint NOT NULL
                            CHECK (charge_pesewas = base_pesewas + processing_fee_pesewas),
  currency                text NOT NULL DEFAULT 'GHS' CHECK (currency = 'GHS'),
  status                  text NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending','success','failed','abandoned')),
  -- What Paystack actually charged, known only after the webhook.
  paystack_fee_pesewas    bigint CHECK (paystack_fee_pesewas IS NULL OR paystack_fee_pesewas >= 0),
  channel                 text,
  authorization_url       text,
  paid_at                 timestamptz,
  failure_reason          text,
  created_at              timestamptz NOT NULL DEFAULT now(),
  updated_at              timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payments_user_idx ON payments (user_id, created_at DESC);
CREATE INDEX payments_purpose_idx ON payments (purpose, purpose_ref);
CREATE TRIGGER set_updated_at BEFORE UPDATE ON payments
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- One row per provider event, which is what makes webhook handling idempotent:
-- the unique (provider, event_key) means a replay loses the insert race and
-- the handler returns 200 without a second effect.
CREATE TABLE webhook_events (
  id           bigserial PRIMARY KEY,
  provider     text NOT NULL,
  -- '<event>:<data.id>', e.g. 'charge.success:302961'.
  event_key    text NOT NULL,
  event_type   text NOT NULL,
  -- Stored for audit. It carries customer emails and phone numbers, so it is
  -- never logged and never returned by an API.
  payload      jsonb NOT NULL,
  received_at  timestamptz NOT NULL DEFAULT now(),
  processed_at timestamptz,
  -- 'processed' | 'ignored' | 'rejected:<reason>'
  outcome      text,
  UNIQUE (provider, event_key)
);
