# ADR-0024: One seller-order uses its highest applicable commission rate

- **Status:** Accepted (Phase 15a).
- **Date:** 2026-09-26

## Context

DOMAIN §2.1 resolves commission first by a listing's category, then by its
parent, then by the default. A seller-order can nevertheless contain listings
from different categories. Phase 15a stores one `commission_rate_bps` per
order, so a single rate has to be chosen when its lines resolve differently.

## Decision

Resolve each line under DOMAIN §2.1—its own category override first, then its
parent override, then the default—and use the highest of those line-level rates
for the order. The order's commission is then calculated on the whole order
subtotal at that rate.

## Consequences

- A lower child override still wins over a higher parent override for a
  one-category order, preserving DOMAIN §2.1's hierarchy.
- Mixed-category orders never undercharge the platform relative to any line's
  applicable rate.
- Phase 15b must snapshot the selected rate on the order before later config
  edits, because the applicable rates can change after a quote.
