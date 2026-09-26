# ADR-0015: CategoryAttribute carries its id

- **Status:** Accepted (Phase 9).
- **Date:** 2026-09-26

## Context

The Phase 9 spec defines `CategoryAttribute` as
`{key, label, type, options, required}` but addresses attributes by
`{attributeId}` in the PATCH and DELETE endpoints. With no id anywhere in
the API, clients could never discover one: POST 201 returns a
`CategoryAttribute`, and the detail view embeds the same schema.

## Decision

`CategoryAttribute` gains `id` (uuid, required). Nothing else changes: the
admin endpoints, status codes and merge rules are exactly as specified.

## Consequences

- Admin clients learn ids from POST 201 and the detail view, then use them
  in PATCH/DELETE paths.
- Public projections are unaffected (the tree never embeds attributes).
