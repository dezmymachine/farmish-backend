# ADR-0005: Database foundation choices

- **Status:** Accepted
- **Date:** 2026-09-25
- **Phase:** 2

## Decisions

1. **Migrations run through our own `cmd/migrate`, not the golang-migrate CLI.**
   - It uses the golang-migrate library with the `pgx5` driver and migrations embedded via `embed.FS` (`migrations/`).
   - Nobody has to install a CLI built with the right tags, tests apply exactly the embedded files, and the binary ships in the Docker image as `/migrate`. Phase 22's release step is then `docker run --entrypoint /migrate <image> up`.
   - Make targets: `migrate-up`, `migrate-down N=…|all`, `migrate-version`, `migrate-new name=…`.

2. **sqlc runs from its pinned Docker image** (`sqlc/sqlc:1.31.1`), because sqlc needs cgo to build.
   - `make ci` runs `sqlc diff` + `sqlc vet`, so generated code can't drift.
   - sqlc reads the schema straight from `migrations/`.

3. **Postgres pool** (`internal/database`, a package not listed in §4):
   - connect timeout 5s
   - max conn lifetime 30m (±10% jitter)
   - idle 5m
   - health check every 30s
   - server-side `statement_timeout` from `DB_STATEMENT_TIMEOUT` (default 30s)
   - `application_name=farmish-api`

   Parameters set in the URL win over these defaults. The API pings at startup and exits if the DB is unreachable. `/readyz` then tracks the DB at runtime (2s ping budget, generic 503).

4. **Pool shutdown is bounded to 5s.** The Phase 2 smoke test found that with the DB unreachable, `pgxpool.Close()` hangs on the connection whose ping timed out, so SIGTERM ended in SIGKILL. `database.Close` now gives up after 5s.

5. **Test isolation: one fresh database per test.** `dbtest.EmptyURL` / `dbtest.Pool` create `farmish_test_<random>` on `TEST_DATABASE_URL` and `DROP … WITH (FORCE)` it on cleanup.
   - Without `TEST_DATABASE_URL` the DB tests skip, unless `DBTEST_REQUIRED=1`. `make test` sets both, so the gate never skips them.
   - Per-test databases are cheap while the migration set is small. If they get slow, switch to a migrated template database.

6. **Compose Postgres listens on host port 54320**, not 5432, to avoid a locally installed Postgres. `FARMISH_PG_PORT` overrides it. The network is named `farmish-backend` so the smoke test can attach to it.

7. **New env vars:** `DATABASE_URL` (required, `postgres://` or `postgresql://`), `DB_MAX_CONNS` (default 10, 1–100) and `DB_STATEMENT_TIMEOUT` (default 30s).

8. **`make smoke` got stronger.** It now:
   - migrates a fresh `farmish_smoke` DB with the image's `/migrate`
   - checks `/healthz` and `/readyz`
   - cuts the container's DB network and expects `/readyz` 503 while `/healthz` stays 200
   - checks that SIGTERM exits 0
