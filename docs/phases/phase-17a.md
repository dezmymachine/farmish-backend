# Phase 17a: Escrow release & refunds

**Depends on:** 16 · **Size:** medium

## Goal

Real implementations of the jobs Phase 16 left as log-only stubs:
- **`ReleaseEscrow`:** held money → the seller's payable plus platform commission.
- **`RefundOrder`:** returns buyer money through the Paystack Refund API, while the order's escrow is still held.

Both are idempotent, with ledger postings per DOMAIN §5.3.2–3, and Paystack refund webhooks drive the final state.

## Schema: `migrations/000014_refunds.up.sql`

```sql
CREATE TABLE refunds (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  order_id           uuid NOT NULL REFERENCES orders(id),
  amount_pesewas     bigint NOT NULL CHECK (amount_pesewas > 0),
  reason             text NOT NULL CHECK (reason IN ('seller_rejected','buyer_cancelled','seller_timeout',
                                                     'stock_unavailable','dispute_refund','dispute_partial')),
  status             text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','pending','processed','failed')),
  paystack_refund_id text UNIQUE,
  failure_reason     text,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX refunds_one_full_per_order ON refunds (order_id)
  WHERE reason IN ('seller_rejected','buyer_cancelled','seller_timeout','stock_unavailable','dispute_refund');
```

## Jobs

**`orders.release_escrow {OrderID}`** (unique by args). In one tx:
1. Lock the order. Require `status='completed'` and `escrow_state ∈ {held, partially_refunded}`, otherwise no-op, logged.
2. Remaining escrow `b' = base - refunded_pesewas`.
3. **Commission** on the remaining subtotal: follow **DOMAIN §4.1** exactly, the partial-refund rule where the delivery fee is refunded first.
4. Post `escrow_release` (ref = order id) with `b'`, `k'`.
5. `escrow_state = released`.
6. Enqueue `notify.sms order_completed_seller` if not already sent by the 16 transition.

**`orders.refund {RefundID}`** (the refund row is created **in the transition's tx** by the side effect, `status=queued`; this job does the external call):
1. Lock the refund row. If it isn't `queued`, no-op.
2. Lock the order. Require `escrow_state ∈ {held, refund_pending, partially_refunded}`. If the escrow is `released`: mark the refund `failed` with reason `refund_after_release`, log an Error, write an audit event, and **stop**. That's the **manual path**: an admin handles it outside the system.
3. Commit the `pending` state first, then call Paystack `CreateRefund(transaction = payment.reference, amount)` **outside** the tx, then store `paystack_refund_id` in a new tx.
4. Paystack error → back to `queued`, and return the error so River retries with backoff (max attempts 10).

## Webhooks (registered in the 13a event registry)

- **`refund.processed`**, matched by `paystack_refund_id` (or `data.transaction_reference` plus amount when the id isn't set yet). In one tx:
  - refund `processed`
  - post `order_refund` (ref = refund id, amount `r`)
  - order `refunded_pesewas += r`
  - `escrow_state = refunded` if `refunded == base`, else `partially_refunded`
  - notify the buyer
- **`refund.failed`:** refund `failed`, an Error log, an audit event, and escrow stays held. Admins retry via 17b tooling.
- **`refund.pending` / `refund.processing`:** set `pending` (informational).

Verify the event names and payload fields against Paystack's refund webhook docs, and record them in the ADR.

## Rules

- **No refund after release** (plan Done-when). It's enforced in the job **and** in the transition layer: a dispute can't be opened after completion, because DOMAIN §4 forbids it.
- The processing fee is non-refundable (DOMAIN §2.2). A full refund = the order `base`.
- The sum of refunds for an order must never exceed `base`. Check it under the order lock.

## Tests

| Test | Proves |
|---|---|
| `TestReleaseEscrow_PostsAndIsIdempotent` | A completed order → `seller_payable` = base − commission, `platform_commission` = commission (DOMAIN example numbers); running the job twice → one posting |
| `TestReleaseEscrow_WrongStateNoop` | Not completed, or already released → no posting |
| `TestRefund_FullFlow` | A seller reject → refund queued → job calls the fake Paystack → webhook `refund.processed` → ledger `order_refund`, `escrow_state=refunded`; a webhook replay → no second posting |
| `TestRefund_Idempotent` | The job runs twice → one Paystack call (the `queued` check); the unique index prevents two full refunds |
| `TestRefund_AfterReleaseGoesManual` | Release first, then a forced refund → refund `failed:refund_after_release`, audit row, no Paystack call |
| `TestRefund_ProviderErrorRetries` | Fake 500 → the job errors and the refund stays queued; the next attempt succeeds |
| `TestPartialRefund_CommissionOnRemaining` | Base 10,500 (subtotal 10,000 + delivery 500), partial refund 2,500 → remaining 8,000; commission on subtotal 8,000 = 400; seller gets 7,600 |
| `TestLedger_ReconcileAfterMixedFlows` | Several orders: released, refunded, partially refunded, held → escrow balance = Σ of the held remainder (DOMAIN §5.4) |

## Pitfalls

- Never call Paystack while holding row locks in an open tx.
- Refund amounts are **order-level**, but Paystack refunds against the **transaction**, which covers the whole checkout. Multiple partial refunds against one transaction are fine, as long as the total stays ≤ what was charged.
