# Phase 20a: Reviews & favorites

**Depends on:** 16 · **Size:** small–medium

## Goal

Buyers review listings they bought in **completed** orders, and users favourite listings. Seller rating aggregates appear on `PublicSeller` and `ListingDetail`. Rules are in DOMAIN §11.

## Schema: `migrations/000018_reviews_favorites.up.sql`

```sql
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

CREATE TABLE favorites (
  user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  listing_id uuid NOT NULL REFERENCES listings(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, listing_id)
);
```

## API

| Method & path | Auth | Request | Responses |
|---|---|---|---|
| `POST /v1/orders/{orderId}/reviews` | bearer, `sensitive` | `{listingId, rating 1–5, comment?}` | 201 `Review`; 403 (not the buyer); 409 `order_not_completed` \| `already_reviewed`; 400 (listing not in the order) |
| `GET /v1/listings/{slug}/reviews` | public | `?page&limit` | 200 `{items: PublicReview[], meta, summary: {average, count}}` |
| `PUT /v1/me/favorites/{listingId}` | bearer | none | 204 (idempotent) |
| `DELETE /v1/me/favorites/{listingId}` | bearer | none | 204 (idempotent) |
| `GET /v1/me/favorites` | bearer | `?page&limit` | 200 `{items: ListingSummary[], meta}` (inactive listings included, flagged `available: false`) |
| `POST /v1/admin/reviews/{id}/hide` | admin | `{reason}` | 200; audited |

- **`PublicReview`:** `{id, rating, comment, reviewerName (display_name first word, or "Buyer"), createdAt}`. No reviewer id, email or phone.
- Add `rating: {average (1 decimal, as a number), count}` to `PublicSeller` and `ListingDetail` (the seller's aggregate).

## Rules

- A review requires: the order is `completed`, the caller is its buyer, and `listingId` is among its items.
- Favorite PUT and DELETE maintain `listings.favorite_count` **in the same tx**: increment only if the insert happened (`ON CONFLICT DO NOTHING RETURNING`), and decrement only if the delete removed a row.
- The average is computed in SQL: `round(avg(rating)::numeric, 1)`, with hidden reviews excluded.

## Tests

| Test | Proves |
|---|---|
| `TestReview_RequiresCompletedOrder` | Paid/shipped/delivered → 409; completed → 201 |
| `TestReview_UniquePerOrderListingReviewer` | Second → 409 `already_reviewed` |
| `TestReview_OnlyBuyer` / `TestReview_ListingMustBeInOrder` | |
| `TestReview_HiddenExcludedFromAggregates` | |
| `TestFavorites_IdempotentAndCounter` | PUT×2 → count 1; DELETE×2 → count 0; concurrent PUTs by different users → exact count |
| `TestPublicReview_NoPII` | |
