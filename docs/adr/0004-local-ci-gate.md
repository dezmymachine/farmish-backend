# ADR-0004: Local `make ci` gate while GitHub Actions is off

- **Status:** Accepted (temporary)
- **Date:** 2026-09-25

## Context

The GitHub account is locked over a billing issue, so no Actions job can start, even on public repos. Phase 1's "CI is green" check can't be met. Later phases (3: codegen drift and spec lint) also assume CI.

## Decision

- Remove `.github/workflows/ci.yml` for now.
- `make ci` is the required gate before every push and for every phase's "passes locally and in CI" check. In order, it runs:
  1. `tidy-check`: `go mod tidy -diff`
  2. `fmt-check`: `golangci-lint fmt --diff`
  3. `lint`: `go vet` + golangci-lint
  4. `test`: `go test -race`
  5. `vuln`: govulncheck, pinned
  6. `smoke`: Docker build, then `scripts/smoke.sh` checks `/healthz` returns 200 `{"status":"ok"}` and that SIGTERM exits 0 with the graceful-shutdown log
- A phase's CI requirements, such as Phase 3's drift check and spec lint, become `make ci` steps.
- Phase 1 counts as done on a green `make ci`.

## Consequences

- There's no server-side enforcement. A push that skips `make ci` can land broken code. Run it every time.
- When billing is fixed, restore a workflow that just runs `make ci` (backlog item) and retire this ADR.
