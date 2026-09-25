#!/usr/bin/env bash
# Create migrations/NNNNNN_<name>.{up,down}.sql with the next sequence number.
set -euo pipefail

name="${1:-}"
if [[ ! "$name" =~ ^[a-z0-9_]+$ ]]; then
  echo "usage: make migrate-new name=<snake_case_name>" >&2
  exit 1
fi

cd "$(dirname "$0")/../migrations"
last="$(ls -1 [0-9]*_*.up.sql 2>/dev/null | sed -E 's/^0*([0-9]+)_.*/\1/' | sort -n | tail -1)"
next="$(printf '%06d' $(( ${last:-0} + 1 )))"

for dir in up down; do
  f="${next}_${name}.${dir}.sql"
  printf -- '-- %s: %s\n' "$dir" "$name" > "$f"
  echo "created migrations/$f"
done
