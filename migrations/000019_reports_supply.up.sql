-- Reports and supply requests (Phase 20b). Reports target exactly one of
-- a listing or a user, with at most one open report per (reporter, target).
-- Supply requests carry their items and an append-only event trail.
CREATE TABLE reports (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  reporter_id      uuid NOT NULL REFERENCES users(id),
  listing_id       uuid REFERENCES listings(id) ON DELETE CASCADE,
  reported_user_id uuid REFERENCES users(id),
  reason           text NOT NULL CHECK (reason IN ('spam','fraud','prohibited_item','offensive','wrong_category','other')),
  description      text CHECK (char_length(description) <= 1000),
  status           text NOT NULL DEFAULT 'open' CHECK (status IN ('open','actioned','dismissed')),
  action           text CHECK (action IN ('none','suspend_listing','hide_review')),
  resolution_note  text CHECK (char_length(resolution_note) <= 1000),
  resolved_by      uuid REFERENCES users(id),
  resolved_at      timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now(),
  CHECK ((listing_id IS NULL) <> (reported_user_id IS NULL)),
  CHECK (reason <> 'other' OR description IS NOT NULL)
);
CREATE UNIQUE INDEX reports_one_open_per_target ON reports
  (reporter_id, coalesce(listing_id, reported_user_id)) WHERE status = 'open';
CREATE INDEX reports_status_idx ON reports (status, created_at);

CREATE TABLE supply_requests (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  request_number   text NOT NULL UNIQUE CHECK (request_number ~ '^SUP-[0-9]{8}-[0-9A-Z]{6}$'),
  user_id          uuid NOT NULL REFERENCES users(id),
  delivery_name    text CHECK (char_length(delivery_name) <= 120),
  delivery_phone   text CHECK (delivery_phone ~ '^\+233[0-9]{9}$'),
  delivery_address text CHECK (char_length(delivery_address) <= 300),
  expected_date    date,
  notes            text CHECK (char_length(notes) <= 1000),
  status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','confirmed','processing','delivered','cancelled')),
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX supply_requests_user_idx ON supply_requests (user_id, created_at DESC);
CREATE INDEX supply_requests_status_idx ON supply_requests (status, created_at);
CREATE TRIGGER supply_requests_set_updated_at BEFORE UPDATE ON supply_requests
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE supply_request_items (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  supply_request_id uuid NOT NULL REFERENCES supply_requests(id) ON DELETE CASCADE,
  category_id       uuid NOT NULL REFERENCES categories(id),
  product_name      text NOT NULL CHECK (char_length(product_name) BETWEEN 2 AND 100),
  quantity          int NOT NULL CHECK (quantity >= 1),
  unit              text NOT NULL,
  sort_order        int NOT NULL
);

CREATE TABLE supply_request_events (
  id                bigserial PRIMARY KEY,
  supply_request_id uuid NOT NULL REFERENCES supply_requests(id),
  from_status       text,
  to_status         text NOT NULL,
  actor_id          uuid,
  note              text CHECK (char_length(note) <= 500),
  created_at        timestamptz NOT NULL DEFAULT now()
);
