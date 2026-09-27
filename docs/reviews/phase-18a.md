# Review packet: Phase 18a Seller payout accounts

## Summary

Sellers set a mobile-money or GhIPSS payout account behind step-up re-auth (the first production use of `x-farmish-step-up`). The service validates the bank code against Paystack's list, resolves the holder name, name-checks it against the business and display names, encrypts the number (AES-256-GCM), creates the Paystack transfer recipient and upserts the masked account — with a 48h cooldown on change, a masked audit trail and a security SMS in one transaction. `needs_review` accounts wait for the new admin approve endpoint. The banks list is cached in memory for an hour.

## Commits

`git log --oneline a6007bb..HEAD` output (plus the docs commit below):

```text
0795859 Phase 18a: banks 502 on provider outage
40c56bf Phase 18a: payout account tests
a51b73c Phase 18a: seller payout accounts
```

## Done-when checklist

- [x] Setting requires step-up: `TestPayoutStepUp` — stale stub-verifier token → 401 `reauth_required` with the `WWW-Authenticate` re-auth header, fresh token → 200 (`internal/http/payouts_test.go`)
- [x] Resolved, name-checked, encrypted, never unmasked: `TestSetAccount_FirstSetupVerified` (normalized number sent, `v1:` ciphertext stored, mask returned), `TestPayoutAccount_MaskedRoundTrip`, `TestPayoutAccount_NeverUnmasked`
- [x] Change starts 48h cooldown: `TestSetAccount_ChangeStartsCooldown` (first setup none, change `now+48h`, old/new masks in audit, 2 security SMS), plus the endpoint `TestPayoutAccount_Cooldown`
- [x] Name check + admin approve: `TestSetAccount_NeedsReviewAndApprove`, `TestPayoutAccount_Approve` (403/404/409 paths)
- [x] Unresolvable → 422: `TestSetAccount_Unresolvable`, `TestPayoutAccount_Unresolvable422`
- [x] Cached banks: `TestBanks_CachedList` (two calls → one Paystack call), `TestBanks_CachedList` endpoint, `TestBanks_ProviderDown` (502)

## make ci

Last lines of `make ci` output, ending in `ci: all checks passed`:

```text
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

The gate also passed `migrations_test` (up → down → up over the new 000015) and caught one issue mid-phase: unmapped ListBanks provider errors were a 500, now 502 with `TestBanks_ProviderDown` pinning it.

## Manual QA

Live API (`FIREBASE_PROJECT_ID=demo-farmish`, Auth emulator, local Postgres/Redis; tokens masked). The local Paystack stand-in (`127.0.0.1:9911`) is down, so anything past bank-code validation proves the error shapes instead of a full setup; the full setup runs in the endpoint tests against the scripted fake.

```console
$ curl -s ".../v1/payouts/banks?type=mobile_money"                          # no token
{"error":{"code":"unauthorized","message":"Authentication required"}}

$ curl -s ".../v1/payouts/banks?type=mobile_money" -H "Authorization: Bearer <fresh>"
{"error":{"code":"payment_provider_error","message":"The payment provider is unavailable; try again"}}

$ curl -s -X PUT .../v1/seller/payout-account -d '{"type":"mobile_money","bankCode":"MTN","accountNumber":"0241234567"}' -H "Authorization: Bearer <stale>"
{"error":{"code":"reauth_required","message":"Please sign in again to continue"}}

$ curl -s -X PUT .../v1/seller/payout-account -d '{...}' -H "Authorization: Bearer <fresh>"
{"error":{"code":"forbidden","message":"A seller profile is required"}}

$ # after PUT /v1/me/seller-profile → 200
$ curl -s -X PUT .../v1/seller/payout-account -d '{...}' -H "Authorization: Bearer <fresh>"
{"error":{"code":"payment_provider_error","message":"The payment provider is unavailable; try again"}}

$ curl -s .../v1/seller/payout-account -H "Authorization: Bearer <fresh>"
{"error":{"code":"not_found","message":"Payout account not found"}}
```

The stale-token 401 is the first live proof of the step-up middleware on a production operation. A log sample held no account numbers, emails or tokens. Note: the shared dev DB sat at migration 14 with the code at 15, so the new endpoints 500ed until `make migrate-up` ran (version 15, clean) — test databases migrate fresh and never saw this.

## Files changed

`git diff --stat a6007bb..HEAD` plus the docs commit (`AGENTS.md`, `REDEVELOPMENT_PLAN.md`, `docs/adr/0029-*.md`, this file). New files and their purpose:

- `migrations/000015_payout_accounts.{up,down}.sql`: the `seller_payout_accounts` table (spec shape verbatim).
- `db/queries/payouts.sql` → `internal/db/payouts.sql.go`: get/upsert/approve-verified (approve returns no row unless `needs_review`).
- `internal/payouts/payouts.go`: types, MoMo/GhIPSS normalisation, masking, token-overlap name check.
- `internal/payouts/service.go`: banks cache, get/set/approve with the audit + SMS transaction.
- `internal/payouts/service_test.go`: unit tables plus the full setup/review/cooldown/validation flows.
- `internal/http/handlers/payouts.go`: the four endpoints and the masked mapper (recipient code never mapped).
- `internal/http/payouts_test.go`: stub-verifier step-up tests plus contract-validated endpoint tests.
- `internal/payments/fake/paystack.go`: scriptable resolve/banks/recipient results with call records.

## Schema changes

New migration 000015 (one table + `set_updated_at` trigger, working down). `migrations_test` passes inside `make ci`. No new env vars.

## API changes

New `payouts` tag; new shared `Unprocessable` (422) response:

- `GET /v1/payouts/banks` (`listPayoutBanks`, optional `?type=`) → `BankList`; 400/401/429/502
- `GET /v1/seller/payout-account` (`getMyPayoutAccount`) → `PayoutAccount`; 401/403/404/429
- `PUT /v1/seller/payout-account` (`setMyPayoutAccount`, `x-farmish-step-up: true`, `sensitive`) → `PayoutAccount`; 400/401/403/422/429/502
- `POST /v1/admin/payout-accounts/{sellerId}/approve` (`approvePayoutAccount`, admin) → `PayoutAccount`; 401/403/404/409/429

`make api-lint` and `generate-check` pass; `api.gen.go` generated only.

## Deviations from the spec

All in ADR-0029; business rules unchanged:

1. Every resolve error (including transport) is 422; bank-list/recipient outages are 502.
2. Name check is token overlap (≥ 1 shared token of ≥ 3 chars), not equality.
3. MoMo normalises to local `0XXXXXXXXX`; the live format and the `basilisk` recipient type await a test-mode check (Transfers disabled, plan §11).
4. Any PUT over an existing row restarts the 48h cooldown (spec-literal read of "a row already existed").
5. Banks cache is in-process (no Redis); 502 added to the banks responses (spec lists none, the guide requires applicable 5xx declared).

## Open questions / risks

- Live Paystack verification (resolve format, recipient type, Transfers enablement + OTP disabled) still gated on plan §11 before 18b moves real money.
- Step-up staleness at the endpoint level is proven with a stub verifier (AuthTime control); the real-emulator path only ever yields fresh tokens, so a live stale-token 401 was instead observed with a previous session's token during QA.

## Backlog additions

None.
