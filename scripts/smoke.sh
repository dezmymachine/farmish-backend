#!/usr/bin/env bash
# End-to-end check of the built image against compose Postgres:
#   1. /migrate up on a fresh farmish_smoke database
#   2. /healthz and /readyz return 200 {"status":"ok"}
#   3. cut the DB network: /readyz returns 503, /healthz stays 200
#   4. SIGTERM exits 0 with the graceful-shutdown log
# Usage: scripts/smoke.sh <image>   (needs `make db-up`)
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

# expect <path> <status> [body]
expect() {
  local out code body
  out="$(curl -sS -m 5 -w '\n%{http_code}' "${base}$1" || true)"
  code="${out##*$'\n'}"; body="${out%$'\n'*}"
  [[ "$code" == "$2" ]] || fail "$1: status $code, want $2 (body: $body)"
  [[ -z "${3:-}" || "$body" == "$3" ]] || fail "$1: body $body, want $3"
}

psql "DROP DATABASE IF EXISTS ${db} WITH (FORCE)" 2>/dev/null
psql "CREATE DATABASE ${db}"
docker run --rm --network "$net" -e DATABASE_URL="$db_url" --entrypoint /migrate "$image" up >/dev/null \
  || fail "/migrate up failed"

# The published port lives on the default bridge; the DB network is attached
# separately so it can be cut later without losing the port.
docker create --name "$name" -p "127.0.0.1:${port}:8080" \
  -e APP_ENV=test -e CORS_ORIGINS=http://localhost:3000 -e DATABASE_URL="$db_url" "$image" >/dev/null
docker network connect "$net" "$name"
docker start "$name" >/dev/null

for _ in $(seq 1 40); do
  curl -fsS -m 1 "${base}/healthz" >/dev/null 2>&1 && break
  sleep 0.25
done
expect /healthz 200 '{"status":"ok"}'
expect /readyz 200 '{"status":"ok"}'

docker network disconnect "$net" "$name"
expect /readyz 503
expect /healthz 200 '{"status":"ok"}'

docker stop -t 10 "$name" >/dev/null
code="$(docker inspect -f '{{.State.ExitCode}}' "$name")"
[[ "$code" == "0" ]] || fail "container exited $code after SIGTERM"
docker logs "$name" 2>&1 | grep -q '"msg":"http server stopped"' || fail "no graceful shutdown log"

echo "smoke: migrate ok, /healthz ok, /readyz 200 -> 503 on DB loss, graceful shutdown ok"
