# ADR-0033: Reports, supply requests and shared spec parsing

- **Status:** Accepted
- **Date:** 2026-09-28
- **Phase:** 20b

## Context

Reports moderate listings and users; supply requests revive the legacy
bulk-quote flow with a real status machine.

## Decisions

1. **Report `hide_review` names its review.** The resolve body takes an
   optional `reviewId`: required for `hide_review`, forbidden otherwise,
   hiding in the same transaction (owner decision).
2. **Suspension reuses the listing write path.** `listings.SuspendInTx`
   moves any status to `suspended` in the report's transaction; the
   Phase 11 guards (no owner edits, invisible to search) already cover
   the rest, and are pinned by reusing the Phase 11 test shape.
3. **Supply creation writes a pending event and SMS.** Strictly the spec
   requires events per transition; creation emits both too, so every
   request opens with confirmation and a trail row.
4. **MoMo-style contact defaults.** Omitted delivery name/phone fall back
   to the user's display name and phone (legacy behavior), normalized
   through `geo.NormalizeGhanaPhone`; dates compare as days in
   Africa/Accra (UTC+0).
5. **The parsed OpenAPI spec is memoized.** The http suite outgrew the
   10-minute package timeout: every router build parsed the spec 4–5
   times and every contract assertion re-parsed it, ~600 parses per run.
   `api.CachedSpec` parses once per process (3× faster, 10:07 → 3:21
   solo). The spec is immutable at runtime, so sharing is safe.
