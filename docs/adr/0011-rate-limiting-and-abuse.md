# ADR-0011: Rate limiting, client IP and Turnstile

- **Status:** Accepted
- **Date:** 2026-09-26
- **Phase:** 6

## Decisions

1. **In-process token buckets** (`internal/ratelimit.Memory`) behind a `Limiter` interface. v1 runs one API instance. A shared backend (Redis/Postgres) replaces `Memory` when there are several, which is already in the Backlog.
   - **Bounded memory:** IPv6 clients are keyed by `/64`. Idle buckets are swept every minute. Above 100k keys, new keys share one overflow bucket per rule: still limited, as a group.
   - `Rule.Validate` refuses rules that refill slower than the sweep's idle threshold (15 min). A swept bucket restarts full, so that would hand out free tokens.
   - **Limiter errors fail open** (logged), so an outage of a future shared backend doesn't take the API down.

2. **Limits** (`middleware.DefaultRateLimits`):

   | Rule | Key | Limit | Applies to |
   |---|---|---|---|
   | `ip` | client address (IPv6 /64) | 300/min, burst 100 | every request except `/healthz`, `/readyz` |
   | `user` | user ID | 120/min, burst 60 | signed-in requests |
   | `sensitive` | user ID, else address | 10/min, burst 5 | operations with `x-farmish-rate-limit: sensitive` |

   The per-IP limit is deliberately generous: Ghanaian mobile carriers put many subscribers behind one address (CGNAT). The per-user and per-operation limits do the real work.

3. **Headers:** `X-RateLimit-Limit` (burst), `X-RateLimit-Remaining` and `X-RateLimit-Reset` (seconds until full) on every limited route. A 429 adds `Retry-After` (seconds until one token), with body `rate_limited`. CORS exposes these headers, and the limiter runs after CORS, so browsers can read 429s.

4. **Client IP** (`middleware.ClientIPResolver`, config `TRUSTED_PROXIES` + `TRUST_CLOUDFLARE`):
   - Start from the TCP peer.
   - Step back through `X-Forwarded-For` only while the current hop is a trusted proxy.
   - Believe `CF-Connecting-IP` only if that hop is inside Cloudflare's **published ranges**, which are embedded in the code (fetched 2026-09-26).
   - Nothing is trusted by default, so direct clients can't spoof their address.
   - A stale range list only means falling back to the edge address, never trusting a spoofed header.
   - Gin's own `ClientIP()` is unused; access logs use the resolved address.

5. **Turnstile** (`internal/turnstile`) is required on operations with `x-farmish-turnstile: true`. The token is read from `X-Turnstile-Token`, which CORS allows.
   - Missing or rejected → 400 `turnstile_failed`.
   - Cloudflare unreachable, or our secret misconfigured → **fail closed** with 503 `unavailable`.
   - Turnstile runs after rate limiting, so floods never reach Cloudflare, and before validation.
   - `TURNSTILE_SECRET` is required. Cloudflare's test secrets are refused in staging/production.

6. **Protection is declared in the spec,** like auth. Unknown `x-farmish-rate-limit` policies or non-boolean `x-farmish-turnstile` values fail startup. Chain order: RequestID → ClientIP → AccessLog → Recovery → CORS → IP limit → Authenticate → user/operation limits → Turnstile → validation.

## Consequences

- **Deployment (Phase 22):** set `TRUSTED_PROXIES` to the Railway edge's addresses and `TRUST_CLOUDFLARE=true`, and verify the logged `client_ip` is the real client. Consider locking the origin so it's reachable only through Cloudflare. This is tracked in §11.
- No production operation uses `sensitive` or Turnstile yet. Later phases opt in through the spec: messaging, supply requests, reports.
