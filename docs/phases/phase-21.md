# Phase 21: Observability & hardening

**Depends on:** 18b, 19, 20a, 20b · **Size:** medium

## Goal

Production readiness:
- Prometheus metrics
- log redaction guarantees
- audit coverage
- security headers
- k6 smoke load tests
- a documented alerts list

## Metrics (`/metrics`)

Use `github.com/prometheus/client_golang`. Mount `/metrics` on a **separate listener**, `METRICS_ADDR` (default `127.0.0.1:9090`), not the public port. Railway can scrape it privately. The main router gets none of it.
- **HTTP:** `http_requests_total{method,route,status}`, `http_request_duration_seconds{method,route}` (histogram). Use `c.FullPath()` for `route`: **never** the raw path, which would cause cardinality explosions.
- **DB pool:** `db_pool_acquired_conns`, `db_pool_idle_conns`, `db_pool_total_conns`, `db_pool_acquire_duration_seconds` (from `pool.Stat()`, collected on scrape).
- **River:** `jobs_queue_depth{state}` (a count query on scrape, cached for 15s), `jobs_completed_total{kind}`, `jobs_failed_total{kind}`, from River event subscription.
- **Money:**
  - `payments_total{purpose,status}`, `payment_amount_mismatch_total`
  - `payouts_total{status}`, `payout_failures_total`, `refunds_total{status}`
  - `ledger_reconcile_violations` (gauge, set by the reconcile job)
- **Rate limiting:** `ratelimit_denied_total{rule}`, `ratelimit_fallback_total`.

## Redaction

- **`pkg/redact`:** `Phone(s)`, `AccountNumber(s)`, `Email(s)`.
- A **slog handler wrapper** that redacts attributes by key (`phone`, `phone_e164`, `account_number`, `token`, `authorization`, `id_number`, `email`, `secret`) and by value pattern:
  - strings matching `^\+233[0-9]{9}$` → masked
  - a JWT-looking `eyJ…` → `[redacted-jwt]`

  Install it in `logger.New`.
- **Test** `TestLogs_NoSecretsOrPII`: run the full e2e scenario (sign-in, listing, checkout, webhook, payout) with the logger writing to a buffer, then assert the buffer contains **no** test phone, JWT, account number, Paystack secret or webhook signature. This is the plan's Done-when.

## Audit coverage

Ensure `audit_events` exist for:
- payment success
- amount mismatch
- refund queued, processed and failed
- payout created, succeeded and failed
- payout account set and approved
- dispute opened and resolved
- seller verified and rejected
- report resolved
- admin role changes (`cmd/admin`)
- supply request admin transitions

`TestAudit_Coverage` table-drives each action and asserts the row.

## Security headers (middleware, API responses)

- `X-Content-Type-Options: nosniff`
- `Referrer-Policy: no-referrer`
- `X-Frame-Options: DENY`
- `Strict-Transport-Security: max-age=31536000; includeSubDomains` (only when `APP_ENV` is staging or production)
- `Cache-Control: no-store` default on authenticated responses, unless a handler set one

Test them all.

## k6 (`loadtest/`)

Scripts: `browse.js` (search + detail), `checkout.js` (quote + checkout against the fake-Paystack mode), `webhook.js` (signed replays).
- **Thresholds:** `http_req_failed < 1%`, `p(95) < 300ms` for browse, `< 800ms` for checkout, locally against compose.
- Add `make loadtest`, running k6 from its Docker image `grafana/k6` (pinned).
- A **fake-Paystack mode** for load tests: `PAYSTACK_BASE_URL` points at a tiny stub server in `loadtest/paystack-stub` (Go) that returns success. **Never** enable it in production: config refuses a non-`api.paystack.co` URL when `APP_ENV=production`.

## Alerts list (`docs/runbooks/alerts.md`)

For each alert: signal (metric or log), threshold, severity, first response.
- payment amount mismatch (any)
- payout failures (>0/hour)
- ledger reconcile violations (>0)
- refund failures
- River queue depth >1000 for 10 min
- `/readyz` failing
- 5xx rate >2%
- Upstash fallback active >5 min
- Firebase SMS budget alert (GCP)
- mNotify balance low (a daily check job: mNotify has a balance endpoint; **verify it** and add `notify.check_balance`)

## Done when

The k6 thresholds pass locally, the log-sample test finds no secrets or PII, the metrics endpoint exposes all of the above, and the audit coverage test passes.
