# Review packet: Phase 14 Promotions

## Summary

Implemented the first real payment flow: seeded promotion packages, grossed-up
promotion purchases, idempotent CRD grants on `payments.succeeded`, locked
credit applications to the buyer's own active listings, and the five promotion
endpoints. Same-tier purchases queue behind the current window, higher tiers
replace it immediately, and lower tiers are rejected while a higher tier is
active. The end-to-end test proves purchase, webhook, grant, application and
promoted-first ranking, including replay safety.

## Commits

```
04fe8cf Phase 14: promotion endpoints and production wiring
79b8e46 Phase 14: promotion purchases, grants and applications
ee03449 Phase 14: promotion schema, seeds and shared seams
```

## Done-when checklist

Plan §6 Phase 14:

- [x] The end-to-end run goes purchase → webhook → credits → apply → listing ranked as promoted: `TestEndToEnd_PurchaseWebhookApplyRank` (`internal/http/promotions_test.go`)
- [x] A webhook replay doesn't double-credit: `TestHandlePromotionPaid_GrantsSnapshotCreditsOnce`, `TestWebhook_ReplaysAreNoOps`-style replay assertions in the end-to-end test, and unchanged CRD totals after the replay
- [x] Concurrent applies never make credits negative: `TestApply_ConcurrentNeverNegative` (`internal/promotions/service_test.go`)

Spec test table:

- [x] `TestPromotionConfigs_SeededAndPublic`: `TestPromotionConfigs_Seeded` and `TestPromotionConfigs_SeededAndPublic`
- [x] `TestPurchase_InitializesGrossedUpCharge`: `TestPurchase_InitializesGrossedUpCharge`, with the expected amount computed from `money.GrossUp`
- [x] `TestPurchase_WebhookGrantsCreditsOnce`: `TestHandlePromotionPaid_GrantsSnapshotCreditsOnce` and the webhook/replay section of `TestEndToEnd_PurchaseWebhookApplyRank`
- [x] `TestApply_DeductsCreditsAndRanks`: `TestApply_DeductsCreditsAndRanks` plus the ranking assertions in `TestEndToEnd_PurchaseWebhookApplyRank`
- [x] `TestApply_ExtendSameTier`: `TestApply_ExtendUpgradeDowngrade`
- [x] `TestApply_UpgradeReplaces` / `TestApply_DowngradeRejected`: `TestApply_ExtendUpgradeDowngrade`
- [x] `TestApply_InsufficientCredits409`: `TestApply_InsufficientCredits`
- [x] `TestApply_NotOwner403` / `TestApply_InactiveListing409`: `TestApply_ValidationOwnershipAndState` and `TestPromotions_EndpointFailures`
- [x] `TestApply_ConcurrentNeverNegative`: `TestApply_ConcurrentNeverNegative`
- [x] `TestEndToEnd_PurchaseWebhookApplyRank`: `TestEndToEnd_PurchaseWebhookApplyRank`

Supporting coverage:

- [x] `TestRequireAdvertisable` (`internal/listings/promotion_support_test.go`)
- [x] `TestInitialize_StoresAndForwardsMetadata` (`internal/payments/service_test.go`)
- [x] `TestApplications_IncludesInactiveHistory` (`internal/promotions/service_test.go`)

## make ci

```
#13 DONE 0.1s

#14 [build 6/6] RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build     CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/api ./cmd/migrate
#14 DONE 6.6s

#15 [stage-1 2/2] COPY --from=build /out/api /out/migrate /
#15 DONE 0.1s

#16 exporting to image
#16 exporting layers
#16 exporting layers 3.6s done
#16 exporting manifest sha256:649df7cc0f9c94e123e2899ba71138a1e712afd0cde5b18c4e5beb50614cfe3b 0.0s done
#16 exporting config sha256:6391135326c4ab439131199c66c49bf4217138d9b2c6c9f19f99ef6bbe3543f9 0.0s done
#16 exporting attestation manifest sha256:39469298f81cdc6136291a9b8ab0a002c1182ae4b8eedd15d831cc39d6d93ee6 0.0s done
#16 exporting manifest list sha256:4c8e73e0512bbc92bca5f7381f1ed333c6c661e1433f7713438cc45451e235d3 0.0s done
#16 naming to docker.io/library/farmish-backend:dev done
#16 unpacking to docker.io/library/farmish-backend:dev
#16 unpacking to docker.io/library/farmish-backend:dev
#16 unpacking to docker.io/library/farmish-backend:dev 0.4s done
#16 DONE 4.1s
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

## Manual QA

The local `.env` has no Paystack test keys, so the spec's live-card path was
adapted without weakening what it proves: the running API used a local
Paystack stand-in only for transaction initialization, at the configurable
`PAYSTACK_BASE_URL`. The signed production webhook path, River grant, credit
balance, application rules and search ranking were all exercised for real.

Setup:

```
$ make migrate-up
ok
$ make seed
{"level":"INFO","msg":"catalog seeded"}
{"level":"INFO","msg":"promotion tiers seeded"}
$ make migrate-version
version 11 (dirty: false)
```

Package list and purchase:

```
$ curl /v1/promotions/configs
top/5500/55, vip/7500/75, diamond/10000/100, enterprise/15000/150

$ curl -X POST /v1/promotions/purchases -d '{"tier":"top"}'
{
  "reference": "FMS-sdeyz2zl44456y22cduq",
  "authorizationUrl": "https://pay.local/checkout/FMS-sdeyz2zl44456y22cduq",
  "price": {"amount": 5500, "currency": "GHS"},
  "processingFee": {"amount": 110, "currency": "GHS"},
  "charge": {"amount": 5610, "currency": "GHS"},
  "credits": 55
}
```

Signed webhook, replay and balance:

```
$ curl -X POST /v1/webhooks/paystack \
  -H 'x-paystack-signature: <valid HMAC-SHA512>' \
  -d '<charge.success for the purchase reference>'
200
$ curl /v1/me/promotion-credits
{"balance":55}
$ # identical replay
200
$ curl /v1/me/promotion-credits
{"balance":55}
```

Application, ranking and history:

```
$ curl -X POST /v1/promotions/applications \
  -d '{"listingId":"<promoted listing>","tier":"top"}'
{
  "tier": "top",
  "creditsSpent": 55,
  "startsAt": "2026-09-26T20:59:47Z",
  "endsAt": "2026-10-03T20:59:47Z"
}

$ curl '/v1/listings?q=QA14+Heifer+<suffix>&limit=20'
[
  ["<promoted listing>", "Promoted QA14 Heifer ...", {"tier": "top"}],
  ["<plain listing>", "Plain QA14 Heifer ...", null]
]

$ curl /v1/me/listings/<promoted listing>/promotions
{"items": [{"tier": "top", "creditsSpent": 55, ...}]}
```

## Files changed

`git diff --stat ee03449^..04fe8cf` — 25 files, +3537/−234:

- `db/queries/promotions.sql` and `db/queries/payments.sql` — promotion, listing-promotion and payment-metadata queries.
- `internal/db/promotions.sql.go`, `internal/db/payments.sql.go`, `internal/db/models.go`, `internal/db/querier.go` — generated sqlc accessors; never hand-edited.
- `internal/promotions/promotions.go`, `tiers.go`, `service.go` — tier model, seed data, purchases, snapshot grants, locked applications, balances and history.
- `internal/promotions/service_test.go` — service, idempotency, concurrency and tier-transition tests.
- `internal/payments/payments.go`, `service.go`, `service_test.go` — durable and forwarded purchase metadata.
- `internal/listings/service.go` and `promotion_support_test.go` — locked owner/active checks for promotion use.
- `api/openapi.yaml` and `internal/http/api/api.gen.go` — five contract-first promotion operations.
- `internal/http/handlers/promotions.go`, `health.go`, `router.go`, `cmd/api/main.go` — handlers, seams and the registered real `promotion` purpose handler.
- `internal/http/promotions_test.go` — public config, failure-mapping and end-to-end tests.
- `cmd/seed/main.go` — seeds promotion tiers after catalog data.
- `migrations/000011_promotions.{up,down}.sql` — promotion configs, application credits and payment metadata.

## Schema changes

- `000011_promotions` — `promotion_configs`; `listing_promotions.credits_spent`; and `payments.metadata jsonb NOT NULL DEFAULT '{}'`.
- `TestUpDownUp` passes through `make ci`. `make migrate-version` on the development database reports `version 11 (dirty: false)`.
- `make seed` now seeds both catalog and promotion data idempotently.

## API changes

| Method | Path | operationId | Auth | Notes |
|---|---|---|---|---|
| GET | `/v1/promotions/configs` | `listPromotionConfigs` | public (`security: []`) | 200 / 429; `Cache-Control: public, max-age=300` |
| POST | `/v1/promotions/purchases` | `purchasePromotion` | bearer | 201 / 400 / 401 / 404 / 429 / 502; `x-farmish-rate-limit: sensitive` |
| GET | `/v1/me/promotion-credits` | `getMyPromotionCredits` | bearer | 200 / 401 / 429 |
| POST | `/v1/promotions/applications` | `applyPromotion` | bearer | 201 / 400 / 401 / 403 / 404 / 409 / 429; `x-farmish-rate-limit: sensitive` |
| GET | `/v1/me/listings/{id}/promotions` | `listMyListingPromotions` | bearer | 200 / 401 / 403 / 404 / 429 |

New schemas: `PromotionTier`, `PromotionConfig`, `PromotionConfigList`,
`PurchasePromotionRequest`, `PromotionPurchase`, `PromotionBalance`,
`ApplyPromotionRequest`, `PromotionApplication` and `PromotionApplicationList`.
The detailed application schema is intentionally not named `ListingPromotion`,
because that name already denotes Phase 12's search-result badge.
`make api-lint` and `make generate-check` pass.

## Deviations from the spec

1. **Purchase-time credits are stored in `payments.metadata`** (ADR-0023). The
   spec allowed either provider metadata or a `payments.metadata` column. The
   durable database snapshot is authoritative; the same value is also forwarded
   to Paystack for reconciliation.
2. **Same-tier renewals insert follow-on rows** (ADR-0022). The spec allowed
   either an extended row or a second row starting at the first row's end. This
   preserves promotion history and matches the service's insert-row flow.
3. **An immediate higher-tier replacement can leave a one-microsecond bridge**
   (ADR-0022). `listing_promotions` requires `ends_at > starts_at`, so a row
   replaced at its exact start time is ended one microsecond later. Search still
   selects the highest active tier rank.

## Open questions / risks

- **CRD balances are displayed by negating the ledger's raw liability sum.**
  That matches DOMAIN §5.1, but the service treats any positive raw balance as
  corrupt rather than displaying a negative balance.
- **Inactive tier purchases and applications are 404.** A tier discontinued
  after payment cannot be applied through the current request shape, because an
  apply request carries a tier but no payment reference. Current configs avoid
  the case, but a future retirement flow may need grandfathering.
- **Manual QA used a local Paystack stand-in for initialization only.** The
  webhook signature, River grant, ledger postings, application locks and ranking
  were production paths, but no live Paystack card or dashboard webhook was
  used because the local environment has no Paystack test keys.
- **Promotion history is available for inactive listings.** `Apply` requires an
  active listing, while `Applications` requires only ownership, so an owner can
  still inspect a retired promotion window.

## Backlog additions

none.
