# ADR-0012: Upstash Redis for shared rate limits (hybrid)

- **Status:** Accepted. Implements the Backlog item from ADR-0011.
- **Date:** 2026-09-26

## Context

Phase 6 limits requests in-process, so with more than one API instance each instance enforces its own budget. We chose **Upstash Redis**, a hosted Redis with a free tier: 500K commands/month, 256 MB, then $0.20 per 100K commands. Upstash bills per command, so every rate-limit check costs money once past the free tier.

## Decisions

1. **Hybrid:**
   - The **per-IP** flood limit stays **in-process**. It's free and instant, and a bot flood can't burn the Upstash quota even though every request it sends is rejected.
   - **Per-user** and **per-operation** (`sensitive`) limits go to Redis, where accuracy across instances matters.
   - Cost: 0 commands for anonymous traffic; 1 per signed-in request, 2 on `sensitive` operations. The free tier covers roughly 250K–500K signed-in requests a month.

2. **TCP + TLS with go-redis** (`REDIS_URL`, `rediss://`), not Upstash's REST API. That means persistent connections, EVALSHA, and the same code against local Docker Redis, Upstash or any other Redis. The client is tuned for a metered, remote server:
   - no automatic retries
   - no `HELLO` / `CLIENT SETINFO` handshake per connection
   - context deadlines honoured

3. **Atomic Lua token bucket** (`ratelimit.Redis`): one round trip per check.
   - The Redis server clock (`TIME`) is shared by all instances.
   - Keys expire once refilled, so no sweeper is needed.
   - Key format: `farmish:<APP_ENV>:rl:<rule>:<key>`.
   - Semantics and headers match the in-process limiter.
   - Verified against the real Upstash database: allowed, allowed, then denied with the correct retry.

4. **Degrade, don't switch off.** `ratelimit.Fallback` gives each Redis call `REDIS_TIMEOUT` (default 200ms). On any error it uses the in-process limiter and warns at most every 30s. An unreachable Redis at startup logs a WARN and doesn't block boot. `/readyz` doesn't depend on Redis. This replaces Phase 6's fail-open for the shared path.

5. **Config:**
   - `REDIS_URL` is optional; unset means in-process only.
   - It must be `rediss://` in staging/production.
   - Its value, which embeds the password, is never echoed in errors.
   - `REDIS_TIMEOUT` accepts 10ms–5s.

6. **Tests never touch Upstash.** They run against docker-compose `redis:8-alpine`, via `redistest` and `REDISTEST_REQUIRED=1` in `make test`. The smoke test runs the image against it and asserts the Redis backend is active.

## Consequences

- **Put the Upstash database in the same region as Railway.** Measured from the developer's machine, a call took ~190–200ms. That's at the default timeout, so calls fall back often. Co-located, a call takes a few ms.
- **Local dev:** use `make redis-up` (`redis://127.0.0.1:63790`), or Upstash with a higher `REDIS_TIMEOUT` (e.g. 800ms).
- **Watch Upstash usage:** the console shows commands per day. Rate-limit traffic is the main consumer until caching lands.
- The Redis client (`internal/redisx`) is ready for Redis-backed caching when a phase needs it (Backlog).
- The credential first pasted into a chat must be rotated before use in any deployed environment.
