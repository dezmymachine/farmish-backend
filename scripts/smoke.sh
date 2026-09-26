#!/usr/bin/env bash
# End-to-end check of the built image against compose Postgres:
#   1. /migrate up on a fresh farmish_smoke database
#   2. /healthz and /readyz return 200 {"status":"ok"}
#   2b. /v1/me: 401 without a token, 200 with an Auth-emulator token
#   2c. shared rate limits run on Redis
#   3. cut the DB network: /readyz returns 503, /healthz stays 200
#   4. SIGTERM exits 0 with the graceful-shutdown log
# Usage: scripts/smoke.sh <image>   (needs `make db-up auth-up redis-up`)
set -euo pipefail

image="${1:?usage: smoke.sh <image>}"
name="farmish-smoke-$$"
port="${SMOKE_PORT:-18099}"
net="farmish-backend"
db="farmish_smoke"
db_url="postgres://farmish:farmish@postgres:5432/${db}?sslmode=disable"
base="http://127.0.0.1:${port}"

psql() { docker compose exec -T postgres psql -U farmish -d farmish -v ON_ERROR_STOP=1 -qAtc "$1"; }
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  psql "DROP DATABASE IF EXISTS ${db} WITH (FORCE)" >/dev/null 2>&1 || true
}
trap cleanup EXIT
fail() { echo "smoke: $*" >&2; docker logs "$name" >&2 2>/dev/null || true; exit 1; }

# expect <path> <status> [body] [curl args...]
expect() {
  local path="$1" want="$2" want_body="${3:-}" out code body
  shift $(( $# < 3 ? $# : 3 ))
  out="$(curl -sS -m 5 -w '\n%{http_code}' "$@" "${base}${path}" || true)"
  code="${out##*$'\n'}"; body="${out%$'\n'*}"; body="${body%$'\n'}" # ignore trailing newline
  [[ "$code" == "$want" ]] || fail "$path: status $code, want $want (body: $body)"
  [[ -z "$want_body" || "$body" == "$want_body" ]] || fail "$path: body $body, want $want_body"
}

psql "DROP DATABASE IF EXISTS ${db} WITH (FORCE)" 2>/dev/null
psql "CREATE DATABASE ${db}"
docker run --rm --network "$net" -e DATABASE_URL="$db_url" --entrypoint /migrate "$image" up >/dev/null \
  || fail "/migrate up failed"

# The published port lives on the default bridge; the DB network is attached
# separately so it can be cut later without losing the port.
docker create --name "$name" -p "127.0.0.1:${port}:8080" \
  -e APP_ENV=test -e CORS_ORIGINS=http://localhost:3000 -e DATABASE_URL="$db_url" \
  -e FIREBASE_PROJECT_ID=demo-farmish -e FIREBASE_AUTH_EMULATOR_HOST=firebase-auth:9099 \
  -e TURNSTILE_SECRET=1x0000000000000000000000000000000AA -e REDIS_URL=redis://redis:6379 "$image" >/dev/null
docker network connect "$net" "$name"
docker start "$name" >/dev/null

for _ in $(seq 1 40); do
  curl -fsS -m 1 "${base}/healthz" >/dev/null 2>&1 && break
  sleep 0.25
done
expect /healthz 200 '{"status":"ok"}'
expect /readyz 200 '{"status":"ok"}'

expect /v1/me 401 '{"error":{"code":"unauthorized","message":"Authentication required"}}'
emu="http://127.0.0.1:${FARMISH_AUTH_EMULATOR_PORT:-9099}"
token="$(curl -fsS -X POST "${emu}/identitytoolkit.googleapis.com/v1/accounts:signUp?key=emulator" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"smoke-$$-${RANDOM}@farmish.test\",\"password\":\"smoke-pass\",\"returnSecureToken\":true}" \
  | sed -E 's/.*"idToken":"([^"]+)".*/\1/')" || fail "could not get an emulator token"
expect /v1/me 200 "" -H "Authorization: Bearer ${token}"
docker logs "$name" 2>&1 | grep -q '"shared":"redis"' || fail "shared rate limiter is not on Redis"
docker logs "$name" 2>&1 | grep -q '"msg":"redis connected"' || fail "API did not connect to Redis"

docker network disconnect "$net" "$name"
expect /readyz 503
expect /healthz 200 '{"status":"ok"}'

docker stop -t 10 "$name" >/dev/null
code="$(docker inspect -f '{{.State.ExitCode}}' "$name")"
[[ "$code" == "0" ]] || fail "container exited $code after SIGTERM"
docker logs "$name" 2>&1 | grep -q '"msg":"http server stopped"' || fail "no graceful shutdown log"

echo "smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok"
