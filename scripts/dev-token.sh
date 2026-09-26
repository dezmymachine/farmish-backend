#!/usr/bin/env bash
# Print a Firebase ID token for manual API testing (curl -H "Authorization: Bearer ...").
#
#   Real dev project (email/password account must already exist, or pass --signup):
#     FIREBASE_WEB_API_KEY=AIza... scripts/dev-token.sh you@example.com 'password' [--signup]
#   Local Auth emulator (no key needed; creates the account if missing):
#     FIREBASE_AUTH_EMULATOR_HOST=127.0.0.1:9099 scripts/dev-token.sh you@example.com 'password'
#
# The web API key is the public client key from Firebase console > Project
# settings > General > Your apps. It is not stored anywhere by this script.
set -euo pipefail

email="${1:?usage: dev-token.sh <email> <password> [--signup]}"
password="${2:?usage: dev-token.sh <email> <password> [--signup]}"
mode="${3:-}"

if [[ -n "${FIREBASE_AUTH_EMULATOR_HOST:-}" ]]; then
  base="http://${FIREBASE_AUTH_EMULATOR_HOST}/identitytoolkit.googleapis.com/v1"
  key="emulator"
  mode="--signup-if-missing"
else
  base="https://identitytoolkit.googleapis.com/v1"
  key="${FIREBASE_WEB_API_KEY:?set FIREBASE_WEB_API_KEY (or FIREBASE_AUTH_EMULATOR_HOST)}"
fi

body="$(printf '{"email":"%s","password":"%s","returnSecureToken":true}' "$email" "$password")"
post() { curl -sS -X POST "${base}/$1?key=${key}" -H 'Content-Type: application/json' -d "$body"; }
extract() { sed -nE 's/.*"idToken": ?"([^"]+)".*/\1/p' | head -1; }

if [[ "$mode" == "--signup" ]]; then
  out="$(post accounts:signUp)"
else
  out="$(post accounts:signInWithPassword)"
  if [[ "$mode" == "--signup-if-missing" && "$out" == *EMAIL_NOT_FOUND* ]]; then
    out="$(post accounts:signUp)"
  fi
fi

token="$(tr -d '\n' <<<"$out" | extract)"
if [[ -z "$token" ]]; then
  echo "dev-token: no token returned: $out" >&2
  exit 1
fi
echo "$token"
