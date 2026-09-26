# ADR-0009: Postgres 18 locally, to match Neon

- **Status:** Accepted
- **Date:** 2026-09-26

## Context

Phase 2 pinned the local/test database to `postgres:16`. The Neon project, which will be staging/production, runs **Postgres 18.6**. Testing against a different major version risks behaviour differences: planner changes, new defaults, deprecations. Those would only show up on Neon.

## Decision

- `docker-compose.yml` uses `postgres:18`, following 18.x minor releases, so dev, tests, `make ci` and the smoke test all run on Neon's major version.
- Postgres 18 images store data under `/var/lib/postgresql/<major>/docker`. The compose volume is `pgdata18`, mounted at `/var/lib/postgresql`.
- The old Postgres 16 volume (`farmish-backend_pgdata`) is not reused, because 18 can't read 16's files without `pg_upgrade`. It's left in place, not deleted. Local data is disposable and is recreated with `make migrate-up`.
- When Neon moves to a new major version, bump the compose image in the same change.

## Consequences

- Local dev databases created before this change start empty. Run `make migrate-up`.
- `docker volume rm farmish-backend_pgdata` reclaims the old volume once it's no longer needed.
