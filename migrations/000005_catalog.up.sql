-- Catalog (Phase 9): two-level category tree with typed attributes.
-- Parents carry listing_group; children inherit it (NULL). Groups map to
-- item states and units in Go (internal/catalog/groups.go, DOMAIN §8).
CREATE TABLE categories (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  parent_id     uuid REFERENCES categories(id) ON DELETE RESTRICT,
  name          text NOT NULL CHECK (char_length(name) BETWEEN 2 AND 80),
  slug          text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
  icon          text,
  listing_group text CHECK (listing_group IN ('equipment','quality','livestock','land','service')),
  sort_order    int  NOT NULL DEFAULT 0,
  is_active     boolean NOT NULL DEFAULT true,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  -- parents carry the group; children inherit it (NULL)
  CHECK ((parent_id IS NULL) = (listing_group IS NOT NULL))
);
CREATE INDEX categories_parent_idx ON categories (parent_id, sort_order);
CREATE TRIGGER set_updated_at BEFORE UPDATE ON categories FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE category_attributes (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  category_id uuid NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
  key         text NOT NULL CHECK (key ~ '^[a-z][a-z0-9_]{1,40}$'),
  label       text NOT NULL CHECK (char_length(label) BETWEEN 1 AND 80),
  type        text NOT NULL CHECK (type IN ('text','number','boolean','select','date')),
  options     text[] NOT NULL DEFAULT '{}',
  required    boolean NOT NULL DEFAULT false,
  sort_order  int NOT NULL DEFAULT 0,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (category_id, key),
  CHECK ((type = 'select') = (cardinality(options) > 0))
);
CREATE TRIGGER set_updated_at BEFORE UPDATE ON category_attributes FOR EACH ROW EXECUTE FUNCTION set_updated_at();
