# Phase 8: Profiles & seller onboarding

**Depends on:** 4 (and uses Phase 7's `internal/geo` if done; if not, create `internal/geo/regions.go` here) · **Size:** medium

## Goal

Users become sellers by creating a seller profile. Admins verify sellers, which sets `users.seller_verified` and the Firebase `seller_verified` claim, with an audit trail. This phase also creates cross-cutting building blocks used by every later phase: `database.InTx`, `internal/validation`, `internal/crypto` and `internal/audit`.

## Scope

- **Building blocks** (see ENGINEERING_GUIDE §2–3):
  - `internal/database/tx.go`: `InTx(ctx, pool, fn)`
  - `internal/validation`: `Error{Fields}`, `Add`, `OrNil`, plus a handler helper mapping it to a 400 `validation_failed` with details
  - `internal/crypto`: AES-256-GCM `Encrypt(plaintext) (string, error)` / `Decrypt(string) ([]byte, error)`
    - The key is `DATA_ENCRYPTION_KEY` (base64 of 32 bytes).
    - Output format: `v1:` + base64(nonce‖ciphertext).
    - The `v1` prefix allows key rotation later.
  - `internal/audit`: `Record(ctx, tx, Event{ActorID *uuid.UUID, Action, TargetType, TargetID string, Metadata map[string]any}) error`
- **Seller profiles:** the migration, service and endpoints below.
- **Admin verification:** the endpoint, the audit event, and the DB flag plus Firebase claim.

**Out of scope:**
- ID document **images** (they need Phase 10 media: Backlog)
- payout accounts (18a)
- ratings and listing counts on the public seller (added by Phases 11/20a; include the fields later, not now)

## Schema: `migrations/000004_seller_profiles_audit.up.sql`

```sql
CREATE TABLE seller_profiles (
  user_id            uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  business_name      text NOT NULL CHECK (char_length(business_name) BETWEEN 2 AND 120),
  region             text NOT NULL,              -- validated in Go against geo.Regions
  district           text NOT NULL CHECK (char_length(district) BETWEEN 2 AND 80),
  bio                text CHECK (char_length(bio) <= 1000),
  show_phone         boolean NOT NULL DEFAULT false,
  show_whatsapp      boolean NOT NULL DEFAULT false,
  whatsapp_e164      text CHECK (whatsapp_e164 ~ '^\+233[0-9]{9}$'),
  verification_status text NOT NULL DEFAULT 'unverified'
                     CHECK (verification_status IN ('unverified','pending','verified','rejected')),
  id_type            text CHECK (id_type IN ('ghana_card','passport','voters_id','drivers_license')),
  id_number_enc      text,                        -- crypto.Encrypt output; never selected by public queries
  id_number_last4    text CHECK (char_length(id_number_last4) = 4),
  submitted_at       timestamptz,
  reviewed_at        timestamptz,
  reviewed_by        uuid REFERENCES users(id),
  rejection_reason   text CHECK (char_length(rejection_reason) <= 500),
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  CHECK ((id_type IS NULL) = (id_number_enc IS NULL))
);
CREATE INDEX seller_profiles_status_idx ON seller_profiles (verification_status);
CREATE TRIGGER set_updated_at BEFORE UPDATE ON seller_profiles FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE audit_events (
  id          bigserial PRIMARY KEY,
  actor_id    uuid REFERENCES users(id),        -- NULL = system
  action      text NOT NULL,                    -- e.g. 'seller.verify', 'seller.reject'
  target_type text NOT NULL,
  target_id   text NOT NULL,
  metadata    jsonb NOT NULL DEFAULT '{}',
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_events_target_idx ON audit_events (target_type, target_id, created_at);
-- Append-only: block UPDATE/DELETE.
CREATE FUNCTION forbid_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION '% on % is not allowed (append-only)', TG_OP, TG_TABLE_NAME; END; $$;
CREATE TRIGGER audit_events_append_only BEFORE UPDATE OR DELETE ON audit_events
  FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
```

The down migration drops both tables and `forbid_mutation()`. Phase 13b's ledger reuses `forbid_mutation()`, so do **not** drop it there.

## Queries: `db/queries/sellers.sql`, `db/queries/audit.sql`

- `GetSellerProfile(user_id)`
- `UpsertSellerProfile(...)`: `INSERT … ON CONFLICT (user_id) DO UPDATE SET …`
- `SetSellerIdentity(user_id, id_type, id_number_enc, id_number_last4)`: also sets status `pending` and `submitted_at = now()`
- `SetSellerVerification(user_id, status, reviewed_by, rejection_reason)`
- `ListSellerProfilesByStatus(status, limit, offset)` + `CountSellerProfilesByStatus`
- `GetPublicSeller(user_id)`: selects **only** the public columns
- `SetUserSellerVerified(user_id, bool)` (in `users.sql`)
- `InsertAuditEvent(...)`

## API (add to `api/openapi.yaml`, tag `sellers`)

| Method & path | Auth | Request | Responses |
|---|---|---|---|
| `GET /v1/me/seller-profile` | bearer | none | 200 `SellerProfile`, 404 (none yet) |
| `PUT /v1/me/seller-profile` | bearer, `x-farmish-rate-limit: sensitive` | `SellerProfileInput` | 200 `SellerProfile`, 400 |
| `GET /v1/sellers/{userId}` | `security: []` | none | 200 `PublicSeller`, 404 |
| `GET /v1/admin/sellers` | admin | `?status=pending&page&limit` | 200 `{items: SellerProfileAdmin[], meta: PageMeta}` |
| `POST /v1/admin/sellers/{userId}/verification` | admin | `{decision: approve\|reject, reason?}` | 200 `SellerProfileAdmin`, 400, 404, 409 (not `pending`) |

- **`SellerProfileInput`** (`additionalProperties: false`):
  - `businessName` (2–120)
  - `region` (enum of the 16 regions)
  - `district` (2–80)
  - `bio?` (≤1000)
  - `showPhone?`, `showWhatsapp?` (bool)
  - `whatsapp?` (string, normalised server-side with `geo.NormalizeGhanaPhone`)
  - `idType?` (enum) and `idNumber?` (4–30 chars, `^[A-Za-z0-9-]+$`): **both or neither**
- **`SellerProfile`** (the owner's view): all input fields, plus `verificationStatus`, `idType`, `idNumberLast4`, `submittedAt`, `reviewedAt`, `rejectionReason`. **Never** `idNumber`.
- **`PublicSeller`:** `userId`, `businessName`, `region`, `district`, `bio`, `verified` (bool = status `verified`), `memberSince` (users.created_at). **Nothing else.**
- **`SellerProfileAdmin`:** `SellerProfile` plus `userId`, `displayName`, `email`, `phone`, for the admin review UI.

## Rules

- `region` must be `geo.IsRegion`. The spec enum enforces it at the edge too.
- **Submitting or changing** `idType` and `idNumber`:
  - encrypt the number and store `last4`
  - set status `pending`
  - if the seller was verified, **revoke** verification: `users.seller_verified = false`, and set the claim to false
  - all in one tx (`InTx`), plus the audit event `seller.identity_submitted`
- **Editing** other fields never changes the verification status.
- **Admin approve:** only from `pending` (else 409 `invalid_transition`). In **one tx**:
  - status `verified`, `reviewed_at`, `reviewed_by`
  - `users.seller_verified = true`
  - audit `seller.verify` with metadata `{previous_status}`

  **After** the commit, set the Firebase claim `seller_verified: true`. That's an external call, so it's outside the tx: log and return 200 even if the claim update fails, because the DB is authoritative (same pattern as `users.SetRole`). Generalise the claims setter: add `auth.Firebase.SetClaim(ctx, uid, key string, value any)` that merges, keeping `SetRoleClaim` working.
- **Admin reject:** requires `reason` (1–500). Status `rejected`, audit `seller.reject`, `seller_verified = false` (DB + claim).
- **Public seller:** 404 if the profile doesn't exist.

## Config

`DATA_ENCRYPTION_KEY`: **required**, base64 that decodes to exactly 32 bytes.
- `.env.example` ships a clearly labelled **dev-only** key.
- Config **refuses** that exact dev key when `APP_ENV` is staging or production (same pattern as the Turnstile test secret).
- Document it in §7.

## Tests

| Test | Proves |
|---|---|
| `TestCrypto_RoundTrip`, `TestCrypto_TamperFails`, `TestCrypto_WrongKeyFails`, `TestCrypto_UniqueNonces` | AES-GCM correctness |
| `TestInTx_CommitsAndRollsBack` | The helper commits on nil and rolls back on error or panic (re-panics) |
| `TestAudit_AppendOnly` | UPDATE/DELETE on `audit_events` raise |
| `TestSellerProfile_UpsertAndGet` | Create, then edit; status unchanged by edits |
| `TestSellerProfile_IdentitySubmissionSetsPending` | Encrypted at rest (read the raw column: no plaintext), last4 correct |
| `TestSellerProfile_IdentityChangeRevokesVerification` | verified → change ID → pending + `users.seller_verified=false` |
| `TestSellerProfile_Validation` | Region not in the list, idType without idNumber, bad WhatsApp → 400 with field details |
| `TestPublicSeller_SafeProjection` | Response JSON has **no** keys among `idNumber, idNumberLast4, idType, email, phone, whatsapp, firebaseUid, role`; `assertContract` |
| `TestAdminVerify_ApproveAndClaim` | Emulator user + admin: approve → DB verified, `users.seller_verified`, audit row, Firebase claim `seller_verified=true` |
| `TestAdminVerify_RejectRequiresReason` / `_NotPendingIs409` / `_NonAdminIs403` | Guards |
| `TestConfig_DataEncryptionKey` | Missing/short key errors; dev key refused in production |

## Manual QA

1. `make run` with emulator config. Create a user token, then `PUT /v1/me/seller-profile` with an ID. `GET` shows `idNumberLast4` only.
2. `make grant-admin EMAIL=…` for a second user. `GET /v1/admin/sellers?status=pending` lists the first, then approve it. `GET /v1/sellers/{id}` shows `verified: true` and nothing private.
3. `psql`: `select id_number_enc from seller_profiles` shows a `v1:…` ciphertext.

## Pitfalls

- Don't put `id_number_enc` in any query used by public or listing endpoints. Keep a narrow `GetPublicSeller`.
- A nonce **must** be random per encryption (`crypto/rand`, 12 bytes).
- The Firebase claim update is outside the tx and best-effort. The DB flag is the truth.
