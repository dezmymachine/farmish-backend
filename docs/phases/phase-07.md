# Phase 7: Phone sign-in hardening & step-up re-auth

**Depends on:** 4 · **ADR:** next free number · **Size:** small

## Goal

Phone sign-in is a Firebase provider (ADR-0007). This phase:
- hardens it operationally (a console checklist)
- adds **step-up re-authentication** for sensitive operations: a fresh sign-in within 5 minutes plus a revocation check, declared in the spec
- adds shared phone helpers (normalise, validate, mask) for later phases

## Scope

- `docs/runbooks/firebase-phone.md`: the console checklist below, written as step-by-step instructions.
- **`x-farmish-step-up: true`** operation extension, enforced by the auth middleware.
- `Verifier.VerifyStrict`: a revocation-checking verify.
- `internal/geo`: the 16 regions list (DOMAIN §10) and phone helpers. Districts come in Phase 9.

**Out of scope:** any real endpoint using step-up (the first is Phase 18a); custom OTP (removed by ADR-0007).

## Firebase console checklist (runbook content)

1. Authentication → Sign-in method → **Phone** enabled.
2. Authentication → Settings → **SMS region policy**: *Allow* only **Ghana (+233)**.
3. Authentication → Settings → **Authorized domains**: `localhost`, the staging domain and the production domain. Remove the defaults you don't use.
4. **App Check**: register the web app with reCAPTCHA Enterprise, and enforce it for Authentication once the frontend ships (F2).
5. Google Cloud console → Billing → **Budget alert** on the Firebase project (e.g. GHS-equivalent of $20/month, alerts at 50/90/100%).
6. Blaze plan confirmed (phone auth SMS is billed).
7. Optional test phone numbers (Authentication → Sign-in method → Phone → *Phone numbers for testing*), for manual QA without real SMS.

## Design

- **Extension:** `x-farmish-step-up: true` on an operation. It requires `bearerAuth`: validate this at startup, like `x-farmish-role`, and fail if it's not a boolean or is set on a public operation.
- **Middleware:** in `middleware.Authenticate`, for step-up operations use `VerifyStrict` (not `Verify`). Then require `now - identity.AuthTime <= StepUpMaxAge` (5 minutes; a const in `middleware`, with an injectable clock for tests).
- **Failure:** 401 with a **distinct** code, `reauth_required` (message "Please sign in again to continue"), plus `WWW-Authenticate: Bearer error="insufficient_user_authentication"`. This is the one exception to "generic 401", because the client must know to re-authenticate rather than refresh. A missing or invalid token on a step-up operation still gets the generic `unauthorized`.
- **`auth.Verifier`** gains `VerifyStrict(ctx, token) (Identity, error)`, which calls `client.VerifyIDTokenAndCheckRevoked`. A revoked or disabled account → `ErrInvalidToken` wrapped. Update every fake verifier in tests.
- **`internal/geo/phone.go`:**
  - `NormalizeGhanaPhone(s string) (string, error)`: strip spaces, dashes and parentheses. `0XXXXXXXXX` → `+233XXXXXXXXX`, `233XXXXXXXXX` → `+233…`. Validate `^\+233[0-9]{9}$`. Return `ErrInvalidPhone` otherwise.
  - `MaskPhone(e164) string`: `+233*****4567`, keeping the first 4 and last 4 characters.
- **`internal/geo/regions.go`:** `var Regions = []string{...16...}`, `IsRegion(s) bool`.

## Files

`internal/auth/auth.go` (interface), `internal/auth/firebase.go` (VerifyStrict), `internal/http/middleware/auth.go` (+ tests, fixture `testdata/auth.yaml` gains a step-up operation), `internal/http/apierror/apierror.go` (`CodeReauthRequired = "reauth_required"`), `api/openapi.yaml` (document the extension + error code in `info.description` and the `ErrorBody.code` description), `internal/geo/{phone,regions}.go` + tests, `docs/runbooks/firebase-phone.md`.

## Tests (every one must exist and pass)

| Test | Proves |
|---|---|
| `TestStepUp_FreshTokenPasses` | An emulator user signed in just now gets 200 on the step-up fixture op |
| `TestStepUp_StaleTokenRejected` | Clock advanced 6 min → 401 `reauth_required` + the `WWW-Authenticate` error param |
| `TestStepUp_RevokedTokenRejected` | Admin SDK `RevokeRefreshTokens(uid)`, then the same token on a step-up op → 401 (it still works on a normal op until expiry: assert both) |
| `TestStepUp_NonStepUpUsesFastVerify` | A normal op never calls `VerifyStrict` (fake verifier counts calls) |
| `TestStepUp_BadExtensionRejectedAtStartup` | Non-bool value, or on a `security: []` op → constructor error |
| `TestPhoneSignIn_MeReportsPhone` | Already covered in `me_test.go`: keep it passing |
| `TestNormalizeGhanaPhone` | Table: `0241234567`, `233241234567`, `+233 24 123 4567`, `024-123-4567` → `+233241234567`; `+23324123456` (8 digits), `+234…`, `abc` → error |
| `TestMaskPhone` | `+233241234567` → `+233*****4567`; short input doesn't panic |
| `TestIsRegion` | All 16 true; `"Accra"`, `""` false; case-sensitive exact match |

For the revocation test, the emulator supports `RevokeRefreshTokens`. Add a helper to `authtest` if needed (`authtest.Revoke(t, fb, uid)` through an exported method on `auth.Firebase`, e.g. `RevokeSessions(ctx, uid)`. It's useful later for admin tooling too).

## Manual QA

1. `make db-up auth-up redis-up && make test`: all green.
2. With the emulator, sign in (`scripts/dev-token.sh`), wait 6 minutes, and call the fixture step-up op through a test binary. Simpler: rely on the clock-injected test, and paste its `-v` output into the packet.
3. Walk the runbook in the real Firebase console, and tick each item in the review packet (screenshots optional).

## Pitfalls

- `VerifyIDTokenAndCheckRevoked` makes a network call to Firebase. Use it **only** for step-up operations.
- `auth_time` is the time of the last **sign-in**, not the token refresh. That's exactly what we want. Don't use `iat`.
- Keep the generic 401 for everything except the stale-auth case.
