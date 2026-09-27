# ADR-0025: A late charge.success settles an abandoned payment

- **Status:** Accepted (Phase 15b).
- **Date:** 2026-09-27

## Context

The checkout expiry sweep marks its payment `abandoned`: the buyer has not
paid, so the verify fallback should stop asking Paystack about it. But Paystack
checkout sessions outlive our 30-minute window, and a buyer can pay at minute
29 and have the webhook arrive at minute 31. Phase 15b's spec requires that
late payment to be handled — orders revive when the stock could be re-reserved,
and cancel with a refund when it could not.

The payments layer refused to settle anything but a `pending` payment, so a
charge.success arriving after the sweep would have been recorded as `ignored`
and the money would have sat in Paystack with no order and no escrow entry.

## Decision

`SettlePaymentSuccess` settles from `pending` **or** `abandoned`. Abandoned
meant "we no longer expect a charge", and the webhook is the fact that proves
the guess wrong. Every other state still refuses: `success` is already done,
`failed` needs a human, and a payment that never existed stays not-found.

The checkout purpose handler is the layer that knows about the expiry race; it
locks the checkout and applies the DOMAIN §4 recovery rows (expired → paid, or
expired → cancelled with a full-refund trail).

## Consequences

- A late webhook cannot strand money: it settles the payment, posts
  `checkout_paid`, and the orders either revive or enter refund_pending.
- The `payments.succeeded` job enqueues in the same transaction as the
  settlement, exactly as the normal path does.
- A payment marked `failed` (a Paystack refusal) can still not be revived by a
  webhook; that asymmetry is deliberate — a refusal is authoritative, an
  abandonment is a guess.
