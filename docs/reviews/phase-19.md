# Review packet: Phase 19 Messaging (REST + realtime push)

## Summary

Buyer↔seller conversations about a listing, one per (listing, buyer), with
participant-only access, cursor history, read markers, a `messaging`
30/min policy and hourly-throttled SMS nudges — plus push-only WebSocket
delivery. REST stays the only write path and the source of truth: writes
publish `pg_notify` inside their own transaction, and a per-process LISTEN
listener routes events to local sockets. Sends never touch the hub, so the
api/worker split stays safe.

## Commits

`git log --oneline 5b2bfdb..HEAD` output (plus the docs commit below):

```text
b966626 Phase 19: deterministic slow-consumer drop and test hardening
f4831be Phase 19: coalesce missing cover image key
0c36160 Phase 19: tidy gorilla/websocket as direct dependency
91a3e31 Phase 19: messaging and realtime tests
a5aab34 Phase 19: messaging domain, realtime hub, contract and wiring
```

## Done-when checklist

Plan §6 (non-participant 403, cursor pagination, duplicate returns
existing, messaging rate limit + SMS throttling) plus the spec's realtime
table:

- [x] Duplicate start → 200 same id + append: `TestStartConversation_DuplicateReturnsExisting`
- [x] Non-participant 403 (read/send/mark) + unknown 404: `TestConversation_NonParticipant403`
- [x] Own listing 403, dead listing 404: `TestStartConversation_OwnListingAndInactive`
- [x] Cursor 75 → 30/30/15, no gaps/dups: `TestMessages_CursorPagination`
- [x] Unread counts + markers: `TestUnreadCounts`
- [x] No PII in summaries: `TestConversationSummary_NoPII`
- [x] 11th send 429 + headers: `TestMessagingEndpoints_RateLimit` (endpoint)
- [x] 5 messages → 1 SMS: `TestSMSNudge_Throttled`
- [x] Auth gate (silence/bad/query-token → 4401): `TestRealtime_AuthFirstFrame`
- [x] Origin refused: `TestMessagingEndpoints_WebSocket` (endpoint, evil origin)
- [x] Participants-only delivery, schema-valid: `TestRealtime_DeliveredToParticipantsOnly`
- [x] Cross-instance via one DB: `TestRealtime_CrossInstance`
- [x] Rollback emits nothing: `TestRealtime_RollbackEmitsNothing`
- [x] Read events with byMe: `TestRealtime_ReadEvent` (+ endpoint delivery in `TestMessagingEndpoints_WebSocket`)
- [x] Slow consumer dropped alone (1013): `TestRealtime_SlowConsumerDropped`
- [x] Listener reconnect → resync: `TestRealtime_ListenerReconnectResync`
- [x] Token expiry 4001, fresh auth survives: `TestRealtime_TokenExpiry`
- [x] 6th socket 4429: `TestRealtime_ConnectionCap`
- [x] Unknown client frame 4400: `TestRealtime_UnknownClientFrame`
- [x] Shutdown 1001: `TestRealtime_ShutdownCloses1001`

## make ci

Last lines, ending in `ci: all checks passed`:

```text
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

Mid-phase work fixed three real bugs the tests surfaced: `resync` published to a nil user list reached nobody (new `Hub.Broadcast`), the slow-consumer drop closed TCP before flushing 1013 (the writer now owns the close), and gorilla only answers pings while the app reads (test sockets drain continuously; production browsers read to render). A coalesce was needed for imageless cover keys. The gate also caught tidy drift, bodyclose/errcheck/gosec findings, and an over-broad test helper.

## Manual QA

Live API (Auth emulator, local Postgres/Redis; tokens masked; `websocat`
absent so a python `websocket-client` script drove the sockets):

```console
$ curl -X PUT .../v1/me/seller-profile ...           # 200
$ dial ws://localhost:8080/v1/ws Origin:https://evil.test
refused: Handshake status 403 Forbidden
$ dial (allowed origin), no auth frame → server closes (4401 proven in tests)
$ dial + {"type":"auth","token":<buyer>}  → {"type":"ready"}
$ dial + {"type":"auth","token":<seller>} → {"type":"ready"}
$ curl -X POST .../v1/conversations {...}             # 201
buyer frame: {"type":"message.created",...,"body":"Is this still available?"}
seller frame: {"type":"message.created",...}          # same id both sides
$ curl -X POST .../v1/conversations (again)           # 200, same id
buyer frame2: {"type":"message.created",...,"body":"Hello again"}
$ curl -X POST .../v1/conversations/{id}/read (seller) # 204
buyer frame: {"type":"conversation.read",...,"byMe":false}
seller frame: {"type":"conversation.read",...,"byMe":true}
$ kill -TERM <api>; socket drops, server log shows clean
"shutting down http server" → "http server stopped" (1001 asserted in tests)
```

A log sample held no tokens, emails, phones or message bodies. The full
matrix (replay, resync, slow consumer, expiry, caps) runs in tests.

## Files changed

`git diff --stat 5b2bfdb..HEAD` plus the docs commit (`AGENTS.md`,
`REDEVELOPMENT_PLAN.md`, `docs/adr/0031-*.md`, this file; the owner's
`docs/phases/phase-19.md` rewrite also lands here). New files and purpose:

- `migrations/000017_messaging.{up,down}.sql`: conversations + messages.
- `db/queries/messaging.sql`: conversation/message reads and writes, cursor
  history, unread counts, listing gate, `pg_notify` in-tx publish.
- `internal/messaging/messaging.go`: types, cursor codec, truncation.
- `internal/messaging/service.go`: start/list/history/send/read with nudge
  + notify in one tx each.
- `internal/messaging/service_test.go`: REST-domain table.
- `internal/messaging/realtime/{hub,client,listener}.go`: hub + pumps +
  LISTEN routing; `jobs.go` untouched (no River kinds: delivery is
  synchronous fan-out, not a job).
- `internal/messaging/realtime/*_test.go`: socket, cross-instance,
  rollback, resync, expiry, cap and shutdown tests.
- `internal/http/handlers/messaging.go`: five endpoints + safe mappers.
- `internal/http/ws.go` + `router.go` + `cmd/api/main.go`: raw upgrade
  route (api/all only), hub/listener lifecycle, 1001 shutdown hook.
- `api/openapi.yaml` (+ generated): messaging tag, REST endpoints and
  `RealtimeEvent` oneOf schemas; `x-farmish-rate-limit: messaging` (new
  30/min policy in middleware).
- `internal/auth/{auth,firebase}.go`: `Identity.Expires` for the 4001 rule.

## Schema changes

New migration 000017, `migrations_test` green inside `make ci`. No new
env vars (timeouts/caps are constants, tests use `realtime.Options`).

## API changes

- REST (codegen, contract-validated): `POST/GET /v1/conversations`,
  `GET/POST /v1/conversations/{id}/messages` (cursor `before`, limit ≤ 50),
  `POST /v1/conversations/{id}/read` (204).
- `GET /v1/ws`: raw upgrade, per-IP limited, auth in first frame.
`make api-lint` and `generate-check` pass; `api.gen.go` generated only.

## Deviations from the spec

All in ADR-0031: empty-Origin upgrades allowed (CSRF reasoning);
`Conversation` schema defined (was missing); pings serialize through the
single writer; handlers never touch the hub.

## Open questions / risks

- `gorilla/websocket` is archived: CVE watch via `make vuln`; migrate to
  `coder/websocket` on any websocket CVE before feature work.
- Cross-instance presence (skip SMS for online recipients) deferred to §10
  backlog by spec; online recipients currently also get the hourly nudge.
- Replica count still undecided (Phase 22): fan-out is DB-based, so any
  replica topology works, but load-test the LISTEN fan-out before launch.

## Backlog additions

None (presence deferral was already spec'd into §10 by the owner).
