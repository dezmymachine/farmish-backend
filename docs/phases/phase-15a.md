# Phase 15a: Checkout foundations (pricing, delivery, orders schema)

**Depends on:** 12, 13b · **Size:** medium

## Goal

All the pieces checkout needs, **except** payment:
- the orders schema
- commission configs (DOMAIN §2.1)
- the `delivery` package (`DeliveryProvider` + `manual`)
- a pure, heavily tested **pricing** function
- a **quote** endpoint the frontend can call before paying

## Schema: `migrations/000012_orders.up.sql`

```sql
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
-- plus set_updated_at triggers on checkouts, orders, commission_configs
```

## `internal/delivery`

```go
type Quote struct{ FeePesewas int64; Method string }
type Provider interface {
    Quote(ctx context.Context, in QuoteInput) (Quote, error)            // per seller-order
    CreateShipment(ctx context.Context, orderID uuid.UUID) (ref string, err error)
    Track(ctx context.Context, ref string) (Status, error)
}
```

`manual` implementation:
- `pickup` → fee 0.
- `seller_delivery` → fee = **max** over the order's items of `listings.seller_delivery_fee_pesewas`. One delivery per seller-order. This must be offered by **every** item in that order, else `ErrDeliveryNotOffered`.
- `courier` → `ErrNotSupported` (Backlog).
- `CreateShipment` / `Track` return `ErrNotSupported` for now; the seller sets the tracking ref manually in 16.

## Pricing: `internal/checkout/pricing.go` (pure; no DB in the core function)

```go
type CartLine struct{ ListingID uuid.UUID; Quantity int }
type Cart struct{ Lines []CartLine; Delivery map[uuid.UUID]DeliveryChoice } // keyed by seller_id
// PriceCart groups lines by seller → one OrderQuote per seller, then sums and grosses up.
func PriceCart(listings map[uuid.UUID]ListingSnapshot, cart Cart, rates CommissionRates, feeBps int, deliv delivery.Provider) (CheckoutQuote, error)
```

**Rules**, all errors being field-level validation errors:
- 1–50 lines; each listing at most once. Duplicate listing lines → 400.
- The listing is active and not expired.
- The buyer isn't the seller (`cannot_buy_own_listing`).
- `min_order_qty ≤ quantity ≤ quantity_available`.
- The delivery method is offered; the address, recipient name and phone are required for `seller_delivery`.
- Per order:
  - `subtotal = Σ unit_price × qty`
  - `delivery_fee` comes from the provider
  - `base = subtotal + delivery_fee`
  - `rate` resolved per DOMAIN §2.1, using the category of the order's **first** item. Mixed categories in one seller-order use the **highest** applicable rate. **Document this in the ADR**, and test it.
  - `commission = money.Commission(subtotal, rate)`
- Checkout: `base = Σ order bases`, and `charge`/`processing_fee` come from `money.GrossUp(base, PAYSTACK_FEE_BPS)`.
- **Prices come from the DB snapshot only.** Cart lines carry no prices at all, so tampering is impossible by construction.

## API

`POST /v1/checkout/quote` (bearer):
- **Request:** `{lines: [{listingId, quantity}], delivery: [{sellerId, method, address?, region?, district?, recipientName?, recipientPhone?}]}`.
- **200** `CheckoutQuote {orders: [{sellerId, sellerName, items: [{listingId, title, unit, unitPrice: Money, quantity, lineTotal: Money}], subtotal, deliveryFee, base}], base, processingFee, charge}`, with every amount as `Money`.
- **400** with field details.
- The commission is **not** exposed to buyers.

## Tests

| Test | Proves |
|---|---|
| `TestPriceCart_TwoSellersTwoOrders` | 3 lines across 2 sellers → 2 order quotes; sums, gross-up exact |
| `TestPriceCart_Validation` | Table: own listing, inactive, qty < min, qty > available, delivery not offered, missing address, duplicate line, 51 lines |
| `TestPriceCart_DeliveryFeeMax` | Two items with fees 500 and 800 → 800 |
| `TestPriceCart_CommissionResolution` | Default 500; parent override; child override wins; mixed categories → highest |
| `TestPriceCart_NoClientPrices` | The request schema has no price fields (spec test: the `CartLine` schema has only `listingId` and `quantity`) |
| `TestQuoteEndpoint_Contract` | `assertContract`; 401 anonymous |
| `TestMoney_ExamplesFromDomain` | Re-asserts the DOMAIN §2 examples through `PriceCart` |

## Pitfalls

- `PriceCart` must be deterministic: sort orders by seller id and lines by listing id.
- Don't reserve stock or create rows in 15a. That's 15b.
