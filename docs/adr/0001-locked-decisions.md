# ADR-0001: Adopt the locked architecture decisions

- **Status:** Accepted
- **Date:** 2026-09-25
- **Source:** `REDEVELOPMENT_PLAN.md` §2

## Context

Farmish Ghana is being rewritten from scratch, replacing the legacy Next.js/Prisma app at `~/work/farmgate`. That app stays frozen as read-only reference for business rules. No data or users are migrated.

The marketplace needs:
- listings and search
- buyer/seller messaging
- checkout with escrow
- delivery tracking
- seller payouts
- promotions, reviews and supply requests

All of this is for a Ghanaian market, where mobile money and phone-number login matter as much as cards and email.

## Decision

| Area | Decision |
|---|---|
| Layout | Originally one repo holding both apps. **Superseded by ADR-0003:** separate `farmish-backend` and `farmish-frontend` repos |
| Backend | Go (latest stable, pinned in `go.mod`) + Gin. Contract-first OpenAPI 3.1 → `oapi-codegen` |
| Frontend | TanStack Start, built after the backend v1 contract is frozen |
| Hosting | Backend on Railway (Docker) behind Cloudflare (DNS/CDN/WAF/Turnstile) |
| Database | Neon Postgres (direct connection; Hyperdrive pooling later), `pgx` + sqlc, golang-migrate |
| Jobs | River (Postgres-native, same DB, so jobs are enqueued in the same transaction). Temporal is deferred behind a `Workflow` interface |
| Auth | Firebase for social + email/password via the client SDK. Phone login: Gin + mNotify OTP → Firebase custom token. No account linking in v1 |
| SMS | mNotify/BMS (`sms_type: "otp"`). We generate and hash codes; mNotify only delivers |
| Media | Cloudflare R2 presigned uploads + CDN |
| Payments | Paystack (card + MoMo). Farmish is merchant of record: funds are held in escrow on the Paystack balance and released to sellers via Paystack Transfers minus commission |
| Delivery | v1 stub: delivery method/address/fee/statuses on orders, plus a `DeliveryProvider` interface with a `manual` impl. Courier integration later |
| Money | Integer pesewas (`bigint`) everywhere in the DB and API (`amount` + `currency: "GHS"`). The frontend formats for display |
| Scope | Full rewrite, no data/user migration |

## Consequences

- **One datastore.** Postgres holds the app data, the job queue and the double-entry ledger. A state change, its ledger entries and its follow-up jobs commit atomically. There's no Redis or broker to run in v1.
- **Contract-first API.** The OpenAPI spec drives generated Gin server interfaces and, later, the typed frontend client. The frontend waits for the v1 contract freeze (Phase 22). F0–F2 may start after Phase 12.
- **Escrow model.** Farmish holds buyer funds and pays sellers itself. That makes it responsible for:
  - refunds, disputes and payout reconciliation
  - chargebacks that arrive after release
  - regulatory exposure under Bank of Ghana Act 987 (see plan §11)

  The fallback is Paystack split payments with delayed settlement.
- **Two auth paths.** Firebase handles social/email. We run our own phone OTP, with rate limits, Turnstile and hashed codes, then mint Firebase custom tokens. Either way the backend only ever trusts a verified Firebase ID token.
- **Integer money.** Pesewas as `bigint` removes floating-point rounding errors. Every API consumer must format for display.
- **Delivery is stubbed.** Orders carry delivery fields now, so a courier integration can land later behind `DeliveryProvider` without a schema rework.
