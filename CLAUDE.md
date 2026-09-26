# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`farmish-backend` is the Go + Gin API for the Farmish Ghana agricultural marketplace, a greenfield rewrite. It's one of two repos checked out side by side in `~/work/farmish`:
- `farmish-backend` (this repo): built first
- `farmish-frontend`: TanStack Start, built after the backend v1 contract is frozen

`REDEVELOPMENT_PLAN.md` (in this repo) is the source of truth for both apps: architecture, locked decisions, conventions and the phased roadmap. Read it before doing anything.

## How to work here (mandatory)

- **One phase per task.** Find the first unchecked phase in `REDEVELOPMENT_PLAN.md` §6 whose "Depends on" phases are all `[x]`, and implement only that phase. Never one-shot multiple phases. If the user names a phase, confirm its dependencies are done first.
- A phase is done only when every "Done when" check passes. When it's done:
  - tick its checkbox
  - add a row to §9 Progress log
  - log any deviation in §8 Decisions log, plus an ADR in `docs/adr/`
- If a phase is too large, split it into sub-phases in the plan *before* coding. Out-of-scope discoveries go to §10 Backlog.
- Follow §5 Backend conventions in every phase:
  - identity comes from the verified token only
  - money is integer pesewas
  - webhooks, payouts and refunds are idempotent
  - state change + ledger + job enqueue happen in one DB transaction
  - public responses use safe projections
- Frontend phases (F0–F9) are implemented in `../farmish-frontend`, but their plan/ADR updates are committed here.

## Legacy reference

The old Next.js/Prisma app is at `~/work/farmgate` and is **read-only**. Use it to port business rules, e.g.:
- the category and promotion-tier seed in `prisma/seed.ts`
- the supply-request flow in `app/api/supply-requests`
- the Paystack webhook in `app/api/webhooks/paystack`

Never edit it or import from it.

## Commands

- `make db-up` / `db-down` / `db-reset`: compose Postgres 16 on `127.0.0.1:54320`. `db-reset` deletes the data
- `make auth-up`: Firebase Auth emulator on `127.0.0.1:9099` (project `demo-farmish`). `.env.example` points at it by default
- `make run`: runs the API on `:8080`, loading `.env` if present, else `.env.example`. Needs `make db-up` and `make migrate-up`
- `make build`: static binary at `bin/api`
- `make test`: starts compose Postgres + the Auth emulator, then `go test -race -count=1 ./...` with DB and auth tests required
  - single test: `TEST_DATABASE_URL='postgres://farmish:farmish@127.0.0.1:54320/farmish?sslmode=disable' FIREBASE_AUTH_EMULATOR_HOST=127.0.0.1:9099 go test -race -run TestName ./internal/...`
  - without `TEST_DATABASE_URL` / `FIREBASE_AUTH_EMULATOR_HOST`, DB / auth tests skip
- `make migrate-up`, `make migrate-down N=1|all`, `make migrate-version`: run `cmd/migrate` against `DATABASE_URL`
- `make grant-admin EMAIL=…` / `make revoke-admin EMAIL=…` (or `FUID=<firebase uid>`): sets `users.role` and mirrors the Firebase claim. The user must have signed in once
- `scripts/dev-token.sh <email> <password>`: prints an ID token for curl (emulator, or a real project with `FIREBASE_WEB_API_KEY`)
- `make migrate-new name=snake_case`: creates the next `migrations/NNNNNN_name.{up,down}.sql` pair
- `make generate`: regenerates `internal/http/api/api.gen.go` from `api/openapi.yaml` (oapi-codegen, pinned). Never hand-edit it
- `make api-lint`: Redocly lint of the spec (pinned Docker image, rules in `redocly.yaml`)
- `make sqlc`: regenerates `internal/db` from `db/queries/*.sql` + `migrations/` (pinned Docker image). Never hand-edit `internal/db`
- `make lint`: `go vet` plus golangci-lint v2, which is pinned and auto-installed into `bin/`
- `make fmt`: gofumpt + goimports
- `make docker-build`: builds the distroless image
- `make smoke`: builds the image, migrates a throwaway DB with `/migrate`, checks `/healthz`, `/readyz` (200, then 503 after cutting the DB network) and graceful SIGTERM
- `make ci`: **the required gate** before every push and to close a phase (ADR-0004). It runs tidy-check, fmt-check, api-lint, generate-check, sqlc-check, lint, test, vuln (govulncheck) and smoke

GitHub Actions is off for now (account billing lock). Never claim a phase is done without a green `make ci`.

**Adding an endpoint:** edit `api/openapi.yaml` (camelCase JSON/query, `$ref` the shared `Error`/`Money`/`PageMeta`, declare 4xx responses), run `make generate`, then implement the new method on `handlers.Server`. Requests are validated against the spec automatically.

**Auth is declared in the spec, not in code:**
- operations require a Firebase ID token by default
- `security: []` makes one public
- `x-farmish-role: admin` makes it admin-only

In handlers, get the caller with `users.FromContext(ctx)`. Never take a user ID from the request. Build errors with `apierror` (stable snake_case codes). Never return `err.Error()` to clients.

Auth tests use `authtest.EmailUser(t)` / `authtest.PhoneUser(t)` (real emulator accounts + tokens) and `authtest.UnsignedToken(claims)` for crafted bad tokens.

DB tests use `dbtest.Pool(t)` (fresh migrated database per test) or `dbtest.EmptyURL(t)` (unmigrated). Both are in `internal/database/dbtest`.

Update this section as each phase lands.
