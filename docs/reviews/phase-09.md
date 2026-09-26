# Review packet: Phase 09 Catalog (categories, attributes, locations)

## Summary

The reference catalog is live: a two-level category tree with typed
attributes and listing groups (DOMAIN §8–9), an idempotent `make seed`
ported from the legacy app, public reads with `Cache-Control`, admin
category/attribute CRUD, and the 16-region locations endpoint with district
suggestions. One gap fix (ADR-0015): `CategoryAttribute` carries its `id`
so admin clients can address attributes.

## Commits

`git log --oneline 56f1c55..HEAD` output (plus this packet commit):

```
b640d52 Phase 9: catalog migration and queries
8d73b8e Phase 9: slug, groups and districts reference data
6fdf02e Phase 9: catalog service, seed and seed command
fc63a45 Phase 9: catalog endpoints, handlers and wiring
458868a Phase 9: lint fixes for catalog code
(plus this packet commit; verify with git log --oneline 56f1c55..HEAD)
```

## Done-when checklist

From REDEVELOPMENT_PLAN.md §6 (Phase 9) and the spec test table:

- [x] Seed is idempotent: `TestSeed_MatchesDomainAndIsIdempotent` (`internal/catalog/seed_test.go`) asserts 12 parents, 72 children, per-parent attribute counts, then re-seeds and asserts counts plus `updated_at` unchanged
- [x] Tree endpoint matches the seed data: `TestCategoriesTree_MatchesSeed` (`internal/http/catalog_test.go`) asserts order, names, the prefixed `seeds-seedlings-flowers-ornamentals` slug, `Cache-Control: public, max-age=300` and `assertContract`
- [x] A child inherits its group and attributes: `TestCategoryDetail_ChildInheritsGroupAndAttributes` asserts the livestock states, COUNT + heads units and the parent's `breed/vaccinated/weight_kg` attributes on `livestock-poultry-cattle`
- [x] `/v1/locations` serves the 16 regions: `TestLocations_SixteenRegions` asserts 16 regions, Greater Accra districts and `Cache-Control: public, max-age=86400`
- [x] `TestSlugify` (`internal/text/slug_test.go`): `&` dropped, accents folded, 80-char cap, no edge dashes
- [x] `TestCategoryDetail_UnknownSlug404`
- [x] `TestAdminCategories_CRUDAndGuards`: parent + child create, child-of-child 400, duplicate slug 409, non-admin 403, deactivate hides from tree
- [x] Child attribute override/merge (Pitfalls): `TestChildAttributes_OverrideAndMerge` plus endpoint coverage in `TestAdminAttributes_CRUD` (201/409/400/200/204/404, cross-category scoping 404)

Extras: `TestGroups`, `TestRegionDistricts` (sorted, deduped, cross-region shares kept).

## make ci

Last 25 lines of `make ci` output, ending in `ci: all checks passed`:

```
#13 DONE 0.2s

#14 [build 6/6] RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build     CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/api ./cmd/migrate
#14 DONE 8.1s

#7 [stage-1 1/2] FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
#7 CACHED

#15 [stage-1 2/2] COPY --from=build /out/api /out/migrate /
#15 DONE 0.1s

#16 exporting to image
#16 exporting layers
#16 exporting layers 9.4s done
#16 exporting manifest sha256:e0237a352fbf0ffec364b4985e6965c36a8792d3cb450e1a5ee7d5f923c179a4 0.0s done
#16 exporting config sha256:9546eae701001fdcbb7e54b2d3fa94a8bb6a9a02d14b1684e21f7eb20e25a225 0.0s done
#16 exporting attestation manifest sha256:91bc74bf650ea48620fb8d046cea4899ae0c064d8c0df3152a25871d9b5374ea 0.0s done
#16 exporting manifest list sha256:7b4b90e9ee9a02e9407b3c0415c5165f758a50944f3274986eced4e71d913d51 0.0s done
#16 naming to docker.io/library/farmish-backend:dev done
#16 unpacking to docker.io/library/farmish-backend:dev
#16 unpacking to docker.io/library/farmish-backend:dev 1.1s done
#16 DONE 10.7s
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

## Manual QA

Spec QA script against `make run` (dev DB at version 5, seeded):

1. `make seed` twice (both log `catalog seeded`), then:
   `curl localhost:8080/v1/categories | jq '.items | length'` → `12`
2. `Cache-Control` on GETs: `public, max-age=300` (categories, detail)
   and `public, max-age=86400` (locations). (Note: `curl -I` sends HEAD,
   which the routes don't serve; headers verified on GET.)
3. `curl /v1/categories/land-leasing | jq .attributes`:
   `land_size_acres` (`number`, `required: true`), `soil_type` and
   `water_source` selects with the DOMAIN §9 options.

## Files changed

`git diff --stat 56f1c55..HEAD` (plus this packet):

- `migrations/000005_catalog.{up,down}.sql`: categories + attributes tables
- `db/queries/catalog.sql` → generated `internal/db/catalog.sql.go`
- `internal/text/slug.go`: Slugify (new package)
- `internal/catalog/`: `groups.go`, `catalog.go`, `service.go`, `seeddata.go`, `seed.go` + tests
- `internal/geo/districts.go`: districts port
- `cmd/seed/main.go` + `make seed`
- `api/openapi.yaml` + regenerated `internal/http/api/api.gen.go`: 8 catalog operations, 9 schemas, shared `Conflict` reuse
- `internal/http/handlers/catalog.go`, `Server.Catalog`, `Deps.Catalog`, `cmd/api` wiring, `apierror.CodeConflict`
- `internal/http/catalog_test.go`: endpoint tests
- `docs/adr/0015-category-attribute-id.md`, `docs/reviews/phase-09.md`
- `AGENTS.md` (`make seed` row, Phase 10 next), `REDEVELOPMENT_PLAN.md` (§6 tick, §8/§9 rows)

## Schema changes

New migration `000005_catalog` (categories, category_attributes). Down
drops both. `migrations_test` up→down→up passes under `make test`.

## API changes

Tag `catalog`: `listCategories`, `getCategory`, `getLocations` (public,
`Cache-Control` response headers), `createCategory` (201),
`updateCategory`, `createCategoryAttribute` (201),
`updateCategoryAttribute`, `deleteCategoryAttribute` (204, admin).
`make api-lint` and `generate-check` pass.

## Deviations from the spec

ADR-0015: `CategoryAttribute` gains required `id`. The spec's PATCH/DELETE
paths address attributes by id, but no schema exposed one, so admin
clients could never discover it. Nothing else changes.

## Open questions / risks

- Reviewer: PATCH on categories deliberately never changes slug or group
  (stable URLs; group moves would orphan listing validation) — spec is
  silent, so this is a judgment call, not a deviation.
- Reviewer: admin review-queue-style ordering isn't specified for
  `ListActiveCategories`; tree order is (sort_order, name) at both
  levels, enforced in Go for determinism.
- `go mod tidy` promoted `golang.org/x/text` to a direct dependency
  (accent folding in Slugify).

## Backlog additions

None.
