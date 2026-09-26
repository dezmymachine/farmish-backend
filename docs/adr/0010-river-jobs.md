# ADR-0010: River job infrastructure

- **Status:** Accepted
- **Date:** 2026-09-26
- **Phase:** 5

## Decisions

1. **River's schema lives in our golang-migrate files.** Migration `000003_river_queue` is generated with `river migrate-get --all --exclude-version 1` (River v0.47.0, River schema versions 2–7).
   - River's own migration table (version 1) is excluded, because golang-migrate tracks versions. We keep one migration system, and `cmd/migrate` / the release step cover it.
   - `TestRiverSchemaVersionMatchesMigrations` fails when an upgraded River library knows a newer schema version than we've migrated to. The test explains how to generate the next migration.
   - sqlc sees the River tables and generates a few unused `River*` models. Harmless.

2. **`internal/jobs`:**
   - A `Registry` that every feature registers workers and periodic jobs on (`jobs.Register`, `reg.Every`). `cmd/api`'s `registry()` lists them all.
   - `NewClient` with explicit defaults: **10 attempts** (River's default is 25), River's `attempt^4` backoff (so ~7h of retries), a **1-minute** job timeout, and 10 workers on the default queue (`JOBS_MAX_WORKERS`).
   - Unique helpers `Unique()` (by kind + args) and `UniqueWithin(d)`. Job uniqueness is documented as a double-enqueue guard, **not** the idempotency mechanism. Completed jobs are pruned after 24h, so money-moving work must also be idempotent in its own tables.
   - A demo `NoopJob`.

3. **Enqueue inside the business transaction** with `client.InsertTx(ctx, tx, args, opts)`. Tests prove the core rules:
   - a job in a rolled-back transaction never exists or runs, and neither does its business row
   - a job in a committed transaction runs exactly once

4. **`RUN_MODE`** (`all` | `api` | `worker`, default `all`):
   - `api`: insert-only River client, no workers.
   - `worker`: runs workers and serves **only** `/healthz` and `/readyz` (`httpapi.NewProbeRouter`), so a worker process never exposes the API.
   - Periodic jobs run only on River's elected leader, so each fires once per interval across processes.

5. **Graceful shutdown under one budget.** `SHUTDOWN_TIMEOUT` is now the **total** SIGTERM-to-exit budget. The default dropped from 15s to 9s, below Docker's 10s kill grace period. The minimum is 3s.
   - HTTP drain and job stop run **concurrently**. Jobs get ⅔ of the drain to finish, then are cancelled.
   - The last ≤2s closes the pool.
   - `jobClient.Start` gets a non-cancelling context. Cancelling Start's context would abort running jobs immediately; shutdown goes through `jobs.Stop` (soft, then hard).
   - Found by the smoke test: with the database unreachable, River's stop keeps retrying cleanup (leader resign, unlisten) until its timeout. Sequential HTTP → jobs → pool timeouts exceeded 10s and got SIGKILLed.

6. **River UI is deferred** (Backlog). It's a browser app: behind our Bearer-token auth it would need a session or cookie flow we don't have. The admin guard exists (`middleware.RequireAuth` + `RequireAdmin`), so wiring it later is small.

## Consequences

- Upgrading River means bumping the module, running the schema-version test, and adding a migration if it fails.
- Worker-only deployments are a config change (`RUN_MODE=worker`), with no code split.
- Platform grace periods must be at least `SHUTDOWN_TIMEOUT` plus about 1s.
