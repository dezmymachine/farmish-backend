-- Checkout foundations (Phase 15a): commission overrides, checkouts, orders,
-- order items and order events. Phase 15b creates checkout and order rows;
-- this phase only prepares the schema and loads the default commission.
CREATE TABLE commission_configs (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  category_id uuid UNIQUE REFERENCES categories(id),     -- NULL = default (exactly one)
  rate_bps    int NOT NULL CHECK (rate_bps BETWEEN 0 AND 3000),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX commission_configs_default_idx ON commission_configs ((category_id IS NULL)) WHERE category_id IS NULL;
INSERT INTO commission_configs (category_id, rate_bps) VALUES (NULL, 500);   -- DOMAIN §2.1

CREATE TABLE checkouts (
  id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  buyer_id               uuid NOT NULL REFERENCES users(id),
  idempotency_key        uuid NOT NULL,
  request_hash           text NOT NULL,                   -- sha256 of the canonical request (15b)
  base_pesewas           bigint NOT NULL CHECK (base_pesewas > 0),
  processing_fee_pesewas bigint NOT NULL CHECK (processing_fee_pesewas >= 0),
  charge_pesewas         bigint NOT NULL,
  payment_id             uuid REFERENCES payments(id),
  status                 text NOT NULL DEFAULT 'pending_payment'
                         CHECK (status IN ('pending_payment','paid','expired','failed')),
  expires_at             timestamptz NOT NULL,
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now(),
  UNIQUE (buyer_id, idempotency_key)
);

CREATE TABLE orders (
  id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  checkout_id            uuid NOT NULL REFERENCES checkouts(id),
  buyer_id               uuid NOT NULL REFERENCES users(id),
  seller_id              uuid NOT NULL REFERENCES users(id),
  status                 text NOT NULL DEFAULT 'pending_payment'
                         CHECK (status IN ('pending_payment','paid','accepted','shipped','delivered','completed',
                                           'cancelled','disputed','refunded','expired')),
  escrow_state           text NOT NULL DEFAULT 'none'
                         CHECK (escrow_state IN ('none','held','released','refund_pending','refunded','partially_refunded')),
  subtotal_pesewas       bigint NOT NULL CHECK (subtotal_pesewas > 0),
  delivery_fee_pesewas   bigint NOT NULL CHECK (delivery_fee_pesewas >= 0),
  base_pesewas           bigint NOT NULL CHECK (base_pesewas = subtotal_pesewas + delivery_fee_pesewas),
  commission_rate_bps    int    NOT NULL,
  commission_pesewas     bigint NOT NULL CHECK (commission_pesewas >= 0),
  refunded_pesewas       bigint NOT NULL DEFAULT 0 CHECK (refunded_pesewas >= 0),
  delivery_method        text NOT NULL CHECK (delivery_method IN ('pickup','seller_delivery','courier')),
  delivery_address       text CHECK (char_length(delivery_address) <= 300),
  delivery_region        text,
  delivery_district      text,
  recipient_name         text CHECK (char_length(recipient_name) <= 120),
  recipient_phone        text CHECK (recipient_phone ~ '^\+233[0-9]{9}$'),
  tracking_ref           text CHECK (char_length(tracking_ref) <= 120),
  paid_at                timestamptz,
  accepted_at            timestamptz,
  shipped_at             timestamptz,
  delivered_at           timestamptz,
  completed_at           timestamptz,
  cancelled_at           timestamptz,
  auto_complete_at       timestamptz,
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now(),
  CHECK (seller_id <> buyer_id),
  CHECK (delivery_method = 'pickup' OR delivery_address IS NOT NULL)
);
CREATE INDEX orders_buyer_idx  ON orders (buyer_id, created_at DESC);
CREATE INDEX orders_seller_idx ON orders (seller_id, status, created_at DESC);
CREATE INDEX orders_timers_idx ON orders (status, auto_complete_at);
CREATE INDEX orders_paid_idx ON orders (status, paid_at);
CREATE INDEX orders_checkout_idx ON orders (checkout_id);

CREATE TABLE order_items (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  order_id           uuid NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
  listing_id         uuid NOT NULL REFERENCES listings(id),
  title              text NOT NULL,             -- snapshot
  unit               text NOT NULL,             -- snapshot
  unit_price_pesewas bigint NOT NULL CHECK (unit_price_pesewas > 0),   -- snapshot
  quantity           int NOT NULL CHECK (quantity > 0),
  line_total_pesewas bigint NOT NULL CHECK (line_total_pesewas = unit_price_pesewas * quantity)
);
CREATE INDEX order_items_order_idx ON order_items (order_id);

CREATE TABLE order_events (
  id         bigserial PRIMARY KEY,
  order_id   uuid NOT NULL REFERENCES orders(id),
  from_status text,
  to_status  text NOT NULL,
  actor_type text NOT NULL CHECK (actor_type IN ('buyer','seller','admin','system')),
  actor_id   uuid,
  note       text CHECK (char_length(note) <= 500),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER order_events_append_only BEFORE UPDATE OR DELETE ON order_events FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_order_fk FOREIGN KEY (order_id) REFERENCES orders(id);
CREATE TRIGGER commission_configs_set_updated_at BEFORE UPDATE ON commission_configs FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER checkouts_set_updated_at BEFORE UPDATE ON checkouts FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER orders_set_updated_at BEFORE UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION set_updated_at();
