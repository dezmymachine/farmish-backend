# Engineering guide: how code is written here

This is the house style, with recipes. **Imitate the referenced files.** They're reviewed, tested and idiomatic for this repo. If a pattern you need isn't here, find the closest existing one and follow it. Don't invent a new style.

## 1. Architecture at a glance

```
HTTP request
 → middleware chain (internal/http/router.go):
     RequestID → ClientIP → AccessLog → Recovery → CORS → RateLimitIP
     → Authenticate (spec-driven) → RateLimitOperations → Turnstile → OpenAPIValidator
 → generated strict server (internal/http/api/api.gen.go)
 → handlers.Server method (internal/http/handlers/*.go)   thin: map request → service → map result/errors
 → domain service (internal/<domain>/service.go)          business rules, transactions, ledger, jobs
 → sqlc queries (internal/db, generated from db/queries/*.sql)
 → Postgres (pgxpool)
Background: River workers (internal/jobs registry, cmd/api registry()) run domain job handlers.
```

Layering rules:
- **handlers → domain → db.** Never the reverse.
- Domain packages don't import `internal/http`.
- Handlers never run SQL or hold business rules.
- Cross-domain calls go through the other domain's **service interface**, not its tables. Exception: read-only joins in sqlc queries for list/detail projections are fine.

## 2. Package layout for a domain

```
internal/<domain>/
  <domain>.go       exported types, enums/consts, sentinel errors, Clock
  service.go        type Service struct {...}; New(...); methods
  service_test.go   real-Postgres tests (dbtest.Pool)
  jobs.go           River args + workers for this domain (if any)
  jobs_test.go
  <client>.go       external API interface + real impl (payments, notify, media)
  fake/             in-memory fake of the external interface, used by tests of OTHER packages
```

Existing example: `internal/users/users.go`, which holds the type, sentinel errors, `Service`, and the context helpers.

**Sentinel errors.** Define them once per domain:

```go
var (
    ErrNotFound          = errors.New("listing not found")
    ErrForbidden         = errors.New("not the owner of this listing")
    ErrInvalidTransition = errors.New("invalid status transition")
    ErrConflict          = errors.New("listing slug conflict")
)
```

Wrap them with context: `fmt.Errorf("%w: listing %s", ErrNotFound, id)`. Check them with `errors.Is`.

**Validation errors** carry field details, so handlers can return `validation_failed` with details:

```go
// internal/validation/validation.go (create in the first phase that needs it; Phase 8)
type Error struct{ Fields []Field }        // implements error
type Field struct{ Name, Message string }  // Name is the camelCase API field or a JSON pointer
func (e *Error) Add(name, msg string)
func (e *Error) OrNil() error              // nil if no fields
```

Handlers map it to `apierror.WithDetails("validation_failed", ..., details with location "body")`.

## 3. Transactions: the one rule

A state change, its ledger entries and its job enqueue commit **together**. Use the helper (create it in Phase 8 as `internal/database/tx.go`):

```go
// InTx runs fn in a transaction. It commits if fn returns nil, otherwise rolls back.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
    tx, err := pool.Begin(ctx)
    if err != nil { return fmt.Errorf("begin: %w", err) }
    defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit
    if err := fn(tx); err != nil { return err }
    if err := tx.Commit(ctx); err != nil { return fmt.Errorf("commit: %w", err) }
    return nil
}
```

Usage inside a service:

```go
err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
    q := db.New(tx)
    order, err := q.GetOrderForUpdate(ctx, id)          // SELECT ... FOR UPDATE
    if err != nil { return mapNotFound(err) }
    if err := checkTransition(order.Status, to, actor); err != nil { return err }
    if _, err := q.UpdateOrderStatus(ctx, ...); err != nil { return err }
    if err := s.ledger.Post(ctx, tx, "escrow_release", order.ID.String(), entries...); err != nil { return err }
    _, err = s.jobs.InsertTx(ctx, tx, ReleaseEscrowArgs{OrderID: order.ID}, jobs.Unique())
    return err
})
```

- **Row locks:** use `SELECT ... FOR UPDATE` on the row whose state you check. For per-user balances, use `pg_advisory_xact_lock(hashtext('promo:' || $1))`.
- **Never** call external HTTP APIs (Paystack, mNotify, R2) inside a DB transaction. Enqueue a job in the tx; the job calls the API and records the result in a new tx.

## 4. Adding an endpoint (the recipe)

1. **Spec:** add the operation to `api/openapi.yaml`.
   - `operationId` is camelCase (e.g. `getListing`); tags match the domain.
   - Declare the request schema with `additionalProperties: false`, required fields and limits (minLength, maximum, pattern, enum) **matching DOMAIN.md**.
   - Declare responses: 2xx schema; 400 `BadRequest`; 401 `Unauthorized` (if authed); 403 `Forbidden`; 404 `NotFound`; 409 `$ref Conflict` (add a shared `Conflict` response if missing); 429 `TooManyRequests`.
   - Auth: omit `security` to require a token. Use `security: []` for public endpoints, and add `x-farmish-role: admin` for admin endpoints.
   - Abuse controls: `x-farmish-rate-limit: sensitive` for money, notification or anonymous writes; `x-farmish-turnstile: true` for anonymous forms.
   - Public projections get their own schema (`PublicSeller`, `ListingSummary`) with **no** private fields.
2. Run `make api-lint` and fix everything. Then run `make generate`.
3. **Handler:** implement the new method on `handlers.Server`, in a file per domain (`internal/http/handlers/listings.go`). The compiler lists what's missing.
4. **Deps:** add the service interface to `handlers.Server`, `httpapi.Deps` and `httpapi.UserService`-style composites. Wire it in `cmd/api/main.go`, and extend `newTestRouter` in tests.
5. **Tests:** see §8. At minimum:
   - the happy path with `assertContract`
   - 401 without a token (if authed)
   - 403 for another user's resource
   - 400 for an invalid body (validator)
   - 404 for an unknown id
   - 409 for a state conflict

**Handler shape.** Copy this; see `internal/http/handlers/me.go`:

```go
func (s Server) GetListing(ctx context.Context, req api.GetListingRequestObject) (api.GetListingResponseObject, error) {
    l, err := s.Listings.GetPublic(ctx, req.Slug)
    switch {
    case errors.Is(err, listings.ErrNotFound):
        return api.GetListing404JSONResponse{NotFoundJSONResponse: api.NotFoundJSONResponse(
            apierror.New(apierror.CodeNotFound, "Listing not found"))}, nil
    case err != nil:
        return nil, err // → generic 500 via internalError; never return err text to clients
    }
    return api.GetListing200JSONResponse(toListingDetail(l)), nil
}
```

- Get the caller with `u, ok := users.FromContext(ctx)` (see `me.go`) and pass `u.ID` into the service. **Never** read a user id from the request.
- `toXxx` mappers live next to the handler. They convert domain types to generated API types, and they are the place where private fields are left out.

## 5. SQL and sqlc

- **One file per domain:** `db/queries/<domain>.sql`. Name queries `Get<X>`, `Get<X>ForUpdate`, `List<X>s`, `Count<X>s`, `Insert<X>`, `Update<X><What>`, `Delete<X>`.
- Use `sqlc.narg('name')` for optional filters: `WHERE (sqlc.narg('region')::text IS NULL OR region = sqlc.narg('region'))`.
- **Pagination:** `LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset')`, plus a separate `Count<X>s` with the same `WHERE`. `offset = (page-1)*limit`, and `limit ≤ 50` (the spec enforces it).
- **Generated types:** `uuid` → `uuid.UUID`, `timestamptz` → `time.Time` (`*time.Time` when nullable), nullable text → `*string` (see `sqlc.yaml`). Convert in the service to domain types; see `users.fromRow`.
- **Enum-like columns:** `text` + `CHECK (col IN (...))`. No Postgres ENUM types: they're painful to migrate. Mirror them as Go `const`s in the domain package.
- **Money:** `bigint` columns named `*_pesewas`, with `CHECK (x >= 0)` where applicable.
- **Every table:** `id uuid PK DEFAULT gen_random_uuid()` (except natural keys), `created_at`/`updated_at timestamptz NOT NULL DEFAULT now()`, and the `set_updated_at` trigger (see `migrations/000002_create_users.up.sql`).
- **Indexes** go on every FK and every filter column used by list endpoints. Verify with `EXPLAIN` in a test when the phase says so.
- **Idempotency** uses unique constraints: `UNIQUE(reference)`, `UNIQUE(kind, reference)`, `UNIQUE(provider, event_id)`. On a duplicate insert, `ON CONFLICT DO NOTHING RETURNING` returns no row, which is `pgx.ErrNoRows`. Treat that as "already processed" (see `users.Resolve` for the pattern).

## 6. Migrations

- `make migrate-new name=create_listings` creates the next `NNNNNN_create_listings.{up,down}.sql`.
- Migration numbers in phase specs are indicative. Use `make migrate-new`, which always picks the next free number.
- One logical change per migration. `down` must fully undo `up` (`migrations_test` runs up → down → up and checks nothing is left behind).
- **Never edit a committed migration.** Fix forward with a new one.
- Seed data (categories, promotion tiers, the commission default) belongs in **idempotent Go seed commands** (`cmd/seed` in Phase 9) or in a data migration with `ON CONFLICT DO NOTHING`. Prefer the Go seed for reference data that changes; use a migration for defaults that code depends on, like the default commission row.

## 7. Background jobs (River)

See `internal/jobs/noop.go` and `internal/jobs/jobs.go`.

```go
// internal/orders/jobs.go
type AutoCompleteArgs struct{}
func (AutoCompleteArgs) Kind() string { return "orders.auto_complete" }

type AutoCompleteWorker struct {
    river.WorkerDefaults[AutoCompleteArgs]
    Svc *Service
}
func (w *AutoCompleteWorker) Work(ctx context.Context, _ *river.Job[AutoCompleteArgs]) error {
    return w.Svc.AutoComplete(ctx) // business logic lives in the service, testable without River
}
```

- **Kind names:** `<domain>.<verb>`. Args are small JSON: **ids only**, never whole rows and never secrets.
- Register in `cmd/api/main.go` `registry()`. Periodic jobs use `r.Every(interval, func() river.JobArgs { return X{} }, runOnStart)`.
- **Workers must be idempotent.** They may run more than once (retries, crashes). Re-check the state in the DB before acting.
- **Enqueue** with `InsertTx` inside the business tx. Use `jobs.Unique()` when duplicates would be harmful.
- **Testing:** test the service method directly (with an injected clock), plus one integration test that enqueues via `InsertTx` and waits for completion (see `internal/jobs/jobs_test.go` `waitCompleted`).

## 8. Testing recipes

**Real infrastructure** (never mock these):

```go
pool := dbtest.Pool(t)                 // fresh migrated database, dropped after the test
fb   := authtest.Firebase(t)           // Admin SDK bound to the emulator
eu   := authtest.EmailUser(t)          // real emulator account + ID token (eu.Token)
pu   := authtest.PhoneUser(t)          // phone sign-in through the emulator's SMS flow
rc, prefix := redistest.Client(t)      // real Redis, keys under prefix cleaned up
```

**Router-level tests.** See `internal/http/me_test.go` and `router_test.go`:
- `e2eRouter(t)` / `newTestRouter(t, Deps{...})` build the real router.
- `serve(t, r, req)` runs a request.
- `assertContract(t, req, w)` validates the response against `api/openapi.yaml`. **Call it on every success and error response you assert.**
- `assertErrorEnvelope(t, body, code)` checks the error shape and code.

**Service tests** live in the domain package, with `dbtest.Pool(t)`. Test business rules directly, without HTTP.

**Other patterns:**
- **Fixture specs** test middleware against operations that don't exist in the real spec (`internal/http/middleware/testdata/*.yaml`).
- **Time:** services take `Now func() time.Time` (default `time.Now`). Tests set a fixed clock and advance it. Never sleep for business timers.
- **External APIs:** the real client is tested with `httptest.NewServer` returning recorded-shape JSON (see `internal/turnstile/turnstile_test.go`). Other packages use `<pkg>/fake`.
- **Idempotency tests:** call the same webhook, job or operation twice, then assert a single effect (row counts, ledger sums, balances).
- **Concurrency tests:** where races matter (checkout stock, credits, find-or-create), run N goroutines and assert the invariant (see `TestResolve_ConcurrentFirstSignIn`, `TestRedis_ConcurrentExactlyBurst`).
- **Test names:** `Test<Unit>_<Behaviour>`, e.g. `TestCheckout_TamperedPriceIgnored`. Use table tests for validation matrices.
- **Security assertions:** public responses are unmarshalled into `map[string]any` and checked for the **absence** of forbidden keys (`phone`, `email`, `firebaseUid`, `role`, …).

## 9. Config

See `internal/config/config.go`.

- Add a field to `Config` (or a sub-struct), parse and validate it in `FromLookup`, and collect errors. Never exit early.
- Required-when-deployed variables use `cfg.Env == EnvStaging || cfg.Env == EnvProduction` checks.
- **Never** echo secret values in errors.
- Add tests in `config_test.go`: defaults, overrides, and each invalid case (the `base(...)` helper).
- Document the variable in `.env.example` (with where to get it) and in `REDEVELOPMENT_PLAN.md` §7.

## 10. Logging

- Get the request logger with `logger.FromContext(ctx)`. It already carries `request_id` (and `user_id` after auth).
- Levels:
  - **Info:** notable business events (`order paid`, `payout sent`).
  - **Warn:** degraded behaviour.
  - **Error:** something failed that needs attention.
- Use structured attrs: `slog.String("order_id", id.String())`.
- **Never** log tokens, full phones (`maskPhone("+233241234567") = "+233*****4567"`, to be added in `internal/geo` or `pkg/redact` in Phase 7), account numbers, ID numbers, or raw webhook bodies. Log `event`, `reference` and `status` instead.

## 11. External services (Paystack, mNotify, R2)

The pattern is an interface in the domain, a real client, and a fake:

```go
// internal/payments/paystack.go
type Paystack interface {
    InitializeTransaction(ctx context.Context, in InitializeInput) (InitializeResult, error)
    VerifyTransaction(ctx context.Context, reference string) (Transaction, error)
    // ...
}
type PaystackClient struct { secret, baseURL string; http *http.Client } // real, 10s timeout
// internal/payments/fake/paystack.go : programmable fake (record calls, script results)
```

- The real client is tested with `httptest.NewServer`: assert the request method, path, auth header and body; feed realistic JSON.
- Timeouts are always set. Errors never include secrets.
- Base URLs are configurable, so tests point at an httptest server.

## 12. Style

- Go: gofumpt + goimports (`make fmt`), golangci-lint v2 clean (`make lint`). No `//nolint` without a reason comment (see `internal/config/config.go`, G304).
- **Doc comments** on every exported identifier. Explain *why*, not *what*. Match the comment density of the surrounding code.
- **Naming:** no stutter (`listings.Service`, not `listings.ListingService`); receivers are short (`s *Service`).
- Keep functions small; extract helpers when a function passes about 60 lines.
- `context.Context` is the first parameter everywhere I/O happens.
- **Don't** add dependencies casually. Check the standard library and existing deps first, and pin versions. The Makefile pins its tool versions.

## 13. Before you say "done"

- [ ] `make ci` green (full output ends with `ci: all checks passed`)
- [ ] Every Done-when item mapped to a named test
- [ ] The manual QA script run, with its output captured
- [ ] Plan checkbox, §8/§9 rows, ADR(s), `.env.example`, §7, `AGENTS.md` commands (if new)
- [ ] `docs/reviews/phase-NN.md` written
- [ ] `git status` clean (in particular, `.env` not staged)
