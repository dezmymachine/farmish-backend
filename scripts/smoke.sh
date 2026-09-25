#!/usr/bin/env bash
# Boot the API image, assert /healthz returns 200 {"status":"ok"} and that
# SIGTERM stops the container cleanly. Usage: scripts/smoke.sh <image>
set -euo pipefail

image="${1:?usage: smoke.sh <image>}"
name="farmish-smoke-$$"
port="${SMOKE_PORT:-18099}"

cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT

docker run -d --name "$name" -p "127.0.0.1:${port}:8080" \
  -e APP_ENV=test -e CORS_ORIGINS=http://localhost:3000 "$image" >/dev/null

body=""
for _ in $(seq 1 40); do
  if body="$(curl -fsS "http://127.0.0.1:${port}/healthz" 2>/dev/null)"; then break; fi
  sleep 0.25
done
if [[ "$body" != '{"status":"ok"}' ]]; then
  echo "smoke: /healthz failed (body: ${body:-<none>})" >&2
  docker logs "$name" >&2
  exit 1
fi

docker stop -t 10 "$name" >/dev/null
code="$(docker inspect -f '{{.State.ExitCode}}' "$name")"
if [[ "$code" != "0" ]]; then
  echo "smoke: container exited $code after SIGTERM" >&2
  docker logs "$name" >&2
  exit 1
fi
if ! docker logs "$name" 2>&1 | grep -q '"msg":"http server stopped"'; then
  echo "smoke: no graceful shutdown log" >&2
  docker logs "$name" >&2
  exit 1
fi
echo "smoke: /healthz ok, graceful shutdown ok"
