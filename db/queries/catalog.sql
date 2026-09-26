-- name: ListActiveCategories :many
SELECT * FROM categories WHERE is_active ORDER BY sort_order, name;

-- name: GetCategoryBySlug :one
SELECT * FROM categories WHERE slug = $1;

-- name: GetCategoryByID :one
SELECT * FROM categories WHERE id = $1;

-- name: ListAttributesByCategory :many
SELECT * FROM category_attributes WHERE category_id = $1 ORDER BY sort_order, key;

-- name: InsertCategory :one
-- Returns no row when the slug is taken (callers map that to 409).
INSERT INTO categories (name, slug, parent_id, icon, listing_group, sort_order)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (slug) DO NOTHING
RETURNING *;

-- name: UpdateCategory :one
UPDATE categories SET
  name = COALESCE(sqlc.narg('name'), name),
  icon = COALESCE(sqlc.narg('icon'), icon),
  sort_order = COALESCE(sqlc.narg('sort_order'), sort_order),
  is_active = COALESCE(sqlc.narg('is_active'), is_active)
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: UpsertCategoryBySlug :one
-- Seed upsert: updates only when something changed (the WHERE guard keeps a
-- repeat run from touching updated_at). Returns no row when unchanged.
INSERT INTO categories (name, slug, parent_id, icon, listing_group, sort_order)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (slug) DO UPDATE SET
  name = EXCLUDED.name, icon = EXCLUDED.icon,
  listing_group = EXCLUDED.listing_group, sort_order = EXCLUDED.sort_order
WHERE categories.name IS DISTINCT FROM EXCLUDED.name
   OR categories.icon IS DISTINCT FROM EXCLUDED.icon
   OR categories.listing_group IS DISTINCT FROM EXCLUDED.listing_group
   OR categories.sort_order IS DISTINCT FROM EXCLUDED.sort_order
RETURNING *;

-- name: InsertAttribute :one
-- Returns no row when the key is taken on this category (409).
INSERT INTO category_attributes (category_id, key, label, type, options, required, sort_order)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (category_id, key) DO NOTHING
RETURNING *;

-- name: UpdateAttribute :one
-- Scoped to the category: an attribute of another category reads as missing.
UPDATE category_attributes SET
  label = COALESCE(sqlc.narg('label'), label),
  type = COALESCE(sqlc.narg('type'), type),
  options = COALESCE(sqlc.narg('options'), options),
  required = COALESCE(sqlc.narg('required'), required),
  sort_order = COALESCE(sqlc.narg('sort_order'), sort_order)
WHERE id = sqlc.arg('id') AND category_id = sqlc.arg('category_id')
RETURNING *;

-- name: DeleteAttribute :one
DELETE FROM category_attributes WHERE id = $1 AND category_id = $2 RETURNING id;

-- name: GetCategoryAttribute :one
SELECT * FROM category_attributes WHERE id = $1 AND category_id = $2;

-- name: UpsertCategoryAttribute :one
-- Seed upsert with the same no-change guard as categories.
INSERT INTO category_attributes (category_id, key, label, type, options, required, sort_order)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (category_id, key) DO UPDATE SET
  label = EXCLUDED.label, type = EXCLUDED.type, options = EXCLUDED.options,
  required = EXCLUDED.required, sort_order = EXCLUDED.sort_order
WHERE category_attributes.label IS DISTINCT FROM EXCLUDED.label
   OR category_attributes.type IS DISTINCT FROM EXCLUDED.type
   OR category_attributes.options IS DISTINCT FROM EXCLUDED.options
   OR category_attributes.required IS DISTINCT FROM EXCLUDED.required
   OR category_attributes.sort_order IS DISTINCT FROM EXCLUDED.sort_order
RETURNING *;

-- name: CountCategoryChildren :one
-- A listing's category must be a leaf (or a parent with no children).
SELECT count(*) FROM categories WHERE parent_id = $1;

-- name: ListCategoryAndChildIDs :many
-- A category filter on a parent slug must include its children (DOMAIN §9);
-- on a child slug it returns just that child.
SELECT c.id FROM categories c
WHERE c.slug = $1
   OR c.parent_id = (SELECT p.id FROM categories p WHERE p.slug = $1);
