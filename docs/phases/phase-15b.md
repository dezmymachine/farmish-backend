# Phase 15b: Checkout payment & escrow hold

**Depends on:** 15a · **Size:** large

## Goal

`POST /v1/checkout`:
- is idempotent (`Idempotency-Key`)
- reserves stock
- creates one checkout, one order per seller and a payment
- starts a Paystack charge

On `charge.success`, the `checkout` purpose handler confirms: orders become `paid`, escrow is held, and the ledger records `checkout_paid`. Unpaid checkouts expire and restore stock. Buyers and sellers can read their orders.

## API

**`POST /v1/checkout`** (bearer, `x-farmish-rate-limit: sensitive`)
- **Header:** `Idempotency-Key: <uuid>` (required; add it as a spec header parameter).
- **Body:** same as the quote.
- **201** `{checkoutId, reference, authorizationUrl, quote: CheckoutQuote, expiresAt}`.
- **200** with the **same** body when replayed with the same key and the same payload.
- **409** `idempotency_key_reused` (same key, different payload).
- **400** / **409** `insufficient_stock`.
- **502** `payment_provider_error`.

**`GET /v1/checkouts/{id}`** (buyer only; else 404)
- `{id, status, charge, expiresAt, orders: OrderSummary[]}`. The frontend polls this after the Paystack redirect.

**`GET /v1/orders`** (bearer): the buyer's orders, `?status&page&limit` → `{items: OrderSummary[], meta}`.

**`GET /v1/seller/orders`** (bearer): the seller's orders, same shape.

**`GET /v1/orders/{id}`** (bearer): the buyer **or** the seller of the order; anyone else → **404**.
- Returns `OrderDetail`: items, amounts, status, escrowState, delivery info, timestamps, events.
- The seller sees the recipient name and phone (needed to deliver). The buyer sees the seller's `PublicSeller`. The commission is shown to the **seller** only (`commission`, `sellerNet`).

## Checkout algorithm (service `checkout.Service.Create`)

1. Canonicalise the request: sort lines and delivery entries, JSON-marshal, and compute `request_hash = sha256`.
2. **Idempotency:** in a tx, `SELECT … FROM checkouts WHERE buyer_id=$1 AND idempotency_key=$2`.
   - If found and the hash matches → return the stored checkout (with its authorization URL).
   - If found and the hash differs → `ErrIdempotencyKeyReused`.
3. **In one tx:**
   1. Load listing snapshots `FOR UPDATE`, in a deterministic listing-id order to avoid deadlocks.
   2. `PriceCart(...)`.
   3. **Reserve stock** per line: `UPDATE listings SET quantity_available = quantity_available - $q WHERE id=$1 AND quantity_available >= $q` → 0 rows affected → `ErrInsufficientStock` (rollback).
   4. Insert the checkout (`status=pending_payment`, `expires_at = now + CHECKOUT_EXPIRY_MINUTES`), the orders, the order items (snapshots), and one `order_events` row (null → `pending_payment`, actor buyer) per order.
   5. Insert the payment row (`purpose='checkout'`, `purpose_ref=checkout_id`, base and fee from the quote) through a `payments` function that works inside the caller's tx.
   6. **Commit.** The insert of `(buyer_id, idempotency_key)` races safely: a unique violation → go back to step 2.
4. **Outside the tx:** Paystack `InitializeTransaction(amount = charge)`, then store `authorization_url` on the payment.
   - On failure: in a new tx, mark the checkout and payment `failed`, set the orders to `expired`, **restore stock**, and return 502.

## Purpose handler `checkout` (from the `payments.succeeded` job; one tx)

1. Lock the checkout `FOR UPDATE`.
2. **If `pending_payment`:**
   - checkout `paid`
   - each order: `pending_payment → paid` via `orders.Transition` (Phase 16 builds the full function; **in 15b implement just this transition** in `internal/orders`, with the event row)
   - `escrow_state = held`, `paid_at = now`
3. **Ledger `checkout_paid`** (DOMAIN §5.3.1), with one escrow credit entry per order (`order_id` set), `f = payment.paystack_fee_pesewas`, `P = processing_fee`. `ErrDuplicate` → already done.
4. **If the checkout is `expired`** (payment arrived after expiry):
   - try to **re-reserve** stock for all lines. If that succeeds: proceed as step 2.
   - otherwise: mark the orders `cancelled` (note `stock_unavailable_after_expiry`), post `checkout_paid`, and enqueue a **full refund** per order (Phase 17a job).
   - until 17a exists, enqueue a `orders.refund_needed` log-only job and add an audit event. **Document this clearly in the review packet**, since 17a will wire it.
5. **If `paid`** → no-op (idempotent).

Notifications to sellers come in Phase 16. Leave a `// Phase 16: notify seller` call site: a function in `orders` that 16 will implement. It should be a no-op for now.

## Jobs

`checkout.expire_unpaid`: periodic **every 5 minutes**. Checkouts with `pending_payment` and `expires_at <= now` → in one tx each (with `FOR UPDATE SKIP LOCKED`):
- checkout `expired`
- orders `pending_payment → expired` (events)
- **restore stock**
- payment `abandoned`

## Tests

| Test | Proves (plan Done-when first) |
|---|---|
| `TestCheckout_TwoSellersTwoOrdersOneCharge` | One checkout, two orders, **one** Paystack initialize with the grossed-up total |
| `TestCheckout_TamperedClientPriceIgnored` | A body with extra `price`/`unitPrice` fields → 400 from the validator (`additionalProperties: false`); the charge always equals the DB-derived quote |
| `TestCheckout_AmountMismatchLeavesUnpaid` | A webhook with a wrong amount → orders stay `pending_payment`, Error log + audit (via 13a) |
| `TestCheckout_WebhookReplayNoDoubleEscrow` | Replay `charge.success` → one `checkout_paid` transaction; the escrow balance equals Σ bases once |
| `TestCheckout_ExpiresAndRestoresStock` | Clock +31 min → the job expires it; `quantity_available` restored exactly |
| `TestCheckout_Idempotent` | The same key+payload twice → the same checkout id and URL, and **one** Paystack call; the same key with a different payload → 409 |
| `TestCheckout_ConcurrentLastUnit` | Stock 1, 5 concurrent buyers → exactly 1 checkout succeeds, 4 → 409 `insufficient_stock`; final stock 0 |
| `TestCheckout_ProviderFailureRollsBackStock` | Fake Paystack 500 → 502; stock restored; checkout failed |
| `TestCheckout_PaidAfterExpiry` | Expired, then webhook: stock still available → paid; stock gone → cancelled + refund-needed recorded |
| `TestOrders_AccessControl` | The buyer and the seller can read it; a third user → 404; the seller sees the recipient phone, the buyer doesn't see the commission |
| `TestLedger_EscrowEqualsHeldOrders` | After several paid checkouts: `Balance(escrow)` (as a liability, `-Σ`) = Σ `base_pesewas` of held orders |

## Manual QA

Test keys:
1. Two sellers, one buyer. `POST /v1/checkout/quote`, then `POST /v1/checkout` with a fresh key.
2. Pay with the test card. Poll `GET /v1/checkouts/{id}` until `paid`.
3. `psql`: the ledger `checkout_paid` entries sum to 0; the escrow entries carry `order_id`s.
4. Repeat the POST with the same key → the same response.

## Pitfalls

- **Lock order:** always lock listings in ascending id order across all code paths.
- Never call Paystack inside the DB tx.
- The stock restore on expiry and on failure must be exact: use the order items, not the request.
