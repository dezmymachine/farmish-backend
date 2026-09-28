-- Reviews and favorites (Phase 20a). A review ties a listing to the
-- completed order it was bought in: one per (order, listing, reviewer).
-- Hidden reviews stay in place for moderation history but leave every
-- aggregate. Favorites are idempotent by primary key, with the listing's
-- counter maintained in the same transaction.
CREATE TABLE reviews (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  order_id    uuid NOT NULL REFERENCES orders(id),
  listing_id  uuid NOT NULL REFERENCES listings(id) ON DELETE CASCADE,
  seller_id   uuid NOT NULL REFERENCES users(id),
  reviewer_id uuid NOT NULL REFERENCES users(id),
  rating      smallint NOT NULL CHECK (rating BETWEEN 1 AND 5),
  comment     text CHECK (char_length(comment) <= 1000),
  hidden_at   timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (order_id, listing_id, reviewer_id)
);
CREATE INDEX reviews_listing_idx ON reviews (listing_id, created_at DESC) WHERE hidden_at IS NULL;
CREATE INDEX reviews_seller_idx  ON reviews (seller_id) WHERE hidden_at IS NULL;
CREATE TRIGGER reviews_set_updated_at BEFORE UPDATE ON reviews
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE favorites (
  user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  listing_id uuid NOT NULL REFERENCES listings(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, listing_id)
);
