# ADR-0002: Backend skeleton choices

- **Status:** Accepted
- **Date:** 2026-09-25
- **Phase:** 1

## Context

Phase 1 left a few details open, and needed one small piece of Phase 3 early.

## Decisions

1. **Module path:** `github.com/dezmymachine/farmish-backend`, matching the GitHub repo (ADR-0003).
   - A bare `farmish-backend` path has no dot in its first element, so gofumpt treats it as a standard-library import.
   - It then fights goimports' `local-prefixes` grouping, and lint can never pass.

2. **Minimal error envelope pulled into Phase 1.** `internal/http/apierror` writes the §5 `{ "error": { "code", "message" } }` envelope.
   - Recovery (500), 404 and 405 need a body, and Gin's defaults aren't JSON.
   - Phase 3 still owns the full helper (details, validation errors) and may reshape this package.

3. **Extra optional env vars:** `LOG_LEVEL` (default `info`) and `SHUTDOWN_TIMEOUT` (default `15s`). Both are added to §7.
   - Required in Phase 1: `APP_ENV` (`development|test|staging|production`) and `CORS_ORIGINS` (explicit allowlist, `*` rejected).
   - `PORT` defaults to `8080`. Railway sets it.
   - Every later phase adds its own required vars to `internal/config` and `.env.example`.

4. **Gin always runs in release mode.** Debug mode prints non-JSON banners to stdout, which breaks the slog-JSON log rule (§5).

5. **No trusted proxies yet.** `SetTrustedProxies(nil)` means `ClientIP()` is the socket peer. Phase 6 adds `CF-Connecting-IP` handling.

6. **CORS doesn't allow credentials.** Auth is a bearer token (Firebase ID token), so cookies are never needed cross-origin.

7. **golangci-lint v2** is pinned in the `Makefile` and CI (`v2.14.0`) and installed into `bin/` by `make lint`, so no global install is needed.

## Consequences

- Import paths are long. Renaming the module later is a mechanical `go mod edit -module` + `sed`.
- Phase 3 has to reconcile its generated error types with `apierror.Envelope`.
