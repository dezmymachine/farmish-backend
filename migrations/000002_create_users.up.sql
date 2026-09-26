-- One row per Firebase account. Identity comes only from verified ID tokens
-- (firebase_uid); email/phone are mirrored from the token for display/contact.
CREATE TABLE users (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  firebase_uid    text NOT NULL UNIQUE,
  signup_method   text NOT NULL CHECK (signup_method IN ('social', 'email', 'phone')),
  email           citext,
  email_verified  boolean NOT NULL DEFAULT false,
  phone_e164      text CHECK (phone_e164 ~ '^\+[1-9][0-9]{6,14}$'),
  display_name    text CHECK (char_length(display_name) BETWEEN 1 AND 80),
  role            text NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'admin')),
  seller_verified boolean NOT NULL DEFAULT false,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);

-- Not unique: without account linking, one email/phone can back several accounts.
CREATE INDEX users_email_idx ON users (email);
CREATE INDEX users_phone_e164_idx ON users (phone_e164);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON users
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();
