# Phase 12: Search & browse

**Depends on:** 11 · **Size:** medium

## Goal

Public search and browse over active listings, with full-text search, filters, sorting and **promoted-first ranking** (the table is created here and filled in Phase 14). Also: a public listing detail with a safe seller projection, the authenticated contact reveal (DOMAIN §7), `ETag`/`Cache-Control`, and index-use checks.

## Schema: `migrations/000008_search.up.sql`

```sql
CREATE EXTENSION IF NOT EXISTS pg_trgm;

ALTER TABLE listings ADD COLUMN search_vector tsvector
  GENERATED ALWAYS AS (
    setweight(to_tsvector('simple', coalesce(title,'')), 'A') ||
    setweight(to_tsvector('simple', coalesce(description,'')), 'B')
  ) STORED;
CREATE INDEX listings_search_idx ON listings USING gin (search_vector);
CREATE INDEX listings_title_trgm_idx ON listings USING gin (title gin_trgm_ops);
CREATE INDEX listings_price_idx ON listings (price_pesewas) WHERE status = 'active';
CREATE INDEX listings_published_idx ON listings (published_at DESC) WHERE status = 'active';

-- Filled by Phase 14; created now so the search SQL is final.
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
```

The down migration drops `listing_promotions`, the indexes and the column. It does **not** drop the `pg_trgm` extension if other objects use it; drop it last with `IF EXISTS`.

The `'simple'` config is used because listings mix English and Ghanaian languages. Stemming would do harm.

## Search query (sqlc, `db/queries/search.sql`)

`SearchListings` with named, nullable filters:

```sql
-- name: SearchListings :many
WITH active_promo AS (
  SELECT DISTINCT ON (listing_id) listing_id, tier, tier_rank
  FROM listing_promotions
  WHERE starts_at <= sqlc.arg('now') AND ends_at > sqlc.arg('now')
  ORDER BY listing_id, tier_rank DESC
)
SELECT l.id, l.slug, l.title, l.price_pesewas, l.unit, l.region, l.district, l.item_state,
       l.published_at, c.slug AS category_slug, c.name AS category_name,
       p.tier AS promo_tier, coalesce(p.tier_rank, 0)::int AS promo_rank,
       (SELECT m.key FROM listing_images li JOIN media_objects m ON m.id = li.media_id
         WHERE li.listing_id = l.id ORDER BY li.sort_order LIMIT 1) AS cover_key,
       sp.business_name AS seller_name, (sp.verification_status = 'verified') AS seller_verified
FROM listings l
JOIN categories c ON c.id = l.category_id
JOIN seller_profiles sp ON sp.user_id = l.seller_id
LEFT JOIN active_promo p ON p.listing_id = l.id
WHERE l.status = 'active' AND l.expires_at > sqlc.arg('now')
  AND (sqlc.narg('q')::text IS NULL OR l.search_vector @@ websearch_to_tsquery('simple', sqlc.narg('q'))
       OR l.title % sqlc.narg('q'))
  AND (sqlc.narg('category_ids')::uuid[] IS NULL OR l.category_id = ANY(sqlc.narg('category_ids')))
  AND (sqlc.narg('region')::text IS NULL OR l.region = sqlc.narg('region'))
  AND (sqlc.narg('district')::text IS NULL OR l.district ILIKE sqlc.narg('district'))
  AND (sqlc.narg('min_price')::bigint IS NULL OR l.price_pesewas >= sqlc.narg('min_price'))
  AND (sqlc.narg('max_price')::bigint IS NULL OR l.price_pesewas <= sqlc.narg('max_price'))
  AND (sqlc.narg('item_state')::text IS NULL OR l.item_state = sqlc.narg('item_state'))
ORDER BY promo_rank DESC,
  CASE WHEN sqlc.arg('sort') = 'price_asc'  THEN l.price_pesewas END ASC,
  CASE WHEN sqlc.arg('sort') = 'price_desc' THEN l.price_pesewas END DESC,
  CASE WHEN sqlc.arg('sort') = 'relevance' AND sqlc.narg('q')::text IS NOT NULL
       THEN ts_rank(l.search_vector, websearch_to_tsquery('simple', sqlc.narg('q'))) END DESC,
  l.published_at DESC, l.id
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');
```

Plus `CountSearchListings` with the same `WHERE`.

- **Category filter:** a parent slug expands to the parent plus all its children (resolve IDs in Go first). This fixes the legacy exact-match-only behaviour.
- If sqlc can't handle some construct, split the query or use `CASE` columns. **Keep the SQL in sqlc.**

## API (tag `listings`)

**`GET /v1/listings`** (public, `security: []`)
- **Query parameters:**
  - `q` (1–100)
  - `category` (slug)
  - `region` (enum)
  - `district`
  - `minPrice`, `maxPrice` (int64 pesewas)
  - `itemState`
  - `sort` ∈ `relevance|newest|price_asc|price_desc` (default `newest`; `relevance` requires `q`, otherwise it falls back to `newest`)
  - `page`, `limit` (≤ 50, default 20)
- **200** `{items: ListingSummary[], meta: PageMeta}`.
  - `ListingSummary` = `{id, slug, title, price: Money, unit, region, district, itemState, category: {slug, name}, coverImageUrl?, promoted: {tier}|null, seller: {name, verified}, publishedAt}`.
- **Headers:**
  - `Cache-Control: public, max-age=30`
  - a weak **`ETag`** over the response body (e.g. `W/"<first 16 hex of sha256>"`)
  - `If-None-Match` match → **304** with no body. Add a 304 response to the spec.

**`GET /v1/listings/{slug}`** (public)
- **200** `ListingDetail`: the summary plus `description`, `quantityAvailable`, `minOrderQty`, `isNegotiable`, `area`, `deliveryOptions`, `images[]`, `attributes: [{key, label, value}]`, `seller: PublicSeller` (Phase 8 projection), `expiresAt`, `favoriteCount`.
- **404** if not active or expired. **Never** include a phone, an email or `seller_id` internals beyond `PublicSeller.userId`.
- Same caching headers, `max-age=60`.
- Increments `view_count`. Do it cheaply: an async River job `listings.count_view` (unique per (listing, ip-hash, hour)), or a plain `UPDATE` in the request. **Choose the job** to keep GET latency low, and document the choice.

**`POST /v1/listings/{id}/contact`** (bearer, `x-farmish-rate-limit: sensitive`)
- **200** `{phone?: string, whatsapp?: string}`, only the fields the seller opted into.
- **404** if the listing isn't active; **403** if it's your own listing.
- Increments `contact_count`. The seller's phone comes from `users.phone_e164` (only when `show_phone`) and `seller_profiles.whatsapp_e164` (only when `show_whatsapp`).

## Tests

| Test | Proves |
|---|---|
| `TestSearch_Filters` | Table over a fixture of ~15 listings: each filter alone and in combinations (q+category, region+price range, parent-category expansion, itemState) returns exactly the expected ids |
| `TestSearch_Sorts` | newest, price_asc, price_desc, relevance order |
| `TestSearch_ExcludesInactiveAndExpired` | draft/sold/archived/suspended/expired, and active with `expires_at < now`, are never returned |
| `TestSearch_PromotedFirst` | Insert `listing_promotions` rows directly: an active enterprise promo ranks above vip, above an unpromoted one; an **expired** promo ranks as unpromoted |
| `TestSearch_Pagination` | `limit ≤ 50` enforced (51 → 400), `meta.total` correct, pages disjoint |
| `TestSearch_UsesIndexes` | `EXPLAIN (FORMAT JSON)` of the main query shapes, with ~2000 rows inserted, then `ANALYZE`: the plan uses `listings_search_idx` for `q`, and `listings_public_idx`/`listings_region_idx` for status/region filters (assert the index names appear) |
| `TestListingDetail_SafeProjection` | Decoded JSON contains no `phone`, `email`, `whatsapp`, `firebaseUid`, `role`, `idNumber*`, `sellerId`; `assertContract` |
| `TestListing_ETag304` | A second request with `If-None-Match` → 304, empty body |
| `TestContactReveal_OptInOnly` | Seller opted out → `{}`; opted in → phone shown; own listing → 403; anonymous → 401; `contact_count` incremented |

## Manual QA

Seed categories, create 3 listings (one with a promotion row inserted by SQL), then check:
- `curl '/v1/listings?q=maize&sort=price_asc'`
- `curl -I` to see the ETag, then `curl -H 'If-None-Match: …'` → 304

## Pitfalls

- `websearch_to_tsquery` never errors on user input, but `to_tsquery` does: don't use `to_tsquery`.
- Always pass `now` from the service clock, not SQL `now()`, so tests are deterministic.
- ETags must be computed over the exact body bytes sent.
