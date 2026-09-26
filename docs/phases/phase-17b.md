# Phase 17b: Disputes & ledger reconciliation

**Depends on:** 17a · **Size:** medium

## Goal

Admins resolve disputes (refund the buyer, release to the seller, or split), with an audit trail, and a daily reconciliation job checks the DOMAIN §5.4 ledger invariants.

## API (tag `admin`, all `x-farmish-role: admin`)

| Method & path | Request | Responses |
|---|---|---|
| `GET /v1/admin/disputes` | `?status=open&page&limit` | 200 `{items: DisputeAdmin[], meta}` |
| `GET /v1/admin/disputes/{id}` | none | 200 `DisputeAdmin` (with the full `OrderDetail` and events), 404 |
| `POST /v1/admin/disputes/{id}/resolve` | `{outcome: refund_buyer\|release_seller\|partial, refundAmount?: Money, note: 1–2000}` | 200 `DisputeAdmin`, 400, 404, 409 (not open) |
| `GET /v1/admin/orders/{id}` | none | 200 `OrderDetail` (admin view: both parties' contact details, ledger entries for the order) |
| `POST /v1/admin/refunds/{id}/retry` | none | 200 (failed → queued, re-enqueued), 409 (not failed, or `refund_after_release`) |

## Rules (one tx each, using `orders.Transition` with actor admin)

- **`refund_buyer`:** disputed → refunded. Refund row `dispute_refund` = full remaining escrow. Enqueue `orders.refund`.
- **`release_seller`:** disputed → completed. Enqueue `orders.release_escrow`.
- **`partial`:** requires `0 < refundAmount < remaining escrow`.
  - disputed → completed
  - refund row `dispute_partial` = refundAmount
  - enqueue `orders.refund`, **then** release
  - Release must wait until the partial refund is `processed`: the release job treats `refund_pending` as "retry later" by returning an error with `river.JobSnooze(10*time.Minute)`. Use River's snooze and cover it in the test.
- Every resolution: dispute `resolved`, outcome, amount, note, `resolved_by`, `resolved_at`, and **an audit event** `dispute.resolve`. Notify both parties via SMS.

## Job: `ledger.reconcile` (periodic daily, 03:00 Africa/Accra)

It computes, and **logs at Error level plus writes an audit event** on any violation:
1. Σ entries per currency = 0.
2. Escrow balance = Σ over orders in escrow states of `(base - refunded)`, excluding released ones.
3. No negative `promo_credits:*` or `seller_payable:*` balances.
4. Every `completed` order older than 1h has an `escrow_release` transaction; every `processed` refund has an `order_refund`.

It returns nil either way. It's a detector, not a fixer. Also expose `ledger.Reconcile(ctx) (Report, error)` for tests and a future admin endpoint.

## Tests

| Test | Proves |
|---|---|
| `TestResolve_RefundBuyer` | disputed → refunded, refund job, ledger, audit |
| `TestResolve_ReleaseSeller` | disputed → completed, release posted |
| `TestResolve_PartialThenRelease` | Refund processed first, then release with the commission on the remainder (17a formula); release snoozes while the refund is pending |
| `TestResolve_Validation` | Partial with amount ≥ remaining or ≤ 0 → 400; resolving twice → 409; non-admin → 403 |
| `TestReconcile_DetectsViolations` | Insert a deliberately broken state through raw SQL in a test DB (e.g. a completed order without a release) → the report lists it + Error log |
| `TestReconcile_CleanAfterFullScenario` | Run the full 15b–17b scenario (paid, completed, refunded, partial) → the report is clean |
| `TestRetryRefund` | failed → queued → processed |

## Pitfalls

- Admins act through the same `Transition` function. There are no status shortcuts.
- Reconciliation must never mutate the ledger.
