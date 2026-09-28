# Review packet: Phase 20a Reviews & favorites

## Summary

Buyers review listings bought in completed orders (unique per
order/listing/reviewer, hidden ones excluded from aggregates), and users
favourite listings idempotently with an exact `favorite_count` maintained
in the same transaction. Seller aggregates (one-decimal average, count)
ride on `PublicSeller`, `ListingDetail` and `OrderDetail`; admins hide
reviews with an audit event.

## Commits

`git log --oneline c16373f..HEAD` output (plus the docs commit below):

```text
9f1496b Phase 20a: engagement endpoints, ratings and tests
9db5499 Phase 20a: reviews and favorites endpoints with seller ratings
2bd1311 Phase 20a: hide distinguishes missing from already-hidden
c91ac1a Phase 20a: reviews and favorites domain
```

## Done-when checklist

- [x] Review without a completed order rejected: `TestReview_RequiresCompletedOrder` (paid/shipped/delivered → 409, completed → 201)
- [x] Uniqueness: `TestReview_UniquePerOrderListingReviewer` (second → 409 `already_reviewed`, other order fine)
- [x] Buyer-only + listing-in-order: `TestReview_OnlyBuyerAndListingInOrder` (stranger/seller → 403, unknown → 404, foreign → 400, bad rating/comment → 400)
- [x] Hidden excluded: `TestReview_HiddenExcludedFromAggregates` (list + 4.0×2 → 3.0×1, second hide 409, audit row)
- [x] Idempotent favourites, exact counter: `TestFavorites_IdempotentAndCounter` (PUT×2 → 1, DELETE×2 → 0, 8 concurrent adds → 8, unknown 404, no-op remove safe)
- [x] No PII: `TestConversationSummary_NoPII`-style key asserts in `TestEngagementEndpoints_ReviewFlow` (no reviewer id/email/phone keys)
- [x] Endpoints contract-valid incl. rating display: `TestEngagementEndpoints_RatingDisplayed` (4.5 × 2 on seller, listing and embedded seller)

## make ci

Last lines, ending in `ci: all checks passed`:

```text
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

Mid-phase CI-adjacent catches: a Gin wildcard clash (`{orderId}` vs `{id}` routes → path param renamed to `{id}`), unmapped provider-style 500s avoided by explicit error mapping, and the `sensitive` budget raised in the fixture (review writes are rate-limited).

## Manual QA

Live API (Auth emulator, local Postgres; tokens masked). Order/listing
seeded via SQL since checkout needs live Paystack keys:

```console
$ curl -X POST .../v1/orders/<id>/reviews -d '{"listingId":<id>,"rating":5,"comment":"Great maize."}'
{"comment":"Great maize.",...,"rating":5}
$ curl -X POST ... (again, rating 4)
{"error":{"code":"already_reviewed","message":"This listing was already reviewed in this order"}}
$ curl .../v1/listings/<slug>/reviews
{"items":[{...,"reviewerName":"Buyer"}],"meta":{...},"summary":{"average":5,"count":1}}
$ curl -X PUT .../v1/me/favorites/<id>  (twice)   → 204, 204
$ curl .../v1/me/favorites
{"items":[{"available":true,...}],"meta":{"limit":20,"page":1,"total":0→1}}
$ curl .../v1/sellers/<sellerId>  → "rating":{"average":5,"count":1}
$ curl -X POST .../v1/admin/reviews/<id>/hide (admin) -d '{"reason":"Spam."}'  → 200
$ curl .../v1/listings/<slug>/reviews
{"items":[],"meta":{...},"summary":{"average":0,"count":0}}
```

`favorite_count` read back as 1 after two PUTs. A log sample held no
comment bodies, emails or tokens (only listing slugs in access-log paths,
as with every listing route).

## Files changed

`git diff --stat c16373f..HEAD` plus the docs commit (`AGENTS.md`,
`REDEVELOPMENT_PLAN.md`, `docs/adr/0032-*.md`, this file). New files:

- `migrations/000018_reviews_favorites.{up,down}.sql`: reviews + favorites.
- `db/queries/engagement.sql`: review CRUD, aggregates, favorite add/remove with counter, favorites list projection.
- `internal/engagement/service.go`: buyer-gated review creation, aggregates, terminal hides, idempotent favorites.
- `internal/engagement/service_test.go`: spec table at service level.
- `internal/http/handlers/engagement.go`: six endpoints + safe mappers.
- `internal/http/engagement_test.go`: contract-validated endpoint tests.
- `internal/orders/reads.go`: `BuyerOf` for 403/404 separation.

## Schema changes

New migration 000018, `migrations_test` green inside `make ci`. No new
env vars.

## API changes

New `engagement` tag; new schemas `Review`, `PublicReview`,
`PublicReviewList`, `SellerRating`, `CreateReviewRequest`,
`HideReviewRequest`, `FavoriteListing`, `FavoriteList`; `rating` added to
`PublicSeller`, `ListingDetail` and `OrderDetail` (always present, zeros
when unreviewed):

- `POST /v1/orders/{id}/reviews` (`sensitive`) → 201/400/401/403/404/409
- `GET /v1/listings/{slug}/reviews` (public) → 200/400/404
- `PUT` / `DELETE /v1/me/favorites/{listingId}` → 204 (idempotent)
- `GET /v1/me/favorites` → 200 with `available` flags
- `POST /v1/admin/reviews/{id}/hide` (admin) → 200/400/401/403/404/409

`make api-lint` and `generate-check` pass; `api.gen.go` generated only.

## Deviations from the spec

All in ADR-0032. Notable: hide of an already-hidden review is 409 (spec
lists hide without the case); path param named `{id}` not `{orderId}`
(Gin wildcard clash); ratings use one extra indexed query per response.

## Open questions / risks

- N+1 rating lookups on search pages (≤ 50 indexed queries); batch if
  measurement ever complains.
- Zero-time `publishedAt` for favourited drafts (never-published
  listings): schema-valid, ugly. Flagging in case drafts should be
  unfavouritable.

## Backlog additions

None.
