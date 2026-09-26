# Review packet: Phase 11 Listings CRUD

## Summary

Sellers create, edit, publish, renew, mark sold, archive and delete
listings, with every DOMAIN §7 rule validated server-side: unit and item
state against the category's listing group, the 16 regions, the
seller-delivery fee, minimum order quantity and typed attribute values
(`select` options, boolean, number, date, length). Images are attached
through `media.Attach` inside the listing transaction, slugs are generated
on create with `-2`, `-3` … collision suffixes, and an hourly River job
expires listings past their 30-day window. Ownership is enforced under a
row lock (403), unknown ids are 404, suspended listings are frozen (409)
and every illegal state change is 409.

## Commits

`git log --oneline 18095f9..HEAD` output (plus this packet commit):

```
0e734b1 Phase 11: listings migration and queries
f778599 Phase 11: cross-domain seams for listings
64b7dd8 Phase 11: listings service with tests
21d84fb Phase 11: lint fixes in listings (bounded image count, test cleanup)
892f76a Phase 11: listings endpoints, handlers and wiring
3d1813c Phase 11: hourly listings.expire job
b7ac329 Phase 11: plan and handbook updates
(plus this packet commit; verify with git log --oneline 18095f9..HEAD)
```

## Done-when checklist

From REDEVELOPMENT_PLAN.md §6 (Phase 11) and the spec test table:

- [x] create → get → update → publish → archive works: `TestListings_ContractValid` and `TestListings_Lifecycle` (`internal/http/listings_test.go`), plus `TestListing_StatusTransitions` and `TestUpdate_KeepsActiveAndDropsForeignAttributes` (service)
- [x] a non-owner gets 403: `TestListing_OwnershipEnforced` (service) and `TestListings_OwnershipAndProfileGuards` (GET/PATCH/publish/archive/delete by another seller → 403, unknown → 404, no profile → 403 `seller_profile_required`, anonymous → 401)
- [x] invalid attributes return 400: `TestCreateListing_Attributes` (missing required, bad select, bad number, unknown key, bad date, bad boolean, too long, empty) and `TestListings_ValidationErrors` (unit, parent category, required attribute, unknown attribute, non-GHS currency, empty patch)
- [x] the expiry job flips past-due listings: `TestExpireListings` (service: only the due active one, drafts/sold/fresh untouched, then publishable again) and `TestExpireListings_Job` (through River)
- [x] `TestCreateListing_DraftAndPublish`: draft without images, publish fails on a missing image with a field error, then image + publish → active with `expires_at = published_at + 30d`
- [x] `TestCreateListing_Validation`: 15 table cases (title 4/101, price 0/over max, wrong unit/item state, min > qty, bad region, no delivery, missing/negative fee, >10 images, unknown/parent category, negative quantity) with the right field names
- [x] `TestListing_SlugUniqueness`: `x`, `x-2`, `x-3` and 5 concurrent creates all succeed with distinct slugs (no 500)
- [x] `TestListing_SuspendedIsFrozen`: suspended via SQL, then update/publish/renew/sold/archive/delete → 409
- [x] `TestListing_ImagesAttachOnce`: a media object already used by listing A → 409 `conflict` on B; another seller's media refused; the first listing keeps it
- [x] `TestListings_ContractValid`: `assertContract` on create, get, list and patch responses

Extras: `TestListOwn_FiltersAndCounts`, `TestCreateListing_RequiresSellerProfile`.

## make ci

Last 25 lines of `make ci` output, ending in `ci: all checks passed`:

```
#13 DONE 0.1s

#14 [build 6/6] RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build     CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/api ./cmd/migrate
#14 DONE 5.0s

#7 [stage-1 1/2] FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
#7 CACHED

#15 [stage-1 2/2] COPY --from=build /out/api /out/migrate /
#15 DONE 0.1s

#16 exporting to image
#16 exporting layers
#16 exporting layers 3.1s done
#16 exporting manifest sha256:1fd1ae1333aab07adf6b5ab20f7f8ee2a5dd2550c8343a4ebd11286be78ac7b3 0.0s done
#16 exporting config sha256:e85e979b99a014d21d7b5698d61a59d65917c3434ade30bbaeedda02a3f6cc3d 0.0s done
#16 exporting attestation manifest sha256:eb04f5b98549065cc238f3989114d767d760f8d8cdd1a871303bd71751ca8768 0.0s done
#16 exporting manifest list sha256:8baa6829f50b3c1da98b29caa32b42b2effaeee048c60391008210aad8e92837 0.0s done
#16 naming to docker.io/library/farmish-backend:dev done
#16 unpacking to docker.io/library/farmish-backend:dev
#16 unpacking to docker.io/library/farmish-backend:dev 0.3s done
#16 DONE 3.5s
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

## Manual QA

Against `make run` with the compose S3 stand-in, emulator user and a seller
profile (dev database, migration 7, catalog seeded):

1. Upload an image and create a listing with `publish: true` → 201 active
   with an expiry 30 days out:

```
upload=200
{
    "id": "48237198-8a8f-46a0-98ec-d918f909a703",
    "slug": "qa-healthy-friesian-heifer",
    "status": "active",
    "images": [{"mediaId": "31c20bdf-...", "sortOrder": 0,
                "url": "http://127.0.0.1:9000/farmish-dev/listings/01850f1e-.../038cb103-....jpg"}],
    "expiresAt": "2026-10-26T14:13:44.742262Z",   (publishedAt + 30d)
    ...
}
```

2. `GET /v1/me/listings` lists it; `PATCH` the price; `mark-sold`; a renew
   of a sold listing is 409:

```
GET /v1/me/listings  → {'limit': 20, 'page': 1, 'total': 1} [('qa-healthy-friesian-heifer', 'active', 1)]
PATCH price           → price {'amount': 900000, 'currency': 'GHS'} status active slug qa-healthy-friesian-heifer
POST /mark-sold       → status sold
POST /renew           → {"error":{"code":"invalid_transition","message":"That action is not allowed in this state"}}
```

3. A second seller `PATCH`es it → 403; a user without a seller profile gets
   403 `seller_profile_required`:

```
patch_by_other=403 {"error":{"code":"forbidden",...}}
{"error":{"code":"seller_profile_required","message":"Create a seller profile before listing"}}
```

The server was stopped afterwards; no presigned URL appears in its log
(`grep -c X-Amz-Signature /tmp/api11.log` → 0).

## Files changed

`git diff --stat 18095f9..HEAD` (26 files, +6363/-261). New files:

- `migrations/000007_listings.{up,down}.sql`: listings, listing_images,
  listing_attribute_values
- `db/queries/listings.sql` → generated `internal/db/listings.sql.go`
- `internal/listings/{listings,service,jobs}.go` + `service_test.go`,
  `jobs_test.go`
- `internal/http/handlers/listings.go`, `internal/http/listings_test.go`
- `docs/reviews/phase-11.md` (this packet)
- Changed: `internal/catalog/service.go` (`Resolved`, `IsLeaf`),
  `internal/media/service.go` (`PublicURL`), `internal/sellers/service.go`
  (`Exists`), `api/openapi.yaml` + regenerated client, `apierror` codes,
  `cmd/api/main.go` (services built once, shared by workers and router),
  `AGENTS.md`, `REDEVELOPMENT_PLAN.md`

## Schema changes

New migration `000007_listings`: `listings` (DOMAIN §7 limits, delivery
CHECKs, status CHECK, and "published implies published_at + expires_at"),
`listing_images` (a media object attaches to at most one listing) and
`listing_attribute_values`. Down drops all three. `migrations_test`
up→down→up passes under `make test`.

## API changes

Tag `listings` with nine operations: `createListing` (201, sensitive),
`listMyListings`, `getMyListing`, `updateListing`, `deleteListing` (204),
`publishListing`, `renewListing`, `markListingSold`, `archiveListing`.
New schemas: `CreateListingRequest`, `UpdateListingRequest`,
`ListingDelivery`, `SellerListing`, `SellerListingImage`,
`SellerListingAttribute`, `SellerListingSummary(List)`; new error codes
`seller_profile_required`, `listing_suspended`, `invalid_transition`.
`make api-lint` and `generate-check` pass.

## Deviations from the spec

- `PATCH /v1/listings/{id}` takes a fully optional body. The spec says
  "ListingInput (all fields optional)", which the create schema is not, so
  the service takes a pointer `Patch`, merges it over the stored row inside
  the locked transaction, and validates the merged result. That is the only
  way a partial edit can still be checked as a whole (a bare
  `minOrderQty ≤ quantity` rule needs both values).
- `SellerListing` deliberately carries no seller identifier, email, phone
  or rating: the public listing detail is Phase 12 and this is the owner's
  own view. No ADR needed; noted for the reviewer.

## Open questions / risks

- Reviewer: the four status operations map errors explicitly in each
  handler (no shared generic helper): Go generics over four different
  generated response types cost more readability than they save here.
- Reviewer: admin transitions (any → suspended, suspended →
  active/archived) are Phase 20b; this phase only refuses edits while
  suspended, and the test suspends via SQL because moderation doesn't
  exist yet.
- Deleting only drafts is enforced in the service; the API answers 409
  `invalid_transition` with a message pointing at archive.
- `cmd/api/main.go` now builds Firebase, media, sellers and listings once,
  before the job client, so the expiry sweep also runs in
  `RUN_MODE=worker` (it needs no HTTP). The Firebase emulator warning now
  prints in worker mode too.

## Backlog additions

- Detached-media cleanup when an image is removed from a listing: the
  media row stays `attached` (spec: Backlog).
