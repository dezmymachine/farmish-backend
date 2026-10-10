# Review packet: Frontend F2 Auth

## Summary

Built the F2 auth layer per `docs/phases/frontend.md`: Firebase Auth
(Google, email/password, phone OTP via invisible reCAPTCHA) on
`/sign-in`, `/sign-up` and a guarded `/account` (GET/PATCH `/v1/me`);
an `AuthProvider` owning the per-session QueryClient and the Bearer-token
wiring with SDK persistence; pure `RequireAuth`/`RequireAdmin` guards
(role from `/v1/me`); a provider-aware step-up `ReauthDialog` opened by
the query client's global `reauth_required` net; and App Check init when
a site key is provisioned.

## Commits

Frontend repo, on top of `b51f9aa`:

- `cdf71c8` F2 setup: auth/form deps plus pre-existing gate fixes
- `143622b` F2 auth foundations: phone helpers, firebase init, error copy
- `fcee6dd` F2 auth: provider, guards, re-auth dialog, me queries, routes
- `bcc35b0` F2 e2e: scope page-link assertions to main
- `8ddf6fa` F2 fixes: SSR-consistent auth rendering

## Done-when checklist

Per `docs/phases/frontend.md` F2:

- [x] `/sign-in` (Google, email/password, phone `signInWithPhoneNumber` + invisible `RecaptchaVerifier` + OTP): `src/routes/sign-in.tsx`, proven by e2e "shows all three methods" and the emulator flows below
- [x] `/sign-up`: `src/routes/sign-up.tsx`, proven by e2e render test
- [x] `/account` (GET/PATCH `/v1/me`): `src/routes/account.tsx` + `src/api/queries/me.ts`, proven by `me.test.tsx` (MSW fetch + mutation cache) and the emulator flows
- [x] `AuthProvider` with session persistence: `src/auth/AuthProvider.tsx` (SDK persistence, `onIdTokenChanged` → token provider + `qk.me` invalidation)
- [x] Route guards (`requireAuth`, `requireAdmin` from `/v1/me.role`): `src/auth/guards.tsx`, proven by `guards.test.ts` (9-case matrix) + e2e logged-out redirect
- [x] Re-auth dialog for `reauth_required` (phone: `reauthenticateWithPhoneNumber`; email/Google: own provider): `src/auth/ReauthDialog.tsx`, proven by `ReauthDialog.test.tsx` (4 method branches + fallback)
- [x] App Check initialised: `src/auth/firebase.ts` (`initAppCheck` with reCAPTCHA v3 when `VITE_FIREBASE_APPCHECK_SITE_KEY` is set; documented skip otherwise)
- [x] Guard-redirect tests: `guards.test.ts` + e2e "account redirects logged-out visitors to sign-in"
- [x] Playwright sign-in flow vs Firebase Auth emulator: `tests/e2e/auth.spec.ts` emulator block (email, gated on `FARMISH_AUTH_E2E=1`); phone verified by manual script (see Manual QA)
- [x] `pnpm ci` green (typecheck + drift check + lint + format + 45 unit + build + 6 e2e passed / 1 emulator-gated skip)

## pnpm ci

Green on the default build (exit 0): `tsc6 --noEmit` clean,
`gen:api:check` no drift, ESLint clean, Prettier clean,
12 test files / 45 tests pass, `vite build` clean, Playwright
6 passed + 1 skipped (emulator flow, needs `FARMISH_AUTH_E2E=1`).

## Manual QA

Backend run with the documented default pairing (owner `.env` untouched;
`FIREBASE_PROJECT_ID`/`FIREBASE_AUTH_EMULATOR_HOST` overridden in the
environment only), frontend built with `VITE_USE_AUTH_EMULATOR=true`:

```sh
$ curl -H "Authorization: Bearer <emulator-token>" localhost:8080/v1/me
{"createdAt":"...","email":"probe5@example.com","emailVerified":false,
 "id":"9a49e808-...","role":"user","sellerVerified":false,
 "signupMethod":"email","updatedAt":"..."}  # me:200
$ FARMISH_AUTH_E2E=1 npx playwright test tests/e2e/auth.spec.ts -g emulator
1 passed  # email sign-in reaches /account showing the address
```

Phone script (new number, code read from the emulator `verificationCodes`
API, confirmed through the UI): reached `/account` showing
`024 765 4321` and signup method `phone`, zero page errors.
Hydration check (`/`, `/sign-in`, `/sign-up`, `/account`): no React 418
after the SSR-consistency fix (see Deviations).

Note: the owner's backend `.env` is real-project mode
(`FIREBASE_PROJECT_ID=farmish-dev`, no emulator host), so emulator QA
needs the override above; no backend files were changed.

## Files changed (frontend repo)

`git diff --stat b51f9aa..HEAD`: 31 files, +2891/−54.

New: `src/lib/phone.ts` (Ghana normalise/validate/format mirroring
`internal/geo`), `src/lib/firebase-env.ts` (web config + emulator/App
Check flags, null-safe), `src/auth/firebase.ts` (lazy init, emulator,
App Check), `src/auth/errors.ts` (Firebase code → copy),
`src/auth/AuthProvider.tsx`, `src/auth/guards.tsx`,
`src/auth/ReauthDialog.tsx`, `src/api/queries/me.ts`,
`src/routes/sign-in.tsx`, `sign-up.tsx`, `account.tsx`,
`tests/e2e/auth.spec.ts`, plus `*.test.ts(x)` for each.
Edited: `__root.tsx` (provider + Account/Sign in nav), `client.ts`
(optional response fields), `tests/setup.ts` (explicit cleanup),
`.prettierignore` (ignore generated schema), package/pnpm files (new
deps, firebase build approvals), prettier-only reformats.

Backend repo (this packet + plan tick/log). No backend code changed.

## Schema changes

none.

## API changes

none (contract read-only; F3+ waits for the Phase 22 freeze).

## Deviations from the spec

- No post-login deep-link return: sign-in/up always land on `/account`.
  Kept F2 small; return URLs can land with F3 routing.
- `unauthorized` navigates to sign-in without a token-refresh retry: the
  client always sends a fresh `getIdToken()`, so a 401 means a revoked or
  disabled session, not a stale token.
- Emulator Playwright flow is CI-skipped (`FARMISH_AUTH_E2E=1` gate):
  CI builds have no backend or emulator. Proven in manual QA instead.
- Drive-by gate repairs (all red at HEAD, verified via stash): msw v3
  `onUnhandledFrame`, optional openapi-fetch response fields,
  require-await/void-operator lint, prettier reformats, generated schema
  excluded from prettier (drift still guarded).
- SSR-consistency fixes found by manual QA: `isFirebaseConfigured` is a
  pure env read (server and client agree), and `RequireAuth` renders one
  shared loading tree pre-settle (React 418 otherwise). No ADR: behaviour
  matches the spec, only the rendering path changed.

## Open questions / risks

- `VITE_FIREBASE_APPCHECK_SITE_KEY` is still unprovisioned: App Check
  initialises only once the owner adds a reCAPTCHA v3 key (Firebase
  console > App Check). Phone sign-in works without it in dev.
- Owner's backend `.env` runs real-project Firebase while the checked-in
  `.env.example` documents emulator defaults; emulator e2e documents the
  override. Worth confirming which mode the owner wants as the daily
  default before F3.
- `logo.png` at the frontend repo root is untracked and unrelated; left
  alone (owner to keep or delete).

## Backlog additions

none (F9 already owns monitoring/redirects; no new items found).
