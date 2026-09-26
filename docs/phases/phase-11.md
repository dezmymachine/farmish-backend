# Phase 11: Listings CRUD

**Depends on:** 8, 9, 10 · **Size:** large (do it as 3–4 commits: schema+queries → service+tests → endpoints+tests → expiry job)

## Goal

Sellers create, edit, publish, renew, mark sold and archive listings, with validated attributes (per category), images (attached media) and slug generation. An hourly job expires listings. Everything follows DOMAIN §7–8.

## Schema: `migrations/000007_listings.up.sql`

```sql
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
  CHECK (offers_pickup OR offers_seller_delivery),
  CHECK (NOT offers_seller_delivery OR seller_delivery_fee_pesewas IS NOT NULL),
  CHECK (status = 'draft' OR (published_at IS NOT NULL AND expires_at IS NOT NULL))
);
CREATE INDEX listings_seller_idx   ON listings (seller_id, status);
CREATE INDEX listings_category_idx ON listings (category_id);
CREATE INDEX listings_public_idx   ON listings (status, expires_at);
CREATE INDEX listings_region_idx   ON listings (region, district);
CREATE TRIGGER set_updated_at BEFORE UPDATE ON listings FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE listing_images (
  listing_id uuid NOT NULL REFERENCES listings(id) ON DELETE CASCADE,
  media_id   uuid NOT NULL UNIQUE REFERENCES media_objects(id),
  sort_order int  NOT NULL,
  PRIMARY KEY (listing_id, media_id),
  UNIQUE (listing_id, sort_order)
);

CREATE TABLE listing_attribute_values (
  listing_id   uuid NOT NULL REFERENCES listings(id) ON DELETE CASCADE,
  attribute_id uuid NOT NULL REFERENCES category_attributes(id) ON DELETE CASCADE,
  value        text NOT NULL CHECK (char_length(value) <= 200),
  PRIMARY KEY (listing_id, attribute_id)
);
```

Phase 12 adds search indexes (FTS/trigram) in its own migration. Don't add them here.

## Queries: `db/queries/listings.sql`

`InsertListing`, `GetListingByID`, `GetListingByIDForUpdate`, `GetListingBySlug`, `SlugExists`, `UpdateListing` (all editable fields), `SetListingStatus` (+ published_at/expires_at variants), `ListSellerListings(seller_id, status NULL, limit, offset)` + `Count…`, `ReplaceListingImages` (delete + batch insert, via `:copyfrom` or a loop), `ListListingImages`, `ReplaceListingAttributes`, `ListListingAttributes`, `ExpireDueListings(now) RETURNING id` (`UPDATE … SET status='expired' WHERE status='active' AND expires_at <= $1`), `DeleteDraftListing`.

## API (tag `listings`)

Every endpoint needs bearer auth, and the caller must have a seller profile (403 `seller_profile_required`, a new apierror code).

| Method & path | Rate limit | Request | Responses |
|---|---|---|---|
| `POST /v1/listings` | sensitive | `ListingInput` + `publish?: bool` | 201 `SellerListing`, 400, 403 |
| `GET /v1/me/listings` | none | `?status&page&limit` | 200 `{items: SellerListingSummary[], meta}` |
| `GET /v1/me/listings/{id}` | none | none | 200 `SellerListing`, 403, 404 |
| `PATCH /v1/listings/{id}` | none | `ListingInput` (all fields optional) | 200, 400, 403, 404, 409 (suspended) |
| `POST /v1/listings/{id}/publish` | none | none | 200, 403, 404, 409 |
| `POST /v1/listings/{id}/renew` | none | none | 200, 403, 404, 409 |
| `POST /v1/listings/{id}/mark-sold` | none | none | 200, 403, 404, 409 |
| `POST /v1/listings/{id}/archive` | none | none | 200, 403, 404, 409 |
| `DELETE /v1/listings/{id}` | none | none | 204 (drafts only), 403, 404, 409 (not a draft: archive instead) |

**`ListingInput`:**
- `categorySlug`
- `title`, `description`
- `price: Money` (currency must be GHS)
- `unit`, `quantityAvailable`, `minOrderQty?`, `isNegotiable?`, `itemState`
- `region`, `district`, `area?`
- `deliveryOptions: {pickup: bool, sellerDelivery: bool, sellerDeliveryFee?: Money}`
- `attributes: {<key>: string}`
- `imageMediaIds: uuid[]` (0–10, in display order)

Limits are exactly those of DOMAIN §7. **`SellerListing`:** every field, plus `id`, `slug`, `status`, `publishedAt`, `expiresAt`, `images: [{mediaId, url, sortOrder}]` and the counters. A category must be a **child** category (leaf) **or** a parent with no children (e.g. `irrigation`): return 400 otherwise.

## Rules

- **Ownership:** every mutation loads the listing `FOR UPDATE` and checks `seller_id == caller`, else `ErrForbidden` → 403. Unknown id → 404.
- **Validation** (service, returning `validation.Error` field details):
  - `itemState` in the category group's states
  - `unit` in the group's units
  - `minOrderQty ≤ quantityAvailable` when quantity > 0
  - `region` is valid
  - the seller-delivery fee is present iff seller delivery is on
  - attributes pass the DOMAIN §7 attribute rules: unknown key, bad select option, bad number or date format, missing required → a field error on `attributes/<key>`
- **Images:** each media id is attached via `media.Attach(ctx, tx, seller, mediaID)` inside the listing tx. Replacing the image set detaches nothing: old media rows stay `attached`. Removing images from a listing is allowed; orphan cleanup of detached media is Backlog.
- **Slug:** `text.Slugify(title)`, with collision handling per DOMAIN §7, generated on create **only**. Slugs are stable after a title edit (URLs don't break). Retry on a unique violation (race).
- **Publish:** `draft|archived|expired` → `active`. Requires a complete listing: at least 1 image, and every required attribute. Sets `published_at = now` (if null) and `expires_at = now + 30d`.
- **Renew:** `active|expired` → `active`, `expires_at = now + 30d`.
- **Mark sold / archive:** from `active` only (archive also from `expired`).
- **Suspended** listings can't be edited or transitioned by the owner (409 `listing_suspended`).
- **Editing an active listing** keeps it active. Changing its category re-validates attributes (attributes of the old category are dropped).

## Jobs

`listings.expire`: periodic **hourly**, run on start. `ExpireDueListings(now)`. Log the count.

## Tests

| Test | Proves |
|---|---|
| `TestCreateListing_DraftAndPublish` | Create a draft (no images OK), then publish fails without an image (409 or 400 with field details), then add an image, then publish → active with `expires_at ≈ now+30d` |
| `TestCreateListing_Validation` | Table: title 4/101 chars, price 0, wrong unit for group, wrong itemState, `min>qty`, bad region, delivery fee missing, >10 images → 400 with the right field names |
| `TestCreateListing_Attributes` | `land-leasing` without `land_size_acres` → 400; bad select option; bad number; unknown key; valid set stored |
| `TestCreateListing_RequiresSellerProfile` | 403 `seller_profile_required` |
| `TestListing_OwnershipEnforced` | Another seller: PATCH/publish/archive/delete/GET-own → 403; unknown id → 404 |
| `TestListing_SlugUniqueness` | Three listings titled the same → `x`, `x-2`, `x-3`; concurrent creates don't 500 |
| `TestListing_StatusTransitions` | Every allowed transition succeeds; each disallowed one → 409 (table-driven per DOMAIN §7) |
| `TestListing_SuspendedIsFrozen` | Set suspended via SQL → owner edit → 409 |
| `TestListing_ImagesAttachOnce` | Media used by listing A can't be attached to B (unique `media_id`) → 400/409 |
| `TestExpireListings_Job` | A fixed clock past `expires_at`: active → expired; drafts, sold and future ones untouched |
| `TestListings_ContractValid` | `assertContract` on create/get/list/patch responses |

## Manual QA

With the emulator user plus a seller profile:
1. Request an upload URL, PUT an image to MinIO, then `POST /v1/listings` with `publish: true` → 201 active.
2. `GET /v1/me/listings` shows it. `PATCH` the price. `POST …/mark-sold`.
3. A second user → `PATCH` → 403.

## Pitfalls

- The price comes as `Money`. Reject any currency other than GHS (400).
- Don't return `seller_id` as an email or phone anywhere. The public detail is Phase 12.
- Attribute values are stored as strings. Parse them for validation only.
