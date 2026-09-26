-- Search and browse (Phase 12): full-text search, the public query indexes
-- and the promotion table Phase 14 fills in.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- 'simple' (no stemming) because listings mix English with Ghanaian
-- languages; stemming does more harm than good here.
ALTER TABLE listings ADD COLUMN search_vector tsvector
  GENERATED ALWAYS AS (
    setweight(to_tsvector('simple', coalesce(title,'')), 'A') ||
    setweight(to_tsvector('simple', coalesce(description,'')), 'B')
  ) STORED;
CREATE INDEX listings_search_idx ON listings USING gin (search_vector);
-- Trigram fallback so near-miss queries ("friesn") still match a title.
CREATE INDEX listings_title_trgm_idx ON listings USING gin (title gin_trgm_ops);
CREATE INDEX listings_price_idx ON listings (price_pesewas) WHERE status = 'active';
CREATE INDEX listings_published_idx ON listings (published_at DESC) WHERE status = 'active';

-- Promotions are applied in Phase 14; the table exists now so the search SQL
-- (and its promoted-first ordering) is final. A listing is promoted iff
-- starts_at <= now < ends_at (DOMAIN §6).
CREATE TABLE listing_promotions (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  listing_id  uuid NOT NULL REFERENCES listings(id) ON DELETE CASCADE,
  seller_id   uuid NOT NULL REFERENCES users(id),
  tier        text NOT NULL CHECK (tier IN ('top','vip','diamond','enterprise')),
  tier_rank   int  NOT NULL CHECK (tier_rank BETWEEN 1 AND 4),
  starts_at   timestamptz NOT NULL,
  ends_at     timestamptz NOT NULL CHECK (ends_at > starts_at),
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX listing_promotions_active_idx ON listing_promotions (listing_id, ends_at DESC);
