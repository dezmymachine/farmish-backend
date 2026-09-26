# Review packet: Phase 12 Search & browse

## Summary

Public search and browse over active listings, with full-text search (a
generated `tsvector` plus a trigram fallback), the spec's filters and sorts,
and promoted-first ranking. Added the public listing detail page with the
Phase 8 safe seller projection, the authenticated opt-in contact reveal, and
`Cache-Control` + weak `ETag`/`304` on both public reads. A view is counted
through a River job rather than in the request path, deduplicated to one count
per listing, viewer and hour. `EXPLAIN` on production-shaped data proves the
search indexes are used.

## Commits

```
8bf1b5f Phase 12: show the running promotion on the detail page
4760ad6 Phase 12: public browse endpoints and view counting
eecdbbc Phase 12: search, public detail and contact services
4a8a891 Phase 12: search schema and queries
```

## Done-when checklist

Plan §6 Phase 12:

- [x] Filter combinations are tested and active promotions rank first (expired ones don't):
  `TestSearch_Filters` (13 cases: each filter alone, q+category, q+wrong category,
  region+price, parent-category expansion, unknown category, item state, district),
  `TestSearch_PromotedFirst` (enterprise above vip above unpromoted; an expired
  window ranks as unpromoted) — `internal/listings/search_test.go`
- [x] `EXPLAIN` shows index use: `TestSearch_UsesIndexes` asserts
  `listings_search_idx` + `listings_title_trgm_idx` for the free-text predicate,
  `listings_region_idx` for the region filter and `listings_published_idx` for the
  newest browse, and that no shape falls back to a sequential scan —
  `internal/listings/search_test.go`
- [x] Responses contain no private seller fields: `TestListingDetail_SafeProjection`
  decodes the JSON and asserts the absence of phone, email, whatsapp, firebaseUid,
  role, idNumber, sellerId and the actual number strings —
  `internal/http/search_test.go`
- [x] ETag/304 works: `TestListing_ETag304` (both reads: second request with
  `If-None-Match` → 304, empty body, same validator; stale validator → 200;
  `*` → 304) — `internal/http/search_test.go`

Spec test table:

- [x] `TestSearch_Filters`, `TestSearch_Sorts`, `TestSearch_ExcludesInactiveAndExpired`,
  `TestSearch_PromotedFirst`, `TestSearch_Pagination`, `TestSearch_UsesIndexes` —
  `internal/listings/search_test.go`
- [x] `TestListingDetail_SafeProjection`, `TestListing_ETag304`,
  `TestContactReveal_OptInOnly` — `internal/http/search_test.go`

Beyond the spec table, because the reviewer should not have to infer them:

- `TestPublicDetail`, `TestContact` (service level: the whole opt-in matrix, own
  listing, every non-browsable state, the counter) and `TestCountView`.
- `TestPublicDetail_PromotionMatchesSearch` — the detail page and the search row
  must report the same promotion (written after manual QA found they did not).
- `TestCountView_Job` — the job runs through River and the dedupe holds: the same
  viewer twice inside the window counts once, a second viewer counts again.
- `TestNewViewerHasher`, `TestETagMatches`, `TestWeakETag_IsStableOverTheSameBody`
  — `internal/http/handlers/search_test.go`
- `TestSearch_EndpointContract` — every search parameter, plus 400 for a bad
  sort, an over-large limit and an inverted price range.

## make ci

```
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

(tidy-check, fmt-check, api-lint, generate-check, sqlc-check, lint, test with
`-race` and DB/auth/Redis/S3 required, vuln, smoke — all green on
`8bf1b5f` plus the docs commit.)

## Manual QA

Run against the local stack (Postgres 54320, Auth emulator 9099, Redis 63790,
rustfs 9000), the API on `:8080` with `FIREBASE_PROJECT_ID=demo-farmish` and
`FIREBASE_CREDENTIALS_JSON` unset so the emulator's tokens verify.

Setup: `make seed`, a phone-free email seller with a seller profile
(`showPhone: true`), three listings created and published through the real API
with a presigned upload each, then two `listing_promotions` rows inserted by
SQL (`vip` rank 2 running on *Maize Bran Feed*, `enterprise` rank 4 already
expired on *White Maize Premium*).

```
$ curl '/v1/listings?q=maize&sort=price_asc'          # meta and order
meta: {'limit': 20, 'page': 1, 'total': 11}
order: [('Maize Bran Feed', 12000), ('Yellow Maize Bulk 50kg', 40000), ...]
   (Maize Bran Feed first: it is the promoted one, promotion outranks the sort)
   coverImageUrl: http://127.0.0.1:9000/farmish-dev/listings/<id>/<key>.jpg
   promoted: {'tier': 'vip'}   seller: {name: Asante Maize Farm, verified: false}

$ curl -D - -o /dev/null '/v1/listings?q=maize&sort=price_asc'   # headers
HTTP/1.1 200 OK
Cache-Control: public, max-age=30
Etag: W/"1cf42e8f44ac5ed1"

$ curl -D - -H 'If-None-Match: W/"1cf42e8f44ac5ed1"' '/v1/listings?q=maize&sort=price_asc'
HTTP/1.1 304 Not Modified
Cache-Control: public, max-age=30
Etag: W/"1cf42e8f44ac5ed1"
body bytes: 0
```

Note: the spec's `curl -I` returns 405 — the contract declares no HEAD. Headers
were read with `curl -D` instead. Adding HEAD is not in scope; noting it for the
reviewer.

```
$ curl '/v1/listings/maize-bran-feed'          # public detail, anonymous
promoted: {'tier': 'vip'}
seller: Asante Maize Farm | images: 1
top-level keys: attributes, category, coverImageUrl, deliveryOptions, description,
  district, expiresAt, favoriteCount, id, images, isNegotiable, itemState,
  minOrderQty, price, promoted, publishedAt, quantityAvailable, region, seller,
  slug, title, unit            # no phone, no email, no sellerId
seller: {bio, businessName, district, memberSince, region, userId, verified}

filters (total / first titles)
all active             -> total 11 ['Maize Bran Feed', 'White Maize Premium', ...]
q=maize                -> total 11
q=maiz (trigram)       -> total 0            # see "Open questions"
parent category        -> total 11           # fresh-produce includes its children
child category         -> total 11
unknown category       -> total 0
region=Ashanti         -> total 11
region=Volta           -> total 0
district=Kumasi Metro  -> total 11
minPrice=20000         -> total 10
itemState=grade_b      -> total 6
sort=price_desc        -> Maize Bran Feed first (promoted), then by price
page=2&limit=5         -> total 11, a different page of items
```

```
$ # 3 detail reads from one address, then:
select view_count from listings where slug='maize-bran-feed'   ->  1
select kind, state, count(*) from river_job where kind='listings.count_view'
 listings.count_view | completed | 1
```

```
$ # contact reveal (seller phone +233245551122, show_phone on)
buyer, opted in  -> {"phone":"+233245551122"} [200]
buyer, opted out -> {} [200]          # counted anyway
own listing      -> {"code":"forbidden","message":"This is your own listing"} [403]
anonymous        -> {"code":"unauthorized","message":"Authentication required"} [401]
unknown listing  -> {"code":"not_found","message":"Listing not found"} [404]
contact_count: 2  (only the two 200s counted)
```

```
$ # a listing that stops being browsable
mark-sold -> [200]
GET /v1/listings/white-maize-premium-5 -> {"code":"not_found"} [404]   (total 11 -> 10)
GET /v1/listings/no-such-listing-at-all -> {"code":"not_found"} [404]
```

**Bug found and fixed by QA:** the detail page reported `promoted: null` for the
listing search reported as `vip` — `GetPublicListingBySlug` never read the
promotion. Fixed in `8bf1b5f` and pinned by
`TestPublicDetail_PromotionMatchesSearch`.

## Files changed

`git diff --stat ac1e7d1..8bf1b5f` — 25 files, +4655/−618. New files:

- `migrations/000008_search.{up,down}.sql` — search schema: `pg_trgm`, generated
  `search_vector`, GIN and partial indexes, `listing_promotions`.
- `db/queries/search.sql` — the search, count, detail, contact and counter
  queries.
- `internal/db/search.sql.go` — generated; never hand-edited.
- `internal/listings/search.go` — `Search`, `PublicDetail`, `Contact`,
  `CountView` and the public projection types.
- `internal/listings/search_test.go` — the service tests above.
- `internal/http/handlers/search.go` — the three public handlers, the weak-ETag
  and `If-None-Match` helpers, the viewer hasher, and the response mappers.
- `internal/http/handlers/search_test.go` — hasher and ETag unit tests.
- `internal/http/search_test.go` — the four endpoint tests.
- `docs/adr/0018-public-listing-paths.md`, `docs/adr/0019-view-count-viewer-hash.md`.

Changed: `api/openapi.yaml` (3 new operations, 6 moved, new schemas, 304
response, `Cache-Control`/`ETag` headers, `ListingSlug` parameter, `GhanaRegion`),
`internal/http/api/api.gen.go` (generated), `internal/http/handlers/health.go`
(the new seams), `internal/http/router.go`, `cmd/api/main.go`,
`internal/listings/jobs.go`, `internal/listings/service.go` (the `Catalog`
interface), `internal/catalog/service.go` (`IDsForFilter`),
`internal/media/service.go` (nil-safe `PublicURL`), `internal/http/listings_test.go`
(the moved URLs), `db/queries/catalog.sql`.

## Schema changes

- `000008_search` — `CREATE EXTENSION pg_trgm`; generated `search_vector`
  (`simple`, title A / description B); `listings_search_idx`,
  `listings_title_trgm_idx`, `listings_price_idx` and `listings_published_idx`
  (both partial on `status = 'active'`); `listing_promotions` with
  `tier_rank` and a `listing_promotions_active_idx`. The down migration drops the
  table, the indexes and the column, and drops `pg_trgm` last with `IF EXISTS`.
- `TestUpDownUp` (migrations) passes: up → down → up. `migrate version` reports
  `version 8 (dirty: false)`.

## API changes

| Method | Path | operationId | Auth | Extensions |
|---|---|---|---|---|
| GET | `/v1/listings` | `searchListings` | public (`security: []`) | `Cache-Control: public, max-age=30`, weak `ETag`, 304, 400, 429 |
| GET | `/v1/listings/{slug}` | `getPublicListing` | public | `Cache-Control: public, max-age=60`, weak `ETag`, 304, 404, 429 |
| POST | `/v1/listings/{id}/contact` | `revealListingContact` | bearer | `x-farmish-rate-limit: sensitive`; 401/403/404/429 |
| PATCH | `/v1/me/listings/{id}` | `updateListing` | bearer | moved from `/v1/listings/{id}` |
| DELETE | `/v1/me/listings/{id}` | `deleteListing` | bearer | moved |
| POST | `/v1/me/listings/{id}/publish` | `publishListing` | bearer | moved |
| POST | `/v1/me/listings/{id}/renew` | `renewListing` | bearer | moved |
| POST | `/v1/me/listings/{id}/mark-sold` | `markListingSold` | bearer | moved |
| POST | `/v1/me/listings/{id}/archive` | `archiveListing` | bearer | moved |

`If-None-Match` is declared as an optional request header on the two reads so
the 304 is described in the contract rather than being an undocumented
behaviour. New schemas: `ListingSummary`, `ListingDetail`, `ListingSearchResult`,
`ListingContact`, `ListingImage`, `ListingAttribute`, `ListingCategoryRef`,
`ListingSellerRef`, `ListingDeliveryOptions`, `ListingPromotion`, `GhanaRegion`.
`make api-lint` and `make generate-check` pass.

## Deviations from the spec

1. **Seller writes moved to `/v1/me/listings/{id}`** (ADR-0018). OpenAPI forbids
   two paths differing only by a path-parameter name, and the linter rejected the
   spec's `GET /v1/listings/{slug}` outright. Owner decision: move the seller
   writes rather than compromise the public URL.
2. **The view dedup hash is `HMAC(DATA_ENCRYPTION_KEY, "view:"+ip)`** rather than
   a plain digest, and no new env var was added (ADR-0019). Owner decision. An
   unsalted digest of an IPv4 address is reversible in minutes, so "hash the IP"
   needed a keyed construction to be worth anything.
3. **`TestSearch_UsesIndexes` seeds 20,000 production-shaped rows, not ~2,000.**
   The spec's number is a stub-row count: with narrow rows the `listings` heap is
   smaller than the 1.6 MB GIN index, so a sequential scan genuinely *is* the
   cheaper plan and the test would have asserted a planner mistake. Real listings
   have long descriptions (the schema allows 20–5000 characters), which is what
   makes the index pay. The assertions are unchanged; the fixture is honest about
   production shape.
4. **`LEFT JOIN LATERAL` replaced by a CTE + left join in
   `GetPublicListingBySlug`.** A lateral subquery that matches no promotion
   returns *no row*, so a `coalesce` inside it never runs and the column comes
   back NULL, which sqlc typed as a plain string and pgx then refused to scan.
   The CTE shape is the one `SearchListings` already uses.

## Open questions / risks

- **Long fuzzy queries can scan.** Postgres has no statistics for trigram
   similarity, so it estimates `title % $1` at ~1.0 for a long string, and the
   `OR` in the free-text predicate then prefers a sequential scan. Verified in
   psql: with `enable_seqscan` off the planner still picks `listings_seller_idx`
   and filters, not `listings_title_trgm_idx`. Short queries do use both indexes
   (asserted in the test). The spec's `OR` semantics are preserved rather than
   optimised away; the fix (a `UNION` of two indexed queries, or a bounded
   predicate) is in the §10 backlog. Worth a look if search volume grows.
- **Near-miss matching is effectively short-query-only.** `q=maiz` returns 0
   against "Yellow Maize Bulk 50kg" because the trigram similarity is below the
   0.3 default threshold, while `Friesn Heifer` does match "Friesian Heifer
   Calf". This is pg_trgm's behaviour, not a bug, but the UX consequence is that
   a buyer's typo only sometimes helps.
- **The promoted listing wins over the requested sort.** `sort=price_desc` starts
   with the promoted listing regardless of price. That is DOMAIN §6 as specified,
   but it looks surprising in a price-sorted list and the frontend may want a
   "sponsored" label (a Phase 14 concern).
- **`curl -I` (HEAD) returns 405** because the contract declares no HEAD
   operations. Gin's router would need explicit HEAD routes if the frontend or a
   CDN needs them.
- **`media.PublicURL` was made nil-safe.** A nil `*media.Service` in a non-nil
   interface (storage is optional locally) would have panicked on every public
   read. The guard is one line and also protects the pre-existing owner read
   path.
- **The ETag is computed over `json.Marshal` of the response body**, and the
   generated visitor writes those bytes plus a trailing newline. The validator
   therefore covers the JSON bytes, not the trailing newline. This is stable
   because Go marshals struct fields in declaration order, but a future switch to
  a streaming or non-deterministic marshal would break revalidation. Asserted by
  `TestWeakETag_IsStableOverTheSameBody`.
- **View counts are eventually consistent** and only *approximately* deduplicated:
  the River `UniqueWithin(hour)` guard collapses repeats per viewer per hour, so a
  burst from one viewer in the same second may still enqueue once and a reload
  loop within the hour counts once. Also, with `RUN_MODE=api` and no worker
  process, the views queue up rather than being lost.

## Backlog additions

- Bound the trigram branch's selectivity so long near-miss queries use
  `listings_title_trgm_idx` instead of scanning.
- `GhanaRegion` now exists as a schema; the three older inline region enums
  (seller profile, locations, admin) still repeat the list.
