# Review packet: Phase 17b Disputes & ledger reconciliation

## Summary

Admins now resolve disputed orders through the same `orders.Transition` every other move uses: `refund_buyer` (disputed → refunded with a full `dispute_refund`), `release_seller` (disputed → completed with the held escrow released), or `partial` (disputed → completed with a `dispute_partial` refund first, then the release once the refund settles). Every resolution writes the dispute row, a `dispute.resolve` audit event and two SMS jobs in the same transaction. Failed refunds can be retried by admins, one order view shows both parties' contacts plus its ledger entries, and a daily 03:00 Africa/Accra `ledger.reconcile` job detects (never fixes) DOMAIN §5.4 violations with an Error log and an audit event.

## Commits

`git log --oneline c7fc6a8..HEAD` output (plus the docs commit below):

```text
6511840 Phase 17b: dispute, reconcile and admin endpoint tests
42ae3af Phase 17b: disputes, ledger reconciliation and admin endpoints
```

## Done-when checklist

Plan §6 item: the three dispute outcomes work with audit, the reconcile job detects violations, and ledger totals reconcile.

- [x] Three dispute outcomes with audit: `TestResolve_RefundBuyer`, `TestResolve_ReleaseSeller`, `TestResolve_PartialThenRelease` (`internal/orders/disputes_test.go`)
- [x] Release snoozes while the refund is pending: service returns `ErrReleaseDeferred`, worker returns `JobSnooze(10m)` (`TestResolve_PartialThenRelease`)
- [x] Validation: partial amount ≥ remaining or ≤ 0 → 400, resolving twice → 409, non-admin → 403 (`TestResolve_Validation`, `TestAdminDisputes_Validation` in `internal/http/disputes_admin_test.go`)
- [x] Reconcile detects violations: broken state via raw SQL → report lists each check + Error log + audit (`TestReconcile_DetectsViolations` in `internal/ledger/reconcile_test.go`)
- [x] Ledger totals reconcile: escrow balance equals minus the held remainder; clean after the full 15b–17b scenario (`TestReconcile_CleanAfterFullScenario`)
- [x] Retry: failed → queued → processed; live and post-release retries refused (`TestRetryRefund`, `TestAdminRefund_Retry`)

## make ci

Last lines of `make ci` output, ending in `ci: all checks passed`:

```text
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

Full gate (tidy, fmt, api-lint, generate-check, sqlc-check, lint, race tests, vuln, docker smoke) green. The gate caught two real issues mid-phase: the admin retry enqueue swallowed as a River-unique duplicate (fixed to a non-unique insert, pinned by a `river_job` row-count assertion in `TestRetryRefund`) and the validation endpoint test tripping the `sensitive` rate limit (fixture now raises it like the fulfilment fixture).

## Manual QA

Phase 17b's spec ships no QA script, so the checks below ran against the live API (`FIREBASE_PROJECT_ID=demo-farmish`, Auth emulator, local Postgres/Redis). Tokens masked.

```console
$ curl -s http://localhost:8080/v1/me -H "Authorization: Bearer <buyer>"
{"createdAt":"...","email":"liveqa buyer@example.com",...,"role":"user",...}

$ curl -s "http://localhost:8080/v1/admin/disputes?status=open" -H "Authorization: Bearer <buyer>"
{"error":{"code":"forbidden","message":"You do not have access to this resource"}}

$ # after make grant-admin (DB role; the Firebase claim mirror failed on the
$ # emulator uid lookup, which is non-blocking: users.role is authoritative)
$ curl -s "http://localhost:8080/v1/admin/disputes?status=open" -H "Authorization: Bearer <admin>"
{"items":[],"meta":{"limit":20,"page":1,"total":0}}

$ curl -s http://localhost:8080/v1/admin/disputes/00000000-0000-0000-0000-000000000000 -H "Authorization: Bearer <admin>"
{"error":{"code":"not_found","message":"Dispute not found"}}

$ curl -s -X POST http://localhost:8080/v1/admin/refunds/00000000-0000-0000-0000-000000000000/retry -H "Authorization: Bearer <admin>"
{"error":{"code":"not_found","message":"Refund not found"}}

$ curl -s http://localhost:8080/v1/admin/orders/00000000-0000-0000-0000-000000000000 -H "Authorization: Bearer <admin>"
{"error":{"code":"not_found","message":"Order not found"}}

$ curl -s -X POST .../disputes/00000000-0000-0000-0000-000000000000/resolve -d '{"outcome":"partial","note":"x"}' -H "Authorization: Bearer <admin>"
{"error":{"code":"validation_failed","details":[{"field":"refundAmount","location":"body","message":"is required for a partial resolution"}],"message":"Request validation failed"}}

$ curl -s "http://localhost:8080/v1/admin/disputes?status=bogus" -H "Authorization: Bearer <admin>"
{"error":{"code":"validation_failed","details":[{"field":"status","location":"query","message":"value must be one of 'open', 'resolved'"}],"message":"Request validation failed"}}
```

Log sample held no emails, phones or tokens. A full live dispute walk (paid → disputed → resolved) was left to the endpoint tests: the local checkout needs live Paystack keys the dev machine doesn't have.

Environment note: the shared dev database was missing `refunds.attempted_at` (it applied migration 000014 from a stale tree state at some point; `migrate-version` says 14, `migrate-up` says no change). The column was added to the dev DB only (`ALTER TABLE refunds ADD COLUMN attempted_at timestamptz`). Test databases migrate fresh and are unaffected; `migrations_test` (up → down → up) passes.

## Files changed

`git diff --stat c7fc6a8..HEAD` (plus the docs commit: `AGENTS.md`, `REDEVELOPMENT_PLAN.md`, `docs/adr/0028-*.md`, this file):

```text
api/openapi.yaml                         |  320 ++++++
cmd/api/main.go                          |    1 +
db/queries/ledger.sql                    |   48 +
db/queries/orders.sql                    |   26 +
db/queries/refunds.sql                   |   17 +
internal/db/ledger.sql.go                |  194 ++++
internal/db/orders.sql.go                |  157 +++
internal/db/querier.go                   |   32 +
internal/db/refunds.sql.go               |   63 ++
internal/http/api/api.gen.go             | 1786 ++++++++++++++++++++++++------
internal/http/disputes_admin_test.go     |  538 +++++++++
internal/http/handlers/disputes_admin.go |  211 ++++
internal/http/handlers/orders_actions.go |   37 +
internal/jobs/jobs.go                    |   26 +
internal/jobs/jobs_test.go               |   20 +
internal/ledger/jobs.go                  |   86 ++
internal/ledger/reconcile.go             |  121 ++
internal/ledger/reconcile_test.go        |  284 +++++
internal/notify/templates.go             |    6 +
internal/orders/disputes.go              |  408 +++++++
internal/orders/disputes_test.go         |  454 ++++++++
internal/orders/jobs.go                  |    8 +-
internal/orders/orders.go                |   32 +
internal/orders/reads.go                 |   33 +-
internal/orders/refunds.go               |   22 +-
internal/orders/river_timers_test.go     |    9 +
```

New files and their purpose:

- `internal/orders/disputes.go`: dispute list/get/resolve/retry plus the admin order view (contacts, ledger entries).
- `internal/orders/disputes_test.go`: the three outcomes, snooze, validation, retry, list/get, post-release refusal.
- `internal/ledger/reconcile.go`: `Reconcile` over the five DOMAIN §5.4 checks plus the `Report` type.
- `internal/ledger/jobs.go`: `ledger.reconcile` worker (Error log + `ledger.reconcile` audit, nil either way) and its daily registration.
- `internal/ledger/reconcile_test.go`: violation detection (raw-SQL broken state), the clean full scenario, worker log/audit behaviour.
- `internal/http/handlers/disputes_admin.go`: the five admin handlers and their contract mappers.
- `internal/http/disputes_admin_test.go`: contract-validated endpoint tests (happy paths, 400/403/404/409, 401).

## Schema changes

None: no new migrations. New sqlc queries only (disputes list/get/resolve, refund retry/in-flight count, reconcile sums and missing-posting finds, per-order ledger entries). `migrations_test` (up → down → up) passes inside `make ci`.

## API changes

New `admin` tag; all five operations carry `x-farmish-role: admin` (resolve and retry additionally `x-farmish-rate-limit: sensitive`):

- `GET /v1/admin/disputes` (`listAdminDisputes`, `?status=open|resolved&page&limit`) → `DisputeAdminList`
- `GET /v1/admin/disputes/{id}` (`getAdminDispute`) → `DisputeAdmin` with the full `OrderDetail` and events
- `POST /v1/admin/disputes/{id}/resolve` (`resolveDispute`, `{outcome, refundAmount?, note}`) → `DisputeAdmin`
- `GET /v1/admin/orders/{id}` (`getAdminOrder`) → `AdminOrderDetail` (order + both contacts + ledger entries)
- `POST /v1/admin/refunds/{id}/retry` (`retryRefund`) → `Refund`

`make api-lint` and `generate-check` pass; `internal/http/api/api.gen.go` is generated, never hand-edited.

## Deviations from the spec

All recorded in ADR-0028; none change the business rules:

1. Release deferral counts in-flight refunds under the row lock instead of keying on escrow state alone (a partial resolution keeps escrow `held` until settlement).
2. The retry enqueue drops job uniqueness (a unique insert is swallowed by the first attempt's job row).
3. The daily 03:00 run uses a custom `jobs.DailyAt` River schedule (River v0.47 has no cron helper).
4. `DisputeAdmin` embeds the seller-shaped `OrderDetail`; `AdminOrderDetail` wraps it with contacts and entries; the order-event note is `"dispute resolved: <outcome>"` (500-char column vs 2000-char resolution note).

## Open questions / risks

- The live Paystack refund webhook shape still awaits Phase 22 confirmation (carried over from ADR-0027).
- `grant-admin`'s Firebase claim mirror failed against the emulator (`no user exists with the uid`); the DB role carried the QA. Worth a look before relying on the claim in staging, though the claim is UI-only by design.
- `GET /v1/admin/disputes` orders oldest-first; the spec doesn't state an order. Flagging in case newest-first is preferred.

## Backlog additions

None.
