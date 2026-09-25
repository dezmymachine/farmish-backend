# Farmish Redevelopment Plan: Go (Gin) Backend + TanStack Start Frontend

## 0. How to use this plan

- The work is split into **phases**. Each task/PR delivers **exactly one phase**. Never one-shot several.
- Start a phase only when all of its **Depends on** phases are checked `[x]`.
- Every phase lists **Scope**, **Tasks**, and **Done when** (acceptance checks). A phase is done only when every "Done when" item passes locally and in CI.
- At the end of a phase:
  - tick its checkbox in §6
  - add a one-line entry to the §9 Progress log (date, phase, PR/commit, notes)
  - record any deviation from this plan in §8 Decisions log
- If a phase turns out too big, split it into `Nx.1`, `Nx.2`, … here *before* coding.
- Out-of-scope items discovered mid-phase go to §10 Backlog, not into the current phase.
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
| Auth | **Firebase**: social + email/password via client SDK. **Phone**: Gin + **mNotify** OTP → Firebase custom token. No account linking in v1 |
| SMS | mNotify/BMS (`sms_type: "otp"`). We generate and hash codes; mNotify only delivers |
| Media | **Cloudflare R2** presigned uploads + CDN |
| Payments | **Paystack** (card + MoMo). **Farmish is merchant of record**: funds are held in escrow on the Paystack balance and released to sellers via **Paystack Transfers** minus commission |
| Delivery | v1 stub: delivery method/address/fee/statuses on orders, `DeliveryProvider` interface with a `manual` impl. Courier integration later |
| Money | **Integer pesewas (`bigint`)** everywhere in DB and API (`amount` + `currency: "GHS"`). Frontend formats for display |
| Scope | Full rewrite, no data/user migration |

## 3. Target architecture

```
farmish-frontend (TanStack Start) ──HTTPS──▶ Cloudflare (DNS/CDN/WAF/Turnstile, R2 CDN)
                                                  │
                                                  ▼
                                   farmish-backend (Gin on Railway)
                                    ├── Firebase Admin (verify ID tokens, custom tokens, claims)
                                    ├── Neon Postgres (pgx + sqlc, golang-migrate)
                                    ├── River (jobs + periodic jobs, same process in v1)
                                    ├── R2 (presigned PUT, public CDN GET)
                                    ├── Paystack (charges, refunds, transfers, webhooks)
                                    └── mNotify (OTP + transactional SMS)
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
    api/openapi.yaml             # source of truth for the HTTP contract
    migrations/                  # NNNNNN_name.{up,down}.sql
    db/queries/*.sql  sqlc.yaml  # sqlc input → internal/db (generated)
    internal/
      config/ http/ (router, middleware, handlers, api.gen.go) auth/ users/
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
  - GitHub Actions: `go vet`, golangci-lint, `go test`, docker build + `/healthz` smoke test.
- **Done when:** `make run` then `curl /healthz` returns 200 JSON, `make lint test` passes, the Docker image builds and serves `/healthz`, and CI is green.
- [ ] Phase 1

### Phase 2: Database foundation
- **Depends on:** 1
- **Tasks:**
  - `docker-compose.yml` with `postgres:16`.
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
- [ ] Phase 2

### Phase 3: API contract & codegen
- **Depends on:** 1
- **Tasks:**
  - Create `api/openapi.yaml` with info, servers, security scheme (`bearerAuth`), shared schemas (`Error`, `Money{amount:int64,currency}`, `PageMeta`), and `/healthz`, `/readyz`.
  - Set up `oapi-codegen` (Gin strict server) with `make generate` and CI drift check (`git diff --exit-code`).
  - Add Spectral/Redocly lint in CI and request validation middleware against the spec.
  - Error-envelope helper.
- **Done when:** handlers are served through generated interfaces, spec lint passes, an invalid request returns a 400 envelope, and the CI drift check works.
- [ ] Phase 3

### Phase 4: Auth core (Firebase) + users
- **Depends on:** 2, 3
- **Tasks:**
  - Migration: `users` (`id uuid`, `firebase_uid unique`, `signup_method social|email|phone`, `email citext`, `phone_e164`, `display_name`, `role user|admin`, `seller_verified`, timestamps).
  - Firebase Admin init from config.
  - Auth middleware: Bearer ID token → verify → find-or-create `users` row → context.
  - `RequireAuth` / `RequireAdmin` guards.
  - `GET /v1/me`, `PATCH /v1/me`.
  - Role sync: custom claims ↔ `users.role`, plus an admin CLI `make grant-admin`.
  - Tests use the Firebase Auth emulator.
- **Done when:** a valid emulator token returns `/v1/me`, missing/expired/forged tokens return 401 (generic), and non-admins get 403 on an admin test route.
- [ ] Phase 4

### Phase 5: Jobs infrastructure (River)
- **Depends on:** 2
- **Tasks:**
  - River migrations and a client plus workers in the same process (config flag to run API-only / worker-only).
  - `jobs` package with a registration pattern, retry/backoff defaults, and unique-job helpers.
  - Periodic job scheduler.
  - A demo `NoopJob` enqueued inside a DB tx.
  - Optional River UI behind an admin guard.
- **Done when:** a job enqueued in a rolled-back tx never runs and one in a committed tx runs exactly once, and a periodic job fires in tests.
- [ ] Phase 5

### Phase 6: Rate limiting & abuse controls
- **Depends on:** 2, 4
- **Tasks:**
  - `ratelimit` package with token buckets keyed per-IP and per-user. Start in-process with an interface for a Postgres/Redis backend later.
  - Gin middleware that emits `Retry-After` + `X-RateLimit-*`.
  - Client IP from `CF-Connecting-IP` (trusted only behind Cloudflare).
  - Cloudflare Turnstile verification helper for sensitive public endpoints.
- **Done when:** exceeding the limit returns 429 with headers, and a Turnstile failure returns a 400 envelope. Tests cover both.
- [ ] Phase 6

### Phase 7: Phone OTP login (mNotify)
- **Depends on:** 4, 5, 6
- **Tasks:**
  - Migration: `phone_verifications` (phone_e164, code_hash, purpose `login|step_up`, expires_at, attempts, consumed_at, ip).
  - `POST /v1/auth/phone/request`:
    - normalise to +233
    - Turnstile check, plus per-IP and per-phone limits with a 60s resend cooldown
    - crypto-random 6-digit code, store the hash
    - enqueue `SendPhoneOTP`
    - always return a generic 202
  - `POST /v1/auth/phone/verify`: expiry, attempts ≤ 5, constant-time compare, consume → find-or-create Firebase user by phone → custom token.
  - `notify` package with an mNotify client (sandbox/fake in tests).
  - The `step_up` purpose is reused later by payouts.
- **Done when:** the end-to-end test (with a fake mNotify) mints a custom token that the emulator accepts, and expired, reused, and brute-forced codes are all rejected.
- [ ] Phase 7

### Phase 8: Profiles & seller onboarding
- **Depends on:** 4
- **Tasks:**
  - Migration: `seller_profiles` (user_id, business_name, region, district, bio, contact-visibility flags, verification_status, id_type/id_number encrypted-at-rest or omitted until needed).
  - Endpoints:
    - `GET/PUT /v1/me/seller-profile`
    - `GET /v1/sellers/{id}` (safe projection)
    - admin `POST /v1/admin/sellers/{id}/verify`, which sets the `seller_verified` claim
  - Audit log table `audit_events` (actor, action, target, metadata), used from here on.
- **Done when:** the public seller response contains only safe fields (asserted by a test), and verification writes an audit event and updates the claim.
- [ ] Phase 8

### Phase 9: Catalog (categories + attributes)
- **Depends on:** 2, 3
- **Tasks:**
  - Migrations: `categories` (self-referencing tree, slug, sort_order, is_active) and `category_attributes` (key, label, type, options, required).
  - Seed command ported from legacy `~/work/farmgate/prisma/seed.ts` categories.
  - `GET /v1/categories` (tree) and `GET /v1/categories/{slug}` with attributes. Admin CRUD.
  - `Cache-Control` on public GETs.
- **Done when:** the seed is idempotent and the tree endpoint matches the seed data.
- [ ] Phase 9

### Phase 10: Media uploads (R2)
- **Depends on:** 4
- **Tasks:**
  - `media` package with an S3-compatible R2 client.
  - `POST /v1/media/upload-url`: content-type allowlist, max size, key `listings/{user}/{uuid}.{ext}`, short expiry.
  - `media_objects` table (owner, key, status `pending|attached`).
  - Periodic cleanup of orphaned pending uploads.
- **Done when:** a presigned PUT works against R2 (or MinIO in compose), a disallowed type or size is rejected, and orphans are cleaned in the job test.
- [ ] Phase 10

### Phase 11: Listings CRUD
- **Depends on:** 8, 9, 10
- **Tasks:**
  - Migrations:
    - `listings`: seller_id, category_id, title, slug unique, description, `price_pesewas bigint`, unit, quantity_available, min_order_qty, condition, status `draft|active|sold|expired|archived`, region, district, `expires_at default now()+30d`
    - `listing_images` (media key, sort)
    - `listing_attribute_values`
  - Slug generation with a uniqueness loop.
  - Validate attributes against the category schema.
  - Owner-only update/delete.
  - Periodic job `ExpireListings`.
- **Done when:**
  - create → get → update → archive works
  - a non-owner gets 403
  - invalid attributes return 400
  - the expiry job flips past-due listings
- [ ] Phase 11

### Phase 12: Search & browse
- **Depends on:** 11
- **Tasks:**
  - `GET /v1/listings` with `q` (Postgres FTS / `pg_trgm`), `category`, `region`, `district`, `minPrice`, `maxPrice`, `condition`, `sort`, `page`, `limit ≤ 50`.
  - Promoted listings ranked first (the column is ready for Phase 14).
  - Indexes on seller_id, category_id, status, region, district, expires_at, slug.
  - `ETag`/`Cache-Control`.
  - `GET /v1/listings/{slug}` returns a safe seller projection.
- **Done when:** filter combinations are tested, `EXPLAIN` shows index use for the main filters, and responses contain no private seller fields.
- [ ] Phase 12

### Phase 13: Payments core (Paystack) + ledger foundation
- **Depends on:** 5, 4
- **Tasks:**
  - `payments` package with a Paystack client (initialize, verify, refund, transfer recipient, transfer, resolve account) and a fake for tests.
  - Migrations:
    - `payments`: reference unique, purpose `promotion|checkout`, `amount_pesewas`, currency, status `pending|success|failed|abandoned`, channel, raw payload, paid_at
    - `webhook_events`: provider, event_id/reference unique, type, payload, processed_at
  - `POST /v1/webhooks/paystack`: raw body, HMAC-SHA512 check against `x-paystack-signature`, insert `webhook_events` (a duplicate returns 200 no-op), dispatch by `event` type to registered handlers.
  - `GET /v1/payments/verify/{reference}` as a fallback poll.
  - `ledger` package:
    - `ledger_accounts`, `ledger_transactions`, `ledger_entries` (double-entry, append-only, sum per tx = 0)
    - `Post(tx, entries…)` API
    - balance queries
- **Done when:**
  - a bad signature returns 401
  - a replayed event is processed once
  - an amount/currency mismatch is rejected and logged
  - a ledger post with non-zero sum errors
  - a property test holds ledger balances consistent
- [ ] Phase 13

### Phase 14: Promotions (first real payment flow)
- **Depends on:** 12, 13
- **Tasks:**
  - Migrations: `promotion_configs` (tier, price_pesewas, credits, duration_days, active) and `listing_promotions` (listing, tier, starts/ends).
  - Credits are held as ledger account `promo_credits:{user}`.
  - `GET /v1/promotions/configs` and `POST /v1/promotions/purchase` (Paystack init, `purpose=promotion`).
  - Webhook handler `charge.success(promotion)` → `GrantPromotionCredits` (idempotent on reference).
  - `POST /v1/promotions/apply` spends credits and promotes the listing.
  - Seed tiers from `~/work/farmgate/prisma/seed.ts`.
- **Done when:** the end-to-end run (with a fake Paystack) goes purchase → webhook → credits → apply → listing ranked as promoted, and a webhook replay doesn't double-credit.
- [ ] Phase 14

### Phase 15: Checkout & orders (escrow hold)
- **Depends on:** 13, 12
- **Tasks:**
  - Migrations:
    - `checkouts`: buyer, total_pesewas, payment_reference, status, idempotency_key unique
    - `orders`: checkout_id, buyer_id, **single seller_id**, subtotal/delivery_fee/commission/total pesewas, `commission_rate_bps` snapshot, status, `escrow_state none|held|released|refunded`, timestamps, `auto_complete_at`
    - `order_items`: listing snapshot of title/unit/price/qty
    - `order_events`: actor, from→to, note
    - `commission_configs`: category or default, rate_bps
  - Delivery stub fields on `orders`: `delivery_method pickup|seller_delivery|courier`, address, region, district, recipient_name, recipient_phone, delivery_fee_pesewas, tracking_ref, delivered_at.
  - `delivery` package with a `DeliveryProvider` interface (`Quote`, `CreateShipment`, `Track`) and a `manual` impl (seller-set/flat fee).
  - `POST /v1/checkout`:
    - `Idempotency-Key`, cart items
    - server-side price lookup, stock check and **reservation**
    - one order per seller
    - Paystack init for the total
  - Webhook `charge.success(checkout)` → `ConfirmCheckout` job:
    - verify amount/currency
    - mark payment success
    - orders `pending_payment→paid`
    - ledger: `buyer_clearing → escrow`
    - `escrow_state=held`
    - notify sellers
  - Periodic `ExpireUnpaidCheckouts` releases reservations.
  - `GET /v1/orders`, `GET /v1/orders/{id}` (buyer or seller of that order only).
- **Done when:**
  - a two-seller cart produces two orders and one charge
  - a tampered client price is ignored
  - an amount mismatch leaves the order unpaid and raises an alert log
  - webhook replay makes no double escrow
  - unpaid checkouts expire and restore stock
- [ ] Phase 15

### Phase 16: Fulfilment state machine
- **Depends on:** 15
- **Tasks:**
  - Status machine enforced in one place (`orders.Transition`) with a unit test covering the full table:
    `pending_payment → paid → accepted → fulfilling → shipped → delivered → completed`, plus side exits `cancelled`, `disputed`, `refunded`, `expired`.
  - Seller endpoints: `POST /v1/seller/orders/{id}/{accept,reject,ship,mark-delivered}`.
  - Buyer endpoints: `POST /v1/orders/{id}/{cancel,confirm-receipt,dispute}`.
  - Jobs:
    - `AutoCancelUnaccepted`: seller silent for X h → cancel → enqueue refund
    - `AutoCompleteOrders`: N days after `delivered` with no dispute → `completed`
  - Every transition writes `order_events` and notifies (SMS via `notify`).
- **Done when:** illegal transitions return 409, each actor can do only their own transitions, and the timers are tested with a fake clock.
- [ ] Phase 16

### Phase 17: Escrow release, refunds & disputes
- **Depends on:** 16
- **Tasks:**
  - On `completed` → `ReleaseEscrow` (unique per order). Ledger: `escrow → seller_payable:{seller}` (net) and `escrow → platform_commission`. `escrow_state=released`.
  - `refunds` table (order, amount, reason, paystack refund id, status).
  - `RefundOrder` job, allowed only while `escrow_state=held`. Paystack Refund API. Webhook `refund.*` handlers. Ledger `escrow → refunds`.
  - Disputes table and admin `POST /v1/admin/disputes/{id}/resolve` (`refund_buyer | release_seller | partial`), audit-logged.
- **Done when:**
  - release and refund are each idempotent
  - no refund after release (it goes to the manual path)
  - ledger totals reconcile: escrow balance = sum of held orders
- [ ] Phase 17

### Phase 18: Seller payouts (Paystack Transfers)
- **Depends on:** 17, 7
- **Tasks:**
  - `seller_payout_accounts`: type `mobile_money|ghipss`, bank/network code, number (encrypted), masked, account_name, `recipient_code`, verified_at, cooldown_until.
  - `GET|PUT /v1/seller/payout-account`:
    - resolve the account via Paystack and match the name
    - **step-up OTP** (Phase 7 `step_up`)
    - create the transfer recipient
    - 24–48h cooldown on change
  - `payouts` table (seller, amount, reference unique, transfer_code, status `queued|pending|success|failed|reversed`, order ids).
  - `ExecutePayout` job (batched per seller, min payout threshold) debits `seller_payable` into `payout_clearing`.
  - Webhooks `transfer.success|failed|reversed`. Failed/reversed payouts re-credit `seller_payable`.
  - `ReconcileTransfers` daily job.
  - `GET /v1/seller/balance`, `GET /v1/seller/payouts`. Admin `POST /v1/admin/payouts/{id}/retry`.
- **Done when:**
  - the end-to-end run (with a fake Paystack) goes completed order → payable → payout → transfer.success
  - a failed transfer restores the balance
  - no payout during cooldown
  - the payout account is never returned unmasked
- [ ] Phase 18

### Phase 19: Messaging
- **Depends on:** 11
- **Tasks:** `conversations` (unique listing_id + buyer_id) and `messages`. Endpoints to list/create conversations, list/send messages, and mark read. Participant-only access. Rate limits. Optional order link on a conversation.
- **Done when:** a non-participant gets 403, pagination works, and a duplicate conversation returns the existing one.
- [ ] Phase 19

### Phase 20: Reviews, favorites, reports, supply requests
- **Depends on:** 16 (reviews require a completed order), 11
- **Tasks:**
  - `reviews` (unique order/listing + reviewer, only after `completed`)
  - `favorites` (unique user + listing)
  - `reports` with an admin moderation queue
  - `supply_requests` with a status flow ported from `~/work/farmgate` (`app/api/supply-requests`)
- **Done when:** a review without a completed order is rejected, and the uniqueness constraints are tested.
- [ ] Phase 20

### Phase 21: Observability & hardening
- **Depends on:** 18
- **Tasks:**
  - `/metrics` (Prometheus: HTTP, DB pool, River queue depth, payment/payout counters).
  - PII redaction in logs. Audit coverage for payments, payouts, refunds, disputes, and verification.
  - Security headers.
  - k6 smoke tests for browse, checkout (fake Paystack), and webhook.
  - Alerts list (mNotify balance low, payout failures, reconciliation mismatches).
- **Done when:** k6 thresholds pass locally and there are no secrets/OTPs/phones in a log sample.
- [ ] Phase 21

### Phase 22: Deploy (Railway + Neon + Cloudflare) & contract freeze
- **Depends on:** 21
- **Tasks:**
  - Neon project with branches (dev/prod). Railway service with env from §7. Migrations run as a release step.
  - Cloudflare DNS, WAF rules, Turnstile keys, R2 bucket + CDN domain.
  - Paystack live webhook URL. Transfers enabled with OTP disabled (see §11).
  - Tag the OpenAPI spec `v1.0.0` (**contract freeze**).
- **Done when:**
  - staging passes smoke tests end-to-end with Paystack test mode
  - `/readyz` is green
  - the spec is tagged
- [ ] Phase 22

### Frontend phases (start after Phase 22 freezes the contract; F0–F2 may start after Phase 12)
- [ ] **F0 Scaffold:** TanStack Start app in `farmish-frontend/`, TS strict, Tailwind v4 + shadcn, lint/format, CI, deploy target decided (ADR).
- [ ] **F1 API client:** typed client generated from `api/openapi.yaml`, auth header injection, error envelope handling, money formatting (pesewas → GHS).
- [ ] **F2 Auth:** Firebase social/email, phone OTP UI (request/verify → `signInWithCustomToken`), session persistence, route guards.
- [ ] **F3 Browse:** home, categories, search with filters, listing detail.
- [ ] **F4 Sell:** seller onboarding, listing create/edit with R2 uploads, my listings.
- [ ] **F5 Checkout:** cart, delivery details, Paystack inline/redirect, payment status page.
- [ ] **F6 Orders:** buyer and seller order dashboards, fulfilment actions, confirm receipt, disputes.
- [ ] **F7 Payouts:** payout account setup (step-up OTP), balance, payout history.
- [ ] **F8 Engagement:** messaging, reviews, favorites, reports, supply requests, promotions purchase/apply.
- [ ] **F9 Launch:** SEO/meta, performance pass, accessibility pass, production deploy, legacy app retirement plan.

## 7. Environment variables (backend)
`APP_ENV`, `PORT`, `LOG_LEVEL`, `SHUTDOWN_TIMEOUT`, `DATABASE_URL`, `CORS_ORIGINS`, `FIREBASE_PROJECT_ID`, `FIREBASE_CREDENTIALS_JSON`, `MNOTIFY_API_KEY`, `MNOTIFY_SENDER`, `OTP_TTL_MINUTES`, `OTP_MAX_ATTEMPTS`, `TURNSTILE_SECRET`, `R2_ACCOUNT_ID`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_BUCKET`, `R2_PUBLIC_BASE_URL`, `PAYSTACK_SECRET_KEY`, `PAYSTACK_PUBLIC_KEY`, `DATA_ENCRYPTION_KEY`, `ESCROW_AUTO_COMPLETE_DAYS`, `SELLER_ACCEPT_TIMEOUT_HOURS`, `PAYOUT_MIN_PESEWAS`.

## 8. Decisions log
| Date | Decision | ADR |
|---|---|---|
| 2026-09-25 | Adopt §2 locked decisions; escrow + Transfers payment model; delivery stubbed | ADR-0001 |
| 2026-09-25 | Phase 1: module path `github.com/dezmymachine/farmish-backend`; minimal error envelope pulled forward from Phase 3; optional `LOG_LEVEL`/`SHUTDOWN_TIMEOUT` env vars; Gin always in release mode | ADR-0002 |
| 2026-09-25 | Split into two repos (`farmish-backend`, `farmish-frontend`); `~/work/farmish` is a plain folder; plan + ADRs live in the backend repo | ADR-0003 |

## 9. Progress log
| Date | Phase | PR/commit | Notes |
|---|---|---|---|
| 2026-09-25 | 0 | initial commits (both repos) | Redone after the two-repo split (ADR-0003). `.gitignore` excludes `.env*` but keeps `!.env.example` |
| 2026-09-25 | 1 | Phase 1 commit | Local checks pass: `make run` + `/healthz` 200 JSON, `make lint test`, image builds (26.5 MB distroless/nonroot) and serves `/healthz`, workflow passes actionlint. **Checkbox unticked until CI goes green after the first push**. See ADR-0002 |

## 10. Backlog (not scheduled)
- Courier integration via `DeliveryProvider`, and provider-quoted delivery fees
- Account linking across Firebase methods
- Hyperdrive pooling after Neon connection pressure is observed
- Temporal behind the `Workflow` interface
- Redis/Upstash rate-limit backend under abuse
- Seller reserve/holdback against chargebacks

## 11. Open items / risks
- **Regulatory:** holding customer funds (escrow) in Ghana may fall under the Bank of Ghana Payment Systems and Services Act, 2019 (Act 987). Get legal advice before Phase 22 go-live. Fallback: Paystack split payments with delayed settlement.
- **Paystack setup:**
  - confirm GH Transfers are enabled and transfer OTP can be disabled for automation
  - keep settlement on the Paystack balance
  - confirm MoMo recipient support for all networks
- **Fees:** decide who bears Paystack charge and transfer fees (buyer, seller, or platform) and whether they're shown at checkout. Decide before Phase 15.
- **Chargebacks after release:** the platform bears the loss. See the reserve backlog item.
- **mNotify:** sender ID `FARMISH` approval (ship with the default sender) and low-balance alerting.
