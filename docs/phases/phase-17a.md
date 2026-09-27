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
3. Commit the `pending` state first, then call Paystack `CreateRefund(transaction = payment.reference, amount)` **outside** the tx, then store `paystack_refund_id` in a new tx. If that store fails, log at Error with the Paystack id.
4. **Classify Paystack errors.** Never retry blindly: a lost response can hide a refund Paystack already created, and Paystack allows several partial refunds per transaction, so a blind retry **refunds twice**.
   - **Definite rejection** (`ErrRejected`, HTTP 4xx): refund `failed` (`paystack_rejected: <message>`), an audit event, an Error log. No retry; admins retry via 17b.
   - **Ambiguous** (transport error, timeout, 5xx, unreadable body): **keep `pending`**. Snooze and reconcile:
     - `ListRefunds(transactionReference)` (`GET /refund?transaction=…`; add it to the client)
     - if a refund with this amount exists, created after `refunds.created_at` and not linked to another row: store its id and let the webhook settle it
     - only if none exists after 15 minutes: back to `queued`, and retry
   - Test with a fake that creates the refund **and** returns a timeout: exactly one `CreateRefund` overall.

## Webhooks (registered in the 13a event registry)

- **`refund.processed`**. Match by `paystack_refund_id`. **Fallback** (id not stored yet): `data.transaction_reference` plus amount, **only if exactly one** in-flight refund matches. A multi-seller checkout shares one reference, so equal amounts are ambiguous: on two or more candidates, return a transient error so Paystack retries later. In one tx:
  - lock the order
  - require `data.amount == refund.amount_pesewas`, `data.currency == "GHS"` and `refunded + amount <= base`; otherwise outcome `rejected:amount_mismatch` / `rejected:exceeds_base`, an Error log and an audit event, with nothing settled
  - refund `processed`
  - post `order_refund` (ref = refund id, amount `r`)
  - order `refunded_pesewas += r`
  - `escrow_state = refunded` if `refunded == base`, else `partially_refunded`
  - notify the buyer
- **`refund.failed`:** refund `failed`, an **Error log (emitted by the orders handler itself)**, an audit event, and escrow stays held. Admins retry via 17b tooling. Use the webhook outcome constants (ADR-0021).
- **`refund.pending` / `refund.processing`:** set `pending` (informational).

Verify the event names and payload fields against Paystack's refund webhook docs, and record them in the ADR.

## Rules

- **No refund after release** (plan Done-when). It's enforced in the job **and** in the transition layer: a dispute can't be opened after completion, because DOMAIN §4 forbids it.
- The processing fee is non-refundable (DOMAIN §2.2). A full refund = the order `base`.
- The sum of refunds for an order must never exceed `base`. Check it under the order lock, both when **creating** a refund (Σ non-failed + amount ≤ base) and when **settling** one.
- The refund webhooks must be tested **through the real signed webhook endpoint**, using the same registration code as `cmd/api`, not only by calling the handlers directly.

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
