# Review packet: Frontend F0 Scaffold

## Summary

Scaffolded the TanStack Start app in `farmish-frontend` per
`docs/phases/frontend.md` F0: TS strict + `noUncheckedIndexedAccess`, Tailwind
v4 with the Farmish Spectrum-inspired theme, one restyled shadcn Button,
ESLint (typescript-eslint strict) + Prettier, Vitest, Playwright, `pnpm ci`
gate, `.env.example`, and the doc-only visual guide
(`farmish-frontend/docs/design-system.md`). Home renders SSR with a health
badge fed by a route loader calling the backend `/healthz`.

## Commits

Frontend repo, on top of `42ddf6c`:

- `F0: scaffold TanStack Start app, Farmish theme, design guide` (this phase)

## Done-when checklist

- [x] `pnpm ci` green (typecheck + lint + format:check + test + build + e2e smoke): exit 0, full run 2026-10-09
- [x] Home page renders SSR with a health badge calling `/healthz`: verified by curl against the built server (heading + badge present in SSR HTML; badge reads "unreachable" with no backend running) and by `tests/e2e/smoke.spec.ts`
- [x] `.env.example` with `VITE_API_BASE_URL`, Firebase web config, `VITE_TURNSTILE_SITE_KEY`, `VITE_PAYSTACK_PUBLIC_KEY`: committed
- [x] Deploy-target ADR: ADR-0035 (Cloudflare Workers/Pages, adapter wired at F9)

## pnpm ci

Tail of the green run (exit 0):

```text
$ tsc6 --noEmit
$ eslint .
$ prettier --check .
Checking formatting...
All matched files use Prettier code style!
Test Files  2 passed (2) / Tests  4 passed (4)
✓ built in ~1s / Generated .output/nitro.json
1 passed (3.8s)  [playwright smoke]
```

## Manual QA

```sh
$ pnpm build && (node .output/server/index.mjs & sleep 4)
$ curl -s http://localhost:3000/ | python3 -c "..."
SSR heading present: True
health badge present: True
SSR api-unreachable fallback: True
```

Backend was not running: the loader caught the refused connection and SSR
rendered the grey "API unreachable" badge instead of failing the page. No
tokens involved (public `/healthz`, no auth header).

## Files changed (frontend repo)

New: `package.json`, `pnpm-lock.yaml`, `pnpm-workspace.yaml`, `tsconfig.json`,
`vite.config.ts`, `vitest.config.ts`, `playwright.config.ts`, `eslint.config.js`,
`.prettierrc`, `.prettierignore`, `components.json`, `.env.example`,
`src/router.tsx`, `src/routeTree.gen.ts` (generated, committed like the
official example), `src/routes/__root.tsx`, `src/routes/index.tsx`,
`src/styles/app.css` (Farmish theme), `src/components/NotFound.tsx`,
`src/components/DefaultCatchBoundary.tsx`, `src/components/HealthBadge.tsx`,
`src/components/ui/button.tsx`, `src/lib/utils.ts`, `src/lib/env.ts`,
`src/lib/env.test.ts`, `src/lib/utils.test.ts`, `tests/setup.ts`,
`tests/e2e/smoke.spec.ts`, `docs/design-system.md`.
Edited: `README.md`, `CLAUDE.md` (commands), `.gitignore` (test output).
Purpose per file is covered in Summary; theme rationale in ADR-0035.

Backend repo (this packet + ADR-0035 + plan tick/log). No backend code changed.

## Schema changes

none.

## API changes

none (backend contract untouched, still pre-1.0.0; F1 generates types from it).

## Deviations from the spec

- Typecheck runs `tsc6` (TypeScript 6 ships its binary as `tsc6`, not `tsc`).
  ADR-0035.
- `src/routeTree.gen.ts` committed rather than ignored (matches the official
  example; keeps fresh-clone typecheck green). ADR-0035.
- Theming direction fixed by owner decision (Spectrum philosophy on shadcn,
  Farmish palette, doc-only guide; no S2 runtime). ADR-0035.

## Open questions / risks

- TypeScript 6 + ESLint 10 are both very new; the gate is green today but
  plugin churn could bite later. Fallback recorded in ADR-0035 discussion:
  Biome for lint/format if typescript-eslint breaks.
- Cloudflare adapter wiring deferred to F9; if the adapter lags Start 1.168,
  revisit the deploy target then.
- `logo.png` (untracked, predates F0) left untouched.

## Backlog additions

none.
