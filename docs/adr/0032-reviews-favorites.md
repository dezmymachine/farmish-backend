# ADR-0032: Reviews, favorites and rating display

- **Status:** Accepted
- **Date:** 2026-09-28
- **Phase:** 20a

## Context

Reviews tie a listing to the completed order it was bought in; favorites
are idempotent with an exact counter; seller aggregates show on public
surfaces.

## Decisions

1. **Strangers get 403, not 404, on conversations they are not part of —
   and the same shape for reviews.** `BuyerOf` separates "no such order"
   (404) from "not yours" (403) without leaking which, mirroring the
   orders convention.
2. **Hide is terminal and audited.** An already-hidden review refuses with
   409 rather than succeeding silently, so moderation actions stay visible
   in the audit trail.
3. **Favorites resolve to the search projection.** The favorites list query
   mirrors `SearchListings` columns (cover, promo, safe seller) plus an
   `available` flag, so one mapper serves both and inactive listings show
   flagged rather than vanishing.
4. **Ratings ride on existing reads.** `PublicSeller`, `ListingDetail` and
   `OrderDetail` carry the seller aggregate (average to one decimal, 0
   with no reviews) via one indexed query per response; list/search pages
   accept the N+1 until measurement says otherwise.
