#!/usr/bin/env bash
# Sign a Paystack webhook body the way Paystack does, for manual QA.
#
#   scripts/paystack-sign.sh '<json body>'
#   eval "$(scripts/paystack-sign.sh '<json body>')"   # sets BODY and SIGNATURE
#
# The signature is HMAC-SHA512 of the exact body under PAYSTACK_SECRET_KEY, hex
# encoded, and belongs in the x-paystack-signature header. The body must not be
# touched after signing: the endpoint verifies over the bytes it receives.
#
# PAYSTACK_SECRET_KEY comes from .env (sourced) or the environment. The body is
# read as one argument so no trailing newline creeps in.
set -euo pipefail

body="${1:?usage: paystack-sign.sh '<json body>'}"
if [[ -z "${PAYSTACK_SECRET_KEY:-}" ]]; then
  echo "PAYSTACK_SECRET_KEY is not set; source .env first" >&2
  exit 1
fi

signature=$(printf '%s' "$body" \
  | openssl dgst -sha512 -hmac "$PAYSTACK_SECRET_KEY" -hex \
  | awk '{print $NF}')

# Printed as shell assignments so the caller can eval it without re-quoting:
#   eval "$(scripts/paystack-sign.sh "$BODY")"
printf 'BODY=%q\nSIGNATURE=%q\n' "$body" "$signature"
