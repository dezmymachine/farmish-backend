# Domain rules: the single source of truth

Every business rule, money formula and state transition lives here. Phase specs reference these sections. **If code disagrees with this file, the code is wrong.** If this file is silent on something, stop and ask. Don't invent a rule.

Owner decisions are dated. The legacy app (`~/work/farmgate`) was used as input, but its bugs are deliberately **not** ported. Each fix is listed in §12.

---

## 1. Money

- All amounts are **integer pesewas** (`int64` / `bigint`). GHS 1.00 = 100 pesewas. The currency is always `"GHS"`.
- API shape: `Money {amount: int64, currency: "GHS"}`. Clients format for display.
- **Never** use floats for money. Every division has an explicit rounding rule:
  - **Round half up** (`roundHalfUp(a*b, d) = (a*b + d/2) / d`) for commission.
  - **Ceiling** (`ceilDiv(a, d) = (a + d - 1) / d`) for fee gross-up. The platform must never receive less than it needs.
- Implement these once, in `internal/money` (Phase 13a), and table-test them with edge values: 0, 1, 99, 100, 10001 and large amounts near 1e12.

## 2. Fees and commission (owner decisions, 2026-09-26)

### 2.1 Commission

- **Default 5% = 500 basis points**, on the **item subtotal only**. Delivery fees go to the seller in full.
- Per-category overrides live in `commission_configs` (Phase 15a). Resolution order:
  1. the listing's category override
  2. its parent category's override
  3. the default row (`category_id IS NULL`)
- The rate is **snapshotted** on each order (`orders.commission_rate_bps`) at checkout. Later config changes never affect existing orders.
- `commission = roundHalfUp(subtotal * rate_bps, 10000)`.
  - Example: subtotal 12,345 at 500 bps → (6,172,500 + 5,000) / 10,000 = **617**.

### 2.2 Paystack processing fee: paid by the buyer

- Paystack Ghana charges **1.95%** on local card and MoMo transactions. This is config `PAYSTACK_FEE_BPS=195`; confirm against the live Paystack dashboard before go-live.
- The buyer pays a separate, visible **processing fee** line, grossed up so Farmish nets the full base after Paystack's cut:

  ```
  base          = Σ over orders (subtotal + delivery_fee)      // or the promotion price
  charge        = ceilDiv(base * 10000, 10000 - PAYSTACK_FEE_BPS)
  processing_fee = charge - base
  ```

  Example: base 10,000 → charge = ceilDiv(100,000,000, 9,805) = **10,199**, processing fee **199**.
- The same rule applies to **every** Paystack charge: checkout and promotion purchases.
- The actual fee Paystack took arrives in the webhook as `data.fees` (pesewas). The ledger books the **actual** fee (§5). Any difference from our estimate is absorbed by `processing_fee_income` versus `paystack_fees`.
- **The processing fee is non-refundable** and is shown as such at checkout. Refunds return the order's base amount (or a partial amount); Paystack doesn't return its fee. *(Default decision: the owner may revisit before Phase 17.)*

### 2.3 Transfer (payout) fees

- The platform absorbs Paystack transfer fees. Config `PAYSTACK_TRANSFER_FEE_PESEWAS` (default `0` until confirmed from the Paystack dashboard) is booked per successful transfer (§5.6). *(Assumption: confirm the actual GH MoMo/bank transfer fee before Phase 18.)*

## 3. Order timers and payouts (owner decisions, 2026-09-26)

| Setting (env) | Value | Meaning |
|---|---|---|
| `SELLER_ACCEPT_TIMEOUT_HOURS` | **48** | A `paid` order not accepted within 48h auto-cancels, with a refund |
| `ESCROW_AUTO_COMPLETE_DAYS` | **3** | A `delivered` order with no dispute auto-completes 3 days after `delivered_at`, and escrow releases |
| `CHECKOUT_EXPIRY_MINUTES` | **30** | An unpaid checkout expires; reserved stock returns |
| `PAYOUT_MIN_PESEWAS` | **2000** (GHS 20) | The minimum `seller_payable` balance to trigger a payout |
| Payout schedule | **daily, 10:00 Africa/Accra** (UTC+0, no DST) | `ExecutePayouts` periodic job |
| Payout account change cooldown | **48h** | No payouts to a newly changed account for 48h (anti-takeover) |
| Step-up freshness | **5 min** | `RequireRecentAuth(5m)` for payout-account changes |

## 4. Order state machine

States: `pending_payment, paid, accepted, shipped, delivered, completed, cancelled, disputed, refunded, expired`.

The plan's earlier `fulfilling` state is **dropped**: `accepted` covers preparing the order, and `shipped` also means "ready for pickup" for pickup orders.

`escrow_state`: `none → held → released | refund_pending → refunded | partially_refunded`.

| From | To | Actor | Trigger / endpoint | Side effects (same DB tx) |
|---|---|---|---|---|
| pending_payment | paid | system | `ConfirmCheckout` job after `charge.success` | escrow_state=held; ledger §5.2; notify seller |
| pending_payment | expired | system | `ExpireUnpaidCheckouts` (30 min) | restore stock |
| expired | paid | system | `ConfirmCheckout` for a checkout whose payment arrived after expiry, **and** whose stock could be re-reserved | escrow_state=held; ledger §5.2; notify seller |
| expired | cancelled | system | same trigger, but stock could **not** be re-reserved | note `stock_unavailable_after_expiry`; escrow_state=refund_pending; ledger §5.2 posted (the money arrived); enqueue a full refund per order |
| paid | accepted | seller | `POST /v1/seller/orders/{id}/accept` | notify buyer |
| paid | cancelled | seller | `POST /v1/seller/orders/{id}/reject` (reason required) | restore stock; enqueue `RefundOrder`; escrow_state=refund_pending |
| paid | cancelled | buyer | `POST /v1/orders/{id}/cancel` (only before acceptance) | same as reject |
| paid | cancelled | system | `AutoCancelUnaccepted` (48h) | same as reject |
| accepted | cancelled | seller | `POST /v1/seller/orders/{id}/reject` | same as reject |
| accepted | shipped | seller | `POST /v1/seller/orders/{id}/ship` (optional `trackingRef`) | notify buyer |
| shipped | delivered | seller | `POST /v1/seller/orders/{id}/mark-delivered` | delivered_at=now; `auto_complete_at = delivered_at + 3d` |
| shipped, delivered | completed | buyer | `POST /v1/orders/{id}/confirm-receipt` | enqueue `ReleaseEscrow` |
| delivered | completed | system | `AutoCompleteOrders` (now ≥ auto_complete_at) | enqueue `ReleaseEscrow` |
| shipped, delivered | disputed | buyer | `POST /v1/orders/{id}/dispute` (reason required; before auto-complete) | create `disputes` row; stops the auto-complete timer |
| disputed | refunded | admin | resolve `refund_buyer` | enqueue `RefundOrder` (full base) |
| disputed | completed | admin | resolve `release_seller` | enqueue `ReleaseEscrow` |
| disputed | completed | admin | resolve `partial` (refundAmount) | enqueue `RefundOrder(partial)`, then `ReleaseEscrow(remainder)` |

- **Anything not in this table is illegal**: 409 `invalid_transition`.
- Actors are enforced: a buyer can't call seller transitions and vice versa (403), including for their own order in the other role.
- There is **one** function, `orders.Transition(ctx, tx, orderID, to, actor, note)`. It locks the row (`SELECT … FOR UPDATE`), checks the table, writes `order_events (from, to, actor_type, actor_id, note)`, and returns side-effect instructions that the caller executes in the same tx.
- Unit tests cover **every** (from, to, actor) pair: allowed pairs succeed, and all others return `ErrInvalidTransition`.

### 4.1 Partial refunds and commission

A partial refund `r` (dispute outcome `partial`) is taken from the **delivery fee first**, then from the subtotal. Commission is charged only on the subtotal that remains:

```
refunded_subtotal = max(0, refunded_total - delivery_fee)
remaining_base    = base - refunded_total
commission'       = roundHalfUp((subtotal - refunded_subtotal) * rate_bps, 10000)
seller_net        = remaining_base - commission'
```

Example: subtotal 10,000, delivery 500, base 10,500, rate 500 bps, partial refund 2,500:
- refunded_subtotal = 2,000
- remaining_base = 8,000
- commission' = roundHalfUp(8,000 × 500, 10,000) = **400**
- seller_net = **7,600**

A full refund leaves nothing to release: no commission is earned.

## 5. Ledger (double-entry, append-only)

### 5.1 Model

- **`ledger_accounts`** `(id, code text UNIQUE, type, currency, owner_id NULL)`:
  - `type ∈ asset|liability|revenue|expense|equity`
  - `currency ∈ GHS|CRD`
  - codes are strings like `escrow`, `seller_payable:<uuid>`, `promo_credits:<uuid>`
  - created lazily with `INSERT … ON CONFLICT (code) DO NOTHING`
- **`ledger_transactions`** `(id, kind, reference text, created_at)`:
  - `UNIQUE(kind, reference)` is the idempotency key, e.g. `('checkout_paid', <payment_reference>)` or `('escrow_release', <order_id>)`
- **`ledger_entries`** `(id, transaction_id, account_id, amount bigint, currency, order_id NULL, created_at)`:
  - **positive = debit, negative = credit**
  - Σ amount per (transaction, currency) = 0
- **Append-only.** No UPDATE or DELETE on ledger tables: enforce it with a trigger that raises. Corrections are new reversing transactions.
- **`ledger.Post(ctx, tx, kind, reference, entries...)`:**
  - validates non-empty entries, non-zero amounts and per-currency sum = 0
  - inserts everything in the caller's tx
  - returns `ErrDuplicate` if `(kind, reference)` exists, which callers treat as "already done": that's idempotency
- **Balance** of an account = Σ entries. For liability, revenue and equity accounts, report it as `-Σ` so the displayed balance is positive.
- **CRD** is a separate unit for promotion credits. It is never mixed with GHS in one entry. Credits have no cash value.

### 5.2 Accounts

| Code | Type | Currency | Meaning |
|---|---|---|---|
| `paystack_clearing` | asset | GHS | Our money sitting in the Paystack balance |
| `escrow` | liability | GHS | Buyer money held for orders not yet completed or refunded |
| `seller_payable:<seller_id>` | liability | GHS | Released money owed to a seller |
| `payout_clearing` | liability | GHS | Transfers initiated, not yet confirmed |
| `platform_commission` | revenue | GHS | Our commission |
| `promotion_revenue` | revenue | GHS | Promotion package sales |
| `processing_fee_income` | revenue | GHS | Processing fees charged to buyers |
| `paystack_fees` | expense | GHS | Actual Paystack charge fees |
| `transfer_fees` | expense | GHS | Paystack transfer fees (absorbed) |
| `promo_credits:<user_id>` | liability | CRD | A user's promotion credit balance |
| `promo_credits_issued` | equity | CRD | Counterpart for issued and redeemed credits |

### 5.3 Postings (`kind` → entries). `f` = actual Paystack fee from the webhook.

1. **`checkout_paid`** (ref = payment reference). The checkout has charge `C`, base `B = Σ order bases`, processing fee `P = C − B`:
   - Dr `paystack_clearing` C − f
   - Dr `paystack_fees` f
   - Cr `escrow` B, split **one entry per order**, each with its `order_id` and amount = that order's base
   - Cr `processing_fee_income` P
2. **`escrow_release`** (ref = order_id). The order has base `b`, subtotal `s`, commission `k`:
   - Dr `escrow` b
   - Cr `seller_payable:<seller>` b − k
   - Cr `platform_commission` k
3. **`order_refund`** (ref = refund id, amount `r` ≤ remaining escrow of the order):
   - Dr `escrow` r
   - Cr `paystack_clearing` r
4. **`promotion_paid`** (ref = payment reference; price `p`, charge `C`, `P = C − p`):
   - GHS entries:
     - Dr `paystack_clearing` C − f
     - Dr `paystack_fees` f
     - Cr `promotion_revenue` p
     - Cr `processing_fee_income` P
   - CRD entries, in the same transaction:
     - Dr `promo_credits_issued` credits
     - Cr `promo_credits:<user>` credits
5. **`promotion_applied`** (ref = listing_promotion id): credits `c`
   - Dr `promo_credits:<user>` c
   - Cr `promo_credits_issued` c
   - The balance must stay ≥ 0. Check it inside the tx after locking (`SELECT … FOR UPDATE` on a per-user row, or `pg_advisory_xact_lock(hashtext('promo:'||user_id))`). Never go negative.
6. **`payout_initiated`** (ref = payout reference): amount `a`
   - Dr `seller_payable:<seller>` a
   - Cr `payout_clearing` a
7. **`payout_succeeded`** (ref = payout reference):
   - Dr `payout_clearing` a
   - Cr `paystack_clearing` a
   - plus, if the transfer fee `t` > 0:
     - Dr `transfer_fees` t
     - Cr `paystack_clearing` t
8. **`payout_failed`** (ref = payout reference; covers failed or reversed):
   - Dr `payout_clearing` a
   - Cr `seller_payable:<seller>` a (re-credit)

### 5.4 Invariants (a property test, plus a `ReconcileLedger` check in Phase 17)

- Σ of all entries per currency = 0.
- The escrow balance = Σ over orders with escrow_state ∈ {held, refund_pending, partially_refunded} of (base − refunded − released).
- `promo_credits:<user>` ≥ 0 for every user.
- `seller_payable:<seller>` ≥ 0.

## 6. Promotions

Tiers are seeded from the legacy data, converted to pesewas:

| tier | name | price (pesewas) | credits | duration_days | sort | featured | rank |
|---|---|---|---|---|---|---|---|
| `top` | Starter | 5500 | 55 | 7 | 1 | no | 1 |
| `vip` | Growth | 7500 | 75 | 14 | 2 | no | 2 |
| `diamond` | Premium | 10000 | 100 | 21 | 3 | yes | 3 |
| `enterprise` | Enterprise | 15000 | 150 | 30 | 4 | yes | 4 |

Feature bullet texts are marketing copy only; copy them from `~/work/farmgate/prisma/seed.ts` into a `features text[]` column.

- **Buying** a package: charge = gross-up of the price (§2.2). On `charge.success`, grant `credits` (CRD) with posting §5.3.4, idempotent on the payment reference.
- **Applying** a tier to a listing costs **that tier's `credits`**. Rules:
  - The listing must be the caller's own and `active`.
  - Credits are checked and deducted **inside** the tx under a lock (§5.3.5). Insufficient credits → 409 `insufficient_credits`.
  - **Same tier while active:** extend, so the new `ends_at = current ends_at + duration`.
  - **Higher tier while active:** replace, from now: `starts_at = now`, `ends_at = now + duration`. The remaining time of the old tier is forfeited, and the API response says so.
  - **Lower tier while a higher one is active:** 409 `promotion_downgrade`.
  - **No active promotion:** `starts_at = now`, `ends_at = now + duration`.
- **Ranking:** a listing is promoted iff it has a `listing_promotions` row with `starts_at ≤ now < ends_at`. Search orders by the active tier rank DESC, then the requested sort. **Always check `ends_at > now`.** The legacy app never did, so expired promotions ranked forever.
- `featured` = the active tier is diamond or enterprise. That's used by the homepage "featured" list.

## 7. Listings

- **Status:** `draft | active | sold | expired | archived | suspended`. `suspended` is set by admin moderation (Phase 20).

| From | To | Actor |
|---|---|---|
| draft | active | owner (publish) |
| active | sold / archived | owner |
| active | expired | system (`ExpireListings`) |
| expired / archived | active | owner (renew or publish) |
| any | suspended | admin |
| suspended | active / archived | admin |

- **Expiry:** `expires_at = published_at + 30 days`. Renewing sets `expires_at = now + 30d`. Public queries always filter `status='active' AND expires_at > now`.
- **Fields and limits:**
  - `title` 5–100 chars
  - `description` 20–5000
  - `price_pesewas` 1..1,000,000,000, **per unit**
  - `unit` from §9
  - `quantity_available` int ≥ 0 (whole units of `unit`)
  - `min_order_qty` int ≥ 1, default 1, ≤ quantity when quantity > 0
  - `is_negotiable` bool, default true
  - `item_state` from §8, validated against the category group
  - `region` (§10, validated)
  - `district` 2–80 chars (free text; suggestions from §10)
  - `area` ≤ 120 chars, optional
  - `delivery_options`: at least one of `pickup` / `seller_delivery`
  - `seller_delivery_fee_pesewas` ≥ 0, required if `seller_delivery` is offered
  - Courier comes later (Backlog).
- **Images:** up to **10** per listing, each ≤ **5 MB**, `image/jpeg|image/png|image/webp`. GIF is dropped. Sorted by `sort_order`.
- **Slug:** `slugify(title)`: lowercase, ASCII-fold, non-alphanumerics → `-`, collapse and trim dashes, max 80 chars. On collision append `-2`, `-3`, … up to 50 tries, then `-<6 random base36 chars>`. There's a unique index on `slug`.
- **Attributes:** values are validated against the category's attribute definitions (`category_attributes`, resolved through the **parent** category):
  - `select` must be one of `options`
  - `boolean` is true/false
  - `number` is a decimal string within ±1e9
  - `date` is `YYYY-MM-DD`
  - `text` is ≤ 200 chars
  - `required` attributes must be present
  - unknown keys → 400
- **Contact:** a listing's public detail **never** includes phone numbers. `POST /v1/listings/{id}/contact` (authenticated, `sensitive` rate limit) returns the seller's phone or WhatsApp **only if** the seller opted in (`seller_profiles.show_phone` / `show_whatsapp`), and increments `contact_count`. This keeps §5 of the plan: no phones on public endpoints.

## 8. Category groups, item states and units

The group is stored on **parent** categories as `categories.listing_group`; children inherit it.

| Group | Parent slugs | Allowed `item_state` | Allowed units |
|---|---|---|---|
| `equipment` | farm-machinery, storage-packaging, irrigation | brand_new, used, refurbished | COUNT |
| `quality` | seeds-seedlings, fresh-produce, processed-foods, fertilizers-chemicals, feeds-supplements | grade_a, grade_b, organic, premium, standard | WEIGHT + COUNT |
| `livestock` | livestock-poultry | young, adult, mature, breeding_stock | COUNT + heads |
| `land` | land-leasing | developed, partially_developed, undeveloped | AREA |
| `service` | farm-labour, veterinary | experienced, certified, trained | TIME |

Unit sets:
- WEIGHT = kg, grams, metric_ton, pounds, bags_50kg, bags_25kg
- VOLUME = liters, milliliters, gallons (reserved; not offered by any group)
- COUNT = pieces, units, crates, baskets, bundles, dozen, pack, box
- AREA = acres, hectares, square_meters
- TIME = hour, day, week, month

`GET /v1/categories/{slug}` returns the group's allowed item states and units, so clients never hard-code them.

## 9. Category seed

Twelve parents, in this order (sort_order 1–12), with their children in the order listed. Source: `~/work/farmgate/prisma/seed.ts`. The legacy icons are emoji: seed them into `icon` for now.

1. **Seeds & Seedlings** `seeds-seedlings` 🌱: Cereal Seeds, Vegetable Seeds, Fruit Seeds, Tree Crop Seedlings, Flowers & Ornamentals, Pasture Seeds, Seedlings & Cuttings
2. **Fertilizers & Chemicals** `fertilizers-chemicals` 🧪: NPK Fertilizers, Organic Fertilizers, Liquid Fertilizers, Pesticides, Herbicides, Fungicides, Growth Regulators
3. **Farm Machinery** `farm-machinery` 🚜: Tractors, Tillers & Cultivators, Harvesters, Sprayers, Irrigation Equipment, Processing Machines, Tools & Implements
4. **Livestock & Poultry** `livestock-poultry` 🐄: Cattle, Sheep & Goats, Pigs, Chickens, Turkeys, Rabbits, Grasscutters, Exotic Animals
5. **Feeds & Supplements** `feeds-supplements` 🌾: Poultry Feed, Livestock Feed, Fish Feed, Pet Food, Feed Supplements, Vitamins & Minerals, Feed Ingredients
6. **Fresh Produce** `fresh-produce` 🥬: Vegetables, Fruits, Root Crops, Grains & Cereals, Legumes, Herbs & Spices, Honey & Bee Products
7. **Processed Foods** `processed-foods` 🥫: Dried & Smoked Fish, Cooking Oils, Flours & Powders, Packaged Foods, Beverages, Snacks, Preserves & Sauces
8. **Farm Labour** `farm-labour` 👷: Field Workers, Harvesting Services, Planting Services, Spraying Services, Consulting, Agribusiness Training
9. **Land & Leasing** `land-leasing` 🌳: Farmland for Rent, Farmland for Sale, Warehouse Space, Cold Storage, Office Space, Greenhouses
10. **Storage & Packaging** `storage-packaging` 📦: Sacks & Bags, Crates & Baskets, Packaging Materials, Storage Containers, Labels & Stickers
11. **Veterinary Services** `veterinary` 💉: Vaccines, Medications, Veterinary Services, Artificial Insemination, Animal Health Products
12. **Irrigation Equipment** `irrigation` 💧: no children

**Child slug** = `<parent-slug>-<slugify(child name)>`, using the §7 slugify, which **drops** `&`. For example `seeds-seedlings-flowers-ornamentals`. The legacy `seed.ts` produced `flowers-&-ornamentals`: don't copy that.

**Attributes** go on parents, `required=false` unless marked:
- seeds-seedlings:
  - Germination Rate: select [95%+, 90-95%, 85-90%, Below 85%]
  - Treatment: select [Treated, Untreated, Organic]
  - Certified: boolean
- fertilizers-chemicals:
  - NPK Ratio: text
  - Organic: boolean
  - Application Method: select [Foliar, Soil, Fertigation, Broadcast]
- farm-machinery:
  - Horsepower: text
  - Fuel Type: select [Diesel, Petrol, Electric, Hybrid]
  - Year of Manufacture: number
- livestock-poultry:
  - Breed: text
  - Vaccinated: boolean
  - Weight (kg): number
- feeds-supplements:
  - Protein Content: text
  - Feed Type: select [Starter, Grower, Finisher, Layer, Breeder]
- fresh-produce:
  - Harvest Date: date
  - Organic: boolean
  - Shelf Life (days): number
- processed-foods:
  - Expiry Date: date
  - Storage Type: select [Room Temperature, Refrigerated, Frozen]
  - Packaging: text
- farm-labour:
  - Experience (years): number
  - Certified: boolean
  - Availability: select [Full-time, Part-time, Contract, Seasonal]
- land-leasing:
  - Land Size (acres): number, **required**
  - Soil Type: select [Loamy, Sandy, Clay, Silt, Mixed]
  - Water Source: select [River, Borehole, Rain-fed, Irrigation, None]
- storage-packaging:
  - Material: select [Plastic, Jute, Polypropylene, Paper, Metal]
  - Reusable: boolean
- veterinary:
  - Species: select [Cattle, Poultry, Pigs, Sheep, Goats, Multi-species]
  - Prescription Required: boolean
- irrigation:
  - Irrigation Type: select [Drip, Sprinkler, Flood, Center Pivot, Micro-sprinkler]
  - Powered By: select [Electric, Solar, Manual, Engine]

Each attribute gets a stable machine `key` = snake_case of its name without units, e.g. `germination_rate`, `weight_kg`, `land_size_acres`. API values are keyed by `key`; `label` holds the display name.

## 10. Ghana locations

- **Regions**, validated: exactly these 16. Store as text; validate against this list in Go (`internal/geo`).
  - Ahafo, Ashanti, Bono, Bono East, Central, Eastern, Greater Accra, North East, Northern, Oti, Savannah, Upper East, Upper West, Volta, Western, Western North
- **Districts** are free text (2–80 chars). The legacy list is **partial**, and some districts appear under two regions.
  - Port `~/work/farmgate/lib/data/ghana-locations.ts` into `internal/geo/districts.go` as suggestions only.
  - Serve it from `GET /v1/locations` (public, cacheable).
  - Don't reject a district that's missing from the list.
- **Phone numbers:** E.164 `+233` followed by 9 digits (`^\+233[0-9]{9}$`). Normalise user input: strip spaces and dashes, then turn a leading `0` or `233` into `+233`. Firebase phone sign-in already yields E.164.

## 11. Other domain rules

- **Seller profile** (Phase 8):
  - `business_name` 2–120 chars, `region` (§10), `district`, `bio` ≤ 1000
  - `show_phone` / `show_whatsapp` (default false), `whatsapp_e164` (optional, validated)
  - `verification_status`: `unverified | pending | verified | rejected`
  - `id_type ∈ ghana_card | passport | voters_id | drivers_license`
  - `id_number` is stored **encrypted** (AES-256-GCM, `DATA_ENCRYPTION_KEY`) and never returned, except masked (last 4) to the owner and admins.
- **Reviews** (Phase 20a):
  - `rating` int 1–5, `comment` ≤ 1000
  - A review is for a listing within a **completed** order the reviewer bought. Uniqueness: `UNIQUE(order_id, listing_id, reviewer_id)`.
  - Published immediately; admins can hide it (`hidden_at`).
  - The seller's rating = average and count of visible reviews.
- **Favorites** (Phase 20a):
  - `UNIQUE(user_id, listing_id)`; add and remove are idempotent (PUT/DELETE)
  - `listings.favorite_count` is maintained in the same tx
- **Reports** (Phase 20b):
  - The target is exactly one of a listing or a user
  - `reason ∈ spam | fraud | prohibited_item | offensive | wrong_category | other`
  - `description` ≤ 1000 (required when the reason is `other`)
  - `status ∈ open | actioned | dismissed`
  - One open report per (reporter, target)
  - Admins resolve with an action: `none | suspend_listing | hide_review`
- **Supply requests** (Phase 20b), porting the legacy flow and adding the transitions it never had:
  - `request_number` = `SUP-YYYYMMDD-XXXXXX` (6 random uppercase base36), retried on collision
  - Items: 1–20 per request, each `{categorySlug (parent), productName 2–100, quantity int ≥ 1, unit (§8 sets)}`
  - `deliveryName`, `deliveryPhone` (§10 phone), `deliveryAddress` ≤ 300, `expectedDate` ≥ today, `notes` ≤ 1000
  - Status: `pending → confirmed → processing → delivered`, and any non-terminal status → `cancelled`
  - The owner can cancel while `pending`. Admins drive all other transitions.
  - Each transition writes a `supply_request_events` row and enqueues an SMS notification (Phase 16 `notify`).
- **Messaging** (Phase 19):
  - One conversation per (listing, buyer); the seller can't start one with themselves
  - Message `body` 1–2000 chars
  - Participants only (non-participants get 403)
  - A `messaging` rate-limit policy of 30/min, burst 10
  - Unread counts come from `last_read_at` per participant

## 12. Legacy bugs deliberately not ported

| Legacy behaviour | Rewrite rule |
|---|---|
| Money as `Decimal(10,2)` cedis | Integer pesewas everywhere (§1) |
| Webhook check-then-act race; no amount or currency check | `webhook_events` unique + a state-guarded update in one tx; amount and currency must match or be rejected and alerted (Phase 13a) |
| Promotions ranked forever (`promotion_expires` never checked) | Ranking checks `ends_at > now` (§6) |
| Credits checked outside the tx, able to go negative | Lock + check inside the tx; balance ≥ 0 invariant (§5.3.5) |
| Re-applying a promotion silently lost the remaining time | Extend / upgrade / reject-downgrade rules (§6) |
| No listing expiry job; status never flipped | `ExpireListings` periodic job (Phase 11) |
| Supply requests stuck in `pending` (no transitions) | Admin transitions + owner cancel (§11) |
| Child slugs with `&` | Proper slugify (§9) |
| `show_phone` written to a missing column (500) | Explicit seller-profile fields (§11) |
| Counters never updated (`favorite_count`, `contact_count`) | Maintained in the same tx (§7, §11) |
| Owner-scoped update returned 500 for non-owners | 403 `forbidden` from a service-layer ownership check |
