#!/usr/bin/env bash
# Local k6 smoke load test (Phase 21). Runs the API and a Paystack stub on the
# host, against compose Postgres, Redis and the Auth emulator, then drives them
# with k6 from its pinned Docker image:
#   browse.js    anonymous search + detail        p95 < 300 ms
#   checkout.js  quote + checkout (stub Paystack) p95 < 800 ms
#   webhook.js   signed webhook replays           p95 < 300 ms
# Every script also needs http_req_failed < 1%.
# Usage: scripts/loadtest.sh <k6-image>   (needs `make db-up auth-up redis-up`)
set -euo pipefail

k6_image="${1:?usage: loadtest.sh <k6-image>}"
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

db="farmish_load"
pg_port="${FARMISH_PG_PORT:-54320}"
api_port="${LOADTEST_API_PORT:-18100}"
stub_port="${LOADTEST_STUB_PORT:-19999}"
metrics_port="${LOADTEST_METRICS_PORT:-19100}"
secret="sk_test_loadtest"
tmp="$root/loadtest/.tmp"
pids=()

psql() { docker compose exec -T postgres psql -U farmish -d farmish -v ON_ERROR_STOP=1 -qAtc "$1"; }
cleanup() {
  for pid in "${pids[@]:-}"; do [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  psql "DROP DATABASE IF EXISTS ${db} WITH (FORCE)" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT
fail() {
  echo "loadtest: $*" >&2
  # Warnings are the API rejecting things on purpose (forged webhooks); only
  # errors explain a failed run.
  # The failing requests' access lines repeat the same status; the line that
  # names the cause is the handler's own error log.
  [[ -f "$tmp/api.log" ]] && grep '"level":"ERROR"' "$tmp/api.log" | grep -v '"msg":"http_request"' | head -n 10 >&2
  [[ -n "${LOADTEST_KEEP_LOG:-}" && -f "$tmp/api.log" ]] && cp "$tmp/api.log" "$LOADTEST_KEEP_LOG"
  exit 1
}

mkdir -p "$tmp" bin
psql "DROP DATABASE IF EXISTS ${db} WITH (FORCE)" >/dev/null 2>&1
psql "CREATE DATABASE ${db}"

# The API runs the way production does except for three things: the Paystack
# base URL (the stub), loopback as the trusted proxy (so each virtual user can
# present its own client address), and the Auth emulator.
export APP_ENV=test PORT="$api_port" LOG_LEVEL=warn
export METRICS_ADDR="127.0.0.1:${metrics_port}"
export DATABASE_URL="postgres://farmish:farmish@127.0.0.1:${pg_port}/${db}?sslmode=disable"
export CORS_ORIGINS=http://localhost:3000
export FIREBASE_PROJECT_ID=demo-farmish
export FIREBASE_AUTH_EMULATOR_HOST="127.0.0.1:${FARMISH_AUTH_EMULATOR_PORT:-9099}"
export TURNSTILE_SECRET=1x0000000000000000000000000000000AA
export REDIS_URL="redis://127.0.0.1:${FARMISH_REDIS_PORT:-63790}"
export PAYSTACK_SECRET_KEY="$secret" PAYSTACK_PUBLIC_KEY=pk_test_loadtest
export PAYSTACK_CALLBACK_URL=https://farmish.gh/payments/status
export PAYSTACK_BASE_URL="http://127.0.0.1:${stub_port}"
export TRUSTED_PROXIES=127.0.0.1/32
# The dev-only key from .env.example (config refuses it outside dev/test).
export DATA_ENCRYPTION_KEY=9MkjfhxyTr/ApaqBuztKBSrbWYo6kr/wIcg5/RfbmUs=

go run ./cmd/migrate up >/dev/null || fail "migrate up failed"
go run ./cmd/seed >/dev/null || fail "catalog seed failed"
go build -o bin/api ./cmd/api
go build -o bin/paystack-stub ./loadtest/paystack-stub
go build -o bin/loadseed ./loadtest/seed

bin/paystack-stub -addr "127.0.0.1:${stub_port}" >"$tmp/stub.log" 2>&1 &
pids+=("$!")
bin/api >"$tmp/api.log" 2>&1 &
pids+=("$!")

for _ in $(seq 1 80); do
  curl -fsS -m 1 "http://127.0.0.1:${api_port}/healthz" >/dev/null 2>&1 && break
  sleep 0.25
done
curl -fsS -m 2 "http://127.0.0.1:${api_port}/readyz" >/dev/null || fail "API is not ready"

bin/loadseed -out "$tmp/fixtures.json" || fail "seeding failed"

status=0
for script in browse checkout webhook; do
  echo "== k6: ${script}.js"
  docker run --rm --network host -u "$(id -u):$(id -g)" \
    -v "$root/loadtest:/scripts:ro" -v "$tmp:/data:ro" \
    -e BASE_URL="http://127.0.0.1:${api_port}" -e PAYSTACK_SECRET="$secret" \
    "$k6_image" run --quiet "/scripts/${script}.js" || status=1
done

# The run must also have produced metrics: a load test that never moved the
# counters would mean the private listener is not wired.
metrics="$(curl -fsS -m 3 "http://127.0.0.1:${metrics_port}/metrics")" || fail "metrics listener is not answering"
grep -q '^http_requests_total{method="GET",route="/v1/listings",status="200"}' <<<"$metrics" || fail "http_requests_total did not record the browse traffic"
grep -q '^payments_total{purpose="checkout",status="succeeded"}' <<<"$metrics" || fail "payments_total did not record the settled webhook"
grep -q '^db_pool_total_conns' <<<"$metrics" || fail "db pool metrics are missing"
echo "loadtest: metrics recorded the run"

[[ $status -eq 0 ]] || fail "k6 thresholds failed"
echo "loadtest: all thresholds passed"
