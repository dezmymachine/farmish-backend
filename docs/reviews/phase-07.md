# Review packet: Phase 07 Phone sign-in hardening & step-up re-auth

## Summary

Phase 7 hardens Firebase phone sign-in operationally and adds step-up
re-authentication for sensitive operations. A new `x-farmish-step-up: true`
operation extension routes through `VerifyStrict`
(`VerifyIDTokenAndCheckRevoked`) plus a 5-minute `auth_time` freshness check
(`StepUpMaxAge`); stale sign-ins get 401 `reauth_required` with
`WWW-Authenticate: Bearer error="insufficient_user_authentication"`, while
missing/invalid tokens keep the generic 401. Shared Ghana helpers
(`NormalizeGhanaPhone`, `MaskPhone`, 16 `Regions`) land in a new
`internal/geo` package, and the Firebase console hardening checklist ships
as `docs/runbooks/firebase-phone.md`. One deviation (ADR-0014): the Auth
emulator checks revocation on both verify paths, so the revoked-token test
asserts 401 on the step-up op only.

## Commits

`git log --oneline 9b779c4..HEAD` output:

```
1bf89a4 Phase 7: Ghana phone helpers and regions (internal/geo)
68443ba Phase 7: Firebase VerifyStrict + RevokeSessions
206f2a0 Phase 7: step-up re-auth middleware + contract docs
152bb9d Phase 7: phone runbook, ADR-0014, plan checkbox
(plus this packet commit; verify with git log --oneline 9b779c4..HEAD)
```

## Done-when checklist

From REDEVELOPMENT_PLAN.md §6 (Phase 7) and the spec test table:

- [x] An emulator phone sign-in reaches `/v1/me` as `phone`: `TestGetMe_EmailAndPhoneSignIns` (`internal/http/me_test.go`, kept passing)
- [x] A fresh token passes an `x-farmish-step-up` operation: `TestStepUp_FreshTokenPasses` (`internal/http/middleware/auth_test.go`)
- [x] A stale token gets 401 `reauth_required` + `WWW-Authenticate` error param: `TestStepUp_StaleTokenRejected` (`internal/http/middleware/auth_test.go`)
- [x] A revoked token gets 401 on the step-up op: `TestStepUp_RevokedTokenRejected` (`internal/http/middleware/auth_test.go`; generic `unauthorized` since a revoked token is an invalid token per the spec's Failure rule; normal-op half covered by the fake-routing test per ADR-0014)
- [x] A normal op never calls `VerifyStrict`: `TestStepUp_NonStepUpUsesFastVerify` (`internal/http/middleware/auth_test.go`)
- [x] Bad step-up extensions rejected at startup: `TestStepUp_BadExtensionRejectedAtStartup` (`internal/http/middleware/auth_test.go`)
- [x] Phone normalise/mask table tests: `TestNormalizeGhanaPhone`, `TestMaskPhone` (`internal/geo/phone_test.go`)
- [x] Region test (16 true, others false, case-sensitive): `TestIsRegion` (`internal/geo/regions_test.go`)
- [x] Firebase phone runbook done: `docs/runbooks/firebase-phone.md`

## make ci

Last 25 lines of `make ci` output, ending in `ci: all checks passed`:

```
#12 [build 4/6] RUN --mount=type=cache,target=/go/pkg/mod go mod download
#12 CACHED

#13 [build 5/6] COPY . .
#13 DONE 0.1s

#14 [build 6/6] RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build     CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/api ./cmd/migrate
#14 DONE 7.1s

#15 [stage-1 2/2] COPY --from=build /out/api /out/migrate /
#15 CACHED

#16 exporting to image
#16 exporting layers done
#16 exporting manifest sha256:dfc240828d2602ed82057cf2ad134231f58c29e17e2d3da4ffebd45f0b5b9911 done
#16 exporting config sha256:2f50828ac22813c3f05359f447f53725904f68f63851ec5363be7fccf7376853 done
#16 exporting attestation manifest sha256:1621e055e38e3cf1bcf0515d765952f4ca385009849e5505919ffaecc93888f7 0.0s done
#16 exporting manifest list sha256:5d58f11eda9624cfe34a8e46a7af2eebf8075e2a45e0a9cf4081537681b1ee1f 0.0s done
#16 naming to docker.io/library/farmish-backend:dev done
#16 unpacking to docker.io/library/farmish-backend:dev 0.0s done
#16 DONE 0.2s
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

## Manual QA

Spec QA script, with actual commands and output (tokens masked):

1. `make db-up auth-up redis-up && make test`: all green.

```
ok  github.com/dezmymachine/farmish-backend/cmd/api  1.231s
ok  github.com/dezmymachine/farmish-backend/internal/auth  2.124s
ok  github.com/dezmymachine/farmish-backend/internal/config  1.037s
ok  github.com/dezmymachine/farmish-backend/internal/database  1.858s
ok  github.com/dezmymachine/farmish-backend/internal/db  2.961s
ok  github.com/dezmymachine/farmish-backend/internal/geo  1.026s
ok  github.com/dezmymachine/farmish-backend/internal/http  10.731s
ok  github.com/dezmymachine/farmish-backend/internal/http/middleware  5.677s
ok  github.com/dezmymachine/farmish-backend/internal/jobs  12.567s
ok  github.com/dezmymachine/farmish-backend/internal/ratelimit  2.911s
ok  github.com/dezmymachine/farmish-backend/internal/turnstile  1.447s
ok  github.com/dezmymachine/farmish-backend/internal/users  6.296s
ok  github.com/dezmymachine/farmish-backend/migrations  1.882s
ok  github.com/dezmymachine/farmish-backend/pkg/logger  1.022s
```

2. Clock-injected stale-token check (the spec's sanctioned simpler path;
   no real 6-minute wait):

```
=== RUN   TestNormalizeGhanaPhone
--- PASS: TestNormalizeGhanaPhone (0.00s)
=== RUN   TestMaskPhone
--- PASS: TestMaskPhone (0.00s)
=== RUN   TestIsRegion
--- PASS: TestIsRegion (0.00s)
=== RUN   TestStepUp_FreshTokenPasses
--- PASS: TestStepUp_FreshTokenPasses (0.57s)
=== RUN   TestStepUp_StaleTokenRejected
--- PASS: TestStepUp_StaleTokenRejected (0.56s)
=== RUN   TestStepUp_RevokedTokenRejected
--- PASS: TestStepUp_RevokedTokenRejected (1.76s)
=== RUN   TestStepUp_NonStepUpUsesFastVerify
--- PASS: TestStepUp_NonStepUpUsesFastVerify (0.01s)
=== RUN   TestStepUp_BadExtensionRejectedAtStartup
--- PASS: TestStepUp_BadExtensionRejectedAtStartup (0.02s)
=== RUN   TestGetMe_EmailAndPhoneSignIns
--- PASS: TestGetMe_EmailAndPhoneSignIns (0.89s)
```

3. Runbook walk in the real Firebase console:
   - [ ] 1. Phone provider enabled (needs owner console access)
   - [ ] 2. SMS region policy: allow Ghana (+233) only (needs owner)
   - [ ] 3. Authorized domains set (needs owner)
   - [ ] 4. App Check with reCAPTCHA Enterprise, enforced after F2 (needs owner)
   - [ ] 5. Billing budget alert 50/90/100% (needs owner)
   - [ ] 6. Blaze plan confirmed (needs owner)
   - [ ] 7. Optional test phone numbers (needs owner)
   
   The runbook is written step-by-step at
   `docs/runbooks/firebase-phone.md`, but this environment has no access
   to the real Firebase console (emulator only), so the console walk is
   deferred to the owner before staging go-live.

## Files changed

`git diff --stat 9b779c4..HEAD` plus new-file purposes:

```
 AGENTS.md                                   |   4 +-
 REDEVELOPMENT_PLAN.md                       |   4 +-
 api/openapi.yaml                            |  13 ++-
 docs/adr/0014-step-up-revocation-test.md    |  42 ++++++++
 docs/runbooks/firebase-phone.md             |  54 ++++++++++
 internal/auth/auth.go                       |   5 +
 internal/auth/authtest/authtest.go          |   9 ++
 internal/auth/firebase.go                   |  29 +++++-
 internal/geo/phone.go                       |  49 +++++++++
 internal/geo/phone_test.go                  |  52 ++++++++++
 internal/geo/regions.go                     |  36 +++++++
 internal/geo/regions_test.go                |  19 ++++
 internal/http/api/api.gen.go                | 106 ++++++++++---------
 internal/http/apierror/apierror.go          |   1 +
 internal/http/middleware/auth.go            |  95 ++++++++++++++++-
 internal/http/middleware/auth_test.go       | 155 +++++++++++++++++++++++++++-
 internal/http/middleware/testdata/auth.yaml |   4 +
 internal/http/router_test.go                |   4 +
 docs/reviews/phase-07.md                    | 204 ++++++++++++++++++++++++++++
```

New files: `internal/geo/{phone,regions}.go` (Ghana phone/region
helpers) + tests; `docs/runbooks/firebase-phone.md` (console hardening
checklist); `docs/adr/0014-step-up-revocation-test.md` (emulator
deviation); this packet.

## Schema changes

None: no migrations. `migrations_test` (up → down → up) passes under
`make test` (`internal/database`, `migrations` packages ok).

## API changes

No new operations (the first real step-up endpoint is Phase 18a).
`api/openapi.yaml` documents `x-farmish-step-up: true` and the
`reauth_required` code in `info.description` and the
`ErrorBody.code` description; `make api-lint` and `generate-check`
pass, and `internal/http/api/api.gen.go` was regenerated.

## Deviations from the spec

ADR-0014: `TestStepUp_RevokedTokenRejected` asserts 401 on the step-up
operation after a real `RevokeRefreshTokens`, but not 200 on the normal
operation. The Admin SDK checks revocation on both verify paths when
`FIREBASE_AUTH_EMULATOR_HOST` is set, so the normal op also rejects the
revoked token in tests (in production only `VerifyStrict` checks). The
production routing (normal ops never call `VerifyStrict`) is proved by
`TestStepUp_NonStepUpUsesFastVerify`. The test also sleeps 1.2s before
revoking because the emulator tracks revocation at one-second
granularity — an emulator workaround, not a business timer.

## Open questions / risks

- The real-console runbook walk (Blaze plan, SMS region policy, App
  Check, budget alert) needs owner Firebase access; nothing here was
  verified against the live console.
- Revoked tokens get the generic 401 (invalid token), not
  `reauth_required`, on step-up operations — per the spec's Failure
  rule. The client re-authenticates on either code.
- `stepUpNow` is a package-level clock override for tests; production
  always uses `time.Now`. No concurrency concern (set once per test,
  restored via defer).

## Backlog additions

None.
