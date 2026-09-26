# Phase 9: Catalog (categories, attributes, locations)

**Depends on:** 2, 3 · **Size:** medium

## Goal

A two-level category tree with typed attributes, groups (item states and units), an idempotent seed ported from the legacy app (DOMAIN §8–9), public read endpoints with `Cache-Control`, admin CRUD, and the Ghana locations endpoint.

## Schema: `migrations/000005_catalog.up.sql`

```sql
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
```

Two levels only: a child's parent must itself have `parent_id IS NULL`. Enforce this in the service; a SQL trigger is optional.

## Seed: `cmd/seed/main.go`, `make seed`

- Loads config (DB only) and upserts DOMAIN §9 exactly: 12 parents (sort_order 1–12), their children (sort_order 1..n in the listed order), and the attributes. Each attribute `key` is snake_case of the name without units, e.g. `weight_kg`, `land_size_acres`, `shelf_life_days`, `experience_years`, `year_of_manufacture`, `prescription_required`.
- **Idempotent:** `INSERT … ON CONFLICT (slug) DO UPDATE SET name, icon, listing_group, sort_order`. Attributes use `ON CONFLICT (category_id, key) DO UPDATE`. Running it twice changes nothing: assert this in a test.
- Put the data in `internal/catalog/seeddata.go` as Go literals, not JSON, so the compiler checks it. `cmd/seed` calls `catalog.Seed(ctx, pool)`. Phase 14 adds promotion tiers to the same command.
- `slugify` lives in `internal/text/slug.go` (DOMAIN §7), shared with listings. Tests: `"Flowers & Ornamentals"` → `flowers-ornamentals`, `"Sheep & Goats"` → `sheep-goats`, accents folded (`"Café"` → `cafe`), max 80 chars, and no leading or trailing dashes.

## Queries: `db/queries/catalog.sql`

`ListActiveCategories` (all, ordered parent sort then child sort), `GetCategoryBySlug`, `ListAttributesByCategory`, `InsertCategory`, `UpdateCategory`, `InsertAttribute`, `UpdateAttribute`, `DeleteAttribute`, `CountListingsInCategory`. The last one is a stub until Phase 11: create it in Phase 11 and guard category deletion then.

## API (tag `catalog`)

| Method & path | Auth | Responses |
|---|---|---|
| `GET /v1/categories` | public | 200 `{items: CategoryNode[]}`: parents with `children[]`, active only. Header `Cache-Control: public, max-age=300` |
| `GET /v1/categories/{slug}` | public | 200 `CategoryDetail`, 404. Same cache header |
| `GET /v1/locations` | public | 200 `{regions: [{name, districts: string[]}]}`, `Cache-Control: public, max-age=86400` |
| `POST /v1/admin/categories` | admin | 201 `CategoryDetail`, 400, 409 (slug taken) |
| `PATCH /v1/admin/categories/{id}` | admin | 200, 400, 404 |
| `POST /v1/admin/categories/{id}/attributes` | admin | 201, 400, 404, 409 (key taken) |
| `PATCH /v1/admin/categories/{id}/attributes/{attributeId}` | admin | 200, 400, 404 |
| `DELETE /v1/admin/categories/{id}/attributes/{attributeId}` | admin | 204, 404 |

- **`CategoryNode`:** `{id, name, slug, icon, sortOrder, children: CategoryNode[]}` (children have an empty `children`).
- **`CategoryDetail`:** `{id, name, slug, icon, parent: {id,name,slug}|null, group, itemStates: string[], units: string[], attributes: CategoryAttribute[]}`.
  - For a **child**, `group`, `itemStates`, `units` and `attributes` come from the **parent**.
  - `itemStates` and `units` come from the DOMAIN §8 tables, in Go: `internal/catalog/groups.go`.
- **`CategoryAttribute`:** `{key, label, type, options, required}`.

Setting a response header in a strict handler: the generated 200 response object may not have header fields. If not, add `headers:` with `Cache-Control` to the spec's 200 response. oapi-codegen then generates a `Headers` struct on the response type; set it there.

## Rules

- Admin create: a parent requires `listingGroup`; a child must not have one. Slugs are generated from the name (children get a parent prefix) unless given explicitly, and validated with the regex.
- Deactivating a category hides it from public reads. Children of an inactive parent are hidden too.
- `internal/geo/districts.go` ports `~/work/farmgate/lib/data/ghana-locations.ts`: region → districts. **Deduplicate within a region**, keep a district under both regions where the legacy file does so, and sort the regions list by name. These are suggestions only (DOMAIN §10).

## Tests

| Test | Proves |
|---|---|
| `TestSeed_MatchesDomainAndIsIdempotent` | After the seed: 12 parents, **72** children (7+7+7+8+7+7+7+6+6+5+5+0), the attribute counts per parent match DOMAIN §9; a second run doesn't change `updated_at` |
| `TestSlugify` | Table from above |
| `TestCategoriesTree_MatchesSeed` | `GET /v1/categories`: order, names, slugs (spot-check `seeds-seedlings-flowers-ornamentals`), `Cache-Control` header, `assertContract` |
| `TestCategoryDetail_ChildInheritsGroupAndAttributes` | `livestock-poultry-cattle` returns the livestock item states, COUNT + heads units, and the parent's attributes |
| `TestCategoryDetail_UnknownSlug404` | |
| `TestAdminCategories_CRUDAndGuards` | Create a parent and a child; a child-of-child → 400; duplicate slug → 409; non-admin → 403 |
| `TestLocations_SixteenRegions` | Exactly the 16 DOMAIN regions; Greater Accra has districts |

## Manual QA

`make seed` twice, then `curl localhost:8080/v1/categories | jq '.items | length'` → 12. `curl -I` shows `Cache-Control`. `curl /v1/categories/land-leasing | jq .attributes` shows `land_size_acres` required.

## Pitfalls

- Don't copy the legacy slug bug (`&` in slugs).
- Attributes are defined on parents only in the seed, but the admin API may add them to children. Resolution: child attributes = parent attributes + the child's own (child keys override on conflict). Implement and test this.
