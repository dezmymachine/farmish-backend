# Phase 14: Promotions (first real payment flow)

**Depends on:** 12, 13b · **Size:** medium

## Goal

Sellers buy promotion credit packages through Paystack, receive credits (CRD ledger) idempotently on payment success, and spend them to promote their own listings. Promoted listings rank first in search (Phase 12 SQL). All rules are in DOMAIN §6.

## Schema: `migrations/000011_promotions.up.sql`

```sql
CREATE TABLE promotion_configs (
  tier          text PRIMARY KEY CHECK (tier IN ('top','vip','diamond','enterprise')),
  name          text NOT NULL,
  price_pesewas bigint NOT NULL CHECK (price_pesewas > 0),
  credits       int  NOT NULL CHECK (credits > 0),
  duration_days int  NOT NULL CHECK (duration_days > 0),
  tier_rank     int  NOT NULL UNIQUE CHECK (tier_rank BETWEEN 1 AND 4),
  featured      boolean NOT NULL DEFAULT false,
  description   text NOT NULL DEFAULT '',
  features      text[] NOT NULL DEFAULT '{}',
  sort_order    int  NOT NULL,
  is_active     boolean NOT NULL DEFAULT true,
  updated_at    timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE listing_promotions ADD COLUMN credits_spent int NOT NULL DEFAULT 0 CHECK (credits_spent >= 0);
```

**Seed** through `catalog`-style seed code in `cmd/seed` (`promotions.Seed`): the four tiers from DOMAIN §6, with descriptions and features copied from `~/work/farmgate/prisma/seed.ts`. Idempotent.

## API (tag `promotions`)

| Method & path | Auth | Request | Responses |
|---|---|---|---|
| `GET /v1/promotions/configs` | public, `Cache-Control: public, max-age=300` | none | 200 `{items: PromotionConfig[]}` (active, sorted) |
| `POST /v1/promotions/purchases` | bearer, `sensitive` | `{tier}` | 201 `{reference, authorizationUrl, price: Money, processingFee: Money, charge: Money, credits}`, 400, 502 `payment_provider_error` |
| `GET /v1/me/promotion-credits` | bearer | none | 200 `{balance: int}` (CRD) |
| `POST /v1/promotions/applications` | bearer, `sensitive` | `{listingId, tier}` | 201 `ListingPromotion`, 403 (not the owner), 404, 409 `insufficient_credits` \| `promotion_downgrade` \| `listing_not_active` |
| `GET /v1/me/listings/{id}/promotions` | bearer (owner) | none | 200 `{items: ListingPromotion[]}` |

- **`PromotionConfig`:** `{tier, name, price: Money, credits, durationDays, featured, description, features}`.
- **`ListingPromotion`:** `{id, listingId, tier, startsAt, endsAt, creditsSpent, replacedRemaining?: bool}`.

## Rules

- **Purchase:** `payments.Initialize(purpose="promotion", purpose_ref=tier, base=price_pesewas)`. Any authenticated user may buy; applying requires owning an active listing.
- **Purpose handler `promotion`** (registered in 14 via the 13a registry), in one tx:
  - post `promotion_paid` using DOMAIN §5.3.4, with `f = payment.paystack_fee_pesewas` and credits from the config tier **as it was at purchase time**
    - store the credits on the payment: add `credits int` to the metadata, or a `payments.metadata jsonb` column (**decide and document**)
  - `ErrDuplicate` → no-op
- **Apply:** DOMAIN §6 exactly. In one tx:
  1. Lock the listing `FOR UPDATE` and check owner + active.
  2. Take `pg_advisory_xact_lock(hashtext('promo:'||user_id))`.
  3. Read the credit balance. Less than the tier's credits → `ErrInsufficientCredits`.
  4. Find the current active promotion and apply the extend / upgrade / downgrade rule.
  5. Insert the `listing_promotions` row.
  6. Post `promotion_applied` (ref = new promotion id).
- **Ranking** already works through Phase 12's SQL. Verify it end to end.

## Jobs

None required. Ranking checks `ends_at`. Optional: nightly cleanup of rows with `ends_at < now - 90d` (Backlog).

## Tests

| Test | Proves |
|---|---|
| `TestPromotionConfigs_SeededAndPublic` | 4 tiers, pesewas prices per DOMAIN §6, `assertContract` |
| `TestPurchase_InitializesGrossedUpCharge` | vip: price 7500 → charge = gross-up (compute from `money.GrossUp`); the fake Paystack received that amount |
| `TestPurchase_WebhookGrantsCreditsOnce` | Signed `charge.success` → balance 75; **replay** → still 75; ledger: `promotion_paid` balanced, GHS and CRD |
| `TestApply_DeductsCreditsAndRanks` | Apply top → balance −55; search puts the listing first (Phase 12 endpoint) |
| `TestApply_ExtendSameTier` | Apply top twice → one promotion's `ends_at` extended by 7d (or a second row starting at the first's end; **pick one, document, test**) |
| `TestApply_UpgradeReplaces` / `TestApply_DowngradeRejected` | DOMAIN §6 |
| `TestApply_InsufficientCredits409` | |
| `TestApply_NotOwner403` / `TestApply_InactiveListing409` | |
| `TestApply_ConcurrentNeverNegative` | Balance 75; 5 concurrent applies of top (55) → exactly 1 succeeds; balance 20 |
| `TestEndToEnd_PurchaseWebhookApplyRank` | The **plan's Done-when**: purchase → webhook → credits → apply → ranked promoted; webhook replay doesn't double-credit |

## Manual QA

Test keys: purchase `top`, open the `authorizationUrl`, and pay with a Paystack test card (`4084 0840 8408 4081`, any future expiry, CVV `408`, PIN `0000`, OTP `123456`). The webhook needs a public URL: use the Paystack dashboard's "resend webhook" with a tunnel (`cloudflared tunnel --url localhost:8080`), or call `GET /v1/payments/{ref}` (verify fallback). Then the balance shows 55.

## Pitfalls

- Credits are CRD, never GHS. Keep currencies separate in every posting.
- The credit check and deduction must be in the **same** tx under the advisory lock. The legacy app got this wrong.
