# AGENTS.md: rules for any coding agent working in this repo

This is the canonical instruction file for AI coding agents (Muse Spark, Claude, Codex, …) and humans alike. `CLAUDE.md` points here. Read this file **completely** before touching code, then read:

1. `REDEVELOPMENT_PLAN.md`: architecture, locked decisions, phase checklist (§6), decisions log (§8), progress log (§9).
2. `docs/ENGINEERING_GUIDE.md`: how code is written here, with copyable recipes and the real files to imitate.
3. `docs/DOMAIN.md`: money, fees, ledger postings, order states, Ghana reference data. **Money logic must match it exactly.**
4. `docs/phases/<the phase you're implementing>.md`: the detailed spec for your task.
5. `docs/REVIEW_PROTOCOL.md`: what you must hand over when a phase is done.

## Current state (update this section when a phase completes)

- **Done:** Phases 0–18b (skeleton, Postgres 18 + migrations + sqlc, OpenAPI 3.1 contract + codegen + validation, Firebase auth + users, River jobs, rate limiting + Turnstile, phone hardening + step-up re-auth, profiles & seller onboarding, catalog, media uploads on R2, listings CRUD, search & browse, payments core with Paystack and webhooks, ledger foundation, promotions, checkout foundations, checkout payment & escrow hold, fulfilment state machine & notifications, escrow release & refunds, disputes & ledger reconciliation, seller payout accounts, payout execution), plus Upstash Redis for shared rate limits (ADR-0012).
- **Next:** **Phase 19** (`docs/phases/phase-19.md`) (Messaging; depends on 11, 16, done).
- **Owner decisions** are recorded in `docs/DOMAIN.md` §2–3 (ADR-0013), the Phase 12 path and view-hash decisions in ADR-0018/0019, the Phase 13a webhook decisions in ADR-0020/0021, the Phase 14 promotion-snapshot and renewal-shape decisions in ADR-0022/0023, the Phase 15a mixed-category commission rule in ADR-0024, and the Phase 15b late-payment settlement rule in ADR-0025, the Phase 16 mNotify endpoint verification in ADR-0026, the Phase 17a refund lifecycle and reconciliation in ADR-0027, the Phase 17b dispute resolution mechanics in ADR-0028, and the Phase 18a payout account resolution in ADR-0029, and the Phase 18b payout execution in ADR-0030.

## What this is

`farmish-backend` is the Go 1.27 + Gin API for Farmish Ghana, an agricultural marketplace. Buyers pay Farmish (merchant of record), funds sit in escrow, and sellers are paid out via Paystack Transfers.

The sibling repo `../farmish-frontend` (TanStack Start) is built after the API contract freezes (Phase 22). The legacy app at `~/work/farmgate` is **read-only reference**: never edit it, never import from it.

## The workflow (mandatory)

1. **One phase per task.** Take the first unchecked phase in `REDEVELOPMENT_PLAN.md` §6 whose "Depends on" phases are all `[x]`. Implement only that phase and its spec file.
   - Never start the next phase in the same task.
   - Never "quickly add" things from later phases.
2. **Read the spec file first.** If anything in it contradicts `DOMAIN.md` or this file, **stop and ask**; don't pick one yourself.
   - If a decision isn't covered anywhere, stop and ask.
   - Out-of-scope ideas go to §10 Backlog, not into code.
3. **Commit in small steps** on `main`, which is the working model here: migration + queries, then service + tests, then endpoints + tests, then docs.
   - Every commit must pass `make ci`.
   - Commit messages: `Phase N: <what>`, then a body explaining why, then the attribution line if your tooling requires one.
4. **A phase is done only when:**
   - every "Done when" item in §6 **and** every test listed in the phase spec passes
   - `make ci` is green end to end (the full output ends with `ci: all checks passed`)
   - the manual QA script in the spec has been run, with its results in the review packet
5. **When done:**
   - tick the phase checkbox in §6
   - add a row to §9 Progress log
   - add a row to §8 Decisions log plus an ADR in `docs/adr/` for **every** deviation from the spec or a new decision
   - write the review packet `docs/reviews/phase-NN.md` (template in `docs/REVIEW_PROTOCOL.md`)
6. **Never push.** The owner pushes after review.

## Hard rules (a review fails on any of these)

**Security and identity**
- Identity comes **only** from the verified Firebase token: `users.FromContext(ctx)` in handlers. Never accept `user_id`, `seller_id`, `buyer_id`, `role`, prices or amounts from a request body for anything security- or money-relevant.
- Authorization lives in the spec (`security`, `x-farmish-role: admin`), plus ownership checks in services (e.g. "only the listing's seller may edit it"). Ownership failures return **403** `forbidden`. A missing resource returns **404** `not_found`. Don't leak existence where it matters (see the phase spec).
- Public responses use **safe projections**: never expose phone, email, `firebase_uid`, role, balances, payout details, ID documents or internal notes on public endpoints. Use a dedicated `Public*` schema.
- **Never** log tokens, OTPs, passwords, full phone numbers, full account numbers or raw webhook payloads containing personal data. Use `maskPhone`-style helpers (see the guide).
- **Never** return `err.Error()` to clients. Use `apierror` codes.
- Secrets only come from env/config. Never hard-code or commit them. `.env` is gitignored: never stage it (`git status` before committing).

**Money** (see `docs/DOMAIN.md`)
- Money is **integer pesewas** (`int64` in Go, `bigint` in SQL), plus `currency = 'GHS'`. **Never** use `float` for money, and never divide without an explicit rounding rule from DOMAIN.md.
- Prices, fees, commission and totals are computed **server-side** from the database, never trusted from the client.
- A state change, its ledger entries and its River job enqueue **commit in one DB transaction**: `pgx.Tx` → `db.New(tx)` queries → `ledger.Post(ctx, tx, ...)` → `jobClient.InsertTx(ctx, tx, ...)` → `tx.Commit`.
- Every externally triggered write (webhooks, payouts, refunds, checkout) is **idempotent**: a unique constraint on the external reference plus a state check. It's safe to replay. River's `jobs.Unique()` is an extra guard, not the mechanism.
- The ledger is append-only double-entry: entries in one transaction sum to zero **per currency**. There are no UPDATEs or DELETEs on ledger tables.

**Contract and code**
- **Contract first.** Every endpoint is defined in `api/openapi.yaml` before it's implemented:
  - camelCase JSON and query params
  - `$ref` to shared `Error`, `Money` and `PageMeta`
  - declare 400/401/403/404/409/429 responses as applicable
  - then `make generate`
  - Never hand-edit `internal/http/api/api.gen.go` or `internal/db/*.go`.
- SQL lives in `db/queries/*.sql` (sqlc) and `migrations/`. No SQL strings in Go, except tests and ledger invariant checks.
- **Migrations are forward-only once committed.** Never edit a committed migration: add a new one. Each has a working `.down.sql`, and `migrations_test` (up → down → up) must pass.
- Errors: wrap with `fmt.Errorf("context: %w", err)`. Map domain errors to HTTP in handlers only.
- Every new env var goes into `internal/config` (validated, fail-fast), `.env.example` (documented) and `REDEVELOPMENT_PLAN.md` §7.

**Testing**
- Every service function has tests. Every endpoint has happy-path plus failure-path tests, and responses are validated against the spec (the `assertContract` pattern).
- DB tests use real Postgres via `dbtest.Pool(t)`. Auth tests use real emulator users via `authtest`. Redis tests use `redistest`. **No mocking of the database.** Fake only external HTTP services (Paystack, mNotify, R2), behind interfaces.
- Time-dependent logic takes an injectable clock (`func() time.Time`). Never `time.Sleep` to wait for business timers in tests.
- `make test` sets `DBTEST_REQUIRED=1`, `AUTHTEST_REQUIRED=1` and `REDISTEST_REQUIRED=1`: tests must not silently skip.

## Commands

Run from this repo:

| Command | What |
|---|---|
| `make db-up auth-up redis-up` | Local Postgres 18 (:54320), Firebase Auth emulator (:9099), Redis 8 (:63790) |
| `make rustfs-up` | Local S3 (rustfs, :9000), a stand-in for Cloudflare R2 |
| `make migrate-up` / `migrate-down N=1\|all` / `migrate-version` / `migrate-new name=x` | Migrations (`cmd/migrate`, embedded) |
| `make seed` | Seed reference data (catalog) into `DATABASE_URL` (`cmd/seed`, idempotent) |
| `make sqlc` | Regenerate `internal/db` from `db/queries` + `migrations` |
| `make generate` | Regenerate `internal/http/api/api.gen.go` from `api/openapi.yaml` |
| `make run` | Run the API (`.env`, else `.env.example`) |
| `make test` | All tests with `-race`, DB/auth/Redis required |
| `make lint` / `make fmt` | golangci-lint v2 / gofumpt + goimports |
| `make ci` | **The gate:** tidy-check, fmt-check, api-lint, generate-check, sqlc-check, lint, test, vuln, smoke |
| `make grant-admin EMAIL=x` | Make a user admin (DB role + Firebase claim) |
| `scripts/dev-token.sh <email> <pw>` | ID token for curl (emulator by default) |

Single test: `TEST_DATABASE_URL='postgres://farmish:farmish@127.0.0.1:54320/farmish?sslmode=disable' FIREBASE_AUTH_EMULATOR_HOST=127.0.0.1:9099 REDIS_TEST_URL=redis://127.0.0.1:63790 go test -race -run TestName ./internal/...`

## Local environment gotchas (learned the hard way)

- `.env` is **sourced by the shell**. Quote any value containing `&`, `;`, spaces, `$` or `#` in single quotes. An unquoted `&` silently drops the variable (Neon URLs!).
- Develop against the **local** containers (fast). Neon and Upstash are far from the dev machine: ~250–500ms per query, ~1.6s per new DB connection, ~200ms per Redis call. That causes `/readyz` timeouts and rate-limit fallbacks locally. That isn't a bug.
- Neon: use the **direct** host (no `-pooler`), because River and advisory locks need session semantics.
- The Firebase emulator skips signature checks. Config refuses `FIREBASE_AUTH_EMULATOR_HOST` in staging/production. Never weaken that.
- The smoke test (`make smoke`) has caught real shutdown bugs twice. If it fails, fix the cause. Never loosen the test.
