# Review packet: Phase 08 Profiles & seller onboarding

## Summary

Users become sellers through a seller profile (`PUT /v1/me/seller-profile`);
admins verify submitted IDs, which flips `users.seller_verified` plus the
Firebase `seller_verified` claim, with every step audited. ID numbers are
AES-256-GCM encrypted at rest (`v1:` ciphertexts) and only ever surface as
last4. The phase also lands the shared building blocks every later phase
uses: `database.InTx`, `internal/validation`, `internal/crypto` and
`internal/audit`. No spec deviations; one real bug found by the race
detector and fixed (see Risks).

## Commits

`git log --oneline 11ae0a6..HEAD` output (plus this packet commit):

```
416b4ba Phase 8: seller profiles + audit migration and queries
1f65226 Phase 8: cross-cutting building blocks
38c85bd Phase 8: encryption key config, seller flag, generic claim setter
83b6ee2 Phase 8: sellers service + tests
bf25273 Phase 8: sellers endpoints, handlers and wiring
aa0ea08 Phase 8: lint fixes for sellers plumbing
d4fe12d Phase 8: pass DATA_ENCRYPTION_KEY to the smoke container
(plus this packet commit; verify with git log --oneline 11ae0a6..HEAD)
```

## Done-when checklist

From REDEVELOPMENT_PLAN.md §6 (Phase 8):

- [x] The public seller response contains only safe fields (asserted by a test): `TestPublicSeller_SafeProjection` (`internal/http/sellers_test.go`) unmarshals the JSON and asserts the absence of `idNumber, idNumberLast4, idType, email, phone, whatsapp, firebaseUid, role`, plus `assertContract`
- [x] Verification writes an audit event and updates the DB flag plus the Firebase claim: `TestAdminVerify_ApproveAndClaim` (`internal/sellers/service_test.go`) asserts the `seller.verify` audit row, `users.seller_verified=true` and claim `seller_verified=true` (reject path asserts false/false)
- [x] ID numbers are encrypted at rest: `TestSellerProfile_IdentitySubmissionSetsPending` reads the raw `id_number_enc` column (`v1:` prefix, no plaintext, decrypts to the original)
- [x] `InTx`, `validation`, `crypto` and `audit` exist and are tested: `TestInTx_CommitsAndRollsBack`, `validation_test.go`, `TestCrypto_RoundTrip/TamperFails/WrongKeyFails/UniqueNonces`, `TestAudit_AppendOnly`

Spec test table:

- [x] `TestCrypto_RoundTrip`, `TestCrypto_TamperFails`, `TestCrypto_WrongKeyFails`, `TestCrypto_UniqueNonces` (`internal/crypto/crypto_test.go`)
- [x] `TestInTx_CommitsAndRollsBack` (`internal/database/tx_test.go`; commits on nil, rolls back on error and on panic with re-panic)
- [x] `TestAudit_AppendOnly` (`internal/audit/audit_test.go`; UPDATE/DELETE raise the append-only error)
- [x] `TestSellerProfile_UpsertAndGet` (`internal/sellers/service_test.go`; create, edit, status unchanged by edits)
- [x] `TestSellerProfile_IdentitySubmissionSetsPending` (see above)
- [x] `TestSellerProfile_IdentityChangeRevokesVerification` (verified → new ID → pending + flag/claim false; resubmitting the same ID writes no new audit event)
- [x] `TestSellerProfile_Validation` (`internal/http/sellers_test.go`; region, idType-without-idNumber, bad WhatsApp → 400 `validation_failed` with field details)
- [x] `TestPublicSeller_SafeProjection` (see above)
- [x] `TestAdminVerify_ApproveAndClaim` (see above)
- [x] `TestAdminVerify_RejectRequiresReason` / `_NotPendingIs409` / `_NonAdminIs403` (`TestAdminVerify_Endpoint` in `internal/http/sellers_test.go`: 400/409 `invalid_transition`/403)
- [x] `TestConfig_DataEncryptionKey` (`internal/config/config_test.go`; missing/short errors, dev key refused in staging/production)

Extras beyond the table: `TestUpsertMine_Validation`, `TestVerify_Guards`, `TestListByStatus` (service level), `TestSetClaim_SellerVerified` (claim merge), `TestMySellerProfile_CRUD`, `TestAdminSellers_List`, `TestSellersHandlers_DoNotLeakGinContext` (regression test for the race fix).

## make ci

Last 25 lines of `make ci` output, ending in `ci: all checks passed`:

```
#12 [build 4/6] RUN --mount=type=cache,target=/go/pkg/mod go mod download
#12 CACHED

#13 [build 5/6] COPY . .
#13 DONE 0.2s

#14 [build 6/6] RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build     CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/api ./cmd/migrate
#14 DONE 7.0s

#15 [stage-1 2/2] COPY --from=build /out/api /out/migrate /
#15 CACHED

#16 exporting to image
#16 exporting layers done
#16 exporting manifest sha256:edc7026e6cba45df9caae14126f4e09b4325abfe8530b0641af5536eadb7e129 done
#16 exporting config sha256:99b3c9f01438b9d5860f76a927fd781db9c4b472c76a0690ed3d58a9c6382f1a done
#16 exporting attestation manifest sha256:188d7e25b690317b9d95e6ec3f8d0ae91455867da1d2e27d705e67994548bd86 0.0s done
#16 exporting manifest list sha256:5d58f11eda9624cfe34a8e46a7af2eebf8075e2a45e0a9cf4081537681b1ee1f 0.0s done
#16 naming to docker.io/library/farmish-backend:dev done
#16 unpacking to docker.io/library/farmish-backend:dev 0.0s done
#16 DONE 0.1s
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

## Manual QA

Spec QA script against `make run` (emulator config, dev DB migrated to version 4). Tokens masked.

1. PUT with an ID, then GET shows `idNumberLast4` only:

```
PUT /v1/me/seller-profile
{"bio":"Manual QA farm.","businessName":"QA Acres","district":"Kumasi Metro","idNumberLast4":"89-0","idType":"ghana_card","region":"Ashanti","showPhone":true,"showWhatsapp":false,"submittedAt":"2026-09-26T12:26:04.481872Z","verificationStatus":"pending","whatsapp":"+233241234567"}

GET /v1/me/seller-profile
{"bio":"Manual QA farm.","businessName":"QA Acres","district":"Kumasi Metro","idNumberLast4":"89-0","idType":"ghana_card","region":"Ashanti","showPhone":true,"showWhatsapp":false,"submittedAt":"2026-09-26T12:26:04.481872Z","verificationStatus":"pending","whatsapp":"+233241234567"}
```

2. Second user made admin (direct DB role update for QA; `grant-admin`
   targets the real project). Pending queue lists the first user, approve
   works, public shows `verified: true` and nothing private:

```
GET /v1/admin/sellers?status=pending&page=1&limit=20
{"items":[{"bio":"Manual QA farm.","businessName":"QA Acres","district":"Kumasi Metro","email":"qa-seller8@farmish.test","idNumberLast4":"89-0","idType":"ghana_card","region":"Ashanti","showPhone":true,"showWhatsapp":false,"submittedAt":"2026-09-26T12:26:04.481872Z","userId":"64fd686a-cf9e-4176-947e-f4cfc94f7f6a","verificationStatus":"pending","whatsapp":"+233241234567"}],"meta":{"limit":20,"page":1,"total":1}}

POST /v1/admin/sellers/{id}/verification {"decision":"approve"} → 200 "verificationStatus":"verified"

GET /v1/sellers/{id}
{"bio":"Manual QA farm.","businessName":"QA Acres","district":"Kumasi Metro","memberSince":"2026-09-26T12:26:04.471692Z","region":"Ashanti","userId":"64fd686a-cf9e-4176-947e-f4cfc94f7f6a","verified":true}
```

3. Ciphertext at rest:

```
SELECT left(id_number_enc, 20), id_number_last4 FROM seller_profiles ...
v1:8UZ6UJQWTOrLanHpY|89-0
```

QA rows were left in the local dev database (the append-only trigger
correctly refused `DELETE FROM audit_events`, which doubled as a live
check of `forbid_mutation()`). The QA server was stopped afterwards.

## Files changed

`git diff --stat 11ae0a6..HEAD` (plus `docs/reviews/phase-08.md`, this packet):

```
 .env.example                                     |   12 +-
 AGENTS.md                                        |    4 +-
 REDEVELOPMENT_PLAN.md                            |    3 +-
 api/openapi.yaml                                 |  316 ++++++
 cmd/api/main.go                                  |    7 +
 db/queries/audit.sql                             |    4 +
 db/queries/sellers.sql                           |   53 +
 db/queries/users.sql                             |    3 +
 go.mod                                           |    1 +
 go.sum                                           |    8 +
 internal/audit/audit.go                          |   47 +
 internal/audit/audit_test.go                     |   66 ++
 internal/auth/firebase.go                        |   24 +-
 internal/auth/firebase_test.go                   |   23 +
 internal/config/config.go                        |   23 +
 internal/config/config_test.go                   |   47 +-
 internal/crypto/crypto.go                        |   73 ++
 internal/crypto/crypto_test.go                   |   99 ++
 internal/database/tx.go                          |   28 +
 internal/database/tx_test.go                     |   74 ++
 internal/db/audit.sql.go                         |   47 +
 internal/db/models.go                            |   31 +
 internal/db/querier.go                           |   17 +
 internal/db/sellers.sql.go                       |  352 +++++++
 internal/db/users.sql.go                         |   28 +
 internal/http/api/api.gen.go                     | 1232 ++++++++++++++++++++--
 internal/http/apierror/apierror.go               |   23 +-
 internal/http/handlers/health.go                 |    5 +-
 internal/http/handlers/sellers.go                |  226 ++++
 internal/http/handlers/validation.go             |   22 +
 internal/http/router.go                          |    3 +-
 internal/http/sellers_test.go                    |  395 +++++++
 internal/sellers/sellers.go                      |   97 ++
 internal/sellers/service.go                      |  397 +++++++
 internal/sellers/service_test.go                 |  374 +++++++
 internal/users/users.go                          |    6 +
 internal/validation/validation.go                |   41 +
 internal/validation/validation_test.go           |   26 +
 migrations/000004_seller_profiles_audit.down.sql |    5 +
 migrations/000004_seller_profiles_audit.up.sql   |   45 +
 scripts/smoke.sh                                 |    5 +-
 docs/reviews/phase-08.md                         |  new (this packet)
```

New files: migration `000004`, queries (`sellers/audit`, plus
`SetUserSellerVerified` in `users.sql`), `internal/database/tx.go`,
`internal/validation/`, `internal/crypto/`, `internal/audit/`,
`internal/sellers/`, `handlers/sellers.go` + `validation.go`,
`internal/http/sellers_test.go`.

## Schema changes

New migration `000004_seller_profiles_audit` (seller_profiles,
audit_events, `forbid_mutation()`). Down drops both tables and the
function (Phase 13b must not drop the function). `migrations_test`
up→down→up passes under `make test`.

## API changes

Five operations under tag `sellers` (`getMySellerProfile`,
`updateMySellerProfile` with `x-farmish-rate-limit: sensitive`,
`getPublicSeller` public, `listAdminSellers` + `verifySeller` with
`x-farmish-role: admin`), new schemas (`SellerProfileInput`,
`SellerProfile`, `PublicSeller`, `SellerProfileAdmin[List]`,
`VerifySellerRequest`) and shared `Conflict` response for 409
`invalid_transition`. `make api-lint` and `generate-check` pass.

## Deviations from the spec

None. Notes (not deviations):

- The service takes `limit/offset int32` (instead of int) so the
  sqlc call needs no integer conversion (gosec G115).
- `scripts/smoke.sh` now passes the dev `DATA_ENCRYPTION_KEY` to the
  smoke container; without it the container exits at startup since the
  key is required. The smoke failure caught this (the suite doing its
  job), it was fixed, and `make ci` is green.
- `go mod tidy` pulled `oapi-codegen/nullable` + `go-jsonmerge` via the
  regenerated client runtime.

## Open questions / risks

- **Race fix, please look closely.** The Firebase claim call runs the
  request context through the HTTP keep-alive transport, whose read-loop
  can touch the context after the handler returns. Strict handlers
  receive gin's pooled `*gin.Context`, so gin recycles it under the
  transport → data race (caught by `-race`, flaky ~1/2 runs). Fixed by
  unwrapping to `c.Request.Context()` in the two sellers methods that
  call Firebase (`requestContext` in handlers, same values, mirrors
  what the middleware already does with `c.Request.Context()`), plus
  `TestSellersHandlers_DoNotLeakGinContext` which fails deterministically
  on the old code. Future phases making outgoing HTTP calls from strict
  handlers must use `requestContext` too.
- `make grant-admin` targets the real Firebase project, so manual QA
  used a direct DB role update for the admin user; endpoint auth itself
  is covered by `_NonAdminIs403` against the emulator.
- Reviewer: `ListSellerProfilesByStatus` orders oldest submission
  first (`submitted_at ASC NULLS LAST`) — spec is silent on order.

## Backlog additions

None.
