# Phase 13a: Payments core (Paystack client, payments, webhooks)

**Depends on:** 4, 5, 8 (uses `internal/audit`, `database.InTx` and `forbid_mutation()` from Phase 8) · **Size:** large

## Goal

A tested Paystack client, a `payments` table with fee gross-up (DOMAIN §2.2), a signature-verified, idempotent webhook endpoint that dispatches events to registered handlers, and a verify fallback. The purpose-specific effects (promotion credits, checkout confirmation) are plugged in by Phases 14 and 15b. **This phase provides the mechanism. Tests register a **test handler for the `promotion` purpose** on a service instance built in the test; production registration happens in Phase 14.**

## Packages

- **`internal/money`:** `RoundHalfUp(a, b, d int64)`, `CeilDiv(a, d int64)`, `GrossUp(base int64, feeBps int) (charge, fee int64)`, `Commission(subtotal int64, bps int) int64`. They check for overflow with `math/bits`, or bound inputs to ≤ 1e12 and document it. Table-tested (DOMAIN §1 edge values; the examples in DOMAIN §2 must be test rows).
- **`internal/payments`:**
  - `paystack.go`: the `Paystack` interface plus `PaystackClient`
  - `service.go`: `Initialize`, `HandleWebhook`, `Verify`, and the purpose registry
  - `fake/paystack.go`: a programmable fake

## Paystack client (`PaystackClient`, base URL configurable, 10s timeout, `Authorization: Bearer <secret>`)

Methods, each returning typed results and errors (never exposing the secret):
- `InitializeTransaction(ctx, {Email, AmountPesewas, Reference, CallbackURL, Metadata map[string]any, Channels []string{"card","mobile_money"}, Currency:"GHS"})` → `{AuthorizationURL, AccessCode, Reference}`. `POST /transaction/initialize`.
- `VerifyTransaction(ctx, reference)` → `Transaction{Status, AmountPesewas, Currency, FeesPesewas, Channel, PaidAt, Reference, ID}`. `GET /transaction/verify/:reference`.
- `CreateRefund(ctx, {TransactionReference, AmountPesewas})` → `{RefundID, Status}`. `POST /refund` (used in 17a).
- `ResolveAccount(ctx, {AccountNumber, BankCode})` → `{AccountName}`. `GET /bank/resolve` (18a).
- `ListBanks(ctx, {Currency:"GHS", Type: "mobile_money"|"ghipss"})` → `[]Bank{Name, Code, Type}`. `GET /bank` (18a).
- `CreateTransferRecipient(ctx, {Type, Name, AccountNumber, BankCode, Currency:"GHS"})` → `{RecipientCode}`. `POST /transferrecipient` (18a).
- `InitiateTransfer(ctx, {AmountPesewas, RecipientCode, Reference, Reason})` → `{TransferCode, Status}`. `POST /transfer` (18b).
- `VerifyTransfer(ctx, reference)` → `{Status, TransferCode, FailureReason}`. `GET /transfer/verify/:reference` (18b).

Paystack wraps every response in `{status: bool, message, data}`. Treat `status=false` or a non-2xx code as an error carrying `message`. Tests use `httptest.NewServer` with realistic bodies for **every** method: assert the method, path, headers and JSON body.

## Schema: `migrations/000009_payments.up.sql`

```sql
CREATE TABLE payments (
  id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  reference              text NOT NULL UNIQUE,              -- 'FMS-' || 20 base32 chars, generated server-side
  user_id                uuid NOT NULL REFERENCES users(id),
  purpose                text NOT NULL CHECK (purpose IN ('promotion','checkout')),
  purpose_ref            text NOT NULL,                     -- promotion tier / checkout id
  base_pesewas           bigint NOT NULL CHECK (base_pesewas > 0),
  processing_fee_pesewas bigint NOT NULL CHECK (processing_fee_pesewas >= 0),
  charge_pesewas         bigint NOT NULL CHECK (charge_pesewas = base_pesewas + processing_fee_pesewas),
  currency               text NOT NULL DEFAULT 'GHS' CHECK (currency = 'GHS'),
  status                 text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','success','failed','abandoned')),
  paystack_fee_pesewas   bigint,
  channel                text,
  authorization_url      text,
  paid_at                timestamptz,
  failure_reason         text,
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payments_user_idx ON payments (user_id, created_at DESC);
CREATE INDEX payments_purpose_idx ON payments (purpose, purpose_ref);
CREATE TRIGGER set_updated_at BEFORE UPDATE ON payments FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE webhook_events (
  id           bigserial PRIMARY KEY,
  provider     text NOT NULL,
  event_key    text NOT NULL,                  -- '<event>:<data.id>' (e.g. 'charge.success:302961')
  event_type   text NOT NULL,
  payload      jsonb NOT NULL,                 -- stored for audit; NEVER logged
  received_at  timestamptz NOT NULL DEFAULT now(),
  processed_at timestamptz,
  outcome      text,                           -- 'processed' | 'ignored' | 'rejected:<reason>'
  UNIQUE (provider, event_key)
);
```

## Webhook endpoint

`POST /v1/webhooks/paystack` is public (`security: []`). The spec's request body is `application/json` with `type: object` (loose).

- **Signature, before anything else.** Add `middleware.PaystackSignature(secret)`, mounted in the chain **before** `validate`. It only acts on this exact path:
  - read the raw body, capped at 1 MB (`http.MaxBytesReader`)
  - compute HMAC-SHA512 with `PAYSTACK_SECRET_KEY`, and compare to `x-paystack-signature` with **`hmac.Equal`** (constant time; the legacy app used `!==`)
  - missing or bad → **401** `unauthorized`, with the body not parsed and nothing stored
  - otherwise put the raw bytes in the context and restore `c.Request.Body` for the validator
- **Handler → `payments.Service.HandleWebhook(ctx, raw []byte)`.**
  1. Parse `{event, data}`. Compute `event_key = event + ":" + data.id`.
  2. In **one tx**:
     - `INSERT webhook_events … ON CONFLICT DO NOTHING RETURNING id`
     - no row → a duplicate: commit and return 200 (a replay is a no-op)
     - otherwise dispatch by `event` through the **event registry**: `map[string]EventHandler`, where `EventHandler func(ctx, tx pgx.Tx, data json.RawMessage) (outcome string, err error)`
     - set `processed_at` and `outcome`
  3. Unknown events → outcome `ignored`, 200.
  4. A handler error → roll back (the event row is gone too) and return **500**, so Paystack retries. Only do this for transient DB errors. Business rejections (e.g. an amount mismatch) return outcome `rejected:<reason>` and 200: retrying won't help.
- **Built-in `charge.success` handler** (this phase):
  - Lock the payment by `data.reference` (`FOR UPDATE`). Unknown → `rejected:unknown_reference` (log a warning).
  - If the payment isn't `pending` → `ignored` (already processed through the verify fallback, for example).
  - **Amount/currency check:** `data.amount == charge_pesewas && data.currency == "GHS"`, otherwise:
    - outcome `rejected:amount_mismatch`
    - the payment stays `pending`
    - log at **Error** level `"payment amount mismatch"` with the reference, expected and actual amounts (these are safe fields)
    - an audit event `payment.amount_mismatch`
    - this is the alert hook for Phase 21
  - Otherwise:
    - set `status=success`, `paid_at`, `channel`, `paystack_fee_pesewas = data.fees`
    - enqueue the River job `payments.succeeded{PaymentID}` (`jobs.Unique()`) in the same tx

    The job looks up `purpose` in the **purpose registry** (`map[string]PurposeHandler`, `PurposeHandler func(ctx, tx, payment Payment) error`) and runs it inside its own tx. Phases 14 and 15b register `promotion` and `checkout`. Purpose handlers must be idempotent: the ledger `UNIQUE(kind, reference)` guarantees this from 13b onwards.
- **`charge.failed`** (if Paystack sends it for a reference) → a pending payment becomes `failed` with `failure_reason`.

## Verify fallback

`GET /v1/payments/{reference}` (bearer, the owner only; others get 404, not 403, so existence isn't leaked).
- Returns `PaymentStatus {reference, status, charge: Money, processingFee: Money, base: Money, paidAt?}`.
- If `pending` and created more than 10s ago: call `VerifyTransaction`. If Paystack says success, run the **same** code path as the webhook: build a synthetic event keyed `charge.success:<data.id>` and call the same handler, so a later real webhook is a duplicate. If Paystack says failed or abandoned, mark it accordingly.

## Initialize (used by 14/15b; exposed here only through tests, with a test-registered `promotion` handler)

`payments.Service.Initialize(ctx, tx?, {UserID, Email, Purpose, PurposeRef, BasePesewas})`:
1. Compute the gross-up.
2. Insert the `pending` payment row (**commit**).
3. Call Paystack `InitializeTransaction` **outside** the tx, with metadata `{payment_id, purpose, purpose_ref}` and `callback_url = PAYSTACK_CALLBACK_URL?reference=…`.
4. Store `authorization_url`.
5. On a Paystack error: mark the payment `failed` and return `ErrProviderUnavailable` (handlers return 502 `payment_provider_error`).

For users without an email (phone sign-in), Paystack requires one: use a deterministic placeholder `u<first 12 hex of user id>@users.farmish.gh`.

## Config

- `PAYSTACK_SECRET_KEY` (required; **`sk_live_` required in production**, **`sk_test_` required otherwise**)
- `PAYSTACK_PUBLIC_KEY` (for clients; same prefix rule with `pk_`)
- `PAYSTACK_BASE_URL` (default `https://api.paystack.co`)
- `PAYSTACK_CALLBACK_URL` (required; the frontend payment-status page)
- `PAYSTACK_FEE_BPS` (default 195, range 0–1000)

## Tests

| Test | Proves |
|---|---|
| `TestMoney_*` | Rounding helpers and gross-up (10,000 → 10,199/199), commission (12,345 @ 500 → 617), overflow guard |
| `TestPaystackClient_<Method>` (each) | Request shape and response parsing against httptest; the `status:false` → error path |
| `TestWebhook_BadSignature401` | Wrong or missing signature → 401, no `webhook_events` row |
| `TestWebhook_ChargeSuccessProcessesOnce` | Valid signed event → payment success + `payments.succeeded` job completes + the test purpose handler ran **once**; **replaying the identical body** → 200, no second effect, still 1 event row |
| `TestWebhook_AmountMismatchRejected` | `data.amount` off by 1 → payment still pending, outcome `rejected:amount_mismatch`, Error log emitted (capture the logger), audit row |
| `TestWebhook_CurrencyMismatchRejected` | `NGN` → rejected |
| `TestWebhook_UnknownEventIgnored` | 200, outcome `ignored` |
| `TestWebhook_ConcurrentReplays` | 10 goroutines POST the same event: exactly one effect |
| `TestVerify_FallbackThenWebhookIsDuplicate` | Verify marks success through the fake Paystack; a later webhook is a no-op |
| `TestVerify_OtherUsers404` | |
| `TestInitialize_GrossUpAndProviderFailure` | Payment row amounts per DOMAIN §2.2; a Paystack 500 → payment failed + `ErrProviderUnavailable` |
| `TestConfig_PaystackKeys` | Prefix rules per environment |

Signing test events: `sig := hex(hmacSHA512(secret, body))`. Put a helper in `payments/paystacktest`.

## Manual QA

1. Run `make run` with **test** keys. Use the Paystack dashboard's test webhook, or `curl` a signed body built with the helper script `scripts/paystack-sign.sh` (add it) → 200. Replay → 200 no-op.
2. `psql`: `select event_key, outcome from webhook_events`.

## Pitfalls

- **Verify the signature over the raw bytes**, before any JSON parsing or re-serialisation.
- Paystack amounts are in the **subunit** (pesewas), which already matches us. Don't multiply by 100.
- Never log the payload (it has customer emails and phones) or the secret.
- Paystack sends from fixed IPs. An IP allowlist is optional defence in depth (Backlog). The signature is the control.
