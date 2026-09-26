# Review packet: Phase 13a Payments core (Paystack, webhooks)

## Summary

The payment mechanism, with no purpose-specific effects: a tested Paystack
client, a `payments` table carrying the DOMAIN §2.2 gross-up, a
signature-verified webhook that is idempotent by construction, a verify
fallback that converges on the same code path as the webhook, and the
`payments.succeeded` job that hands a settled payment to a purpose handler.
Phase 14 registers `promotion`, Phase 15b `checkout`; this phase registers
neither, and a settled payment with no handler logs a warning rather than
failing the job.

## Commits

```
5cf64f9 Phase 13a: pass the payments service to the router
97de50f Phase 13a: webhook and payment status endpoints
20486d0 Phase 13a: payment service, webhook dispatch and the succeeded job
685a3b7 Phase 13a: Paystack client
26a5807 Phase 13a: payments and webhook_events schema
f4b1b45 Phase 13a: money helpers and Paystack config
```

## Done-when checklist

Plan §6 Phase 13a:

- [x] A bad signature returns 401: `TestWebhook_BadSignature401` (no signature,
  wrong secret, a signature over different bytes, non-hex, truncated,
  `W/`-prefixed) — `internal/http/payments_test.go`, and the signature
  comparison itself in `internal/http/middleware/paystack.go`
- [x] A replayed event is processed once: `TestWebhook_ChargeSuccessProcessesOnce`
  (three identical deliveries, one `webhook_events` row, one purpose-handler
  call, one job), `TestWebhook_ReplaysAreNoOps`, and
  `TestWebhook_ConcurrentReplays` (10 goroutines, one effect) —
  `internal/http/payments_test.go`, `internal/payments/service_test.go`
- [x] An amount or currency mismatch is rejected and logged:
  `TestWebhook_AmountMismatchRejected` and
  `TestWebhook_CurrencyMismatchRejected` assert the outcome, the payment still
  pending, an **Error**-level log carrying the reference and both amounts, and
  a `payment.amount_mismatch` audit row — `internal/payments/service_test.go`
- [x] The verify fallback and the webhook don't double-process:
  `TestVerify_FallbackThenWebhookIsDuplicate` — the fallback settles through a
  synthetic event keyed on the provider's transaction id, the later webhook
  loses the unique constraint, and the purpose handler has run exactly once
- [x] Money helpers match the DOMAIN §2 examples: `TestRoundHalfUp`,
  `TestGrossUp`, `TestCommission` carry 10,000 → 10,199/199 and
  12,345 @ 500bps → 617 as test rows, plus `TestGrossUp_NeverUnderCovers` and
  the overflow rows — `internal/money/money_test.go`

Spec test table:

- [x] `TestMoney_*` — the four helpers above, 93.6% statement coverage
- [x] `TestPaystackClient_<Method>` for all eight methods, each asserting the
  method, path, query, `Authorization: Bearer`, and JSON body, plus
  `TestPaystackClient_StatusFalseIsAnError` — `internal/payments/paystack_test.go`
- [x] `TestWebhook_BadSignature401`, `TestWebhook_ChargeSuccessProcessesOnce`,
  `TestWebhook_AmountMismatchRejected`, `TestWebhook_CurrencyMismatchRejected`,
  `TestWebhook_UnknownEventIgnored`, `TestWebhook_ConcurrentReplays`
- [x] `TestVerify_FallbackThenWebhookIsDuplicate`, `TestVerify_OtherUsers404`
- [x] `TestInitialize_GrossUpAndProviderFailure`
- [x] `TestConfig_PaystackKeys` — `internal/config/config_test.go`

Beyond the table: `TestWebhook_UnknownReferenceRejected`,
`TestWebhook_ChargeFailedMarksFailed`, `TestWebhook_MalformedBodyIs400`,
`TestWebhook_NoSecretFailsClosed`, `TestWebhook_SecondEventForASettledPaymentIsIgnored`,
`TestVerify_FallbackMarksFailedAndAbandoned`,
`TestVerify_YoungPaymentIsNotVerified`, `TestVerify_SettledPaymentIsNotReverified`,
`TestVerify_ProviderUnavailableLeavesItPending`,
`TestSucceeded_JobNeedsNoRegisteredPurpose`, `TestGetPaymentStatus_OwnerOnly`,
`TestGetPaymentStatus_ProviderDownIs502`, `TestNewPaystackClient_Defaults`.

## make ci

```

#12 [build 4/6] RUN --mount=type=cache,target=/go/pkg/mod go mod download
#12 CACHED

#13 [build 5/6] COPY . .
#13 DONE 0.1s

#14 [build 6/6] RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build     CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/api ./cmd/migrate
#14 DONE 7.6s

#15 [stage-1 2/2] COPY --from=build /out/api /out/migrate /
#15 CACHED

#16 exporting to image
#16 exporting layers done
#16 exporting manifest sha256:9e5592d56bda1a40936eaf372bdcfe9af3a736e1c818aa4a6fe6e09e561ea18c done
#16 exporting config sha256:cba6543918cea41c0ca671a7d84f8b8e5241b1b34027639447f3cfa30cf5ec01 done
#16 exporting attestation manifest sha256:cd53f8f3c39120c6aff1cfc0dbb7d6b20ee7481006581d1f089576d1a2804d2c 0.0s done
#16 exporting manifest list sha256:fb1e753f17295e1674e88e31e570511065f874d1f100d64fb27473db1ada766c 0.0s done
#16 naming to docker.io/library/farmish-backend:dev done
#16 unpacking to docker.io/library/farmish-backend:dev 0.0s done
#16 DONE 0.2s
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

## Manual QA

Local stack (Postgres 54320, Auth emulator 9099, Redis, rustfs), API on `:8081`
with a local test key (`PAYSTACK_SECRET_KEY=sk_test_local_qa`, since `.env` has
no real keys) and `PAYSTACK_CALLBACK_URL` set. The owner's `.env` was not
modified beyond the non-secret callback URL added in the config commit.

`Initialize` is deliberately not exposed over HTTP in this phase (Phase 14/15b
call it), so the payment rows were inserted with SQL, with the amounts DOMAIN
§2.2 prescribes: base 10,000, fee 199, charge 10,199.

```
$ BODY='{"event":"charge.success","data":{"id":302961,"reference":"FMS-fofuk36wffqdnjdyntwq",
          "amount":10199,"currency":"GHS","fees":199,"channel":"card","status":"success",
          "paid_at":"2026-09-26T16:30:00Z"}}'
$ eval "$(scripts/paystack-sign.sh "$BODY")"      # HMAC-SHA512, hex

$ curl -X POST :8081/v1/webhooks/paystack -H "x-paystack-signature: $SIGNATURE" -d "$BODY"
 [200]

$ # identical replay
 [200]

$ # signature of a different body
{"error":{"code":"unauthorized","message":"Invalid signature"}} [401]

$ psql
        reference         | status  |       reason
--------------------------+---------+-------------------
 FMS-fofuk36wffqdnjdyntwq | success | -
 FMS-5wknu254bt7ez6ndvppa | failed  | insufficient funds

         event_key          |          outcome
----------------------------+---------------------------
 charge.success:302961      | processed
 charge.success:400001      | rejected:amount_mismatch
 charge.success:400002      | rejected:currency_mismatch
 subscription.create:400003 | ignored
 charge.success:400004      | rejected:unknown_reference
 charge.failed:400005       | processed

        kind        |   state
--------------------+---------
 payments.succeeded | completed
```

`select status, paid_at is not null, channel, paystack_fee_pesewas` on the
settled payment: `success | t | card | 199`.

The mismatch log line, captured from the running API:

```
{"level":"ERROR","msg":"payment amount mismatch",
 "reference":"FMS-5wknu254bt7ez6ndvppa","expected_pesewas":10199,
 "received_pesewas":10199,"currency":"NGN"}
```

```
$ curl ":8081/v1/payments/$REF" -H "Authorization: Bearer $PAYER"
{"base":{"amount":10000,"currency":"GHS"},"charge":{"amount":10199,"currency":"GHS"},
 "paidAt":"2026-09-26T16:30:00Z","processingFee":{"amount":199,"currency":"GHS"},
 "reference":"FMS-fofuk36wffqdnjdyntwq","status":"success"} [200]

$ # another user
{"error":{"code":"not_found","message":"Payment not found"}} [404]
$ # anonymous
{"error":{"code":"unauthorized","message":"Authentication required"}} [401]
```

**Bug found and fixed by QA:** every signed webhook answered 500 with
"paystack is not configured" — `Deps.Payments` was never set in `main`, so only
the nil guard stood between a request and a panic. The endpoint tests build
their own `Deps` and could not see it. Fixed in `5cf64f9`.

## Files changed

`git diff --stat 0a9f829..5cf64f9` — 31 files, +5224/−177. New:

- `internal/money/money.go`, `money_test.go` — the rounding helpers.
- `migrations/000009_payments.{up,down}.sql` — `payments`, `webhook_events`.
- `db/queries/payments.sql` → `internal/db/payments.sql.go` (generated).
- `internal/payments/paystack.go` — the `Provider` interface and the client.
- `internal/payments/payments.go` — `Payment`, statuses, outcomes, reference
  generation, the placeholder email.
- `internal/payments/service.go` — `Initialize`, `HandleWebhook`,
  `Verify`, the built-in `charge.success`/`charge.failed` handlers.
- `internal/payments/jobs.go` — `payments.succeeded` and its worker.
- `internal/payments/fake/paystack.go` — scripted provider for other packages.
- `internal/payments/paystacktest/paystacktest.go` — body builders and `Sign`.
- `internal/http/middleware/paystack.go` — the signature middleware.
- `internal/http/handlers/payments.go` — the two handlers.
- `internal/http/payments_test.go` — the endpoint tests.
- `scripts/paystack-sign.sh` — signs a body for QA.
- `docs/adr/0020-…`, `docs/adr/0021-…`.

## Schema changes

- `000009_payments` — `payments` (reference, purpose, the base/fee/charge
  triple with a CHECK that the charge is their sum, status, and what Paystack
  reported) and `webhook_events` (one row per provider event, unique on
  `(provider, event_key)`). The down migration drops both.
- `TestUpDownUp` passes. `migrate version` on the dev database: `9`.
- I drafted and then **removed** a `UNIQUE (purpose, purpose_ref) WHERE status
  <> 'abandoned'` index: it would have made a legitimate retry after a failed
  payment collide with the failed row. The spec's non-unique
  `payments_purpose_idx` stands, and Phase 17/18 will need to decide how a retry
  is represented.

## API changes

| Method | Path | operationId | Auth | Notes |
|---|---|---|---|---|
| POST | `/v1/webhooks/paystack` | `paystackWebhook` | public (`security: []`) | 200 / 400 / 401 / 429 / 500 |
| GET | `/v1/payments/{reference}` | `getPaymentStatus` | bearer | 200 / 401 / 404 / 429 / 502 |

New schemas: `PaymentStatus`. New response: `PaymentProviderError` (502). New
error code: `payment_provider_error`. `If-None-Match` is not involved here.
`make api-lint` and `make generate-check` pass.

## Deviations from the spec

1. **`Initialize` takes no transaction** (ADR-0020). The spec writes
   `Initialize(ctx, tx?, …)` next to an explicit "insert the row (**commit**)"
   and a provider call "outside the tx"; those two cannot both hold if the
   caller owns the transaction. `Initialize` manages its own transactions so
   the network call never holds one open.
2. **A malformed body answers 400** (ADR-0021). The spec fixes 200/401/500 but
   is silent on a body that is not an event. 400 because a resend of the same
   bytes fails identically, and 500 would make Paystack retry forever.
3. **No `webhook` rate-limit class.** The spec does not ask for one. The
   endpoint is bounded by the existing per-address limit (300/min). Adding a
   class keyed by address could give a *higher* ceiling than the per-address
   limit, which is a weakening, so it stays out until Phase 21 pairs it with
   Paystack's IP allowlist (added to §10).
4. **Two test additions the spec's table implies but does not name:** an unknown
   reference (`rejected:unknown_reference`, the spec requires a warning log) and
   the no-secret case (the spec requires the signature check, so an unconfigured
   one has to fail closed).

## Open questions / risks

- **`payments.succeeded` with no purpose handler logs a warning and succeeds.**
  That is deliberate: until Phase 14 registers `promotion`, a settled payment
  has nothing to apply, and a job that failed would only retry to the same
  result. The risk is that Phase 14 forgets to register and nobody notices from
  the log. Worth a check when 14 lands.
- **The verify fallback settles inside `HandleWebhook`, so it writes a
  `webhook_events` row keyed on the provider's transaction id.** If Paystack
  never sends a webhook, that row is the only record that the money arrived,
  and its `payload` column holds a synthesised body rather than Paystack's.
  Phase 21's audit review should know that a row's payload is not necessarily
  verbatim.
- **A mismatch leaves the payment `pending` forever.** That is the safe state
  (the money is not ours to keep) but it needs a human, and nothing sweeps it
  until a later phase decides what to do with a payment Paystack collected and
  Farmish rejected. DOMAIN §2.2 says the *fee* difference is absorbed; a
  *charge* difference is not covered by any phase yet. Flagging for Phase 17.
- **The currency check runs before the amount check**, so a charge in the wrong
  currency is recorded as `rejected:currency_mismatch` even when the amount is
  also wrong. Fine today (one currency, `GHS`), but the log line then shows
  equal amounts, which reads oddly — visible in the QA output above.
- **No IP allowlist on the webhook.** The signature is the control, as the
  spec's pitfall list says. An allowlist is defence in depth and is in §10.
- **`money.MaxAmount` (1e12 pesewas) bounds the helpers, and a base near the
  ceiling grosses up past it.** `TestGrossUp_TopOfRangeIsRefused` pins the
  boundary. No real basket comes close.

## Backlog additions

- Allowlist Paystack's published source IPs on the webhook, or add a
  `webhook` rate-limit class keyed by address, if the per-address limit ever
  bites a real Paystack retry burst.
