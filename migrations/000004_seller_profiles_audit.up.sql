-- Seller profiles (Phase 8): users become sellers by creating a profile.
-- Identity fields are validated in Go; id_number_enc holds crypto.Encrypt
-- output and is never selected by public queries.
CREATE TABLE seller_profiles (
  user_id            uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  business_name      text NOT NULL CHECK (char_length(business_name) BETWEEN 2 AND 120),
  region             text NOT NULL,              -- validated in Go against geo.Regions
  district           text NOT NULL CHECK (char_length(district) BETWEEN 2 AND 80),
  bio                text CHECK (char_length(bio) <= 1000),
  show_phone         boolean NOT NULL DEFAULT false,
  show_whatsapp      boolean NOT NULL DEFAULT false,
  whatsapp_e164      text CHECK (whatsapp_e164 ~ '^\+233[0-9]{9}$'),
  verification_status text NOT NULL DEFAULT 'unverified'
                     CHECK (verification_status IN ('unverified','pending','verified','rejected')),
  id_type            text CHECK (id_type IN ('ghana_card','passport','voters_id','drivers_license')),
  id_number_enc      text,                        -- crypto.Encrypt output; never selected by public queries
  id_number_last4    text CHECK (char_length(id_number_last4) = 4),
  submitted_at       timestamptz,
  reviewed_at        timestamptz,
  reviewed_by        uuid REFERENCES users(id),
  rejection_reason   text CHECK (char_length(rejection_reason) <= 500),
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  CHECK ((id_type IS NULL) = (id_number_enc IS NULL))
);
CREATE INDEX seller_profiles_status_idx ON seller_profiles (verification_status);
CREATE TRIGGER set_updated_at BEFORE UPDATE ON seller_profiles FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Audit trail (Phase 8): append-only event log. Phase 13b's ledger reuses
-- forbid_mutation(), so later migrations must not drop it.
CREATE TABLE audit_events (
  id          bigserial PRIMARY KEY,
  actor_id    uuid REFERENCES users(id),        -- NULL = system
  action      text NOT NULL,                    -- e.g. 'seller.verify', 'seller.reject'
  target_type text NOT NULL,
  target_id   text NOT NULL,
  metadata    jsonb NOT NULL DEFAULT '{}',
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_events_target_idx ON audit_events (target_type, target_id, created_at);
-- Append-only: block UPDATE/DELETE.
CREATE FUNCTION forbid_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION '% on % is not allowed (append-only)', TG_OP, TG_TABLE_NAME; END; $$;
CREATE TRIGGER audit_events_append_only BEFORE UPDATE OR DELETE ON audit_events
  FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
