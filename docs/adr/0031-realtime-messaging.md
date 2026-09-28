# ADR-0031: Realtime messaging over push-only WebSockets

- **Status:** Accepted
- **Date:** 2026-09-28
- **Phase:** 19 (owner-rewritten spec: REST writes plus socket push)

## Context

Marketplace chat is async, but buyers staring at a conversation deserve
live delivery. The spec keeps REST the only write path and adds a
push-only socket per tab.

## Decisions

1. **REST writes, socket hints.** Sends stay `POST` (validation, the
   `messaging` rate limit, contract tests all apply unchanged). After
   commit, the writer's `pg_notify` fans out to every api/all instance;
   clients reconcile over REST and de-duplicate by message id.
2. **First-frame auth, never the query string.** Tokens in URLs land in
   access and Cloudflare logs; browsers cannot set upgrade headers, so the
   client sends `{"type":"auth","token"}` within 5s and the server verifies
   through the same Verifier + Users path. `exp` re-arms on fresh auth
   frames, else the socket closes with 4001.
3. **Cross-instance fan-out is Postgres `LISTEN/NOTIFY`, not Redis.**
   The notify rides inside the write transaction, so a rolled-back write
   emits nothing and delivery never outruns commit. Payloads are ids only
   (well under the 8000-byte limit); rows load only where a participant
   holds a socket. No new substrate was needed.
4. **`GET /v1/ws` lives outside codegen** (a 101 has no OpenAPI shape).
   Its event shapes live under `components.schemas` as `RealtimeEvent`
   instead, and frame tests validate raw JSON against them.
5. **Empty-Origin upgrades are allowed; mismatched origins are refused.**
   Only browsers send `Origin`, so an absent one cannot be a cross-site
   hijack (mobile apps and QA tools omit it). A strict reading of the spec
   would refuse those too; the CSRF reasoning above is why not.
6. **Library: `gorilla/websocket` per the spec**, with a CVE watch: it is
   archived upstream and `make vuln` (govulncheck) runs in CI. If a
   websocket CVE lands, migrate to `coder/websocket` before any feature
   work. Pings serialize through the single writer goroutine (gorilla
   forbids concurrent writers).
7. **The full `Conversation` schema** (`id, listingId, orderId?, createdAt,
   lastMessage?`) was undefined in the spec (only the summary was); it is
   defined as written here, participants learn each other through
   summaries only.
