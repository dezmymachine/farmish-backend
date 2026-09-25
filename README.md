# farmish-backend

Go + Gin API for Farmish Ghana. The web app lives in [farmish-frontend](https://github.com/dezmymachine/farmish-frontend).

`REDEVELOPMENT_PLAN.md` (architecture, conventions, phased roadmap for both apps) and `docs/adr/` live in this repo.

## Quick start

Requires Go 1.27+ and Docker.

```sh
cp .env.example .env    # optional; `make run` falls back to .env.example
make run                # serves on :8080
curl localhost:8080/healthz
```

## Make targets

| Target | What |
|---|---|
| `make run` | Run locally, loading `.env` (or `.env.example`) |
| `make build` | Static binary at `bin/api` |
| `make test` | `go test -race ./...` |
| `make lint` | `go vet` + golangci-lint (pinned, auto-installed into `bin/`) |
| `make fmt` | gofumpt + goimports |
| `make docker-build` | Build the distroless image |

## Configuration

Environment variables only; the service exits at startup listing every missing/invalid value. See `.env.example`.

## Layout

- `cmd/api`: wiring only (config → logger → router → server)
- `internal/config`: env loading and validation
- `internal/http`: router, server, `middleware/`, `handlers/`, `apierror/`
- `pkg/logger`: slog JSON logger + context helpers
