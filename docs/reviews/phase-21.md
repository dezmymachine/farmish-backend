# Review packet: Phase 21 Observability & hardening

## Summary

The API is now operable. Prometheus metrics (HTTP by route template, DB pool,
River queue and job events, payment/payout/refund counters, ledger
violations, rate-limit denials and fallbacks) are served on a private
listener (`METRICS_ADDR`), never the public port. Logs pass through a
redacting handler, and a test reads the *raw* logs of a phone sign-in,
checkout, dispute, refund and payout run to prove the call sites are clean.
The audit trail gained the missing actions under a table-driven coverage
test. Responses carry security headers. `make loadtest` runs three k6 scripts
against a local API and a Paystack stub with the spec's thresholds, and
`docs/runbooks/alerts.md` lists the ten alerts. See ADR-0034.

## Commits

`git log --oneline 1a4e8da..HEAD` output (plus the docs commit below):

```text
21e760d Phase 21: k6 load test and metrics smoke checks
3a72bce Phase 21: metrics, security headers, audit coverage, mNotify balance
1f86f68 Phase 21: redact secrets and personal data from logs
```

The three code commits were built from one verified tree (`make ci` green on
it), then split by area; they were not each run through `make ci` separately.

## Done-when checklist

- [x] k6 thresholds pass locally: `make loadtest` (browse p95 36 ms < 300, checkout p95 23 ms < 800, webhook replay p95 4 ms < 300; `http_req_failed` 0.00% in all three) (`loadtest/*.js`)
- [x] No secrets, OTPs or phones in a log sample: `TestLogs_NoSecretsOrPII` (`internal/http/logs_test.go`), plus `TestNew_RedactsSecrets`, `TestHandler_*`, `TestMasks`, `TestString_Patterns`
- [x] Metrics exposed: `TestMetrics_ExposesEverySpecifiedSeries` (`internal/metrics/metrics_test.go`), `TestStart_*`, `TestPoolCollector_ExposesPoolStats`, `TestQueueCollector_*`, `TestWatchJobs_CountsCompletedAndFailedAttempts`, `TestHTTPMetrics_*`, `TestMetricsRoute_NotOnThePublicRouter`; and `make loadtest` and `make smoke` scrape the live listener
- [x] Audit coverage: `TestAudit_Coverage` (`internal/http/audit_coverage_test.go`, 16 actions across four fixtures, fails if the groups stop matching the required list); `TestRun_RoleChangeIsAudited` (`cmd/admin/main_test.go`) for `user.role_change`
- [x] Security headers, all of them: `TestSecurityHeaders_OnEveryKindOfResponse`, `TestSecurityHeaders_HSTSOnlyWhenDeployed`, `TestSecurityHeaders_CacheControl`
- [x] Production refuses a non-Paystack base URL: `TestConfig_ProductionRefusesStubPaystack`; `METRICS_ADDR`: `TestConfig_MetricsAddr`; `MNOTIFY_LOW_BALANCE`: `TestConfig_MNotifyLowBalance`
- [x] `notify.check_balance`: `TestMNotify_Balance`, `TestCheckBalanceWorker`, `TestRegistry_BalanceCheckOnlyWithTheRealGateway`
- [x] Alerts list: `docs/runbooks/alerts.md` (signal, threshold, severity and first response for each of the ten)

Mutation checks run while writing (each reverted after): renaming the
`payout.created` audit action fails `TestAudit_Coverage`; logging an unmasked
phone fails `TestLogs_NoSecretsOrPII`.

## make ci

Last 25 lines, ending in `ci: all checks passed`:

```text
#13 DONE 0.5s

#14 [build 6/6] RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build     CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/api ./cmd/migrate
#14 DONE 8.6s

#8 [stage-1 1/2] FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
#8 CACHED

#15 [stage-1 2/2] COPY --from=build /out/api /out/migrate /
#15 DONE 0.1s

#16 exporting to image
#16 exporting layers
#16 exporting layers 4.7s done
#16 exporting manifest sha256:55b2f1e6f779cdd455159f35dcd4e833b8722f7421fa3831497093689790334f 0.0s done
#16 exporting config sha256:f53a8b55a9134fe46704c9187d27d4ef379412e7bcefed227527f61f6de14af6 0.0s done
#16 exporting attestation manifest sha256:cf74cbdcb2698f5f495d9991d10d660b9f262439f419fbcba588ab615463333b 0.0s done
#16 exporting manifest list sha256:ee369f62ff85e21c63a7cc8023105048ee3a4c931011880b837f64681e69f893 0.0s done
#16 naming to docker.io/library/farmish-backend:dev done
#16 unpacking to docker.io/library/farmish-backend:dev
#16 unpacking to docker.io/library/farmish-backend:dev 0.5s done
#16 DONE 5.3s
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, private /metrics ok, security headers ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

## Manual QA

The spec has no QA script, so this is what was run by hand.

`make loadtest` (real output, trimmed to the threshold lines):

```text
== k6: browse.js
    ✓ 'p(95)<300' p(95)=36.36ms
    ✓ 'rate<0.01' rate=0.00%
    http_req_failed................: 0.00%  0 out of 1200
== k6: checkout.js
    ✓ 'p(95)<800' p(95)=23.23ms
    ✓ 'rate<0.01' rate=0.00%
    http_req_failed................: 0.00%  0 out of 270
== k6: webhook.js
    ✓ 'p(95)<300' p(95)=3.95ms
    ✓ 'rate<0.01' rate=0.00%
    http_req_failed................: 0.00%  0 out of 663
loadtest: metrics recorded the run
loadtest: all thresholds passed
```

The API run locally, one signed-in `GET /v1/me`, one missing category and one
scanner path, then a scrape of the private listener:

```text
--- public port: security headers on an authenticated request
HTTP/1.1 200 OK
Cache-Control: no-store
Referrer-Policy: no-referrer
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
--- public port has no /metrics:
404
--- private listener (selected series):
db_pool_total_conns 2
http_requests_total{method="GET",route="/v1/categories/:slug",status="404"} 1
http_requests_total{method="GET",route="/v1/me",status="200"} 1
http_requests_total{method="GET",route="unmatched",status="404"} 2
jobs_queue_depth{state="available"} 0
ledger_reconcile_violations 0
payment_amount_mismatch_total 0
payout_failures_total 0
ratelimit_fallback_total 0
leaks in whole log: jwt=0 email=0
exit after SIGTERM: 0
```

(No `Strict-Transport-Security` above: `APP_ENV=test`. The unit test covers
staging and production.)

## Files changed

`git diff --stat 1a4e8da..HEAD`: 57 files, +3169/-45 across the three code
commits. New:

- `pkg/redact/`: string masks and the redacting `slog.Handler`
- `internal/metrics/`: collectors, the private listener; `collect/`: pool, queue depth and River event collectors
- `internal/http/middleware/metrics.go`, `securityheaders.go`
- `internal/notify/balance.go`: mNotify balance client and the daily check job
- `loadtest/`: `browse.js`, `checkout.js`, `webhook.js`, `lib.js`, `paystack-stub/`, `seed/`; `scripts/loadtest.sh`
- `internal/http/{audit_coverage,logs,scenarios,logcapture,hardening}_test.go`, `cmd/admin/main_test.go`
- `docs/adr/0034-observability-and-hardening.md`, `docs/runbooks/alerts.md`

## Schema changes

None. One new sqlc query (`CountJobsByState`, reads `river_job`).

## API changes

None to `api/openapi.yaml`: `/metrics` is on a separate listener and is not
part of the contract. `make api-lint` and `generate-check` pass inside
`make ci`. Behaviour change on existing operations: every response now carries
the security headers, and credentialed requests default to
`Cache-Control: no-store`.

## Deviations from the spec

ADR-0034, "Deviations":

- `db_pool_acquire_duration_seconds` is a cumulative counter (pgx exposes only
  the running total), with `db_pool_acquires_total` and
  `db_pool_empty_acquires_total` added.
- Extra series: `mnotify_balance`; `jobs_failed_total` counts failed attempts.
- `/readyz` and the Firebase SMS budget alerts are platform configuration,
  documented in the runbook, not code.
- New env vars beyond the spec: `MNOTIFY_LOW_BALANCE` (the spec asked for the
  balance check but named no threshold).

## Open questions / risks

- **mNotify balance response shape is unverified.** The route was probed live
  (401 JSON for a bad key, HTML for a wrong path), but there is no live key
  here, so the success body is decoded tolerantly and pinned only against
  httptest. Confirm with the real key before relying on the alert.
- **Money counters increment inside the transaction**, so a rollback after the
  increment can count twice. The runbook says to start from the audit row.
- **Found by the load test:** search 500s when a live listing has no image row
  (`cover_key` is scanned into a plain string). Production data cannot get
  there, since publishing requires an image; backlog item added.
- **Found in review of my own change:** `MNotify.Send` put the API key into
  network errors (it is in the URL query, and `net/http` prints the URL). Fixed
  and pinned by `TestMNotify_TransportErrorsDoNotLeakTheKey`.
- `make loadtest` is not in `make ci` (about two minutes plus an image pull).
  Run it before a deploy.
- `dispute.opened` is written in a second transaction after the order moves to
  `disputed`, because that is how `Service.Dispute` was already structured; a
  crash between the two would leave a disputed order with no dispute row and no
  audit row. Not introduced here, not fixed here.

## Backlog additions

Five items in REDEVELOPMENT_PLAN.md §10: the search read surviving an image-less
listing; counting money metrics after commit; verifying the mNotify balance body
with a live key; running `make loadtest` in the restored Actions workflow; and
opening a dispute (order transition, dispute row and audit) in one transaction.
