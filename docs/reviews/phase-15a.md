# Review packet: Phase 15a Checkout foundations

## Summary

Built everything checkout needs except payment: the orders schema with
commission configs, checkouts, orders, order items and append-only order
events; the `delivery` package with the manual provider; a pure, deterministic
`PriceCart` that groups lines into one order per seller and prices entirely
from server-side snapshots; and a contract-first `POST /v1/checkout/quote`
endpoint. No stock is reserved and no rows are created: that is Phase 15b.

## Commits

```
ae89291 Phase 15a: ledger tests create real order rows
096ad40 Phase 15a: checkout pricing, delivery and quote endpoint
fd9f41e Phase 15a: orders schema and checkout queries
```

## Done-when checklist

Plan §6 Phase 15a:

- [x] `PriceCart` is fully table-tested: `TestPriceCart_TwoSellersTwoOrders`,
  `TestPriceCart_Validation` (own listing, inactive, below minimum, above
  available, delivery not offered, missing address, duplicate line, 51 lines,
  unknown listing), `TestPriceCart_DeliveryFeeMax`,
  `TestPriceCart_CommissionResolution` (default, parent override, child
  override wins, mixed categories → highest) — `internal/checkout/pricing_test.go`
- [x] Two sellers → two orders: `TestPriceCart_TwoSellersTwoOrders` and
  `TestService_QuoteTwoSellers` — `internal/checkout/pricing_test.go`,
  `internal/checkout/service_test.go`
- [x] Gross-up exact: asserted in both tests against `money.GrossUp`
- [x] Commission resolution: `TestPriceCart_CommissionResolution` and
  `TestService_QuoteTwoSellers` (with real category rows and overrides)
- [x] The quote endpoint is contract-valid: `TestQuoteEndpoint_Contract`
  (happy path with `assertContract`, anonymous 401, duplicate-line 400) —
  `internal/http/checkout_test.go`

Spec test table:

- [x] `TestPriceCart_TwoSellersTwoOrders`
- [x] `TestPriceCart_Validation`
- [x] `TestPriceCart_DeliveryFeeMax` (fees 500 and 800 → 800)
- [x] `TestPriceCart_CommissionResolution`
- [x] `TestPriceCart_NoClientPrices` — `CheckoutCartLine` has only `listingId`
  and `quantity` JSON fields — `internal/http/checkout_test.go`
- [x] `TestQuoteEndpoint_Contract`
- [x] `TestMoney_ExamplesFromDomain` — DOMAIN §2.2's 10,000 → 10,199/199 and
  §2.1's 12,345 at 500bps → 617 re-asserted through `PriceCart`

Supporting coverage:

- [x] `TestManual_UnsupportedMethods` (`internal/delivery/delivery_test.go`):
  courier and shipment/tracking return `ErrNotSupported`
- [x] `TestService_Rates` and `TestService_QuoteTwoSellers`
  (`internal/checkout/service_test.go`): the seeded 500bps default and
  end-to-end pricing over real listings, categories and overrides
- [x] Ledger tests updated for `ledger_entries_order_fk`:
  `TestPost_Balanced`, `TestPostingTemplates_*` now create minimal checkout
  and order rows

## make ci

```
#13 DONE 0.3s

#14 [build 6/6] RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build     CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/api ./cmd/migrate
#14 DONE 10.1s

#15 [stage-1 2/2] COPY --from=build /out/api /out/migrate /
#15 DONE 0.1s

#16 exporting to image
#16 exporting to image
#16 exporting to layers 9.3s done
#16 exporting to manifest sha256:3e38059ff6e11d633b905ccd1a7afe9d4b074ea230eb904b6155b0de21c8ad90 0.0s done
#16 exporting to config sha256:af6700e3f8fe9bec8c229a035bf4416ee3b868c7072f62e7ec9d14010de53836 0.0s done
#16 exporting to attestation manifest sha256:582c0ddc8f6547758c0955736c7335d98bcbf06adeae2990cb2f577897ed60d0 0.0s done
#16 exporting to manifest list sha256:d8b2e25d5d4f1b4a455de10237f5a8782a37edcadc6860dfc5053f4519b97fae 0.0s done
#16 naming to docker.io/library/farmish-backend:dev done
#16 unpacking to docker.io/library/farmish-backend:dev
#16 unpacking to docker.io/library/farmish-backend:dev 0.9s done
#16 DONE 10.3s
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

## Manual QA

The spec has no QA script for this phase. What was verified by hand against the
running local database, in addition to the automated tests:

```
$ make migrate-up && make seed && make migrate-version
ok
version 12 (dirty: false)

$ psql -c "select count(*) as configs, count(*) filter (where category_id is null)
           as defaults, max(rate_bps) filter (where category_id is null)
           as default_rate_bps from commission_configs;"
 configs | defaults | default_rate_bps
---------+----------+------------------
       1 |        1 |              500
```

`\d checkouts`, `\d orders`, `\d order_items` and `\d order_events` match the
spec's schema, including every CHECK, the five order indexes, the
`ledger_entries_order_fk` foreign key and the three `set_updated_at` triggers.

`order_events` is append-only in practice, not just by trigger definition —
creating a checkout, an order and an event, then trying to tamper:

```
$ psql -c "BEGIN; ... INSERT INTO order_events (...); UPDATE order_events SET note = 'tampered'; COMMIT;"
ERROR:  UPDATE on order_events is not allowed (append-only)
CONTEXT:  PL/pgSQL function forbid_mutation() line 2 at RAISE
```

The whole transaction rolled back, so no test row was left behind.

## Files changed

`git diff --stat ee03449^..HEAD` — 47 files, +6,176/−365. New:

- `internal/checkout/checkout.go` — cart, delivery-choice and quote types.
- `internal/checkout/pricing.go` — pure `PriceCart` with validation, checked
  arithmetic, per-seller grouping, delivery quoting and commission resolution.
- `internal/checkout/service.go` — loads snapshots and commission rows from
  Postgres; nothing else touches the DB.
- `internal/checkout/pricing_test.go`, `service_test.go` — the pricing tables
  and the service tests over real listings.
- `internal/delivery/delivery.go`, `manual.go`, `delivery_test.go` — the
  `Provider` interface and the manual implementation.
- `internal/http/handlers/checkout.go` — the quote handler.
- `internal/http/checkout_test.go` — the endpoint contract tests.
- `migrations/000012_orders.{up,down}.sql` — the orders schema.
- `docs/adr/0024-mixed-category-commission-rate.md`.

## Schema changes

- `000012_orders` — `commission_configs` (default 500bps seeded), `checkouts`,
  `orders`, `order_items` and `order_events`, plus `ledger_entries_order_fk`
  and `set_updated_at` triggers. The down migration drops all five tables and
  the ledger FK.
- `TestUpDownUp` passes through `make ci`. The development database reports
  `version 12 (dirty: false)`.

## API changes

| Method | Path | operationId | Auth | Notes |
|---|---|---|---|---|
| POST | `/v1/checkout/quote` | `quoteCheckout` | bearer | 200 / 400 / 401 / 429 |

New schemas: `CheckoutCartLine` (only `listingId` and `quantity`),
`CheckoutDelivery`, `CheckoutQuoteRequest`, `CheckoutQuoteItem`,
`CheckoutOrderQuote` and `CheckoutQuote`. The commission rate and amount are
deliberately absent from the response. `make api-lint` and `generate-check`
pass.

## Deviations from the spec

1. **Mixed-category orders use the highest line-level applicable rate**
   (ADR-0024). The spec said to use the category of the order's first item and
   asked for an ADR if mixed categories were handled differently. Highest-rate
   never undercharges relative to any line, and a child override still beats
   its parent for single-category orders.
2. **The delivery map is a list in the contract.** JSON has no object keys
   matching `uuid` well; the handler rejects duplicate `sellerId` entries with
   a 400 before pricing, so the map semantics hold.
3. **Seller delivery validates the recipient name too.** The spec listed
   address and phone as required; a Ghanaian delivery also needs someone to
   hand the goods to, and the orders schema stores `recipient_name`.

## Open questions / risks

- **`orders.commission_pesewas` is computed but not written in 15a.** Phase 15b
  must snapshot the resolved rate and commission when creating order rows;
  `PriceCart` returns both so the write cannot drift from the quote.
- **Quantity arithmetic is checked with `math/bits`, not plain `int64`
  multiplication.** A hostile `quantity * unit_price` that overflows becomes a
  field-level validation error, not a wrapped charge.
- **`delivery` is priced per seller-order, not per item** (DOMAIN §5.3.1 books
  one escrow entry per order). `TestPriceCart_DeliveryFeeMax` pins the
  max-item-fee rule.
- **`courier` appears in the contract but always fails with a validation
  error.** The `Provider` interface leaves room for the backlog integration
  without a contract change.

## Backlog additions

none.
