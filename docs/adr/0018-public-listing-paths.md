# ADR-0018: Seller listing writes move under /v1/me

- **Status:** Accepted (Phase 12, owner decision).
- **Date:** 2026-09-26

## Context

The Phase 12 spec defines the public listing page as
`GET /v1/listings/{slug}`. Phase 11 already shipped the seller's writes at
`/v1/listings/{id}` (PATCH, DELETE) and `/v1/listings/{id}/publish`, `/renew`,
`/mark-sold`, `/archive`.

OpenAPI 3.1 forbids two paths that differ only by a path-parameter name, and
the contract linter enforces it:

```
no-identical-paths  The path already exists which differs only by path
                    parameter name(s): `/v1/listings/{slug}` and `/v1/listings/{id}`.
no-ambiguous-paths  Paths should resolve unambiguously.
```

So the spec's URL and the existing seller paths cannot both exist. The
frontend is built from this contract after Phase 22, so the URL had to be
settled now rather than at integration time.

## Decision

The seller's listing operations move to `/v1/me/listings/{id}` (and its four
sub-paths), keeping their operation ids, parameters, status codes and
semantics unchanged. `/v1/listings` becomes purely public: the search
collection, the detail page by slug, and the contact reveal.

`/v1/me/listings` already carried the seller's own list, and
`/v1/me/listings/{id}` their own listing, so the seller's surface was already
coherent under `/v1/me`.

## Consequences

- The spec's public URL is implemented as written.
- One path parameter, one meaning: `{id}` is always a uuid the caller owns,
  `{slug}` is always a public slug. No endpoint accepts "either".
- Clients must be built against the moved seller paths. Nothing outside this
  repo is affected yet, since the frontend starts after the contract freezes.
- The six moved operations are the only breaking change to the contract so
  far; their OpenAPI operation ids are unchanged, so generated method names
  and handler signatures did not move.
