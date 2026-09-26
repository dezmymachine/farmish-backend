# Phase 22: Deploy (Railway + Neon + Upstash + Cloudflare) & contract freeze

**Depends on:** 21 · **Size:** medium, mostly operational. **This phase needs the owner** for accounts, DNS and live keys. The agent prepares everything; the owner performs the steps marked 👤.

## Region decision (do first)

Pick **one** region for Railway, Neon and Upstash. EU (e.g. Railway `europe-west4`, Neon `aws-eu-central-1`, Upstash `eu-west-1` or `eu-central-1`) is the likely best latency for Ghana. Measure from Accra if possible. Record the decision in the ADR. If the existing Neon or Upstash resources are in another region, recreate them there (they hold test data only).

## Tasks

1. **GitHub Actions back** (if billing is fixed): `.github/workflows/ci.yml` runs `make ci` with services (Postgres 18, Redis 8, Firebase emulator image, MinIO). Retire ADR-0004 if it works. Otherwise keep the local gate.
2. **Railway** 👤:
   - Create a project with two services from the same image: `api` (`RUN_MODE=api`, public) and `worker` (`RUN_MODE=worker`, private). Or one `all` service for v1: **decide and record in the ADR**.
   - Set every §7 env var: `APP_ENV=production`, live keys, `rediss://`, `TRUSTED_PROXIES` (Railway's edge range, verified by logging `RemoteAddr` once), `TRUST_CLOUDFLARE=true`, `SHUTDOWN_TIMEOUT` below Railway's drain setting.
   - Health check path `/readyz`.
   - **Release command:** `/migrate up` (the image contains it; ADR-0005).
3. **Neon** 👤: branches `dev` and `prod`, the direct connection string, and `sslmode=require`.
4. **Upstash** 👤: a prod database in the chosen region; a **new** password (never the one used in dev chats).
5. **Cloudflare** 👤:
   - DNS for `api.farmish.gh` (proxied).
   - WAF managed rules on; a rate-limit rule on `/v1/webhooks/*` exempting Paystack IPs.
   - Turnstile production site key and secret.
   - R2 bucket `farmish-media` with a public custom domain `media.farmish.gh`, and bucket CORS allowing PUT from the frontend origin (`docs/runbooks/r2.md`).
6. **Paystack** 👤: business activation done; **Transfers enabled** and **transfer OTP disabled**; the live webhook URL set to `https://api.farmish.gh/v1/webhooks/paystack`; confirm `PAYSTACK_FEE_BPS` and the transfer fee, and update DOMAIN §2 if they differ.
7. **Firebase** 👤: a production project (separate from dev), the phone provider plus the Phase 7 runbook, the service account for `FIREBASE_CREDENTIALS_JSON`, and authorized domains.
8. **mNotify** 👤: `FARMISH` sender ID approved (or use the default), live key, `NOTIFY_SMS_ENABLED=true`.
9. **Staging smoke:** `scripts/staging-smoke.sh` runs against staging with Paystack **test** keys: sign in (test phone number) → seller profile → listing with an image → a second user buys it → pay with a test card → webhook → accept → ship → deliver → confirm → release → payout (test transfer) → balances. Record the output in the review packet.
10. **Contract freeze:** bump `info.version` in `api/openapi.yaml` to `1.0.0`, tag the backend repo `api-v1.0.0`, and document the change policy in `docs/API_VERSIONING.md` (additive only within v1; breaking changes need `/v2`).

## Done when

The staging end-to-end smoke test passes with Paystack test mode, production `/readyz` is green, the spec is tagged `v1.0.0`, and the plan's §11 regulatory item has an owner decision recorded (legal advice obtained, or the fallback of Paystack split payments chosen).
