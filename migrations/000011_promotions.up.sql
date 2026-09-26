-- Promotions (Phase 14): package configs and per-application credits spent.
-- The tier rows themselves are seeded by cmd/seed so descriptions and feature
-- lists stay editable without another schema migration.
CREATE TABLE promotion_configs (
  tier          text PRIMARY KEY CHECK (tier IN ('top','vip','diamond','enterprise')),
  name          text NOT NULL,
  price_pesewas bigint NOT NULL CHECK (price_pesewas > 0),
  credits       int  NOT NULL CHECK (credits > 0),
  duration_days int  NOT NULL CHECK (duration_days > 0),
  tier_rank     int  NOT NULL UNIQUE CHECK (tier_rank BETWEEN 1 AND 4),
  featured      boolean NOT NULL DEFAULT false,
  description   text NOT NULL DEFAULT '',
  features      text[] NOT NULL DEFAULT '{}',
  sort_order    int  NOT NULL,
  is_active     boolean NOT NULL DEFAULT true,
  updated_at    timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE listing_promotions ADD COLUMN credits_spent int NOT NULL DEFAULT 0 CHECK (credits_spent >= 0);
-- The payment row is the only durable record of a purchase as its terms were
-- at checkout time. metadata snapshots that tier's credits; prices and charges
-- are already stored on payments.
ALTER TABLE payments ADD COLUMN metadata jsonb NOT NULL DEFAULT '{}';
