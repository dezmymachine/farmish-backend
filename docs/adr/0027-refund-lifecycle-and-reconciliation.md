# ADR-0027: Refund lifecycle, Paystack reconciliation and webhook matching

- **Status:** Accepted
- **Date:** 2026-09-27
- **Phase:** 17a (written after the review in `docs/reviews/phase-17a.md`)

## Context

Refunds move buyer money back through Paystack. The first 17a implementation followed the spec's rule, "Paystack error → back to queued and River retries". That rule can refund a buyer **twice**:
- A timeout or lost response doesn't prove the refund failed.
- Paystack allows several partial refunds per transaction.

The review also found three more problems:
- The webhook fallback match was ambiguous on multi-seller checkouts, which share one payment reference.
- Settlements didn't check the amount, the currency, or that refunds stay within the order's base.
- The client sent the wrong Create Refund field.

## Verified against Paystack's official OpenAPI spec

Source: `PaystackOSS/openapi`, `dist/paystack.yaml`, fetched 2026-09-27.
- **`POST /refund`** takes the body field **`transaction`** ("the reference of a previously completed transaction"), plus optional `amount` in the subunit, `currency`, `customer_note` and `merchant_note`. The client previously sent `reference`, which would have failed every live refund. It's fixed, and pinned by `TestPaystackClient_CreateRefund`.
- **`GET /refund`** documents only `perPage`, `page`, `from` and `to`. There is **no transaction filter**, so reconciliation lists refunds by date window and filters locally by `transaction_reference` and `amount`.
- **`GET /refund/{id}`** returns one refund. Refund objects carry `id`, `transaction_reference`, `amount`, `currency`, `status` and `createdAt`.
- **Webhook event names** used: `refund.processed`, `refund.failed`, `refund.pending`, `refund.processing`. The OpenAPI spec doesn't define webhook payloads.
  - Per Paystack's refund documentation, the payload carries `transaction_reference`, `refund_reference`, `amount`, `currency` and `status`, and **may carry no top-level `id`**.
  - **Owner action before go-live (Phase 22):** confirm against a live test-mode refund webhook, and record the observed payload here.

## Decisions

1. **The refund job is the guaranteed settlement path; webhooks are the fast path.** The `orders.refund` job snoozes and re-checks with Paystack while a refund is pending. River snoozes don't consume attempts. So settlement never depends on webhook delivery or on the webhook payload's shape.

2. **States and transitions:**
   - **`queued`:** check "no refund after release" (else `failed: refund_after_release`, the manual path). Commit `pending` + `attempted_at`, then call Paystack with no transaction open.
     - Success: store Paystack's id, and poll every 10 minutes.
     - **Definite rejection** (`status:false`, or HTTP 4xx other than 408/429, via `payments.IsDefiniteRejection`): `failed: paystack_rejected: <message>`, an audit event, an Error log. No retry.
     - **Ambiguous** (transport error, timeout, 408, 429, 5xx, an unreadable body): **stay `pending`**. Never automatically back to `queued`.
   - **`pending` with Paystack's id:** `FetchRefund`. `processed` → settle (decision 4); `failed` → fail; otherwise poll.
   - **`pending` without an id:** list Paystack refunds from `attempted_at − 5 min`, and keep those matching our reference and amount that no refund row has claimed yet.
     - Exactly one match → adopt its id.
     - None, and `attempted_at` is at least **15 minutes** old → `queued` for another call. That's the only automatic resend, made only once Paystack provably holds nothing.
     - Several matches → an Error log, and keep polling for an admin.
   - **After 7 days unsettled** → audit `refund.stuck`, an Error log, and polling stops.

3. **Webhook matching:**
   - By Paystack's id when stored.
   - Otherwise by `(payment reference, amount)` over in-flight refunds, **only when exactly one matches**. With two or more, the handler returns `ErrAmbiguousRefundMatch`. The webhook is rolled back and answered 500 (ADR-0021), so Paystack retries after the job has stored the ids; the job would settle it anyway.
   - A fallback match remembers the event's id when present.
   - **Webhook dedupe key:** `<event>:<data.id>`, or `<event>:ref:<data.refund_reference>` when there's no id. Neither → malformed (400).

4. **Settlement** (shared by the webhook and reconciliation), under the order's row lock:
   - Paystack's amount must equal ours, the currency must be GHS, and `refunded + amount ≤ base`.
   - Otherwise: outcome `rejected:amount_mismatch`, `rejected:currency_mismatch` or `rejected:exceeds_base`, an Error log and audit `refund.settlement_rejected`, with nothing settled.
   - On success: `processed`, `order_refund` posted (idempotent on the refund id), `refunded_pesewas`, `escrow_state`, and a buyer SMS.

5. **Over-refund guards:**
   - `CreateRefund` locks the order and refuses a refund that takes Σ(non-failed refunds) above base (`ErrRefundExceedsBase`).
   - A CHECK `orders_refunded_within_base` makes the database refuse it too.
   - `CreateRefund` also refuses to run without a job client, so a refund row is never recorded without the job that moves the money.

6. **Outcomes follow ADR-0021.** Malformed refund bodies return `ErrMalformedEvent` (400), and the handlers return the documented outcome constants.

## Consequences

- A refund can take up to about 15 minutes longer after an ambiguous failure. That's the price of never refunding twice.
- Reconciliation walks at most 10 pages of 100 refunds per check. At our volume that covers days. Revisit it (by narrowing `from`/`to`) if daily refunds grow large.
- The Phase 13a Paystack client wraps a `*payments.StatusError` inside `ErrProviderUnavailable`, so existing callers are unchanged and refunds can classify errors.
