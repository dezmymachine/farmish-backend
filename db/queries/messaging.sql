-- name: InsertConversation :one
-- Opens a conversation. Returns no row when this buyer already has one for
-- the listing: the caller appends to the existing row instead.
INSERT INTO conversations (listing_id, buyer_id, seller_id)
VALUES ($1, $2, $3)
ON CONFLICT (listing_id, buyer_id) DO NOTHING
RETURNING *;

-- name: GetConversationByID :one
SELECT * FROM conversations WHERE id = $1;

-- name: GetConversationForUpdate :one
SELECT * FROM conversations WHERE id = $1 FOR UPDATE;

-- name: GetConversationByListingBuyer :one
-- The duplicate-start match: one conversation per (listing, buyer).
SELECT * FROM conversations WHERE listing_id = $1 AND buyer_id = $2;

-- name: SetConversationLastMessage :exec
UPDATE conversations SET last_message_at = $2 WHERE id = $1;

-- name: SetConversationRead :exec
-- Marks the caller's side read. The column is chosen by the caller from the
-- party they proved, never from the request.
UPDATE conversations SET buyer_last_read_at = $2 WHERE id = $1;
-- name: SetConversationReadSeller :exec
UPDATE conversations SET seller_last_read_at = $2 WHERE id = $1;

-- name: ListConversationsByParticipant :many
-- The caller's conversations on either side, newest activity first.
SELECT * FROM conversations WHERE buyer_id = $1 OR seller_id = $1
ORDER BY last_message_at DESC NULLS LAST, created_at DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountConversationsByParticipant :one
SELECT count(*) FROM conversations WHERE buyer_id = $1 OR seller_id = $1;

-- name: InsertMessage :one
INSERT INTO messages (conversation_id, sender_id, body)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListMessages :many
-- Newest first, before an optional cursor (created_at, id): every row is
-- strictly older than the cursor, so pages never duplicate or skip, even
-- under concurrent inserts.
SELECT * FROM messages
WHERE conversation_id = $1
  AND (sqlc.narg('before_at')::timestamptz IS NULL OR (created_at, id) < (sqlc.narg('before_at'), sqlc.narg('before_id')::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('limit');

-- name: CountUnread :one
-- The caller's unread: others' messages newer than their read marker, or all
-- of them when they never marked read.
SELECT count(*) FROM messages
WHERE conversation_id = $1 AND sender_id <> $2
  AND (sqlc.narg('read_at')::timestamptz IS NULL OR created_at > sqlc.narg('read_at'));

-- name: GetLastMessage :one
-- The newest message of a conversation, for summaries. Returns no row when
-- the conversation has no messages yet.
SELECT * FROM messages WHERE conversation_id = $1
ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: GetListingForMessaging :one
-- The listing a conversation starts from: its seller, status and title for
-- the checks and the summary. Expired counts as inactive: the public reads
-- filter expires_at the same way.
SELECT l.id, l.seller_id, l.status, l.title, l.slug,
       (l.status = 'active' AND l.expires_at > sqlc.arg('now')::timestamptz)::bool AS active,
       COALESCE((SELECT m.key FROM listing_images li JOIN media_objects m ON m.id = li.media_id
         WHERE li.listing_id = l.id ORDER BY li.sort_order LIMIT 1), '') AS cover_key
FROM listings l WHERE l.id = $1;

-- name: NotifyMessagingEvent :one
-- Publishes a realtime event inside the caller's transaction: it fires at
-- commit, so a rolled-back write emits nothing.
SELECT pg_notify('messaging_events', $1) AS notified;
