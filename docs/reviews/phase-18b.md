# Review packet: Phase 18b Payout execution

## Summary

A daily batched payout per seller from `seller_payable` via Paystack
Transfers. `payouts.execute_all` (10:00 Africa/Accra) queues one payout per
payable seller — verified account, past cooldown, nothing in flight, balance
at or above the floor — posting `payout_initiated` in the same transaction.
`payouts.send` calls `InitiateTransfer` outside the tx, reconciling ambiguous
outcomes through `VerifyTransfer` before any resend. Transfer webhooks
settle (success posts `payout_succeeded` with the configured fee; failure or
reversal re-credits via `payout_failed`), a late success after failure goes
to an admin with no posting, and `payouts.reconcile` (11:00) settles
day-old pendings. Sellers see balance/history; admins list and retry
(a fresh row for the re-credited balance).

## Commits

`git log --oneline 3e4f85f..HEAD` output (plus the docs commit below):

```text
2e5a378 Phase 18b: scope webhook reference fallback to transfer events
cef4995 Phase 18b: payout endpoints, wiring and execution tests
fc2e817 Phase 18b: payout execution, send, settle and reconcile
a485d24 Phase 18b: payouts table, queries, config, transfer events and fake
```

## Done-when checklist

- [x] End-to-end completed → payable → payout → transfer.success, ledger balanced: `TestPayout_EndToEnd` (`internal/payouts/run_test.go`)
- [x] Failed transfer restores the balance: `TestPayout_FailedRestoresBalance` (plus reversed: `TestPayout_ReversedSettles`, definite 4xx: `TestPayout_SendDefiniteRejection`)
- [x] No payout during cooldown or below minimum: `TestPayout_NoPayoutDuringCooldown`, `TestPayout_BelowMinimumSkipped`
- [x] One in flight under concurrency: `TestPayout_OneInFlight` (5 goroutines → 1 row)
- [x] Needs-review skipped: `TestPayout_NeedsReviewAccountSkipped`
- [x] Ambiguous send adopts via verify, no duplicate: `TestPayout_SendRetryAdoptsExisting`
- [x] Stuck reconcile: `TestPayout_ReconcileStuck` (25h pending + verify success → success; still-pending → `payout.stuck` audit)
- [x] 10am schedule: `TestSchedule_Next10amAccra` (`internal/jobs/jobs_test.go`)
- [x] Never unmasked: response-body greps across balance/history/admin-list/retry endpoint tests (`internal/http/payouts_exec_test.go`)
- [x] Admin retry flow: `TestPayout_RetryFlow` (failed → fresh row + new reference; live/unknown refuse), `TestPayoutEndpoints_Retry` (200/409/404/403)
- [x] Webhook through the real signed route: `TestPayoutEndpoints_WebhookRouted` (settle + replay ignored)

## make ci

Last lines of `make ci` output, ending in `ci: all checks passed`:

```text
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

Full CI caught one real regression mid-phase: the new webhook reference fallback also accepted id-less charge bodies (caught by the pre-existing `TestWebhook_MalformedBodies`), so it is now scoped to `transfer.*` events (ADR-0030).

## Manual QA

Live API (Auth emulator, local Postgres/Redis; tokens masked). A full money flow needs live Paystack Transfers (disabled, plan §11), so QA proves the shapes and the webhook path; the money flow runs in tests against the scripted fake.

```console
$ curl -s .../v1/seller/balance -H "Authorization: Bearer <fresh>"
{"available":{"amount":0,"currency":"GHS"},"inEscrow":{"amount":0,"currency":"GHS"},"inFlight":{"amount":0,"currency":"GHS"},"paidOut":{"amount":0,"currency":"GHS"}}

$ curl -s .../v1/seller/payouts -H "Authorization: Bearer <fresh>"
{"items":[],"meta":{"limit":20,"page":1,"total":0}}

$ curl -s -X POST .../v1/webhooks/paystack -d '{"event":"transfer.success",...}'  # bad signature
{"error":{"code":"unauthorized","message":"Invalid signature"}}

$ curl -s -X POST .../v1/webhooks/paystack -H "x-paystack-signature: <valid>" -d '{"event":"transfer.success","data":{"id":987654,"reference":"PO-DOESNOTEXIST","amount":100,"currency":"GHS"}}'
# → 200, empty body; webhook_events: transfer.success | transfer.success:987654 | rejected:unknown_reference

$ curl -s -X POST .../v1/admin/payouts/00000000-0000-0000-0000-000000000000/retry -H "Authorization: Bearer <nonadmin>"
{"error":{"code":"forbidden","message":"You do not have access to this resource"}}

$ # after granting the admin role in the DB:
$ curl -s -X POST .../v1/admin/payouts/00000000-0000-0000-0000-000000000000/retry -H "Authorization: Bearer <admin>"
{"error":{"code":"not_found","message":"Payout not found"}}

$ curl -s ".../v1/admin/payouts?status=success" -H "Authorization: Bearer <admin>"
{"items":[],"meta":{"limit":20,"page":1,"total":0}}
```

A log sample held no account numbers, emails or tokens. Note: the shared dev DB sat at migration 15 with the code at 16, so the new tables missed until `make migrate-up` ran (version 16, clean) — test databases migrate fresh and never saw this.

## Files changed

`git diff --stat 3e4f85f..HEAD` plus the docs commit (`AGENTS.md`, `REDEVELOPMENT_PLAN.md`, `docs/adr/0030-*.md`, this file). New files and their purpose:

- `migrations/000016_payouts.{up,down}.sql`: the `payouts` table with the one-in-flight partial unique index.
- `db/queries/payouts.sql` (extended): insert/get/lock/match-by-reference, pending/success/failed transitions, seller/admin lists and counts, candidates, stuck pendings, held remainder, advisory lock.
- `internal/config/config.go` (+ tests, `.env.example`): `Payouts{MinPesewas: 2000, TransferFeePesewas: 0}` from `PAYOUT_MIN_PESEWAS` / `PAYSTACK_TRANSFER_FEE_PESEWAS`.
- `internal/money/money.go` (+ test): `FormatGHS` for SMS text without floats.
- `internal/payments/payments.go`: `transfer.success/failed/reversed` event names.
- `internal/payments/service.go`: reference fallback in webhook keying, scoped to `transfer.*`.
- `internal/payments/fake/paystack.go`: scriptable initiate/verify transfers with a transfer store.
- `internal/payouts/execute.go`: `Payout`/`Balance` types, `PO-` references, balance/history/admin-list reads.
- `internal/payouts/run.go`: execute-all/single, send with verify-before-retry, success/failure settlement, transfer webhooks, stuck reconciliation, admin retry.
- `internal/payouts/jobs.go`: the three workers plus the 10:00/11:00 `DailyAt` schedules; `jobs.AccraLocation` helper.
- `internal/payouts/run_test.go`: the full spec table at service level.
- `internal/notify/templates.go`: `payout_sent` / `payout_failed` SMS texts.
- `api/openapi.yaml` (+ generated): `SellerBalance`, `Payout`, `PayoutList`, `PayoutStatus` and the four endpoints.
- `internal/http/handlers/payouts_exec.go` (+ `PayoutStore` extension): balance/history/admin-list/retry handlers.
- `internal/http/payouts_exec_test.go`: contract-validated endpoint tests incl. the signed webhook route.
- `internal/jobs/jobs_test.go`: the 10:00 schedule test.
- `cmd/api/main.go`: ledger attach, limit configuration, transfer event registration, job registration.

## Schema changes

New migration 000016 (one table, two indexes, one partial unique index, `set_updated_at` trigger, working down). `migrations_test` passes inside `make ci`. New env vars `PAYOUT_MIN_PESEWAS` / `PAYSTACK_TRANSFER_FEE_PESEWAS` in config, `.env.example` and plan §7.

## API changes

Existing `payouts` tag; all responses contract-validated in tests:

- `GET /v1/seller/balance` (`getMyBalance`) → `SellerBalance`; 401/429
- `GET /v1/seller/payouts` (`listMyPayouts`, page/limit) → `PayoutList`; 400/401/429
- `GET /v1/admin/payouts` (`listAdminPayouts`, admin, `?status&sellerId&page&limit`) → `PayoutList`; 400/401/403/429
- `POST /v1/admin/payouts/{id}/retry` (`retryPayout`, admin, `sensitive`) → `Payout`; 401/403/404/409/429

`make api-lint` and `generate-check` pass; `api.gen.go` generated only.

## Deviations from the spec

All in ADR-0030; business rules unchanged. Notable: admin retry is a fresh execute (new row/reference) rather than reusing the failed row; pending payouts reconcile on every send poll (not only daily); the transfer-fee legs post the configured fee at settle time.

## Open questions / risks

- Live transfer webhook field names and the `basilisk` recipient type still await the Phase 22 test-mode payout (Transfers disabled). The fake follows the documented shapes.
- `ledger.CheckoutPaid` with an actual fee of exactly 0 emits a zero leg that `Post` rejects — moved to the §10 backlog; pre-existing, found via fixture work.

## Backlog additions

- `ledger.CheckoutPaid` zero-fee edge (see above).
