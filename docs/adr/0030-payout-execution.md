# ADR-0030: Payout execution, send reconciliation and webhook keying

- **Status:** Accepted
- **Date:** 2026-09-27
- **Phase:** 18b

## Context

Payouts move real money off the platform through Paystack Transfers. Every
step reuses a proven 17a shape, with three new decisions below.

## Decisions

1. **Ambiguous sends reconcile before resending, keyed by our reference.**
   After a timeout/5xx the job calls `VerifyTransfer(reference)`: a held
   transfer is adopted (pending, then polled), a proven-absent one is resent
   under the same unique reference, which Paystack rejects as a duplicate if
   it actually exists. A blind retry can never pay twice. Pending payouts
   reconcile on every send poll too, so a missed webhook stalls at most one
   poll interval before the daily 11:00 check catches it regardless.
2. **A late success after failure goes to an admin, never to the ledger.**
   The failure already re-credited the payable; auto-posting the success
   would pay twice. The webhook logs an Error and writes
   `payout.late_success` with nothing posted.
3. **Admin retry is a fresh execute, not row reuse.** It queues a new payout
   row with a new reference for the current (re-credited) balance, so the
   failed row keeps its history and idempotency keys never collide.
4. **The webhook reference fallback applies to transfer events only.**
   `transfer.*` without an id keys on `data.reference`; charge and refund
   bodies without an id stay malformed (a reference-only charge must never
   reach settlement). Caught by the pre-existing
   `TestWebhook_MalformedBodies` during this phase.
5. **Money as SMS text uses `money.FormatGHS`** (integer division and
   remainder, no floats): "GHS 123.45".
6. **Live transfer payload unverified.** Transfers are disabled on the
   Paystack account (plan §11), so the webhook field names (`reference`,
   `amount`, `currency`) and the `basilisk` recipient type await a live
   test-mode check. **Owner action before go-live (Phase 22):** run one
   test-mode payout end to end and record the observed payloads here.
