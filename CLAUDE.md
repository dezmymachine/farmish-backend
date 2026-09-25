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

- `make run`: runs the API on `:8080`, loading `.env` if present, else `.env.example`
- `make build`: static binary at `bin/api`
- `make test`: `go test -race -count=1 ./...`. For a single test: `go test -race -run TestName ./internal/http/...`
- `make lint`: `go vet` plus golangci-lint v2, which is pinned and auto-installed into `bin/`
- `make fmt`: gofumpt + goimports
- `make docker-build`: builds the distroless image

CI is `.github/workflows/ci.yml`: vet, lint, test, then a docker build with a `/healthz` smoke test.

Phase 2 adds `make migrate-up|migrate-down|sqlc`. Update this section as each phase lands.
