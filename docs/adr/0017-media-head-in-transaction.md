# ADR-0017: Storage Head inside the media attach transaction

- **Status:** Accepted (Phase 10).
- **Date:** 2026-09-26

## Context

`ENGINEERING_GUIDE.md` §3 says never to call an external HTTP API inside a
database transaction. `media.Service.Attach` must check that the bytes the
client was asked to upload really exist in storage, with the declared size
and content type, and Phase 11 will call it inside the listing's
transaction.

## Decision

`Attach` performs the storage `HeadObject` before taking any row lock, and
keeps it inside the caller's transaction (the spec asks for the same).
Rationale: it is a single fast read against our own bucket, not a
third-party API, and it must not race with the state write that follows.

## Consequences

- The listing transaction holds a connection while one HEAD request is in
  flight. The S3 client has a bounded timeout, so the worst case is that
  timeout, and only for attach operations (never for ordinary reads).
- If this ever grows into a multi-call storage check, move the `Head` before
  `InTx` and pass the result in.
