# ADR-0035: Frontend F0 scaffold choices

Date: 2026-10-09 · Phase: F0

## Deploy target: Cloudflare Workers/Pages

The frontend deploys to Cloudflare (Workers/Pages with the TanStack Start
Cloudflare adapter, verified at F9). Rationale: the whole edge stack already
lives there (DNS/CDN/WAF/Turnstile, R2 CDN for media), mobile-first users get
a close PoP, and preview URLs give cheap per-PR review. The F0 build targets
portable Nitro output; the adapter is wired at F9 production deploy.

## Lint/format: ESLint (typescript-eslint strict) + Prettier

Picked over Biome. Rationale: the phase spec names typescript-eslint strict as
the default, the shadcn/TanStack ecosystem documents against it, and strict
type-checked rules already caught real issues in F0 (template `number`,
`import.meta.env` any-typing, constant-condition test). Prettier handles
formatting; `eslint-config-prettier` resolves conflicts.

## Theming: Spectrum philosophy on shadcn, Farmish palette (owner decision)

Per owner choice (visual-style-only, keep brand colors, doc-only guide): no
`@react-spectrum/s2` runtime. The Adobe skill (`react-spectrum-s2`,
installed 2026-10-09) was used as a token/philosophy reference only. Its
guidance to avoid Tailwind/shadcn applies to S2-runtime implementations, not
to this visual borrowing — the F0 spec mandates Tailwind v4 + shadcn/Radix,
which takes precedence. What transferred: attention hierarchy (one accent
action per view), 4pt spacing, role-based type (`t-*` classes), pill primary
buttons, 2px focus rings, semantic color names, S/M/L/XL sizing language.
Documented in `farmish-frontend/docs/design-system.md`.

## Toolchain: follow the official Start example, pinned

React 19.3.0, `@tanstack/react-start` 1.168.60, router 1.170.41, Vite 8.3.4,
Nitro 3 beta, Tailwind 4.3.3, Vitest 5.0.3, Playwright 1.64.0, ESLint 10,
TypeScript 6.0.2 (ships its binary as `tsc6`; `pnpm typecheck` uses it).
Exact pins live in `farmish-frontend/package.json` + `pnpm-lock.yaml`.
`src/routeTree.gen.ts` is committed (as in the official example) so a fresh
clone typechecks without a prior build.
