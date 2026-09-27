# Review packet: Phase 17a Escrow release & refunds

> The first submission (uncommitted, reviewed below as **CHANGES REQUESTED**) was reworked. **The fixes were implemented by Claude Opus 5.5 at the owner's request**, the same model that wrote the review. An independent re-review (a separate reviewer session using `docs/REVIEW_PROTOCOL.md`, base `0b955db`) is recommended before this is treated as accepted.

## Summary

- **Escrow release:** posts each completed order's held remainder, with commission on the remaining subtotal (DOMAIN §4.1). It marks escrow `released` only after posting, replacing Phase 16's premature flag.
- **Refund creation:** refunds are created inside the cancelling transition's tx. A refund that would exceed the order's base is refused under the order lock.
- **The refund job:** `orders.refund` drives each refund to a final state through Paystack.
  - A definite rejection fails the refund.
  - An ambiguous failure stays pending and is reconciled against Paystack's refund list. It's resent only when Paystack provably holds nothing after 15 minutes.
  - Pending refunds are polled until settled, so settlement never depends on webhooks.
- **Webhooks and settlement:** the webhooks settle quickly through the same checks: the amount, the currency, the base, and a single-candidate fallback match.
- **Found along the way:** two further live-breaking bugs.
  - Create Refund sent `reference` instead of Paystack's `transaction`.
  - Refund webhooks may carry no top-level `id`, and were rejected as malformed.

## Commits

```
8ad3a39 Phase 17a: escrow release and refund lifecycle with reconciliation
65daede Phase 17a: Paystack refund API fixes, fetch/list refunds, error classes
3b3dcee Phase 17a: refunds schema and queries
256c676 Review: Phase 17a changes requested; fix refund retry rule in spec
```

Each code commit was checked out alone in a temporary worktree and passes `go vet ./...`. `make ci` was run on the final tree (below).

## Done-when checklist

- [x] **Release and refund are each idempotent:**
  - `TestReleaseEscrow_PostsAndIsIdempotent`, `TestRefund_Idempotent`, `TestRefund_FullFlow` (replay)
  - `TestRefund_TimeoutNeverDoubleRefunds`
  - `TestWebhook_RefundEventsRouted` (the webhook_events dedupe)
  - Files: `internal/orders/refunds_test.go`, `internal/http/refunds_test.go`
- [x] **No refund after release (the manual path):** `TestRefund_AfterReleaseGoesManual`
- [x] **Partial-refund commission follows DOMAIN §4.1:** `TestPartialRefund_CommissionOnRemaining`
- [x] **Ledger reconciles after mixed flows:** `TestLedger_ReconcileAfterMixedFlows`
- [x] **Spec tests:** `TestReleaseEscrow_WrongStateNoop`, `TestRefund_ProviderErrorRetries` (rewritten for the ADR-0027 rule)
- [x] **Review-required tests:** see the resolution table below.

## make ci

Ends with `0 issues.`, 31 packages `ok`, `smoke: … graceful shutdown ok`, `ci: all checks passed` (exit 0). The full output of the final run is recorded at the end of this file.

## Manual QA

The real HTTP Paystack client was run against `scripts/qa/paystack-refund-stub.py` (a local `/refund` endpoint that logs requests):

```
result={RefundID:1 Status:pending} err=<nil>
stub: POST /refund body={"amount":12345,"transaction":"FMS-QA-REF"}
unreachable: definite=false err=paystack: unavailable: POST /refund: Post "http://127.0.0.1:1/refund": dial tcp 127.0.0.1:1: connect: connection refused
```

This shows the corrected wire field (`transaction`), and that an unreachable Paystack is classified as ambiguous (no resend without reconciliation).

**Mutation check:** each fix was reverted in turn, and the test written for it failed:

| Fix reverted | Test that failed |
|---|---|
| Blind requeue on an ambiguous failure | `TestRefund_TimeoutNeverDoubleRefunds` |
| Oldest-candidate fallback | `TestRefundWebhook_AmbiguousFallbackDeferred` |
| Amount check removed | `TestRefundWebhook_AmountMismatchRejected` |
| Error log downgraded | `TestRefundWebhook_FailedAuditsLogsAndKeepsEscrow` |

## Files changed

The diff from `0b955db` to HEAD, excluding generated `internal/db`, is 26 files (+2526/−101). New files:
- `internal/orders/refunds.go`: release, refund lifecycle, reconciliation, settlement and webhook handlers
- `internal/orders/refunds_test.go`: 19 tests
- `internal/http/refunds_test.go`: signed-endpoint routing
- `migrations/000014_refunds.{up,down}.sql`, `db/queries/refunds.sql`
- `docs/adr/0027-refund-lifecycle-and-reconciliation.md`
- `scripts/qa/paystack-refund-stub.py` (moved from the repo root)

## Schema changes

`000014_refunds` adds:
- the `refunds` table (with `attempted_at`)
- the partial unique index for full refunds
- the `orders_refunded_within_base` CHECK

up→down→up passes (`migrations_test` in `make ci`).

## API changes

None. Refunds have no endpoints in 17a. Four webhook event handlers are registered (`refund.processed|failed|pending|processing`).

## Deviations from the spec

All recorded in ADR-0027:
- Reconciliation before any resend. The spec had said "retry on any error", now corrected in the spec.
- The job doubles as the settlement path, by polling.
- Webhook dedupe falls back to `refund_reference`.
- Refund handlers report `processed` / `ignored` outcomes per ADR-0021.

## Open questions / risks

- **Refund webhook payload shape:** Paystack doesn't publish a webhook schema. Confirm the payload from a live test-mode refund before go-live (ADR-0027 owner action). Settlement doesn't depend on it, because the job polls.
- **Refunds list lookup:** List Refunds has no transaction filter, so reconciliation pages by date (≤ 10 × 100). That's fine at the expected volume.

## Backlog additions

None.

---

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

## Resolution of the review findings

| Finding | Fix | Proving test(s) |
|---|---|---|
| **B1** double refund on ambiguous failure | `IsDefiniteRejection`; ambiguous → stay `pending`; reconcile with `ListRefunds` (adopt a unique unclaimed match); resend only after 15 min with nothing at Paystack; `FetchRefund` polling | `TestRefund_TimeoutNeverDoubleRefunds`, `TestRefund_RejectedMarksFailed`, `TestRefund_ProviderErrorRetries` (rewritten), `TestRefund_ReconcileFetchSettlesOrFails`, `TestRefund_GiveUpAfterWindow`, `TestIsDefiniteRejection` |
| **B2** ambiguous fallback match | Fallback acts only on exactly one candidate, else `ErrAmbiguousRefundMatch` (500 → Paystack retries) | `TestRefundWebhook_AmbiguousFallbackDeferred`, `TestRefundWebhook_FallbackMatchSingleCandidate` |
| **B3** no amount/currency/base checks | Shared `settleRefund` under the order lock: amount, GHS and `refunded + amount ≤ base`, else `rejected:*` + Error + audit; `CreateRefund` refuses an over-base refund; DB CHECK | `TestRefundWebhook_AmountMismatchRejected` (amount + currency), `TestCreateRefund_CannotExceedBase` |
| **M4** no Error log on `refund.failed`; off-convention outcomes | `failRefund` logs Error + audit; handlers return ADR-0021 outcomes; malformed → `ErrMalformedEvent` | `TestRefundWebhook_FailedAuditsLogsAndKeepsEscrow`, `TestRefundWebhook_PendingIsInformational` |
| **M5** webhooks never routed through the endpoint | `orders.RegisterRefundEvents`, used by `cmd/api` and the test fixture | `TestWebhook_RefundEventsRouted` (signed, all four events, no-id payload, replay dedupe) |
| **M6** process incomplete | Commits in steps, this packet, ADR-0027, plan §6/§8/§9, `AGENTS.md` current state, stub moved to `scripts/qa/` | n/a |
| **m7** silent skip without a job client | `CreateRefund` errors without one | `TestCreateRefund_RequiresJobClient` |
| **m8** lost Paystack id is silent | Error log with the Paystack id; reconciliation re-finds it | covered by `TestRefund_TimeoutNeverDoubleRefunds` (adoption path) |
| *(new)* Create Refund field `reference` | Sends `transaction` (Paystack OpenAPI spec) | `TestPaystackClient_CreateRefund`, manual QA wire capture |
| *(new)* refund webhooks without `data.id` rejected | Dedupe key falls back to `refund_reference` | `TestWebhook_RefundEventsRouted` (`refund.pending:ref:RF-SECOND`) |

## make ci on HEAD d5cea0d (final)

```
ok  	github.com/dezmymachine/farmish-backend/cmd/api
ok  	github.com/dezmymachine/farmish-backend/internal/audit
ok  	github.com/dezmymachine/farmish-backend/internal/auth
ok  	github.com/dezmymachine/farmish-backend/internal/catalog
ok  	github.com/dezmymachine/farmish-backend/internal/checkout
ok  	github.com/dezmymachine/farmish-backend/internal/config
ok  	github.com/dezmymachine/farmish-backend/internal/crypto
ok  	github.com/dezmymachine/farmish-backend/internal/database
ok  	github.com/dezmymachine/farmish-backend/internal/db
ok  	github.com/dezmymachine/farmish-backend/internal/delivery
ok  	github.com/dezmymachine/farmish-backend/internal/geo
ok  	github.com/dezmymachine/farmish-backend/internal/http
ok  	github.com/dezmymachine/farmish-backend/internal/http/handlers
ok  	github.com/dezmymachine/farmish-backend/internal/http/middleware
ok  	github.com/dezmymachine/farmish-backend/internal/jobs
ok  	github.com/dezmymachine/farmish-backend/internal/ledger
ok  	github.com/dezmymachine/farmish-backend/internal/listings
ok  	github.com/dezmymachine/farmish-backend/internal/media
ok  	github.com/dezmymachine/farmish-backend/internal/money
ok  	github.com/dezmymachine/farmish-backend/internal/notify
ok  	github.com/dezmymachine/farmish-backend/internal/orders
ok  	github.com/dezmymachine/farmish-backend/internal/payments
ok  	github.com/dezmymachine/farmish-backend/internal/promotions
ok  	github.com/dezmymachine/farmish-backend/internal/ratelimit
ok  	github.com/dezmymachine/farmish-backend/internal/sellers
ok  	github.com/dezmymachine/farmish-backend/internal/text
ok  	github.com/dezmymachine/farmish-backend/internal/turnstile
ok  	github.com/dezmymachine/farmish-backend/internal/users
ok  	github.com/dezmymachine/farmish-backend/internal/validation
ok  	github.com/dezmymachine/farmish-backend/migrations
ok  	github.com/dezmymachine/farmish-backend/pkg/logger
0 issues.
Your code is affected by 0 vulnerabilities.
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```
