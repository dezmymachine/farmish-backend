# Farmish Redevelopment Plan: Go (Gin) Backend + TanStack Start Frontend

## 0. How to use this plan

- The work is split into **phases**. Each task/PR delivers **exactly one phase**. Never one-shot several.
- Start a phase only when all of its **Depends on** phases are checked `[x]`.
- Every phase lists **Scope**, **Tasks**, and **Done when** (acceptance checks). A phase is done only when every "Done when" item passes locally and in CI. While GitHub Actions is off, "CI" means a green `make ci` in the backend repo (ADR-0004).
- At the end of a phase:
  - tick its checkbox in §6
  - add a one-line entry to the §9 Progress log (date, phase, PR/commit, notes)
  - record any deviation from this plan in §8 Decisions log
- If a phase turns out too big, split it into `Nx.1`, `Nx.2`, … here *before* coding.
- Out-of-scope items discovered mid-phase go to §10 Backlog, not into the current phase.
- **Implementer handbook:** `AGENTS.md` (rules), `docs/ENGINEERING_GUIDE.md` (patterns), `docs/DOMAIN.md` (money, fees, ledger, states, reference data: **the business source of truth**), `docs/phases/` (per-phase specs), `docs/REVIEW_PROTOCOL.md` (review packet `docs/reviews/phase-NN.md`).
- The legacy Next.js app lives at `~/work/farmgate` (separate repo) and is **frozen reference only**. Read it for business rules, never import from it or edit it. No data or user migration.

## 1. Project

**Farmish Ghana** is Ghana's agricultural marketplace for seeds, fertilizers, machinery, livestock, feeds, fresh produce, processed foods, farm labour, land leasing, storage/packaging, veterinary, and irrigation.

Core flows: browse/search → sell (listings) → buyer↔seller messaging → checkout (**buyer pays Farmish**) → escrow → fulfilment/delivery → seller payout → promotions → reviews/favorites → supply requests.

## 2. Locked decisions

| Area | Decision |
|---|---|
| Layout | Two repos, `farmish-backend` and `farmish-frontend` (github.com/dezmymachine/…), checked out side by side in the plain folder `~/work/farmish`. The backend repo holds this plan and the ADRs (ADR-0003) |
| Backend | Go (latest stable, pinned in `go.mod`) + **Gin**, contract-first **OpenAPI 3.1** → `oapi-codegen` |
| Frontend | **TanStack Start** (built after the backend v1 contract is frozen) |
| Hosting | Backend on **Railway** (Docker) behind **Cloudflare** (DNS/CDN/WAF/Turnstile) |
| Database | **Neon Postgres** (direct connection; Hyperdrive pooling only later), `pgx` + **sqlc**, **golang-migrate** |
| Jobs | **River** (Postgres-native, same DB → transactional enqueue). Temporal deferred behind a `Workflow` interface |
| Auth | **Firebase** for social, email/password **and phone** (Firebase sends and verifies the SMS code via the client SDK + reCAPTCHA/App Check). The backend only verifies Firebase ID tokens; no custom OTP backend (ADR-0007). No account linking in v1 |
| SMS | mNotify/BMS for **transactional notifications only** (order/payout alerts), behind a `notify.Notifier` interface, introduced in Phase 16. Sign-in codes are Firebase's job |
| Media | **Cloudflare R2** presigned uploads + CDN |
| Redis | **Upstash Redis** (TCP + TLS): shared per-user/per-operation rate limits now, caching later. Per-IP limits stay in-process (ADR-0012) |
| Payments | **Paystack** (card + MoMo). **Farmish is merchant of record**: funds are held in escrow on the Paystack balance and released to sellers via **Paystack Transfers** minus commission |
| Delivery | v1 stub: delivery method/address/fee/statuses on orders, `DeliveryProvider` interface with a `manual` impl. Courier integration later |
| Money | **Integer pesewas (`bigint`)** everywhere in DB and API (`amount` + `currency: "GHS"`). Frontend formats for display |
| Commission | **5% (500 bps)** of the item subtotal (delivery excluded), snapshotted per order; per-category overrides (DOMAIN §2.1) |
| Processing fee | **Buyer pays** Paystack's fee as a visible, non-refundable line, grossed up with `PAYSTACK_FEE_BPS` (default 195 = 1.95%) (DOMAIN §2.2) |
| Order timers | Seller accepts within **48h** or auto-cancel + refund; escrow auto-releases **3 days** after delivery (DOMAIN §3) |
| Payouts | Daily at 10:00 Africa/Accra, minimum **GHS 20**, 48h cooldown after a payout-account change, step-up re-auth to change it (DOMAIN §3) |
| Scope | Full rewrite, no data/user migration |

## 3. Target architecture

```
farmish-frontend (TanStack Start) ──HTTPS──▶ Cloudflare (DNS/CDN/WAF/Turnstile, R2 CDN)
                                                  │
                                                  ▼
                                   farmish-backend (Gin on Railway)
                                    ├── Firebase Admin (verify ID tokens, custom claims)
                                    ├── Neon Postgres (pgx + sqlc, golang-migrate)
                                    ├── River (jobs + periodic jobs, same process in v1)
                                    ├── Upstash Redis (shared rate limits, later cache)
                                    ├── R2 (presigned PUT, public CDN GET)
                                    ├── Paystack (charges, refunds, transfers, webhooks)
                                    └── mNotify (transactional SMS alerts)
```

## 4. Repository layout (target)

```
~/work/farmish/                  # plain folder, not a repo
  CLAUDE.md                      # unversioned pointer to the two repos
  farmish-backend/               # repo github.com/dezmymachine/farmish-backend
    REDEVELOPMENT_PLAN.md  CLAUDE.md  README.md
    docs/adr/                    # architecture decision records (one per §8 decision)
    .github/workflows/ci.yml
    cmd/api/main.go              # wiring only: config → logger → db → river → router → run
    api/openapi.yaml             # source of truth for the HTTP contract (+ oapi-codegen.yaml; redocly.yaml at root)
    cmd/migrate/main.go          # embedded golang-migrate runner (ADR-0005)
    cmd/admin/main.go            # operator CLI: grant-admin / revoke-admin
    (jobs: internal/jobs, River schema in migrations/000003_river_queue)
    migrations/                  # NNNNNN_name.{up,down}.sql, embedded via embed.FS
    db/queries/*.sql  sqlc.yaml  # sqlc input → internal/db (generated)
    internal/
      config/ database/ (pgxpool, dbtest) db/ (sqlc) http/ (router, middleware incl. auth, handlers, apierror, api/api.gen.go) auth/ (Firebase, authtest) users/
      catalog/ listings/ media/ search/
      payments/ ledger/ promotions/ checkout/ orders/ escrow/ payouts/ refunds/ delivery/
      messaging/ engagement/ (reviews, favorites, reports) supply/ notify/ jobs/ ratelimit/ audit/
    pkg/logger/
    Makefile  Dockerfile  docker-compose.yml  .env.example  .golangci.yml
  farmish-frontend/              # repo github.com/dezmymachine/farmish-frontend; app scaffolded in Phase F0
```

## 5. Backend conventions (apply in every phase)

- **Identity:** the user comes only from the verified Firebase token → `users.id`. Never trust `user_id`/`seller_id`/amounts from the request body.
- **Public projections:** never expose phone, ID docs, role, credits, balances, payout details, or `firebase_uid` on public endpoints.
- **Errors:** one envelope `{ "error": { "code", "message", "details?" } }` and generic auth errors.
- **Pagination:** `page`/`limit` with `limit ≤ 50`, or a cursor where noted.
- **Idempotency:** every externally triggered write (webhooks, payouts, refunds) has a unique key and is safe to replay. `Idempotency-Key` header on `POST /v1/checkout`.
- **Transactions:** a state change, its ledger entries, and its River job enqueue all commit in one DB transaction.
- **Logs:** slog JSON with request ID. No secrets, OTPs, or full phone numbers in logs.
- **Tests:** each phase ships `go test ./...` coverage for its handlers/services. DB tests run against real Postgres via docker-compose (or testcontainers).

---

## 6. Phases

### Phase 0: Workspace scaffold
- **Depends on:** none
- **Scope:** folders and tooling only. No app code.
- **Tasks:**
  - `git init` (default branch `main`) in `~/work/farmish/farmish-backend` and `~/work/farmish/farmish-frontend`. `~/work/farmish` itself stays a plain folder.
  - Backend repo: `REDEVELOPMENT_PLAN.md`, `CLAUDE.md`, `README.md`, `docs/adr/`, `.editorconfig`, `.gitignore` (Go binaries, `.env*`).
  - Frontend repo: placeholder `README.md` and `CLAUDE.md` pointing at the backend plan, `.editorconfig`, `.gitignore` (`.env*`, `node_modules`, `.output`).
  - Don't touch `~/work/farmgate`.
  - Write ADR-0001 summarising §2.
- **Done when:** the tree matches §4 top level, each repo's initial commit contains its scaffold (backend: plus the plan, `CLAUDE.md` and ADR), and `~/work/farmgate` is unchanged.
- [x] Phase 0

### Phase 1: Backend skeleton
- **Depends on:** 0
- **Scope:** a runnable Gin service with no DB.
- **Tasks:**
  - `go mod init`, Gin, `internal/config` (env + `.env.example`, fail fast on missing required vars), `pkg/logger` (slog JSON).
  - Middleware: request ID, recovery, access log, CORS (allowlist from config).
  - `GET /healthz`. Graceful shutdown on SIGTERM.
  - `Makefile` (`run`, `build`, `test`, `lint`, `fmt`), multi-stage `Dockerfile` (distroless/nonroot), `.golangci.yml`.
  - ~~GitHub Actions~~ `make ci` (ADR-0004): tidy/fmt checks, `go vet`, golangci-lint, `go test -race`, govulncheck, docker build + `/healthz` smoke test.
- **Done when:** `make run` then `curl /healthz` returns 200 JSON, `make lint test` passes, the Docker image builds and serves `/healthz`, and `make ci` is green.
- [x] Phase 1

### Phase 2: Database foundation
- **Depends on:** 1
- **Tasks:**
  - `docker-compose.yml` with `postgres:16` (now `postgres:18` to match Neon, ADR-0009).
  - `pgxpool` wiring with timeouts.
  - golang-migrate with `make migrate-up/down/new`.
  - sqlc setup (`sqlc.yaml`, `make sqlc`).
  - Migration `000001` adds extensions (`pgcrypto`, `citext`) and an `updated_at` trigger function.
  - `GET /readyz` pings the DB.
  - Test helper that creates an isolated schema/DB per test run.
- **Done when:**
  - up → down → up migrations run cleanly
  - `/readyz` returns 200 with the DB up and 503 with it down
  - a sample sqlc query compiles and is tested
- [x] Phase 2

### Phase 3: API contract & codegen
- **Depends on:** 1
- **Tasks:**
  - Create `api/openapi.yaml` with info, servers, security scheme (`bearerAuth`), shared schemas (`Error`, `Money{amount:int64,currency}`, `PageMeta`), and `/healthz`, `/readyz`.
  - Set up `oapi-codegen` (Gin strict server) with `make generate` and CI drift check (`git diff --exit-code`).
  - Add Spectral/Redocly lint in CI and request validation middleware against the spec.
  - Error-envelope helper.
- **Done when:** handlers are served through generated interfaces, spec lint passes, an invalid request returns a 400 envelope, and the CI drift check works.
- [x] Phase 3

### Phase 4: Auth core (Firebase) + users
- **Depends on:** 2, 3
- **Tasks:**
  - Migration: `users` (`id uuid`, `firebase_uid unique`, `signup_method social|email|phone`, `email citext`, `email_verified`, `phone_e164`, `display_name`, `role user|admin`, `seller_verified`, timestamps).
  - Firebase Admin init from config (`FIREBASE_CREDENTIALS_JSON` inline JSON or a file path). `FIREBASE_AUTH_EMULATOR_HOST` refused in staging/production.
  - Auth middleware driven by the spec: operations with `bearerAuth` require a Bearer ID token → verify → find-or-create `users` row (race-safe) → context. `x-farmish-role: admin` operations also require `users.role = admin`.
  - `signup_method` from the token's `sign_in_provider` (`phone` → phone, `password` → email, other IdPs → social).
  - `RequireAuth` / `RequireAdmin` guards.
  - `GET /v1/me`, `PATCH /v1/me`.
  - Role sync: `users.role` is authoritative; the `role` custom claim mirrors it for the UI. Admin CLI `make grant-admin` / `revoke-admin`.
  - Tests use the Firebase Auth emulator (docker-compose).
- **Done when:** a valid emulator token returns `/v1/me`, phone and email sign-ins map to the right `signup_method`, missing/expired/forged tokens return 401 (generic), and non-admins get 403 on an admin test route.
- [x] Phase 4

### Phase 5: Jobs infrastructure (River)
- **Depends on:** 2
- **Tasks:**
  - River migrations and a client plus workers in the same process (config flag to run API-only / worker-only).
  - `jobs` package with a registration pattern, retry/backoff defaults, and unique-job helpers.
  - Periodic job scheduler.
  - A demo `NoopJob` enqueued inside a DB tx.
  - Optional River UI behind an admin guard.
- **Done when:** a job enqueued in a rolled-back tx never runs and one in a committed tx runs exactly once, and a periodic job fires in tests.
- [x] Phase 5

### Phase 6: Rate limiting & abuse controls
- **Depends on:** 2, 4
- **Tasks:**
  - `ratelimit` package with token buckets keyed per-IP and per-user. Start in-process with an interface for a Postgres/Redis backend later.
  - Gin middleware that emits `Retry-After` + `X-RateLimit-*`.
  - Client IP from `CF-Connecting-IP` (trusted only behind Cloudflare).
  - Cloudflare Turnstile verification helper for sensitive public endpoints (our anonymous write endpoints such as supply requests and reports; Firebase's phone sign-in uses its own reCAPTCHA/App Check).
- **Done when:** exceeding the limit returns 429 with headers, and a Turnstile failure returns a 400 envelope. Tests cover both.
- [x] Phase 6

> **Phases 7–22 and F0–F9 have detailed specs in [`docs/phases/`](docs/phases/README.md)** (schema, queries, API, rules, jobs, tests and QA). Implementers must read `AGENTS.md`, `docs/ENGINEERING_GUIDE.md`, `docs/DOMAIN.md` and the phase spec first. The large phases are pre-split into sub-phases (13a/13b, 15a/15b, 17a/17b, 18a/18b, 20a/20b). Each sub-phase is one task with its own checkbox.

### Phase 7: Phone sign-in hardening & step-up re-auth · [spec](docs/phases/phase-07.md)
- **Depends on:** 4
- **Done when:**
  - an emulator phone sign-in reaches `/v1/me` as `phone`
  - a fresh token passes a `x-farmish-step-up` operation, and a stale or revoked one gets 401 `reauth_required`
  - the Firebase phone runbook is done
- [x] Phase 7

### Phase 8: Profiles & seller onboarding · [spec](docs/phases/phase-08.md)
- **Depends on:** 4
- **Done when:**
  - the public seller response contains only safe fields (asserted by a test)
  - verification writes an audit event and updates the DB flag plus the Firebase claim
  - ID numbers are encrypted at rest
  - `InTx`, `validation`, `crypto` and `audit` exist and are tested
- [x] Phase 8

### Phase 9: Catalog (categories, attributes, locations) · [spec](docs/phases/phase-09.md)
- **Depends on:** 2, 3
- **Done when:** the seed (DOMAIN §9) is idempotent, the tree endpoint matches the seed data, a child inherits its group and attributes, and `/v1/locations` serves the 16 regions.
- [x] Phase 9

### Phase 10: Media uploads (R2) · [spec](docs/phases/phase-10.md)
- **Depends on:** 4
- **Done when:** a presigned PUT works against the local S3 stand-in in compose (rustfs; MinIO is no longer pullable, ADR-0016), a disallowed type or size (including a signed content-length mismatch) is rejected, and orphans are cleaned in the job test.
- [x] Phase 10

### Phase 11: Listings CRUD · [spec](docs/phases/phase-11.md)
- **Depends on:** 8, 9, 10
- **Done when:**
  - create → get → update → publish → archive works
  - a non-owner gets 403
  - invalid attributes return 400
  - the expiry job flips past-due listings
- [ ] Phase 11

### Phase 12: Search & browse · [spec](docs/phases/phase-12.md)
- **Depends on:** 11
- **Done when:**
  - filter combinations are tested and active promotions rank first (expired ones don't)
  - `EXPLAIN` shows index use
  - responses contain no private seller fields
  - ETag/304 works
- [ ] Phase 12

### Phase 13a: Payments core (Paystack, webhooks) · [spec](docs/phases/phase-13a.md)
- **Depends on:** 4, 5, 8 (audit events, `forbid_mutation()`, `InTx`)
- **Done when:**
  - a bad signature returns 401
  - a replayed event is processed once
  - an amount or currency mismatch is rejected and logged
  - the verify fallback and the webhook don't double-process
  - money helpers match the DOMAIN §2 examples
- [ ] Phase 13a

### Phase 13b: Ledger foundation · [spec](docs/phases/phase-13b.md)
- **Depends on:** 13a
- **Done when:** a ledger post with a non-zero sum errors (per currency), posts are idempotent on (kind, reference), the tables are append-only, and a property test holds balances consistent.
- [ ] Phase 13b

### Phase 14: Promotions · [spec](docs/phases/phase-14.md)
- **Depends on:** 12, 13b
- **Done when:**
  - the end-to-end run goes purchase → webhook → credits → apply → listing ranked as promoted
  - a webhook replay doesn't double-credit
  - concurrent applies never make credits negative
- [ ] Phase 14

### Phase 15a: Checkout foundations (pricing, delivery, orders schema) · [spec](docs/phases/phase-15a.md)
- **Depends on:** 12, 13b
- **Done when:** `PriceCart` is fully table-tested (two sellers → two orders, gross-up, commission resolution, validation), and the quote endpoint is contract-valid.
- [ ] Phase 15a

### Phase 15b: Checkout payment & escrow hold · [spec](docs/phases/phase-15b.md)
- **Depends on:** 15a
- **Done when:**
  - a two-seller cart produces two orders and one charge
  - a tampered client price is ignored
  - an amount mismatch leaves orders unpaid
  - a webhook replay makes no double escrow
  - unpaid checkouts expire and restore stock
  - the last unit can't be oversold
- [ ] Phase 15b

### Phase 16: Fulfilment state machine & notifications · [spec](docs/phases/phase-16.md)
- **Depends on:** 15b
- **Done when:**
  - illegal transitions return 409
  - each actor can do only their own transitions (the full-table test)
  - the timers are tested with a fake clock
  - every transition enqueues an SMS
- [ ] Phase 16

### Phase 17a: Escrow release & refunds · [spec](docs/phases/phase-17a.md)
- **Depends on:** 16
- **Done when:** release and refund are each idempotent, there's no refund after release (it goes to the manual path), and partial-refund commission follows DOMAIN §4.1.
- [ ] Phase 17a

### Phase 17b: Disputes & ledger reconciliation · [spec](docs/phases/phase-17b.md)
- **Depends on:** 17a
- **Done when:** the three dispute outcomes work with audit, the reconcile job detects violations, and ledger totals reconcile (the escrow balance equals the sum of held orders).
- [ ] Phase 17b

### Phase 18a: Seller payout accounts · [spec](docs/phases/phase-18a.md)
- **Depends on:** 7, 8, 17b
- **Done when:**
  - setting an account requires step-up
  - the account is resolved, name-checked, encrypted and never returned unmasked
  - a change starts a 48h cooldown
- [ ] Phase 18a

### Phase 18b: Payout execution · [spec](docs/phases/phase-18b.md)
- **Depends on:** 18a
- **Done when:**
  - the end-to-end run goes completed order → payable → payout → transfer.success
  - a failed transfer restores the balance
  - no payout happens during cooldown or below the minimum
- [ ] Phase 18b

### Phase 19: Messaging · [spec](docs/phases/phase-19.md)
- **Depends on:** 11, 16
- **Done when:** a non-participant gets 403, cursor pagination works, a duplicate conversation returns the existing one, and the messaging rate limit and SMS throttling work.
- [ ] Phase 19

### Phase 20a: Reviews & favorites · [spec](docs/phases/phase-20a.md)
- **Depends on:** 16
- **Done when:** a review without a completed order is rejected, uniqueness is tested, and favourites are idempotent with an exact counter.
- [ ] Phase 20a

### Phase 20b: Reports & supply requests · [spec](docs/phases/phase-20b.md)
- **Depends on:** 16, 9
- **Done when:** one open report per target, resolving can suspend a listing, and the supply-request status machine is tested with notifications.
- [ ] Phase 20b

### Phase 21: Observability & hardening · [spec](docs/phases/phase-21.md)
- **Depends on:** 18b, 19, 20a, 20b
- **Done when:** k6 thresholds pass locally, there are no secrets, OTPs or phones in a log sample, metrics are exposed, and audit coverage is tested.
- [ ] Phase 21

### Phase 22: Deploy (Railway + Neon + Upstash + Cloudflare) & contract freeze · [spec](docs/phases/phase-22.md)
- **Depends on:** 21 (needs the owner for accounts, DNS and live keys)
- **Done when:** staging passes the end-to-end smoke test with Paystack test mode, `/readyz` is green, the spec is tagged `v1.0.0`, and the regulatory item has an owner decision.
- [ ] Phase 22

### Frontend phases · [spec](docs/phases/frontend.md) (start after Phase 22 freezes the contract; F0–F2 may start after Phase 12)
- [ ] **F0 Scaffold:** TanStack Start, TS strict, Tailwind v4 + shadcn, lint/test/e2e, `pnpm ci`, and a deploy-target ADR.
- [ ] **F1 API client:** types generated from `api/openapi.yaml` (openapi-typescript + openapi-fetch), auth header, error envelope, `formatMoney`.
- [ ] **F2 Auth:** Firebase social/email/phone (`signInWithPhoneNumber` + `RecaptchaVerifier`), a re-auth dialog, route guards.
- [ ] **F3 Browse:** home, categories, search with filters, listing detail.
- [ ] **F4 Sell:** seller onboarding, category-driven listing form, R2 uploads, my listings.
- [ ] **F5 Checkout:** cart, quote with the processing-fee line, an idempotent checkout, the Paystack redirect, a payment status page.
- [ ] **F6 Orders:** buyer and seller dashboards, state-aware actions, disputes.
- [ ] **F7 Payouts:** payout account (step-up), balances, history.
- [ ] **F8 Engagement:** messaging, reviews, favorites, reports, supply requests, promotions.
- [ ] **F9 Launch:** SEO, performance, accessibility, monitoring, production deploy, legacy redirects.

## 7. Environment variables (backend)
`APP_ENV`, `PORT`, `LOG_LEVEL`, `SHUTDOWN_TIMEOUT` (total SIGTERM-to-exit budget), `RUN_MODE` (`all|api|worker`), `JOBS_MAX_WORKERS`, `DATABASE_URL`, `DB_MAX_CONNS`, `DB_STATEMENT_TIMEOUT`, `CORS_ORIGINS`, `FIREBASE_PROJECT_ID`, `FIREBASE_CREDENTIALS_JSON`, `FIREBASE_AUTH_EMULATOR_HOST` (dev/test only, refused in staging/production), `TURNSTILE_SECRET`, `TRUSTED_PROXIES`, `TRUST_CLOUDFLARE`, `REDIS_URL` (optional, `rediss://`), `REDIS_TIMEOUT`, `MNOTIFY_API_KEY`, `MNOTIFY_SENDER` (Phase 16), `R2_ACCOUNT_ID`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_BUCKET`, `R2_PUBLIC_BASE_URL`, `PAYSTACK_SECRET_KEY`, `PAYSTACK_PUBLIC_KEY`, `DATA_ENCRYPTION_KEY`, `R2_ENDPOINT` (MinIO override), `PAYSTACK_BASE_URL`, `PAYSTACK_CALLBACK_URL`, `PAYSTACK_FEE_BPS` (195), `PAYSTACK_TRANSFER_FEE_PESEWAS` (0 until confirmed), `CHECKOUT_EXPIRY_MINUTES` (30), `ESCROW_AUTO_COMPLETE_DAYS` (3), `SELLER_ACCEPT_TIMEOUT_HOURS` (48), `PAYOUT_MIN_PESEWAS` (2000), `NOTIFY_SMS_ENABLED`, `METRICS_ADDR` (Phase 21). Each phase spec lists its variables; add them to `internal/config`, `.env.example` and this list when implementing.

## 8. Decisions log
| Date | Decision | ADR |
|---|---|---|
| 2026-09-25 | Adopt §2 locked decisions; escrow + Transfers payment model; delivery stubbed | ADR-0001 |
| 2026-09-25 | Phase 1: module path `github.com/dezmymachine/farmish-backend`; minimal error envelope pulled forward from Phase 3; optional `LOG_LEVEL`/`SHUTDOWN_TIMEOUT` env vars; Gin always in release mode | ADR-0002 |
| 2026-09-25 | Split into two repos (`farmish-backend`, `farmish-frontend`); `~/work/farmish` is a plain folder; plan + ADRs live in the backend repo | ADR-0003 |
| 2026-09-25 | GitHub Actions removed (account billing lock); local `make ci` is the gate | ADR-0004 |
| 2026-09-25 | Phase 2: own `cmd/migrate` (embedded golang-migrate) instead of the CLI; sqlc via Docker; per-test databases; compose on port 54320; bounded pool close; `DB_MAX_CONNS`/`DB_STATEMENT_TIMEOUT` | ADR-0005 |
| 2026-09-26 | Phase 3: OpenAPI 3.1 + oapi-codegen v2.8.0; generated code in `internal/http/api/`; camelCase JSON/query, snake_case error codes; validator passes unknown routes through and skips auth; Redocly via Docker; drift check in `make ci` | ADR-0006 |
| 2026-09-26 | Firebase phone sign-in replaces the custom mNotify OTP flow; Phase 7 becomes phone hardening + step-up re-auth; mNotify kept for transactional SMS only (Phase 16); Turnstile stays in Phase 6 | ADR-0007 |
| 2026-09-26 | Phase 4: spec-driven auth (`bearerAuth` / `x-farmish-role`) in `internal/http/middleware`; `users.role` authoritative, claim mirrors it; no per-request revocation check; `custom`/`anonymous` providers rejected; `email_verified` column; credentials inline or path; emulator refused when deployed | ADR-0008 |
| 2026-09-26 | Local/test Postgres bumped 16 → 18 to match Neon (18.6); new `pgdata18` volume | ADR-0009 |
| 2026-09-26 | Phase 5: River schema generated into golang-migrate (000003) with a version guard test; 10 attempts / 1m timeout defaults; `RUN_MODE` all/api/worker (worker = probes only); `SHUTDOWN_TIMEOUT` is the total budget (9s) with concurrent HTTP drain + job stop; River UI deferred | ADR-0010 |
| 2026-09-26 | Phase 6: in-process token buckets (ip 300/min, user 120/min, `sensitive` 10/min) with bounded memory, fail-open; spec extensions `x-farmish-rate-limit` / `x-farmish-turnstile`; client IP from `TRUSTED_PROXIES` + Cloudflare ranges only; Turnstile fails closed (503) when Cloudflare is unreachable | ADR-0011 |
| 2026-09-26 | Upstash Redis for shared limits, hybrid (per-IP stays in-process); go-redis over TLS, atomic Lua bucket, in-process fallback on timeout/outage | ADR-0012 |
| 2026-09-26 | Owner business decisions (5% commission; buyer pays the processing fee via gross-up; 48h accept / 3-day auto-release; GHS 20 daily payouts). Implementer handbook (`AGENTS.md`, guide, DOMAIN, phase specs, review protocol). Phases 13/15/17/18/20 pre-split. `fulfilling` state dropped. Legacy bugs not ported (DOMAIN §12) | ADR-0013 |
| 2026-09-26 | Phase 7: step-up revocation test adapted to the Auth emulator (SDK checks revocation on both paths in emulator mode; 1.2s sleep for `validSince` granularity). Production routing proved with a fake instead | ADR-0014 |
| 2026-09-26 | Phase 9: `CategoryAttribute` gains `id` (the spec's PATCH/DELETE paths need discoverable ids) | ADR-0015 |
| 2026-09-26 | Phase 10: rustfs replaces MinIO as the local S3 stand-in (owner decision; MinIO images are no longer pullable); storage `Head` allowed inside the attach transaction before row locks | ADR-0016, ADR-0017 |

## 9. Progress log
| Date | Phase | PR/commit | Notes |
|---|---|---|---|
| 2026-09-25 | 0 | initial commits (both repos) | Redone after the two-repo split (ADR-0003). `.gitignore` excludes `.env*` but keeps `!.env.example` |
| 2026-09-25 | 1 | Phase 1 commit | Local checks pass: `make run` + `/healthz` 200 JSON, `make lint test`, image builds (26.5 MB distroless/nonroot) and serves `/healthz`, `make ci` green (tidy, fmt, vet, lint, race tests, govulncheck, docker smoke incl. graceful SIGTERM). GitHub Actions removed per ADR-0004. See ADR-0002 |
| 2026-09-25 | 2 | Phase 2 commit | `make ci` green. Migrations up→down→up clean (test + Makefile); `/readyz` 200 → 503 (DB stopped) → 200 on the running API; sqlc `ListExtensions` compiled and tested; per-test DB helper. Smoke test caught a pool-close hang on SIGTERM with the DB unreachable, now fixed. See ADR-0005 |
| 2026-09-26 | 3 | Phase 3 commit | `make ci` green. `/healthz` + `/readyz` served via the generated strict interface; responses validated against the spec in tests; Redocly lint passes; the validator returns 400 `validation_failed` with per-field details (fixture spec); drift check fails on spec edits and on hand-edits of generated code. See ADR-0006 |
| 2026-09-26 | 4 | Phase 4 commit | `make ci` green (Postgres + Auth emulator). `/v1/me` with emulator email and phone tokens → 200 with `signupMethod` email/phone; missing/garbage/expired/other-project/tampered/forged (real RS256 signature check) tokens → identical 401; non-admin 403 → admin 200 after `grant-admin` (DB + claim); concurrent first sign-ins create one row; PATCH validation. See ADR-0008 |
| 2026-09-26 | 5 | Phase 5 commit | `make ci` green. Rolled-back tx: job (and business row) never exists or runs; committed tx: runs exactly once; periodic job fires on start and on interval; retry, unique, insert-only (api mode) and soft/hard stop tested; all three `RUN_MODE`s run and exit cleanly. Smoke test caught a SIGKILL on shutdown with the DB unreachable, fixed with one concurrent shutdown budget. See ADR-0010 |
| 2026-09-26 | 6 | Phase 6 commit | `make ci` green. Over the limit → 429 `rate_limited` with `Retry-After` + `X-RateLimit-*` (contract-valid, CORS-readable); probes exempt; per-user budgets independent on a shared IP; missing/bad Turnstile token → 400 `turnstile_failed`, Cloudflare down → 503; spoofed `CF-Connecting-IP`/XFF ignored unless via trusted proxy/Cloudflare edge; live check with Cloudflare test secrets. See ADR-0011 |
| 2026-09-26 | 7 | Phase 7 commit | `make ci` green. Fresh emulator sign-in passes the step-up fixture op; +6 min clock → 401 `reauth_required` with `WWW-Authenticate: Bearer error="insufficient_user_authentication"`; revoked token → 401 on step-up (emulator checks revocation on both paths, see ADR-0014); normal ops never call `VerifyStrict`; `internal/geo` phone/region helpers table-tested; Firebase phone runbook written. See ADR-0014 |
| 2026-09-26 | 8 | Phase 8 commits | `make ci` green. Seller profile CRUD with server-side validation; ID submission encrypts (`v1:` ciphertext) and re-pends in one tx with audit; approve/reject flips the DB flag plus best-effort Firebase claim; public projection test asserts private keys absent; `InTx`/`validation`/`crypto`/`audit` building blocks tested. A pooled-gin-context race on the claim HTTP call was fixed at the handler boundary with a regression test |
| 2026-09-26 | 10 | Phase 10 commits | `make ci` green. Presigned PUT verified end to end against the local S3 stand-in (exact bytes 200, wrong body size 403); type/size allowlist, key format, 401, sensitive rate limit and the attach guards tested; `media.cleanup_orphans` runs hourly through River and the job test proves it. Owner decision: rustfs instead of MinIO. The smoke script needed a `pipefail`-safe log read. See ADR-0016, ADR-0017 |
| 2026-09-26 | 9 | Phase 9 commits | `make ci` green. `make seed` ports DOMAIN §9 (12 parents, 72 children, 32 attributes) idempotently; public tree/detail/locations with Cache-Control; admin category + attribute CRUD (409 on taken slug/key); child inherits parent group/attributes with override merge. See ADR-0015 |

## 10. Backlog (not scheduled)
- Restore GitHub Actions (a workflow that runs `make ci`) once account billing is fixed; retire ADR-0004
- Courier integration via `DeliveryProvider`, and provider-quoted delivery fees
- Account linking across Firebase methods
- Hyperdrive pooling after Neon connection pressure is observed
- Temporal behind the `Workflow` interface
- Redis-backed caching (via `internal/redisx`) when a phase needs it
- River UI for admins (needs a browser session/cookie auth flow; `RequireAuth` + `RequireAdmin` already exist)
- Seller reserve/holdback against chargebacks

## 11. Open items / risks
- **Regulatory:** holding customer funds (escrow) in Ghana may fall under the Bank of Ghana Payment Systems and Services Act, 2019 (Act 987). Get legal advice before Phase 22 go-live. Fallback: Paystack split payments with delayed settlement.
- **Paystack setup:**
  - confirm GH Transfers are enabled and transfer OTP can be disabled for automation
  - keep settlement on the Paystack balance
  - confirm MoMo recipient support for all networks
- **Fees:** decided on 2026-09-26. The buyer pays the charge fee (a visible, non-refundable line) and the platform absorbs transfer fees (DOMAIN §2). **Still to confirm:** the live Paystack fee rate and the GH transfer fee (`PAYSTACK_FEE_BPS`, `PAYSTACK_TRANSFER_FEE_PESEWAS`), and whether the processing fee should be refunded on seller-caused cancellations (default: no, DOMAIN §2.2).
- **Client IP behind Railway + Cloudflare (Phase 22):** find the Railway edge's source addresses for `TRUSTED_PROXIES`, set `TRUST_CLOUDFLARE=true`, verify the logged `client_ip`, and consider restricting the origin to Cloudflare (ADR-0011).
- **Upstash:** free tier is 500K commands/month; watch usage in the console. Keep the Upstash DB in the Railway region (a ~200ms round trip would hit the fallback timeout). Rotate the credential that was pasted into a chat before any deployment (ADR-0012).
- **Chargebacks after release:** the platform bears the loss. See the reserve backlog item.
- **mNotify (transactional SMS):** sender ID `FARMISH` approval (ship with the default sender) and low-balance alerting.
- **Firebase phone auth:**
  - requires the **Blaze (billing) plan**; each SMS is charged
  - SMS-pumping abuse: restrict the SMS region policy to Ghana, enable App Check, set a GCP budget alert (Phase 7)
