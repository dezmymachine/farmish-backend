# ADR-0008: Auth core choices

- **Status:** Accepted
- **Date:** 2026-09-26
- **Phase:** 4

## Decisions

1. **The spec drives authentication.** `middleware.Authenticate` finds each request's OpenAPI operation and enforces what it declares:
   - `bearerAuth` security (the document default) → `RequireAuth`: verify the ID token, resolve the `users` row, and put it on the request context. The request logger gains `user_id`.
   - `x-farmish-role: admin` → additionally `RequireAdmin`.
   - `security: []` → public; the verifier is never called.

   Startup fails on an unknown role value, or a role on a public operation, so a typo can't silently open an admin route. The middleware lives in `internal/http/middleware`, not `internal/auth` as planned, to avoid an `auth` ↔ `users` import cycle.

2. **Auth runs before request validation,** so anonymous callers get a 401 before learning anything about request shapes.

3. **One generic 401** for every failure: missing or malformed header, bad/expired/wrong-project/forged token, unsupported provider. Body: `{"error":{"code":"unauthorized","message":"Authentication required"}}`, plus `WWW-Authenticate: Bearer`. The reason is logged at INFO. 403 is `forbidden`.

4. **`users.role` is authoritative.** Authorization reads the role from the row loaded on every request, so grants and revokes take effect immediately, even on existing tokens. The Firebase `role` custom claim only mirrors it for the frontend. Claim updates merge, so other claims (e.g. `seller_verified`) survive. `make grant-admin` / `revoke-admin` (`cmd/admin`) write the DB first, then the claim, and are safe to re-run.

5. **No per-request revocation check.** `VerifyIDToken` checks the signature against Google's cached public certs, plus issuer, audience and expiry. That's no network call per request. Revoked or disabled accounts keep access until their token expires (≤ 1h). Sensitive operations will use the revocation-checking verify (Phase 7 step-up, Phase 21).

6. **Sign-in provider mapping** (`auth.SignupMethod`):
   - `phone` → phone
   - `password` / `emailLink` → email
   - other IdPs → social
   - `custom` and `anonymous` are **rejected** (401): we never mint custom tokens, and anonymous sessions aren't accounts.

7. **`users` schema additions** beyond the plan:
   - `email_verified`, mirrored from the token
   - CHECKs on the E.164 `phone_e164` format and on `display_name` length (1–80)
   - email and phone are indexed but **not unique**, because there's no account linking and one email can back several accounts; `grant-admin --email` refuses to guess and asks for `--uid`

   Email, email_verified and phone are synced from the token only when they change (no write per request). Find-or-create is race-safe: `INSERT … ON CONFLICT DO NOTHING`, then re-select.

8. **Firebase configuration:**
   - `FIREBASE_PROJECT_ID` is required.
   - `FIREBASE_CREDENTIALS_JSON` takes inline JSON or a file path. It's optional locally (token verification needs only the project ID) and required in staging/production (for claims). It must be a service-account key for the same project, and its content is never echoed in errors.
   - `FIREBASE_AUTH_EMULATOR_HOST` is **refused** in staging/production, because emulator mode skips signature checks. Startup logs a loud WARN whenever it's set.

9. **Emulator for dev and tests.**
   - docker-compose runs `firebase-tools` 15.31.0 (auth only) on `127.0.0.1:9099`, project `demo-farmish`.
   - `.env.example` defaults to it, so `make run` works out of the box.
   - `authtest` creates real email and phone accounts via the emulator's REST API (including its SMS-code endpoint) and crafts unsigned tokens for negative cases.
   - `AUTHTEST_REQUIRED=1` (set by `make test`) makes a missing emulator fail instead of skip.

10. **Forged-signature test.** In emulator mode signatures aren't checked, so that test unsets the emulator host and checks an RS256 token signed by a random key against Google's real certs. It needs network access, like govulncheck. Offline, verification fails closed, so the test still holds.
