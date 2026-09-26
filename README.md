# farmish-backend

Go + Gin API for Farmish Ghana. The web app lives in [farmish-frontend](https://github.com/dezmymachine/farmish-frontend).

`REDEVELOPMENT_PLAN.md` (architecture, conventions, phased roadmap for both apps) and `docs/adr/` live in this repo.

## Quick start

Requires Go 1.27+ and Docker.

```sh
cp .env.example .env    # optional; `make run` falls back to .env.example
make db-up auth-up      # Postgres 18 on :54320, Firebase Auth emulator on :9099
make migrate-up
make run                # serves on :8080
curl localhost:8080/healthz localhost:8080/readyz
T=$(FIREBASE_AUTH_EMULATOR_HOST=127.0.0.1:9099 scripts/dev-token.sh me@farmish.test 'pass1234')
curl -H "Authorization: Bearer $T" localhost:8080/v1/me
```

## Make targets

| Target | What |
|---|---|
| `make run` | Run locally, loading `.env` (or `.env.example`) |
| `make build` | Static binary at `bin/api` |
| `make test` | `go test -race ./...`, including DB tests against compose Postgres |
| `make db-up` / `db-down` / `db-reset` | Compose Postgres (`db-reset` wipes data) |
| `make auth-up` | Firebase Auth emulator |
| `make grant-admin EMAIL=…` / `revoke-admin` | Change a user's role (DB + Firebase claim) |
| `make migrate-up` / `migrate-down N=…` / `migrate-version` / `migrate-new name=…` | Migrations |
| `make generate` | Regenerate the API server code from `api/openapi.yaml` |
| `make api-lint` | Lint the OpenAPI spec (Redocly) |
| `make sqlc` | Regenerate `internal/db` |
| `make lint` | `go vet` + golangci-lint (pinned, auto-installed into `bin/`) |
| `make fmt` | gofumpt + goimports |
| `make docker-build` | Build the distroless image |
| `make smoke` | Build the image, check `/healthz` and graceful shutdown |
| `make vuln` | govulncheck |
| `make ci` | Every check above, plus tidy/fmt checks. **Run before every push** (GitHub Actions is off, see ADR-0004) |

## Configuration

Environment variables only; the service exits at startup listing every missing/invalid value. See `.env.example`.

## Layout

- `cmd/api`: wiring only (config → logger → db → router → server)
- `internal/config`: env loading and validation
- `internal/auth`: Firebase ID-token verification and custom claims; `authtest/` creates emulator users
- `internal/users`: maps verified identities to `users` rows
- `internal/ratelimit`: token-bucket limiter; `internal/turnstile`: Cloudflare Turnstile verification
- `internal/jobs`: River background jobs (registry, client, unique helpers); schema in `migrations/000003_river_queue`
- `internal/database`: pgx pool; `dbtest/` gives each test a throwaway database
- `internal/db`: sqlc-generated queries (from `db/queries/`)
- `migrations/`: embedded SQL migrations; `cmd/migrate` applies them
- `api/openapi.yaml`: the HTTP contract (source of truth)
- `internal/http`: router, server, `middleware/` (incl. OpenAPI request validation), `handlers/` (implements the generated interface), `apierror/`, `api/` (generated)
- `pkg/logger`: slog JSON logger + context helpers
