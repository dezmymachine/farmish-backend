-- Messaging (Phase 19). One conversation per (listing, buyer); the seller
-- cannot open one with themselves (buyer_id <> seller_id). Read markers
-- live on the conversation row; messages are append-only history.
CREATE TABLE conversations (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  listing_id          uuid NOT NULL REFERENCES listings(id) ON DELETE CASCADE,
  buyer_id            uuid NOT NULL REFERENCES users(id),
  seller_id           uuid NOT NULL REFERENCES users(id),
  order_id            uuid REFERENCES orders(id),
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
