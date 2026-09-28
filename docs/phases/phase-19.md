# Phase 19: Messaging

**Depends on:** 11, 16 (notify) · **Size:** medium

## Goal

Buyer ↔ seller conversations about a listing: one conversation per (listing, buyer), participant-only access, cursor pagination, read markers, a messaging rate-limit policy, throttled SMS nudges, and **real-time delivery over a push-only WebSocket**. Rules are in DOMAIN §11.

**Shape:** REST remains the only write path and the source of truth. The WebSocket only pushes events from server to client, so sends keep the contract, validation, the `messaging` rate limit and the tests. Instances fan out through Postgres `LISTEN/NOTIFY`, issued inside the write transaction, so an event exists only if the write committed.

## Schema: `migrations/000017_messaging.up.sql`

```sql
CREATE TABLE conversations (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  listing_id          uuid NOT NULL REFERENCES listings(id) ON DELETE CASCADE,
  buyer_id            uuid NOT NULL REFERENCES users(id),
  seller_id           uuid NOT NULL REFERENCES users(id),
  order_id            uuid REFERENCES orders(id),        -- optional link (plan: "optional order link")
  last_message_at     timestamptz,
  buyer_last_read_at  timestamptz,
  seller_last_read_at timestamptz,
  created_at          timestamptz NOT NULL DEFAULT now(),
  UNIQUE (listing_id, buyer_id),
  CHECK (buyer_id <> seller_id)
);
CREATE INDEX conversations_buyer_idx  ON conversations (buyer_id, last_message_at DESC);
CREATE INDEX conversations_seller_idx ON conversations (seller_id, last_message_at DESC);

CREATE TABLE messages (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  sender_id       uuid NOT NULL REFERENCES users(id),
  body            text NOT NULL CHECK (char_length(body) BETWEEN 1 AND 2000),
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX messages_conversation_idx ON messages (conversation_id, created_at DESC, id DESC);
```

## API (tag `messaging`, all bearer)

| Method & path | Rate limit | Request | Responses |
|---|---|---|---|
| `POST /v1/conversations` | `messaging` | `{listingId, message: 1–2000}` | **201** new / **200** existing (the message is appended in both cases) → `Conversation`; 403 (own listing); 404 (listing not active) |
| `GET /v1/conversations` | none | `?page&limit` | 200 `{items: ConversationSummary[], meta}`, ordered by `last_message_at DESC`, with `unreadCount` for the caller |
| `GET /v1/conversations/{id}/messages` | none | `?before=<cursor>&limit≤50` | 200 `{items: Message[], nextCursor?}`, newest first; 403 non-participant; 404 |
| `POST /v1/conversations/{id}/messages` | `messaging` | `{body}` | 201 `Message`; 403 / 404 |
| `POST /v1/conversations/{id}/read` | none | none | 204: sets the caller's `*_last_read_at = now` |

- **Cursor:** opaque base64 of `created_at|id`. It decodes to a tuple comparison: `(created_at, id) < ($1, $2)`.
- **`ConversationSummary`:** `{id, listing: {id, slug, title, coverImageUrl}, counterpart: {userId, name}, lastMessage: {body (truncated to 120), createdAt, fromMe}, unreadCount}`.
  - The counterpart's name is the seller's `business_name`, or the buyer's `display_name` (else "Buyer").
  - **No phone or email.**

## Rules

- **Participants** = `buyer_id`, `seller_id`. Anyone else → 403. (The conversation's existence isn't secret for a known id; 403 matches the plan's Done-when.)
- **Rate limit:** add a `messaging` policy to `middleware.DefaultRateLimits().Operation` (30/min, burst 10) and use `x-farmish-rate-limit: messaging`.
- **SMS nudge:** on a new message, if the recipient's `last_read_at` is older than the previous message, enqueue `notify.sms message_received` with `jobs.UniqueWithin(time.Hour)` keyed by (conversation, recipient). That's at most one SMS per conversation per hour.
- **Unread count:** messages where `sender_id <> caller AND created_at > caller_last_read_at` (or all, if null).
- **One transaction per write:** the message insert, the `last_message_at` bump, the SMS nudge `InsertTx` and `SELECT pg_notify('messaging_events', …)` (a sqlc query in `db/queries/messaging.sql`) commit together. Mark-read updates `*_last_read_at` and notifies in the same way. A rolled-back write emits nothing.

## Realtime: `GET /v1/ws`

### Endpoint

- Registered directly on Gin in `internal/http/router.go`, next to `api.RegisterHandlersWithOptions`, not in the strict handler: the validator and auth middleware pass unknown routes through (ADR-0006). The per-IP limit still applies to the upgrade.
- Only served when `RUN_MODE` is `api` or `all`; `NewProbeRouter` doesn't get it.
- Library: `github.com/gorilla/websocket`.
- **Origin:** the upgrader checks `Origin` against `cfg.CORSOrigins` and refuses anything else (this blocks cross-site WebSocket hijacking). Never `CheckOrigin: return true`.

### Auth: first frame, never the query string

A token in the URL would land in access logs and Cloudflare logs, so it's ignored there.

1. The client connects, then sends `{"type":"auth","token":"<Firebase ID token>"}` within **5s**.
2. The server verifies it through the same `Verifier` + `Users` path as `middleware.Authenticate` and replies `{"type":"ready"}`.
3. A bad token, a missing frame or a timeout → close **4401**.
4. The server closes with **4001** at the token's `exp` unless the client sent a fresh `auth` frame first (Firebase ID tokens last 1h; the client refreshes with `getIdToken()`).
5. At most **5 sockets per user per instance**; the 6th → close **4429**.
6. `auth` is the only client → server frame. Anything else → close **4400**.

### Events (server → client)

| `type` | Payload | Sent to |
|---|---|---|
| `ready` | `{}` | The authenticated socket |
| `message.created` | `{conversationId, message: Message}` (the same projection as REST) | Both participants, including the sender's other tabs |
| `conversation.read` | `{conversationId, readAt, byMe}` | Both participants |
| `resync` | `{}` | Every local socket after the listener reconnects: some events may be missing |

- Event schemas go under `components.schemas` in `api/openapi.yaml` as `RealtimeEvent` (`oneOf`, discriminated by `type`), so the frontend gets generated types. Tests validate every emitted frame against them (the `assertContract` pattern).
- The same safe-projection rules apply: no phone, email or `firebase_uid` in any frame.

### Architecture: `internal/messaging/realtime`

- **Hub:** `map[userID]set[*client]` behind a mutex. The hub never sends into its own channels.
- **Client:**
  - a buffered `send` chan (32)
  - exactly **one writer goroutine** (gorilla allows only one concurrent writer) and one reader goroutine
  - closed exactly once through a `sync.Once` that the hub owns
- **Slow consumer:** if the `send` buffer is full, that client alone is dropped (close **1013**). It reconnects and refetches.
- **Keepalive:** ping every 25s (under Cloudflare's 100s idle timeout), 60s pong wait, 10s write deadline, 4 KB read limit.
- **Listener:** one dedicated `pgx.Conn` (not a pool connection; Neon direct host, like River) running `LISTEN messaging_events`.
  - Payload is ids only: `{kind, conversationId, messageId?, participantIds, readerId?, readAt?}`. It stays far below the 8000-byte NOTIFY limit, even with a 2000-char body.
  - The message row is loaded (one sqlc query) only when a participant has a socket on this instance.
  - On connection loss it reconnects with backoff, then broadcasts `resync`.
- **Shutdown:** `http.Server.Shutdown` doesn't track hijacked connections. `internal/http/server.go` gets an on-shutdown hook (`srv.RegisterOnShutdown`) that calls `hub.Close`: close **1001** to every socket, wait within `SHUTDOWN_TIMEOUT`, stop the listener. Wired in `cmd/api/main.go`. `make smoke` must stay green.
- **No new env vars.** Timeouts, caps and buffer sizes are constants. Tests override them through `realtime.Options`, which also takes an injectable clock (no `time.Sleep` for business timers).
- **SMS nudge is unchanged.** Skipping it for an online recipient needs cross-instance presence; that's a §10 Backlog item.

### Client contract (frontend F8)

- One socket per tab. Send the `auth` frame from `getIdToken()`, and send a fresh one before `exp`.
- Reconnect with jittered exponential backoff.
- On `ready` and on `resync`, refetch the conversation list and the newest message page over REST.
- De-duplicate by message id. REST is the truth; socket events are hints.

## Tests

| Test | Proves |
|---|---|
| `TestStartConversation_DuplicateReturnsExisting` | Second POST → 200, same id, message appended |
| `TestConversation_NonParticipant403` | A third user: read messages, send, mark read → 403 |
| `TestConversation_OwnListing403` | |
| `TestMessages_CursorPagination` | 75 messages, limit 30 → pages of 30/30/15, no duplicates or gaps, stable under concurrent inserts |
| `TestUnreadCounts` | |
| `TestMessaging_RateLimit` | 11 rapid sends → 429 on the 11th, with headers |
| `TestSMSNudge_Throttled` | 5 messages within an hour → 1 SMS job |
| `TestConversationSummary_NoPII` | No phone or email keys in any response or realtime frame |
| `TestRealtime_AuthFirstFrame` | No frame within 5s, a bad token, or a token only in the query → 4401 |
| `TestRealtime_OriginRejected` | An origin outside `CORS_ORIGINS` → upgrade refused |
| `TestRealtime_DeliveredToParticipantsOnly` | A REST send reaches both participants' sockets with a schema-valid `message.created`; a connected third user receives nothing |
| `TestRealtime_CrossInstance` | Two hubs with two listeners on one DB: a send handled by A reaches a socket on B |
| `TestRealtime_RollbackEmitsNothing` | An aborted transaction → no event |
| `TestRealtime_ReadEvent` | `POST /read` → `conversation.read` to both participants |
| `TestRealtime_SlowConsumerDropped` | A full buffer closes only that client (1013) |
| `TestRealtime_ListenerReconnectResync` | Killing the listener connection → `resync` after reconnect |
| `TestRealtime_TokenExpiry` | The clock passes `exp` → 4001; a fresh `auth` frame keeps the socket open |
| `TestRealtime_ConnectionCap` | The 6th socket for one user → 4429 |
| `TestRealtime_UnknownClientFrame` | A non-`auth` client frame → 4400 |
| `TestRealtime_ShutdownCloses1001` | Shutdown closes every socket with 1001 within the timeout |

## Manual QA

1. `make db-up auth-up redis-up migrate-up run`; get two tokens with `scripts/dev-token.sh` (buyer, seller).
2. Open a socket per user (`websocat ws://localhost:8080/v1/ws -H 'Origin: <a CORS origin>'`) and send the `auth` frame; expect `ready`.
3. Start a conversation and send messages with curl; both sockets get `message.created`. Mark read; both get `conversation.read`.
4. A third user's socket receives nothing; a disallowed `Origin` is refused.
5. Stop the API with SIGTERM; sockets receive 1001. Run `make smoke`.
