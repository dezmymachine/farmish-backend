# Review packet: Phase 17a Escrow release & refunds

> **The implementer's packet is missing.** The phase is uncommitted work in progress on top of `0b955db` (Phase 16). This file holds the review only. The implementer must add the packet sections (REVIEW_PROTOCOL template) when resubmitting.

## Review: 2026-09-27, reviewer: Claude Opus 5.5

**Verdict: CHANGES REQUESTED** (3 blockers, 3 majors)

### make ci

Run by the reviewer against the working tree: **passed**, with lint `0 issues`, all packages ok, and smoke `graceful shutdown ok` → `ci: all checks passed`. The gate is green, but it doesn't exercise the failure modes below.

### Done-when / spec tests

| Item | Status | Evidence |
|---|---|---|
| Release is idempotent | ✅ | `TestReleaseEscrow_PostsAndIsIdempotent` (refunds_test.go:163): the DOMAIN numbers 12,345 → 617 commission; a second run is a no-op, one `escrow_release` transaction |
| Release in the wrong state is a no-op | ✅ | `TestReleaseEscrow_WrongStateNoop` (:206) |
| Refund is idempotent | ⚠️ | `TestRefund_Idempotent` (:332) covers the queued check and the partial unique index, but **not** the ambiguous-failure retry (Blocker 1) |
| No refund after release → manual path | ✅ | `TestRefund_AfterReleaseGoesManual` (:371): no Paystack call, failed/`refund_after_release`, audit row |
| Full flow + webhook replay | ✅ | `TestRefund_FullFlow` (:272), but it calls the handler directly, not through the signed webhook (Major 5) |
| Provider error retries | ❌ | `TestRefund_ProviderErrorRetries` (:410) **asserts the double-refund behaviour as correct**: a `DeadlineExceeded` leads to a second `CreateRefund` |
| Partial-refund commission (DOMAIN §4.1) | ✅ | `TestPartialRefund_CommissionOnRemaining` (:232): 8,000 / 400 / 7,600, pure and posted |
| Ledger reconciles after mixed flows | ✅ | `TestLedger_ReconcileAfterMixedFlows` (:449) |
| `refund.failed` / `refund.pending` handled | ❌ | No tests at all |
| Refunds never exceed the order base | ❌ | Not implemented, not tested (Blocker 3) |

### Findings (most severe first)

- **[BLOCKER] 1. A timed-out refund call is retried, so buyers can be refunded twice**
  - **Where:** `internal/orders/refunds.go:185-195`; the classification is in `internal/payments/paystack.go:423-441`.
  - **Problem:** `ProcessRefund` sets the refund back to `queued` and returns an error on **any** `CreateRefund` error, and River retries. Transport errors, timeouts, 5xx responses and unreadable bodies are all *ambiguous*: Paystack may already have created the refund.
  - **Failure scenario:** `POST /refund` succeeds at Paystack but the response is lost (a timeout or connection reset) → the refund goes back to `queued` → the River retry sends a second `POST /refund` for the same amount. Paystack allows several partial refunds per transaction, so the buyer is refunded twice. Only one `order_refund` is ever posted, so the ledger and the Paystack balance diverge.
  - **Also:** a definite rejection (4xx, or `status:false` → `ErrRejected`) is retried 10 times and never marked failed.
  - **Required fix:**
    - **Definite rejections** (`ErrRejected`, HTTP 4xx): mark the refund `failed` (`paystack_rejected: <message>`), write an audit event and an Error log. No retry.
    - **Ambiguous errors:** **keep `pending`**. Never go back to `queued` automatically. Before any retry, reconcile: add `ListRefunds(ctx, transactionReference)` (`GET /refund?transaction=…`) to the Paystack client. If Paystack has a refund for this transaction with this amount, created after `refunds.created_at` and not linked to another row, store its id and let the webhook or reconcile settle it. Only if none exists after a grace period (e.g. 15 min, via a snoozed job) does the refund go back to `queued`.
  - **Note:** this defect originates in the phase spec ("Paystack error → back to queued, and return the error so River retries"), which the implementer followed. The reviewer will correct `docs/phases/phase-17a.md` accordingly.
  - **Tests to add:**
    - `TestRefund_TimeoutNeverDoubleRefunds`: the fake records the refund and returns a timeout; the retry adopts the existing Paystack refund; `CreateRefund` is called exactly once.
    - `TestRefund_RejectedMarksFailed`: `ErrRejected` → failed + audit, no retry.
    - Rewrite `TestRefund_ProviderErrorRetries` to cover only the "no refund exists at Paystack" case.

- **[BLOCKER] 2. The webhook fallback match can settle the wrong buyer's refund**
  - **Where:** `db/queries/refunds.sql` `FindQueuedOrPendingRefundByReference`; `internal/orders/refunds.go:215-232`.
  - **Problem:** before `paystack_refund_id` is stored, a webhook is matched by `(payment reference, amount)`, taking the **oldest** row. A multi-seller checkout has **one** payment reference for several orders, so two orders with equal bases are indistinguishable.
  - **Failure scenario:** buyer checks out with sellers A and B, both orders GHS 50. Both are cancelled, so two refunds of 5000 exist on one reference. Refund B's `refund.processed` arrives before its id is stored and matches refund **A**, which is marked processed. Refund A later **fails** at Paystack, but it's already `processed`, so `OnRefundFailed` ignores it. Refund B stays `pending` forever. Result: the ledger shows A refunded while Paystack never refunded it.
  - **Required fix:** use the fallback only when **exactly one** candidate matches (change the query to `:many`). If there are two or more, return a transient error so the webhook is rolled back and Paystack retries after the job has stored the ids. Log a warning.
  - **Test to add:** `TestRefundWebhook_AmbiguousFallbackDeferred`: two same-amount refunds on one reference, webhook before ids → an error, nothing settled. After the ids are stored, each webhook settles its own refund.

- **[BLOCKER] 3. `refund.processed` doesn't verify the amount or currency, and refunds can exceed the order base**
  - **Where:** `internal/orders/refunds.go:250-276`.
  - **Problem:**
    - When matched by id, the handler posts `refund.amount_pesewas` without comparing it to `data.amount` or `data.currency`. The charge webhook does this check (Phase 13a).
    - Nothing enforces the spec rule "the sum of refunds for an order must never exceed `base`; check under the order lock". `AddOrderRefundedPesewas` increments unconditionally, and `CreateRefund` doesn't check the running total.
  - **Failure scenario:** Paystack processes a different amount than requested (a partial refund, or a manual dashboard adjustment) → we post our amount, not theirs. With Phase 17b's partial refunds, a full refund plus a partial on the same order pushes `refunded_pesewas` above `base`, and the order's escrow goes negative.
  - **Required fix:**
    - `OnRefundProcessed` locks the order (`GetOrderForUpdate`). It requires `d.Amount == refund.AmountPesewas`, `d.Currency == "GHS"` and `order.RefundedPesewas + amount <= order.BasePesewas`. Otherwise: outcome `rejected:amount_mismatch` / `rejected:exceeds_base`, an Error log, an audit event, and nothing settled.
    - `CreateRefund` checks Σ(non-failed refunds) + amount ≤ base under the order lock.
    - Optionally, a SQL `CHECK (refunded_pesewas <= base_pesewas)` on `orders` in this migration, as defence in depth.
  - **Tests to add:** `TestRefundWebhook_AmountMismatchRejected`, `TestRefundWebhook_CurrencyMismatchRejected`, `TestCreateRefund_CannotExceedBase`.

- **[MAJOR] 4. `refund.failed` doesn't log at Error level**
  - **Where:** `internal/orders/refunds.go:288-319`.
  - **Problem:** the doc comment says "an Error-level log the caller adds", but `payments.HandleWebhook` adds none. The spec requires it: it's the alert hook for Phase 21.
  - **Also:** the handlers return `"failed"` and `"pending"` as webhook outcomes, outside the outcome convention (ADR-0021).
  - **Required fix:** give `orders.Service` a logger, and log at Error with the refund id, order id and amount. Return the documented outcome constants.
  - **Tests to add:** `TestRefundWebhook_FailedAuditsLogsAndKeepsEscrow` (capture the logger; escrow stays `refund_pending`), `TestRefundWebhook_PendingIsInformational`.

- **[MAJOR] 5. The refund webhooks are never tested through the real endpoint**
  - **Problem:** `deliverRefundWebhook` calls `OnRefundProcessed` directly. Nothing proves the four event names are registered in `cmd/api` and routed by `HandleWebhook` (signature verification, `webhook_events` dedupe).
  - **Failure scenario:** a typo in an event constant, or a missing `RegisterEvent`, means refunds never settle in production while every test stays green.
  - **Test to add:** `TestWebhook_RefundEventsRouted` at the payments or router level. Send a signed `refund.processed`, `refund.failed` and `refund.pending` through the real handler, with the same registration as `cmd/api` (extract a `registerRefundEvents(payments, orders)` helper used by both). Assert the outcomes and that a replay is deduped.

- **[MAJOR] 6. Phase process is incomplete**
  - Nothing is committed.
  - No review packet exists.
  - The plan checkbox and the §8/§9 rows are missing.
  - The code cites **ADR-0027** and "DOMAIN's refund webhook decisions": **neither exists**.
  - The spec's "verify event names/payload against Paystack's refund webhook docs and record in the ADR" wasn't done.
  - `AGENTS.md` "Current state" isn't updated.
  - No manual QA output was recorded.
  - A stray `tmp_qa17a_stub.py` sits in the repo root.
  - **Required fix:** write ADR-0027 (the verified refund event names and payload fields, the fallback-match rule, and the ambiguous-failure handling from Blocker 1). Update DOMAIN only if the owner approves a rule change. Move the stub to `scripts/qa/paystack-refund-stub.py`, or delete it. Commit in the usual steps, then write the packet.

- **[MINOR] 7. `CreateRefund` silently skips the enqueue when `s.jobs` is nil**
  - **Where:** refunds.go:63-65.
  - **Problem:** the refund row is created but never processed. That contradicts the "money must never be dropped" rule applied to `ledger` and `paystack`.
  - **Fix:** return an error when jobs are nil, as for the ledger. Tests that need no enqueue can attach a job client or assert the error.

- **[MINOR] 8. Losing the Paystack refund id is silent**
  - **Where:** refunds.go:196-199.
  - **Problem:** if storing `paystack_refund_id` fails after Paystack accepted, the error goes to River, and the retry no-ops because the status is `pending`. The id is lost, and only the (now single-candidate) fallback can match the webhook.
  - **Fix:** log at Error with the Paystack refund id so it can be reconciled.

### Verified OK

- **Migration** `000014_refunds`: matches the spec (CHECKs, the partial unique index for full refunds, the trigger). up→down→up passes (`migrations_test` in `make ci`).
- **`RemainingRelease`** implements DOMAIN §4.1 exactly. The example is covered in both pure and posted tests.
- **`ReleaseEscrow`:**
  - it locks the order and re-checks the status and escrow state
  - it uses the `ledger.EscrowRelease` template, idempotent through `UNIQUE(kind, reference)`
  - it sets `released` **only after posting**, which fixes the Phase 16 shortcut that marked escrow released without any ledger entry (`actions.go`, `jobs.go` `AutoCompleteDelivered`): a good catch
- **Refund rows** are created inside the transition's tx, with the reason derived from the actor. The checkout-expiry path records `stock_unavailable`. The partial unique index blocks two full refunds.
- **Paystack is called with no transaction open.** `pending` is committed before the call.
- **No refund after release:** the job-level guard, audit event and the absence of any Paystack call are all tested.
- **Money types:** no floats; all amounts `int64` pesewas; the webhook replay leaves one `order_refund` posting.
- **Wiring:** the job workers, ledger, Paystack client and event registrations are attached in `cmd/api` before the workers start.

### To resubmit

Fix Blockers 1–3 and Majors 4–6 with the listed tests. Keep `make ci` green, commit in steps, write the packet, then request re-review. The reviewer will verify every finding above is resolved.
