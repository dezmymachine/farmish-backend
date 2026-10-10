# Runbook: Railway deploy (backend)

Deploys `farmish-backend` from its Dockerfile on Railway (ADR-0001), behind
Cloudflare (`api.farmish.gh`). Source: Phase 22 in `REDEVELOPMENT_PLAN.md`.

**Never put secret values in this file, the repo or chat.** Set them only in
Railway's variables. Variable names and defaults follow plan §7 and
`.env.example`.

## Service settings

- Source: this repo, Dockerfile build.
- Run mode: `RUN_MODE=all` for v1 (one service). Split into `api` + `worker`
  later if needed (ADR-0010).
- Health check path: `/readyz`.
- Release command: `/migrate up` (ADR-0005). Runs before traffic.
- Custom domain: `api.farmish.gh`, proxied through Cloudflare.

## Environment variables

### Core

| Variable | Value / note |
|---|---|
| `APP_ENV` | `production` |
| `RUN_MODE` | `all` |
| `LOG_LEVEL` | `info` |
| `SHUTDOWN_TIMEOUT` | Below Railway's drain setting |
| `METRICS_ADDR` | Private address (Phase 21) |

### Database and cache

| Variable | Value / note |
|---|---|
| `DATABASE_URL` | Neon `prod` branch, direct string, `sslmode=require` |
| `DB_MAX_CONNS` | Small, e.g. `10` (Neon connection limit) |
| `DB_STATEMENT_TIMEOUT` | Per plan |
| `REDIS_URL` | Upstash prod, `rediss://`, same region as Railway |
| `REDIS_TIMEOUT` | Per plan |

### Web, auth and abuse

| Variable | Value / note |
|---|---|
| `CORS_ORIGINS` | Frontend origin(s) only |
| `FIREBASE_PROJECT_ID` | Production Firebase project |
| `FIREBASE_CREDENTIALS_JSON` | Production service account. Single quotes, one line |
| `TURNSTILE_SECRET` | Cloudflare production secret |
| `TRUST_CLOUDFLARE` | `true` |
| `TRUSTED_PROXIES` | Railway edge range. Verify logged `client_ip` is the real client |
| `DATA_ENCRYPTION_KEY` | New value, stored safely. Never rotate later (view dedupe depends on it) |

### Media (Cloudflare R2)

| Variable | Value / note |
|---|---|
| `R2_ACCOUNT_ID` | Cloudflare account ID |
| `R2_ACCESS_KEY_ID` | R2 API token |
| `R2_SECRET_ACCESS_KEY` | R2 API token |
| `R2_BUCKET` | `farmish-media` |
| `R2_PUBLIC_BASE_URL` | `https://media.farmish.gh` |

### Payments (Paystack)

| Variable | Value / note |
|---|---|
| `PAYSTACK_SECRET_KEY` | Live key |
| `PAYSTACK_PUBLIC_KEY` | Live key |
| `PAYSTACK_BASE_URL` | Paystack API base URL |
| `PAYSTACK_CALLBACK_URL` | `https://<frontend>/payment/callback` |
| `PAYSTACK_FEE_BPS` | `195`. Confirm live rate first |
| `PAYSTACK_TRANSFER_FEE_PESEWAS` | `0` until confirmed |

### Business rules

| Variable | Value / note |
|---|---|
| `CHECKOUT_EXPIRY_MINUTES` | `30` |
| `ESCROW_AUTO_COMPLETE_DAYS` | `3` |
| `SELLER_ACCEPT_TIMEOUT_HOURS` | `48` |
| `PAYOUT_MIN_PESEWAS` | `2000` (GHS 20) |

### Notifications (mNotify)

| Variable | Value / note |
|---|---|
| `MNOTIFY_API_KEY` | Live key |
| `MNOTIFY_SENDER` | `FARMISH` once approved |
| `NOTIFY_SMS_ENABLED` | `true` |
| `MNOTIFY_LOW_BALANCE` | `200` (credits) |

## Must NOT be set in production

- `FIREBASE_AUTH_EMULATOR_HOST`: refused when deployed.
- `R2_ENDPOINT`: refused when deployed (local MinIO/rustfs override only).

## Before going live

- Confirm `PAYSTACK_FEE_BPS` and the transfer fee with Paystack.
- Record the escrow regulatory decision (plan §11).
- Run the staging smoke test (`scripts/staging-smoke.sh`) with Paystack test
  keys first.
- After the first deploy: `/readyz` green, logged `client_ip` correct,
  `make seed` run against prod (catalog), owner admin granted with
  `make grant-admin`.
