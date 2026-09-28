# Review packet: Phase 20b Reports & supply requests

## Summary

Users report listings or users (exactly one target, one open report per
reporter/target) through an admin queue that can dismiss, act, suspend the
listing or hide a named review — each in one transaction with an audit
event. Supply requests revive the legacy bulk-quote flow with the status
machine it never had (owner cancels from pending; admins drive the rest),
per-transition events plus SMS, parent-category lines, unit/phone/date
validation and collision-retried `SUP-YYYYMMDD-XXXXXX` numbers.

## Commits

`git log --oneline 9d95f9d..HEAD` output (plus the docs commit below):

```text
15ea0fd Phase 20b: memoize the parsed OpenAPI spec in tests and router
4282a19 Phase 20b: reports and supply tests
b2b9d6d Phase 20b: reports and supply endpoints with wiring
6d8bb74 Phase 20b: reports, listing suspend and supply services
c70e336 Phase 20b: reports and supply schema plus queries
```

## Done-when checklist

- [x] One open report per target: `TestReport_OneOpenPerTarget` (second → 409, dismiss frees, other reporters unaffected)
- [x] Resolving suspends: `TestReport_ResolveSuspendsListing` (status flips in-tx + audit; endpoint test proves search exclusion)
- [x] Supply status machine + notifications: `TestSupplyRequest_StatusMachine` (full table), `TestSupplyRequest_NotifiesOnTransition` (exact template order)
- [x] Create validation table: `TestSupplyRequest_CreateValidation`
- [x] Number format + forced-collision retry: `TestNumberCollisionRetries`
- [x] Uniqueness across 20a+20b: `TestUniqueness_Constraints`
- [x] Endpoints contract-valid: `TestReportEndpoints_Flow`, `TestSupplyEndpoints_Flow`, `TestSupplyEndpoints_CreateValidationTable`

## make ci

Last lines, ending in `ci: all checks passed`:

```text
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

The gate twice timed out on the http package (10m) before this phase
landed: the suite outgrew uncached spec parsing (~600 parses/run).
`api.CachedSpec` parses once per process — 10:07 → 3:21 solo — with no
behavior change (ADR-0033). Earlier in the phase, CI-adjacent catches
were the usual ones: Gin wildcard clash (`{orderId}` → `{id}`),
sensitive-budget fixture raising, and missing router deps in fixtures.

## Manual QA

Live API (Auth emulator, local Postgres; tokens masked; order/listing
seeded via SQL since checkout needs live Paystack keys):

```console
$ curl -X POST .../v1/reports -d '{"listingId":<id>,"reason":"spam","description":"Looks off."}'
{"status":"open",...}
$ curl -X POST ... (again) → {"code":"conflict",...}
$ curl -X POST ... -d '{"listingId":<id>,"userId":"000...","reason":"spam"}'
{"code":"validation_failed",...,"field":"target",...}
$ curl ".../v1/admin/reports?status=open" (admin) → 1 item with listing summary
$ curl -X POST .../v1/admin/reports/<id>/resolve -d '{"status":"actioned","action":"suspend_listing","note":"Confirmed."}'
{"status":"actioned","action":"suspend_listing",...}
$ psql: listings.status → suspended
$ curl -X POST .../v1/supply-requests -d '{"items":[...],"deliveryAddress":"Plot 12","expectedDate":"2099-06-01"}'
{"requestNumber":"SUP-20260928-WUZ92G","status":"pending",...}
$ curl -X POST ... -d '{"items":[{bad product, qty 0, bad unit}]} → validation_failed (contract + service agree)
$ curl -X POST .../v1/supply-requests/<id>/cancel → 200 with pending + cancelled events
$ curl -X POST .../cancel (again) → invalid_transition; admin confirm cancelled → invalid_transition
```

A log sample held no product names, phones, emails or tokens. SMS
enqueue counts asserted in tests (6 jobs for create×2 + cancel + 3
admin moves).

## Files changed

`git diff --stat 9d95f9d..HEAD` plus the docs commit (`AGENTS.md`,
`REDEVELOPMENT_PLAN.md`, `docs/adr/0033-*.md`, this file). New files:

- `migrations/000019_reports_supply.{up,down}.sql`: reports, supply requests, items, events.
- `db/queries/supply.sql` + engagement additions: report CRUD, guarded status writes, supply CRUD with filters.
- `internal/engagement/reports.go`: exactly-one-target reports, moderation queue, resolving with suspend/hide.
- `internal/engagement/reports_test.go`: uniqueness, validation, suspend, hide matrix.
- `internal/supply/supply.go`: validated creation with contact defaults, number retry, guarded transitions with SMS.
- `internal/supply/{supply_test,number_test}.go`: validation table, full machine, notify order, forced-collision retry.
- `internal/listings/service.go`: `SuspendInTx` for the report transaction.
- `internal/http/api/spec.go`: memoized parsed spec.
- `internal/http/handlers/{reports,supply}.go`: nine endpoints.
- `internal/http/reports_supply_test.go`: contract-validated endpoint flows.
- `internal/notify/templates.go`: the five `supply_request_*` texts.

## Schema changes

New migration 000019, `migrations_test` green inside `make ci`. No new
env vars.

## API changes

`engagement` tag reused for reports, new `supply` tag; new schemas
`Report*`, `ResolveReportRequest` (with owner-decided optional
`reviewId`), `Supply*`, `SetSupplyStatusRequest`:

- `POST /v1/reports` (`sensitive`) → 201/400/401/404/409
- `GET /v1/admin/reports` (admin, `?status`) → 200 (+ target summary)
- `POST /v1/admin/reports/{id}/resolve` (admin) → 200/400/401/403/404/409
- `POST /v1/supply-requests` (`sensitive`) → 201/400/401
- `GET /v1/supply-requests` (`?status`) → 200 own list
- `GET /v1/supply-requests/{id}` → 200 own (404 others)
- `POST /v1/supply-requests/{id}/cancel` → 200/400/401/404/409
- `GET /v1/admin/supply-requests` (admin) → 200 all
- `POST /v1/admin/supply-requests/{id}/status` (admin, `sensitive`) → 200/400/401/403/404/409

`make api-lint` and `generate-check` pass; `api.gen.go` generated only.

## Deviations from the spec

All in ADR-0033. Notable: creation emits a pending event + SMS (spec
requires them per transition; every request opens confirmed in trail
and inbox); `reviewId` added to resolve (owner decision).

## Open questions / risks

- Suspended listings keep their data and history (spec: only status
  flips); unsuspend has no endpoint yet — 20b needs none, flagging in
  case moderation wants one before launch.
- `expectedDate` compares days in UTC (== Accra, no DST); fine unless
  Ghana ever observes DST.

## Backlog additions

None.
