# Review packet: Frontend F1 API client

## Summary

Built the F1 data layer per `docs/phases/frontend.md`: types generated from
`api/openapi.yaml` (openapi-typescript 7.13.0 → `src/api/schema.d.ts`, 72
paths), a lazily-created openapi-fetch client with injected Bearer-token
provider and envelope normalisation into `ApiError`, `lib/money.ts`
(`formatMoney`), `lib/errors.ts` (code → user copy), central `qk` keys +
stale tiers, a per-session QueryClient with the global auth net, the
`docs/data-fetching.md` agent contract, and a CI drift check. Architecture
borrowed from cyberplug-frontend, adapted to Bearer auth and our envelope.

## Commits

Frontend repo, on top of `2b58c44`:

- `F1: API client, keys, tiers, money/errors, data-fetching guide` (this phase)

## Done-when checklist

- [x] `pnpm gen:api` reads dev `api/openapi.yaml` → `schema.d.ts`: ran, 5412 lines; `FARMISH_OPENAPI` override for CI release file
- [x] `client.ts` auth middleware + error normalisation: `src/api/client.ts` (`getClient`/`setTokenProvider`/`request`)
- [x] `lib/money.ts`, `lib/errors.ts`: committed
- [x] Drift check in CI: `gen:api:check` (regenerate + `git diff --exit-code`) wired into `pnpm ci`, green
- [x] `formatMoney` table (0, 1, 100, 1250, 100000000): `src/lib/money.test.ts`, 5/5 pass
- [x] Error normalisation: `src/lib/errors.test.ts` (envelope parse, graceful degrade, copy map)
- [x] Auth header attached (MSW): `src/api/client.test.ts` (Bearer attach, 401 envelope, network_error)
- [x] `pnpm ci` green end to end including Playwright smoke (exit 0)

## pnpm ci

Green run (exit 0): `tsc6 --noEmit` clean, `gen:api:check` no diff,
ESLint clean, Prettier clean, 6 test files / 21 tests pass, `vite build`
clean, Playwright smoke 1 passed.

## Manual QA

```sh
$ pnpm gen:api && git diff --exit-code -- src/api/schema.d.ts  # exit 0, no drift
$ pnpm test  # 21 passed (money table, envelope, MSW auth header, keys, client defaults)
```

No backend needed (MSW mocks HTTP; drift check reads the checked-in spec).
No tokens involved — the token provider is a test stub in F1, Firebase
wiring lands in F2.

## Files changed (frontend repo)

New: `src/api/schema.d.ts` (generated), `src/api/client.ts`,
`src/api/client.test.ts`, `src/api/query-keys.ts`, `src/api/query-client.ts`,
`src/api/query-client.test.ts`, `src/lib/money.ts`, `src/lib/money.test.ts`,
`src/lib/errors.ts`, `src/lib/errors.test.ts`, `docs/data-fetching.md`.
Edited: `package.json` (deps: openapi-fetch, react-query, msw;
`gen:api`/`gen:api:check`/ci wiring).

Backend repo (this packet + plan tick/log). No backend code changed.

## Schema changes

none.

## API changes

none (contract read-only at v0.1.0; F3+ waits for the Phase 22 freeze).

## Deviations from the spec

- Client is created lazily (`getClient()` + `resetClient()`), not as a module
  singleton: openapi-fetch captures `globalThis.fetch` at creation, so
  import-time creation froze the reference before MSW patches it in tests.
  Same call shape (`request(() => getClient().GET(...))`).
- Token provider injected (`setTokenProvider`) instead of direct Firebase
  import: keeps F1 Firebase-free and MSW-testable; F2's AuthProvider wires
  the real `getIdToken`.
- `qk` factory + QueryClient defaults + `docs/data-fetching.md` included in
  F1 (architecture needs them; domain `queries/` files land with F2+).

## Open questions / risks

- `formatMoney` pins Node's `en-GH` Intl output (`GH₵0.00` style); same pinned
  toolchain runs CI, so deterministic here.
- F2 must provide the QueryClient provider + router context + SSR guard
  before any domain query ships.

## Backlog additions

none.
