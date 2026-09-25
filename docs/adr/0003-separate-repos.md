# ADR-0003: Separate backend and frontend repos

- **Status:** Accepted. Supersedes the Layout row of ADR-0001.
- **Date:** 2026-09-25

## Context

ADR-0001 put both apps in one repo at `~/work/farmish`. The backend and frontend ship on different schedules, and to different hosts: Railway for the backend, the frontend host is still undecided (F0). Separate repos give each app its own CI, releases and access control.

## Decision

- Two GitHub repos:
  - `github.com/dezmymachine/farmish-backend`
  - `github.com/dezmymachine/farmish-frontend`
- Both are checked out side by side in `~/work/farmish`, which is a plain folder, not a repo. It only holds an unversioned `CLAUDE.md` pointing at the two repos.
- `REDEVELOPMENT_PLAN.md` and `docs/adr/` live in the **backend repo**, because the backend owns the API contract and is built first. The frontend repo's `CLAUDE.md` points to them.
- The Go module path is `github.com/dezmymachine/farmish-backend`.
- CI is per repo. The backend workflow runs on every push/PR, with no path filters.

## Consequences

- The frontend consumes the contract across repos. F1 must fetch `api/openapi.yaml` from a tagged backend release, or from the sibling checkout during development, rather than a relative path in the same repo.
- Plan and ADR updates for frontend phases are committed to the backend repo.
- The earlier single-repo history was discarded. Both repos start with fresh commits.
