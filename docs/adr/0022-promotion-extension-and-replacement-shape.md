# ADR-0022: Same-tier promotions queue; replacements bridge by one microsecond

- **Status:** Accepted (Phase 14).
- **Date:** 2026-09-26

## Context

DOMAIN §6 extends an active same-tier promotion by its duration and replaces it
with a higher tier from the replacement time. The Phase 14 service always
inserts a promotion-application row. `listing_promotions` also requires
`ends_at > starts_at`.

Two edge cases need a concrete shape:

1. A same-tier renewal could either update the current row's expiry or insert a
   follow-on row starting at the current expiry.
2. A higher-tier replacement at the exact instant the old window started cannot
   set the old expiry equal to the replacement time without violating
   `ends_at > starts_at`.

## Decision

- Same-tier applications insert a follow-on row whose `starts_at` is the
  current row's `ends_at`. No history is rewritten.
- Higher-tier replacements end every active row, then insert the replacement.
  If the old window started at the replacement instant, its expiry moves one
  microsecond after the replacement time solely to preserve the schema
  invariant. Search already selects the highest active tier rank, so the
  replacement still outranks the forfeited microsecond bridge.

## Consequences

- A same-tier renewal appears as two adjacent history rows rather than one
  edited row.
- At the exact replacement instant, an active-window count may briefly see two
  rows; the highest rank decides which one is effective.
- The old window's remaining time is never silently revived: an upgrade ends it
  immediately, except for the one-microsecond bridge required by the schema.
