# Alerts

Ten alerts guard the money path and the platform. Each has a **signal** (the
metric or log line it comes from), a **threshold**, a **severity** and the
**first response**. The metric names are the ones `/metrics` exposes (Phase 21,
ADR-0034); the audit actions are the ones `audit_events` records.

Severity: **page** means wake someone, **ticket** means handle in working hours.

## Wiring

- `/metrics` is on its own listener, `METRICS_ADDR` (default `127.0.0.1:9090`),
  never the public port. On Railway set `METRICS_ADDR=0.0.0.0:9090` and scrape
  the service over the private network. Every process has its own `/metrics`:
  API instances carry the HTTP series, worker processes (`RUN_MODE=worker`)
  carry the job and payout series, so alert rules **sum across instances**.
- Counters are per process and reset on restart. Alert on `increase()` or
  `rate()`, never on the raw value.
- The money counters are incremented inside the transaction that changes the
  state. A rollback after the increment (a serialization retry) can count an
  event twice, so a "greater than zero" alert may fire on a phantom; the audit
  row is the source of truth, and the first response always starts there.
- The HTTP series exclude `/healthz` and `/readyz`, so the 5xx ratio is real
  traffic.

## The alerts

### 1. Payment amount mismatch: page

- **Signal:** `payment_amount_mismatch_total`; Error log `payment amount
  mismatch`; audit action `payment.amount_mismatch`.
- **Threshold:** `increase(payment_amount_mismatch_total[10m]) > 0`. Any.
- **Meaning:** a `charge.success` webhook carried an amount or currency that
  differs from the payment. The payment was **not** settled and stays pending.
- **First response:**
  1. Find the row: `SELECT * FROM audit_events WHERE action =
     'payment.amount_mismatch' ORDER BY id DESC LIMIT 5`. `metadata` has the
     reference, expected and received amounts and the currency.
  2. Look the reference up in the Paystack dashboard. A real charge for the
     wrong amount is Paystack's or the buyer's problem to explain; do not
     settle it by hand.
  3. If the buyer really paid, refund it in the dashboard and let the checkout
     expire. If it looks like tampering, keep the evidence and escalate.

### 2. Payout failures: page

- **Signal:** `payout_failures_total`, `payouts_total{status="failed|reversed"}`;
  Error log `payout failed; payable re-credited`; audit `payout.failed`.
- **Threshold:** `increase(payout_failures_total[1h]) > 0`.
- **Meaning:** a Paystack transfer failed or was reversed. The seller's payable
  balance was re-credited by the ledger, so no money is lost, but the seller is
  unpaid.
- **First response:** read the audit `metadata.reason`. Fix the cause (a closed
  mobile-money wallet, an insufficient Paystack balance, a bad recipient), then
  retry with `POST /v1/admin/payouts/{id}/retry`. Never retry while the
  original may still be in flight: the endpoint refuses (409) unless the payout
  is failed or reversed.

### 3. Ledger reconcile violations: page

- **Signal:** gauge `ledger_reconcile_violations`; Error log `ledger
  reconciliation violation`; audit `ledger.reconcile`.
- **Threshold:** `ledger_reconcile_violations > 0` (set by the daily 03:00
  Africa/Accra job; it clears on the next clean run).
- **Meaning:** the ledger and the operational tables disagree, or a posting
  does not balance. This is the most serious alert in the system.
- **First response:** stop payouts (the batch runs daily at 10:00 Africa/Accra;
  pause the worker process until this is understood), read the audit row's `metadata.violations` for the failed check,
  and reconcile by hand against `ledger_entries`. Do not post corrections
  without a second pair of eyes. ADR-0028 describes the checks.

### 4. Refund failures: ticket, page during a dispute

- **Signal:** `refunds_total{status="failed"}`; Error log `refund failed;
  escrow stays held for an admin`; audit `refund.failed`.
- **Threshold:** `increase(refunds_total{status="failed"}[15m]) > 0`.
- **Meaning:** Paystack refused the refund. Escrow stays held, so the buyer has
  not been repaid and the seller has not been paid.
- **First response:** read `metadata.reason`; fix it in the Paystack dashboard
  (balance, an already-refunded transaction), then `POST
  /v1/admin/refunds/{id}/retry`. Tell the buyer.

### 5. Job queue backlog: ticket, page if payouts or refunds are stuck

- **Signal:** `jobs_queue_depth{state}`; also `jobs_failed_total{kind}`.
- **Threshold:** `jobs_queue_depth{state="available"} > 1000` for 10 minutes.
- **Meaning:** workers are down, starved or slower than the inflow. Webhooks,
  SMS, refunds and payouts all ride this queue.
- **First response:** is a worker process running (`RUN_MODE=all|worker`)? Check
  `jobs_failed_total` by `kind` for a poison job, and `db_pool_acquired_conns`
  against `DB_MAX_CONNS` for a starved pool. A single kind failing repeatedly is
  a bug to fix; a general backlog wants more `JOBS_MAX_WORKERS` or instances.

### 6. `/readyz` failing: page

- **Signal:** the platform health check or an external uptime probe on
  `/readyz` (503). `/healthz` stays 200 when only the database is down.
- **Threshold:** two consecutive failures over one minute.
- **Meaning:** the API cannot reach Postgres. Requests fail and jobs stop.
- **First response:** Neon status page and console (compute suspended, a
  connection limit hit, credentials rotated), then the API's own Error logs. It
  recovers by itself once the database is back; a restart is only needed if the
  pool never recovers.

### 7. 5xx rate: page

- **Signal:** `http_requests_total{status=~"5.."}`.
- **Threshold:** `sum(rate(http_requests_total{status=~"5.."}[5m])) /
  sum(rate(http_requests_total[5m])) > 0.02` for 5 minutes.
- **First response:** group by `route`. One route means a bug in that handler
  (its Error log names the cause: `handler error`); every route means the
  database or a dependency (see alert 6). Roll back the last deploy if it
  started right after one.

### 8. Upstash fallback active: ticket

- **Signal:** `ratelimit_fallback_total`; Warn log `shared rate limiter
  unavailable; using in-process limits`.
- **Threshold:** `rate(ratelimit_fallback_total[1m]) > 0` for 5 minutes.
- **Meaning:** the shared Redis limiter is failing or slower than
  `REDIS_TIMEOUT`, so per-user limits are per instance. The API is up; abuse
  limits are looser than intended.
- **First response:** Upstash status and the monthly command quota (500K free),
  then the region: Upstash must sit in Railway's region (ADR-0012). Raise
  `REDIS_TIMEOUT` only if the region cannot move.

### 9. Firebase SMS budget: ticket

- **Signal:** a GCP **billing budget alert**, not an application metric.
  Firebase phone sign-in bills every SMS on the Blaze plan.
- **Threshold:** budgets on the Firebase project at 50%, 90% and 100% of the
  monthly amount, e-mailed to the owner. Create it under Billing > Budgets &
  alerts, scoped to the project and the *Identity Platform* SKU.
- **First response:** look at sign-in volume by day. A spike with no matching
  growth is SMS pumping: confirm the SMS region policy allows Ghana only, tighten
  it, and keep App Check on. See `docs/runbooks/firebase-phone.md`.

### 10. mNotify balance low: ticket

- **Signal:** gauge `mnotify_balance` and the daily job `notify.check_balance`
  (08:00 Africa/Accra, only when `NOTIFY_SMS_ENABLED=true`); Error log `mnotify
  balance is low; top up to keep SMS flowing`.
- **Threshold:** `mnotify_balance < 200` (the `MNOTIFY_LOW_BALANCE` credits), or
  `increase(jobs_failed_total{kind="notify.check_balance"}[24h]) > 0`, which
  means the balance could not be read at all (bad key or a changed response).
- **First response:** top up at mnotify.com > Buy SMS. If the check itself
  fails, the message names the reason (`invalid api key`); rotate the key in the
  environment. Order SMS keep failing quietly while the balance is empty, so do
  not wait for the next daily run.

## Rule sketch

The thresholds as Prometheus rules, for whoever runs the alert manager:

```yaml
groups:
  - name: farmish
    rules:
      - alert: PaymentAmountMismatch
        expr: sum(increase(payment_amount_mismatch_total[10m])) > 0
        labels: {severity: page}
      - alert: PayoutFailures
        expr: sum(increase(payout_failures_total[1h])) > 0
        labels: {severity: page}
      - alert: LedgerReconcileViolations
        expr: max(ledger_reconcile_violations) > 0
        labels: {severity: page}
      - alert: RefundFailures
        expr: sum(increase(refunds_total{status="failed"}[15m])) > 0
        labels: {severity: ticket}
      - alert: JobQueueBacklog
        expr: sum(jobs_queue_depth{state="available"}) > 1000
        for: 10m
        labels: {severity: ticket}
      - alert: ServerErrors
        expr: sum(rate(http_requests_total{status=~"5.."}[5m])) / sum(rate(http_requests_total[5m])) > 0.02
        for: 5m
        labels: {severity: page}
      - alert: RateLimitFallback
        expr: sum(rate(ratelimit_fallback_total[1m])) > 0
        for: 5m
        labels: {severity: ticket}
      - alert: MNotifyBalanceLow
        expr: min(mnotify_balance) < 200
        labels: {severity: ticket}
```

`/readyz` and the GCP budget are configured in the platform, not here.
