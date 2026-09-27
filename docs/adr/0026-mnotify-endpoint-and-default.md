# ADR-0026: mNotify quick-endpoint verified, SMS off by default

- **Status:** Accepted (Phase 16).
- **Date:** 2026-09-27

## Context

Phase 16 sends transactional SMS through mNotify, but the spec's endpoint
details were written from memory and explicitly required verification against
mNotify's current docs before recording them.

## Decision

Probed live on 2026-09-26: `POST
https://api.mnotify.com/api/sms/quick?key=<MNOTIFY_API_KEY>` with JSON body
`{recipient: ["0241234567"], sender, message, is_schedule: false,
schedule_date: ""}`. mNotify expects the local `0XXXXXXXXX` format, so the
client converts from the E.164 numbers the users table holds. No test keys
exist locally, so no real SMS was sent; the wire shape is pinned by
`TestMNotify_Client` against httptest.

`NOTIFY_SMS_ENABLED` defaults to false and `MNOTIFY_API_KEY`/`MNOTIFY_SENDER`
are required only when it is true. With SMS off, the log-only sender records
each message with the number masked (`+233*****0001`), and the notify worker
still resolves the recipient's phone at send time — so enabling SMS later
changes delivery, not the state machine.

## Consequences

- Sending to a wrong-format number fails at mNotify, not in our code: the
  E.164→local conversion is the client's to get right, covered by the client
  test.
- A user with no phone on file is quietly skipped (proved by
  `TestNotifyJob_SkipsUserWithoutPhone`); phone capture stays Phase 7's job.
- The optional real-SMS manual QA step was not run (no credentials); the
  packet records the five masked log lines from the local run instead.
