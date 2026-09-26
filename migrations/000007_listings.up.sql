-- Listings (Phase 11): seller listings with validated attributes, attached
-- media and a status machine (DOMAIN §7). Money is integer pesewas; unit and
-- item_state are validated in Go against the category's listing group.
-- Phase 12 adds the search indexes in its own migration.
CREATE TABLE listings (
  id                          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  seller_id                   uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  category_id                 uuid NOT NULL REFERENCES categories(id),
  title                       text NOT NULL CHECK (char_length(title) BETWEEN 5 AND 100),
  slug                        text NOT NULL UNIQUE,
  description                 text NOT NULL CHECK (char_length(description) BETWEEN 20 AND 5000),
  price_pesewas               bigint NOT NULL CHECK (price_pesewas BETWEEN 1 AND 1000000000),
  unit                        text NOT NULL,          -- validated in Go against the group's unit set
  quantity_available          int  NOT NULL CHECK (quantity_available >= 0),
  min_order_qty               int  NOT NULL DEFAULT 1 CHECK (min_order_qty >= 1),
  is_negotiable               boolean NOT NULL DEFAULT true,
  item_state                  text NOT NULL,          -- validated in Go against the group
  status                      text NOT NULL DEFAULT 'draft'
                              CHECK (status IN ('draft','active','sold','expired','archived','suspended')),
  region                      text NOT NULL,
  district                    text NOT NULL CHECK (char_length(district) BETWEEN 2 AND 80),
  area                        text CHECK (char_length(area) <= 120),
  offers_pickup               boolean NOT NULL DEFAULT true,
  offers_seller_delivery      boolean NOT NULL DEFAULT false,
  seller_delivery_fee_pesewas bigint CHECK (seller_delivery_fee_pesewas >= 0),
  published_at                timestamptz,
  expires_at                  timestamptz,
  view_count                  int NOT NULL DEFAULT 0,
  favorite_count              int NOT NULL DEFAULT 0,
  contact_count               int NOT NULL DEFAULT 0,
  created_at                  timestamptz NOT NULL DEFAULT now(),
  updated_at                  timestamptz NOT NULL DEFAULT now(),
  -- At least one delivery option; a seller-delivery fee exactly when offered.
  CHECK (offers_pickup OR offers_seller_delivery),
  CHECK (NOT offers_seller_delivery OR seller_delivery_fee_pesewas IS NOT NULL),
  -- Everything that left draft has been published, so it has an expiry.
  CHECK (status = 'draft' OR (published_at IS NOT NULL AND expires_at IS NOT NULL))
);
CREATE INDEX listings_seller_idx   ON listings (seller_id, status);
CREATE INDEX listings_category_idx ON listings (category_id);
CREATE INDEX listings_public_idx   ON listings (status, expires_at);
CREATE INDEX listings_region_idx   ON listings (region, district);
CREATE TRIGGER set_updated_at BEFORE UPDATE ON listings FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Up to 10 images per listing (enforced in the service), each media object
-- attached to at most one listing.
CREATE TABLE listing_images (
  listing_id uuid NOT NULL REFERENCES listings(id) ON DELETE CASCADE,
  media_id   uuid NOT NULL UNIQUE REFERENCES media_objects(id),
  sort_order int  NOT NULL,
  PRIMARY KEY (listing_id, media_id),
  UNIQUE (listing_id, sort_order)
);

-- Attribute values are stored as strings; the service validates them against
-- the category's attribute definitions (DOMAIN §7).
CREATE TABLE listing_attribute_values (
  listing_id   uuid NOT NULL REFERENCES listings(id) ON DELETE CASCADE,
  attribute_id uuid NOT NULL REFERENCES category_attributes(id) ON DELETE CASCADE,
  value        text NOT NULL CHECK (char_length(value) <= 200),
  PRIMARY KEY (listing_id, attribute_id)
);
