# ADR-0019: View dedup hashes the address with DATA_ENCRYPTION_KEY

- **Status:** Accepted (Phase 12, owner decision).
- **Date:** 2026-09-26

## Context

Phase 12 counts a listing view through the `listings.count_view` job,
deduplicated to one count per listing, viewer and hour. That needs a stable
viewer identifier, and the only per-visitor signal on a public page is the
client address. The address must never be stored: not in the listings table,
not in River's `river_job.args`, not in a log line.

An unsalted digest is not enough. SHA-256 of an IPv4 address is reversible by
brute force over the 4-billion-address space in minutes, so "hashed" would
give a false sense of privacy.

## Decision

Derive the viewer id as `HMAC-SHA256(DATA_ENCRYPTION_KEY, "view:" || ip)`,
truncated to 16 bytes and formatted as a uuid. No new secret: the key is
already required, validated and fail-fast in `internal/config`, and the
`view:` prefix is domain separation so the digest is not interchangeable with
anything else derived from the same key.

The raw address is read once, inside `handlers.NewViewerHasher`, and is never
returned, stored or logged. A request with no resolvable address gets the nil
uuid rather than a shared "unknown" id, so anonymous requests from different
visitors are not collapsed into one viewer.

## Consequences

- Dedupe is stable within a deployment and meaningless outside it: the digest
  cannot be correlated across deployments, and rotating the key shifts view
  counts (harmless, and counts are not money).
- The same key now serves two purposes. That is accepted here because the
  digest is one-way and domain-separated; if a second *reversible* use ever
  needs a key, a dedicated `VIEW_HASH_SECRET` is the right move.
- The job stores only the digest, so a database dump cannot be turned back
  into visitor addresses.
