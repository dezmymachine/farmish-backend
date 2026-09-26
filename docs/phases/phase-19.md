# Phase 19: Messaging

**Depends on:** 11, 16 (notify) · **Size:** medium

## Goal

Buyer ↔ seller conversations about a listing: one conversation per (listing, buyer), participant-only access, cursor pagination, read markers, a messaging rate-limit policy, and throttled SMS nudges. Rules are in DOMAIN §11.

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
| `TestConversationSummary_NoPII` | No phone or email keys in any response |
