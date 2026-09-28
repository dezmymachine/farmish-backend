# ADR-0034: Observability and hardening

- **Status:** Accepted
- **Date:** 2026-09-28
- **Phase:** 21

## Context

Phase 21 makes the API operable: metrics, a log guarantee, audit coverage,
response hardening, a load test and an alert list. Several choices had more than
one reasonable answer, and a few things the spec asked for turned out to need
adjusting once they met the code.

## Decisions

1. **Metrics are package-level collectors on a private registry.**
   `internal/metrics` holds the counters, so a domain package records an event
   in one line and no constructor grows a recorder argument. The collectors that
   read the database or River (`db_pool_*`, `jobs_queue_depth`, the job event
   stream) live in `internal/metrics/collect`, so packages that only count
   (rate limiting, middleware) do not depend on the database layer.
2. **`/metrics` is a separate listener.** `METRICS_ADDR` defaults to
   `127.0.0.1:9090`; `off` disables it. The public router serves no metrics
   route (a test asserts 404). The listener binds synchronously at startup, so a
   taken port fails boot instead of silently losing metrics. It runs in every
   `RUN_MODE`, since worker processes carry the job and payout series.
3. **Route labels are templates.** `c.FullPath()`, with the constant `unmatched`
   for requests no route matched, so scanners cannot mint label values.
   `/healthz` and `/readyz` are excluded from the HTTP series, otherwise
   orchestrator probes dominate the 5xx ratio.
4. **Money counters increment inside the transaction.** The state change is in
   the same closure, so the counter cannot move without the change, but a
   rollback after the increment (a serialization retry) counts twice. For alerts
   that is acceptable, and the runbook says the audit row is the source of
   truth. Counting after commit would need an after-commit hook that the
   codebase does not have, and most of these sites run inside a webhook
   transaction that returns an outcome string.
5. **Logs are redacted twice.** Call sites still mask (`geo.MaskPhone`, now a
   thin wrapper over `redact.Phone`), and `logger.New` wraps its handler in
   `redact.Handler`, which redacts by key (`phone`, `token`, `authorization`,
   `id_number`, `email`, `secret`, `account_number`, and a few Paystack fields)
   and by value (JWTs, bearer credentials, `+233XXXXXXXXX`). `TestLogs_NoSecretsOrPII`
   captures the fixtures' logs from the **raw** JSON handler, without the
   redactor, so it proves the call sites are clean; the handler is the second
   line of defence, not the reason the test passes. A mutation check (log an
   unmasked phone) fails it.
6. **mNotify errors no longer carry the API key.** The key travels in the query
   string, and `net/http` prints the full URL in a transport error, so
   `MNotify.Send` would have put the key into any log that recorded a network
   failure. Both `Send` and the new `Balance` strip the URL from transport
   errors (`TestMNotify_TransportErrorsDoNotLeakTheKey`).
7. **Audit gaps closed.** Added `payment.succeeded`, `refund.queued`,
   `refund.processed`, `payout.created`, `payout.succeeded`, `dispute.opened`
   and `user.role_change`; the others already existed. The operator CLI writes
   the role change and its audit row in one transaction, then mirrors the
   Firebase claim after commit (an external call never runs inside a
   transaction), which split `users.SetRole` into `SetRoleDB` and
   `SyncRoleClaim`. `TestAudit_Coverage` drives every action through the real
   endpoints and workers, and fails if its groups stop covering the required
   list; `cmd/admin` proves `user.role_change` in its own test.
8. **Security headers are set before the handler.** They reach errors,
   preflights and 404s. `Cache-Control: no-store` is the default for any
   request carrying an `Authorization` header (a rejected credential included),
   and a handler that sets its own policy replaces it (the public catalog keeps
   `public, max-age=300` even for a signed-in caller). HSTS only in staging and
   production.
9. **The load test runs on the host.** `make loadtest` builds the API and the
   Paystack stub, runs both against the compose services, seeds fixtures and
   drives them with the pinned `grafana/k6:2.3.0` image on the host network.
   Per-IP limits stay on: each virtual user presents its own address through
   `X-Forwarded-For`, which only works because the run trusts loopback as a
   proxy, and each has its own buyer because checkout is limited per user. The
   stub is guarded in config: `APP_ENV=production` refuses any
   `PAYSTACK_BASE_URL` but `https://api.paystack.co`. The run also asserts the
   metrics recorded the traffic, so it proves the private listener end to end.
   It is not part of `make ci` (it takes about two minutes and pulls an image);
   run it before a deploy.
10. **The mNotify balance check.** `GET https://api.mnotify.com/api/balance/sms?key=`
    was probed live on 2026-09-28: an invalid key answers 401 with a JSON
    `{"error": ...}` envelope, while a wrong path returns an HTML page, which
    confirms the route and the `key` parameter. **The success body was not
    verified**, because no live key exists here, so it is decoded tolerantly
    (a JSON number or a numeric string under `balance`) and both shapes are
    pinned against httptest. `notify.check_balance` runs daily at 08:00
    Africa/Accra, only when `NOTIFY_SMS_ENABLED=true`, logs an Error under
    `MNOTIFY_LOW_BALANCE` (a new variable, default 200 credits) and publishes
    `mnotify_balance`.

## Deviations from the spec

- **`db_pool_acquire_duration_seconds` is a cumulative counter, not a
  histogram.** `pgxpool.Stat` exposes only the running total, so the series is
  that total, with `db_pool_acquires_total` and `db_pool_empty_acquires_total`
  added so an average and a wait rate can be derived.
- **Extra series:** `mnotify_balance` (decision 10) and the two pool counters.
  Every listed series is present; `jobs_queue_depth` exports all River states,
  with an explicit 0 for an empty one.
- **`jobs_failed_total` counts failed attempts,** not jobs that gave up, because
  that is the event River publishes. A job that fails once and then succeeds
  counts in both counters.
- **Two alerts live outside the code:** `/readyz` and the Firebase SMS budget
  are platform configuration (documented in `docs/runbooks/alerts.md`).

## Consequences

- Every process exposes its own `/metrics`; alert rules must sum across
  instances (the runbook says so).
- The load test found nothing in the API, but its first run 500'd on search: a
  seeded listing had no image, and the search read cannot scan a NULL
  `cover_key`. Publishing requires an image, so production data cannot get there,
  and the seed now attaches one. Any path that leaves a live listing without an
  image row would still take search down; that is in the backlog.
- Adding a sensitive log key means adding it to `redact`'s list; the test only
  proves the flows it drives.
