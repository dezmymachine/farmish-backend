# Review packet: Phase 15b Checkout payment & escrow hold

## Summary

`POST /v1/checkout` now reserves stock, creates one checkout with one order per
seller and one payment, and starts a single Paystack charge for the whole
basket — idempotently on the `Idempotency-Key` header. A `charge.success` webhook
runs the `checkout` purpose handler: orders become paid, escrow is held, and
`checkout_paid` is posted with one escrow credit per order. Unpaid checkouts
expire after 30 minutes and restore stock exactly; a late payment either revives
the orders or cancels them with a full-refund trail. Buyers and sellers read
their orders through role-shaped projections.

## Commits

```
cbbeb01 Phase 15b: checkout payment, escrow hold and expiry
```

## Done-when checklist

Plan §6 Phase 15b:

- [x] A two-seller cart produces two orders and one charge: `TestCheckout_TwoSellersTwoOrdersOneCharge` — one provider call whose amount is the grossed-up total — `internal/checkout/checkout_test.go`
- [x] A tampered client price is ignored: `TestCheckout_TamperedClientPriceIgnored` — the validator rejects a body carrying `unitPrice` (400), and the honest body charges the DB-derived total — `internal/http/checkout_test.go`
- [x] An amount mismatch leaves orders unpaid: `TestCheckout_AmountMismatchLeavesUnpaid` — one pesewa off → checkout stays `pending_payment`, no ledger rows, an audit row — `internal/checkout/checkout_test.go`
- [x] A webhook replay makes no double escrow: `TestCheckout_WebhookReplayNoDoubleEscrow` — identical replay → exactly one `checkout_paid` posting, unchanged balances, every escrow credit carries `order_id`
- [x] Unpaid checkouts expire and restore stock: `TestCheckout_ExpiresAndRestoresStock` — clock +31 min → the sweep expires checkout, orders and payment, and restores stock exactly
- [x] The last unit can't be oversold: `TestCheckout_ConcurrentLastUnit` — 5 concurrent buyers, stock 1 → exactly 1 succeeds, 4 get `ErrInsufficientStock`, final stock 0

Spec test table:

- [x] `TestCheckout_Idempotent` — same key+payload → the same checkout id, no second provider call; same key, different payload → `ErrIdempotencyKeyReused`
- [x] `TestCheckout_ProviderFailureRollsBackStock` — fake Paystack 500 → the error, stock restored, checkout `failed`, orders `expired`
- [x] `TestCheckout_PaidAfterExpiry` — expired then paid: stock there → orders paid and escrow held; stock gone → orders cancelled, `escrow_state=refund_pending`, one `orders.refund_needed` job + audit row, and the escrow invariant still balances
- [x] `TestOrders_AccessControl` — buyer and seller read it; a stranger gets `ErrNotFound`; the seller sees the recipient phone and a positive commission snapshot; the buyer's list sees the order, the wrong seller's list does not
- [x] `TestLedger_EscrowEqualsHeldOrders` — after three paid checkouts, `Balance(escrow)` equals −Σ held bases, and the global GHS ledger sum is 0

Endpoint contract (beyond the spec's table):

- [x] `TestCheckoutEndpoint_FullContract` — 201 create, 200 replay with the same body and no second provider call, 409 `idempotency_key_reused`, missing key 400, malformed key 400, anonymous 401, buyer poll 200 → stranger poll 404, settle through the signed webhook, buyer detail without commission and with the seller's public profile, seller detail with commission + `sellerNet`, stranger detail 404, buyer/seller lists contract-valid
- [x] `TestCheckoutEndpoint_InsufficientStock409` — the second buyer's checkout for the last unit → 409 `insufficient_stock`

## make ci

```
#12 [build 4/6] RUN --mount=type=cache,target=/go/pkg/mod go mod download
#12 CACHED

#13 [build 5/6] COPY . .
#13 DONE 0.1s

#14 [build 6/6] RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build     CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/api ./cmd/migrate
#14 DONE 7.3s

#8 [stage-1 1/2] FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
#8 CACHED

#15 [stage-1 2/2] COPY --from=build /out/api /out/migrate /
#15 CACHED

#16 exporting to image
#16 exporting to image
#16 exporting layers 3.8s done
#16 exporting manifest sha256:2987fd04c486a84dbef745dab2cf51275a2fa9eeca139ed524d9c5735451e 0.0s done
#16 exporting config sha256:87924c5f4c6a715ab1ff9b90bdd75a2d99472f2a9b915abd27dd54a9dc44c5 0.0s done
#16 exporting attestation manifest sha256:ffcc59c7cd24348265fe97e2904944289fb8c7e915abd27dd54a9dc44c5 0.0s done
#16 exporting manifest list sha256:6f6cf34d87b1d9d0e859b1f96021d83bcd2430778d00cd43dea61a20aa358ceb 0.0s done
#16 naming to docker.io/library/farmish-backend:dev done
#16 unpacking to docker.io/library/farmish-backend:dev
#16 unpacking to docker.io/library/farmish-backend:dev
#16 unpacking to docker.io/library/farmish-backend:dev 0.5s done
#16 DONE 4.4s
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

## Manual QA

The local environment has no Paystack test keys and no public webhook URL, so
the spec's live-card path was adapted the same way as Phase 14's: the running
API used the local Paystack stand-in for transaction initialization, and the
signed production webhook path ran for real. Two sellers, one buyer, one
stranger, all through the real endpoints:

```
$ python3 /tmp/opencode/qa/qa15.py   # against :8081 with the fake provider
seller profiles: 200 200
listing seller-a: qa15-maize-<suffix> stock 10
listing seller-b: qa15-heifer-<suffix> stock 4
quote: base 852000 charge 868945 orders 2
checkout: <uuid> FMS-vrb5arhzvfs2qzvnsdqa https://pay.local/checkout/FMS-...
replay: 200 <same uuid>
key reuse: 409 idempotency_key_reused
poll before pay: pending_payment orders 2
webhook: 200
poll after pay: paid
buyer orders: 2
buyer view: commission None seller profile QA15 Seller B ...
seller view: commission 42500 net 807500
stranger order: 404
seller A sales: 1
```

The ledger afterwards, per the spec's psql step:

```
       kind        | postings
-------------------+----------
 checkout_paid     |        1        (plus pre-existing promotion rows)

       code              | balance
-----------------------+---------
 escrow                |  -852000
 paystack_clearing     |   874425
 paystack_fees         |      130
 processing_fee_income |   -17055

 escrow_credits_without_order
----------------------------
          0
```

Escrow is −Σ order bases (852000 across the two orders), `checkout_paid` posts
exactly once, and every escrow credit carries its `order_id`.

## Files changed

`git diff --stat 0b6afc5..cbbeb01` — 29 files, +6,176/−308. New:

- `internal/checkout/create.go` — canonical request hashing, idempotency,
  stock reservation in listing-id order, checkout/order/item/event/payment
  inserts in one transaction, provider failure unwind.
- `internal/checkout/confirm.go` — the `checkout` purpose handler: normal,
  revive and stock-gone paths; `checkout_paid` posting; the expiry sweep.
- `internal/checkout/jobs.go` — `checkout.expire_unpaid` (5 minutes, runs on
  start) and the log-only `orders.refund_needed`.
- `internal/checkout/checkout_test.go` — the service test table.
- `internal/orders/orders.go`, `transition.go`, `reads.go` — the DOMAIN §4
  transition subset 15b needs (with the two recovery rows), event writes, and
  the role-checked order reads.
- `internal/payments/checkout_tx.go` — the transaction-scoped payment helpers
  checkout needs (`CreatePendingInTx`, `MarkFailedInTx`, `MarkAbandonedInTx`,
  `SetAuthorizationURL`, `GetByReference`).
- `internal/http/handlers/checkout.go` — create, poll, lists and detail.
- `internal/http/checkout_test.go` — endpoint tests.
- `docs/adr/0025-late-payment-settles-abandoned.md`.

Changed: `docs/DOMAIN.md` §4 (the two owner-approved recovery rows),
`db/queries/orders.sql`, `db/queries/payments.sql`, generated sqlc code,
`api/openapi.yaml` + generated API code (five operations, `IdempotencyKey`
header parameter, order/checkout schemas), `internal/config` (+ tests) and
`.env.example` for `CHECKOUT_EXPIRY_MINUTES`, `cmd/api/main.go`,
router/handler seams.

## Schema changes

none. Phase 15a's `000012_orders` migration already carried everything this
phase writes. The dev database remains at `version 12 (dirty: false)`.

## API changes

| Method | Path | operationId | Auth | Notes |
|---|---|---|---|---|
| POST | `/v1/checkout` | `createCheckout` | bearer | 201 / 200 / 400 / 401 / 409 / 429 / 502; `Idempotency-Key` header (uuid, required); `x-farmish-rate-limit: sensitive` |
| GET | `/v1/checkouts/{id}` | `getCheckout` | bearer | 200 / 401 / 404 / 429 |
| GET | `/v1/orders` | `listMyOrders` | bearer | 200 / 400 / 401 / 429 |
| GET | `/v1/seller/orders` | `listMySales` | bearer | 200 / 400 / 401 / 429 |
| GET | `/v1/orders/{id}` | `getOrder` | bearer | 200 / 401 / 404 / 429 |

New schemas: `CheckoutCreated`, `OrderStatus`, `EscrowState`, `OrderSummary`,
`OrderSummaryList`, `CheckoutStatus`, `OrderEvent`, `OrderDetail`. New
parameter: `IdempotencyKey`. New shared parameters: `CheckoutId`, `OrderId`.
`make api-lint` and `generate-check` pass.

## Deviations from the spec

1. **A late charge.success settles an `abandoned` payment** (ADR-0025). The
   spec's expiry job abandons the payment, yet its recovery section requires a
   late webhook to run the purpose handler — impossible if settlement refuses
   `abandoned`. Abandoned now means "a charge was not expected", and the
   webhook proves the guess wrong. `failed` still cannot be revived.
2. **The paid-after-expiry recovery rows are in DOMAIN §4** (owner decision,
   2026-09-26): `expired → paid` (stock re-reserved) and `expired → cancelled`
   (stock gone, `escrow_state=refund_pending`, refund trail). DOMAIN stays the
   single source of truth rather than carrying the deviation in an ADR alone.
3. **A checkout whose provider call failed replays as `failed` rather than
   re-reserving stock.** The spec says "200 with the same body when replayed";
   a failed checkout therefore replays its failure, and the buyer retries with
   a fresh key. This prevents re-reserving stock on a dead key.
4. **`orders.refund_needed` is a log-only worker plus an audit row**, exactly
   as the spec directed. Phase 17a must replace it with the real `RefundOrder`
   job; the review protocol should check that 17a wires it.

## Open questions / risks

- **The stock race maps to 409, not 400.** A quantity-above-available error
  found while holding the listing lock means another buyer won; that surfaces
  as `insufficient_stock`. A quantity that was already wrong when the client
  loaded the page (below the minimum) is still a 400.
- **`reReserveStock` ignores listing status.** The spec's recovery path says
  only "try to re-reserve stock"; a seller who archived the listing during the
  window could still have their stock taken by a late payment. DOMAIN is
  silent; flagging for Phase 16, which owns listing transitions.
- **`getCheckout` reveals nothing to non-owners** (404), including the
  stranger's own error shape; the poll leaks no existence.
- **The expiry sweep processes up to 100 checkouts per pass, every 5
  minutes.** At current scale that is far more headroom than needed; if
  checkouts ever queue faster than they expire, the limit is the knob.
- **`orders.NotifySeller` is the deliberate Phase 16 no-op call site**; 16
  will implement SMS + in-app notifications there.

## Backlog additions

none.
