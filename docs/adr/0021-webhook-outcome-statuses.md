# ADR-0021: Webhook outcomes map to three different statuses

- **Status:** Accepted (Phase 13a).
- **Date:** 2026-09-26

## Context

The Phase 13a spec fixes most of the webhook contract: a bad signature is 401,
a duplicate is 200, an unknown event is `ignored` with 200, a business rejection
such as an amount mismatch is `rejected:<reason>` with 200, and a handler error
rolls back and answers 500 "so Paystack retries — only do this for transient
DB errors".

It does not say what an *unparseable* body should answer. It falls between two
of the spec's cases: it is not a transient DB error, and retrying it cannot
help, because the same bytes will parse the same way next time.

## Decision

Three statuses, chosen by whether a retry could change the answer:

| Outcome | Status | Why |
|---|---|---|
| processed, ignored, duplicate, `rejected:<reason>` | 200 | The answer is final. Asking again wastes both sides. |
| malformed body (`payments.ErrMalformedEvent`) | 400 | The bytes are not an event. A resend of the same bytes fails identically. |
| anything else (a database or provider failure) | 500 | Transient. Paystack retries with backoff. |

The 500 path rolls back the `webhook_events` row along with everything else, so
a retry is a clean attempt rather than a row that claims to have been handled.

## Consequences

- Paystack's dashboard shows 400s as permanent failures, which is what they are.
  Investigating one means looking at the reference, not waiting for a retry.
- A body that is valid JSON but an event Farmish has never heard of is
  `ignored` and 200, not 400: the provider is behaving correctly, we simply
  do not act on that event yet.
- The distinction lives in one sentinel error (`ErrMalformedEvent`) and one
  branch in the handler, so adding a new permanent rejection later means
  returning that sentinel rather than inventing a status.
