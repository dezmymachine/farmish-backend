# Phase 18a: Seller payout accounts

**Depends on:** 7 (step-up), 8 (crypto, seller profiles), 17b · **Size:** medium

## Goal

Sellers register a mobile-money or bank (GhIPSS) payout account, protected by **step-up re-auth**. The account is resolved via Paystack, name-checked, encrypted at rest, turned into a Paystack transfer recipient, and subject to a 48h cooldown on change (DOMAIN §3). The number is never returned unmasked.

## Schema: `migrations/000015_payout_accounts.up.sql`

```sql
CREATE TABLE seller_payout_accounts (
  seller_id           uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  type                text NOT NULL CHECK (type IN ('mobile_money','ghipss')),
  bank_code           text NOT NULL,             -- Paystack bank/telco code (e.g. MTN, VOD, ATL for MoMo)
  bank_name           text NOT NULL,
  account_number_enc  text NOT NULL,             -- crypto.Encrypt
  account_number_mask text NOT NULL,             -- e.g. '******4567'
  account_name        text NOT NULL,             -- as resolved by Paystack
  recipient_code      text NOT NULL,             -- Paystack RCP_...
  status              text NOT NULL CHECK (status IN ('verified','needs_review')),
  verified_at         timestamptz,
  cooldown_until      timestamptz,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now()
);
```

History: every change writes an audit event with the old and new **masked** numbers.

## API (tag `payouts`)

| Method & path | Auth | Request | Responses |
|---|---|---|---|
| `GET /v1/payouts/banks` | bearer, `Cache-Control: private, max-age=3600` | `?type=mobile_money\|ghipss` | 200 `{items: [{code, name}]}` (from Paystack `ListBanks`, cached in memory for 1h) |
| `GET /v1/seller/payout-account` | bearer (seller profile required) | none | 200 `PayoutAccount`, 404 |
| `PUT /v1/seller/payout-account` | bearer, **`x-farmish-step-up: true`**, `sensitive` | `{type, bankCode, accountNumber}` | 200 `PayoutAccount`, 400, 401 `reauth_required`, 422 `account_unresolvable` |

- **`PayoutAccount`:** `{type, bankCode, bankName, accountNumberMasked, accountName, status, cooldownUntil?}`. **Never** the full number.
- `accountNumber`: 10–20 digits; for MoMo, normalise a `0`/`233`-prefixed number to Paystack's expected format. **Verify Paystack's MoMo format**: local `0XXXXXXXXX` is expected for GH MoMo resolution.

## Rules (`payouts.Service.SetAccount`)

1. Validate the bank code against `ListBanks(type)`.
2. Paystack `ResolveAccount`. An error → 422 `account_unresolvable`.
3. **Name check:** normalise both names (uppercase, strip punctuation, split into tokens). **Verified** if the resolved name shares ≥ 1 token of ≥ 3 characters with the seller's `business_name` **or** `users.display_name`; otherwise `needs_review`. Payouts only go to `verified` accounts, and an admin can approve `needs_review` via `POST /v1/admin/payout-accounts/{sellerId}/approve`. That's admin-only and audited: add it here.
4. `CreateTransferRecipient` → `recipient_code`.
5. Upsert. On **change** (a row already existed), `cooldown_until = now + 48h`; on the first setup, no cooldown.
6. Audit `payout_account.set`, and an SMS security alert to the seller's phone: "Your Farmish payout account was changed. If this wasn't you, contact support."

Steps 2 and 4 are external calls outside the tx. Only the final upsert, audit and notify enqueue are in one tx.

## Tests

| Test | Proves |
|---|---|
| `TestSetAccount_StepUpRequired` | A token older than 5 minutes (fake clock) → 401 `reauth_required`; fresh → 200 |
| `TestSetAccount_ResolvesEncryptsMasks` | The DB column is ciphertext; the response and GET only show the mask; the name comes from the fake Paystack |
| `TestSetAccount_NameCheck` | Matching token → verified; no match → needs_review; admin approve → verified + audit |
| `TestSetAccount_ChangeStartsCooldown` | First setup: no cooldown; change → `cooldown_until = now+48h`; SMS alert enqueued |
| `TestSetAccount_Unresolvable422` | |
| `TestPayoutAccount_NeverUnmasked` | Grep every response body in these tests for the full number: never present |
| `TestBanks_CachedList` | Two calls → one Paystack call |

## Pitfalls

- Paystack must have **Transfers enabled** and **transfer OTP disabled** on the account (plan §11) before 18b can work live.
- The step-up check happens in middleware. Don't duplicate it in the service, but do test it at the endpoint level.
