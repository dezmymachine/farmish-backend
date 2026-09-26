# Phase 20b: Reports & supply requests

**Depends on:** 16 (notify), 9 (categories) · **Size:** medium

## Goal

- User reports on listings and users, with an admin moderation queue that can suspend listings.
- **Supply requests**, ported from the legacy app, with the transitions it never had (DOMAIN §11).

## Schema: `migrations/000019_reports_supply.up.sql`

```sql
CREATE TABLE reports (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  reporter_id      uuid NOT NULL REFERENCES users(id),
  listing_id       uuid REFERENCES listings(id) ON DELETE CASCADE,
  reported_user_id uuid REFERENCES users(id),
  reason           text NOT NULL CHECK (reason IN ('spam','fraud','prohibited_item','offensive','wrong_category','other')),
  description      text CHECK (char_length(description) <= 1000),
  status           text NOT NULL DEFAULT 'open' CHECK (status IN ('open','actioned','dismissed')),
  action           text CHECK (action IN ('none','suspend_listing','hide_review')),
  resolution_note  text CHECK (char_length(resolution_note) <= 1000),
  resolved_by      uuid REFERENCES users(id),
  resolved_at      timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now(),
  CHECK ((listing_id IS NULL) <> (reported_user_id IS NULL)),
  CHECK (reason <> 'other' OR description IS NOT NULL)
);
CREATE UNIQUE INDEX reports_one_open_per_target ON reports
  (reporter_id, coalesce(listing_id, reported_user_id)) WHERE status = 'open';
CREATE INDEX reports_status_idx ON reports (status, created_at);

CREATE TABLE supply_requests (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  request_number   text NOT NULL UNIQUE CHECK (request_number ~ '^SUP-[0-9]{8}-[0-9A-Z]{6}$'),
  user_id          uuid NOT NULL REFERENCES users(id),
  delivery_name    text CHECK (char_length(delivery_name) <= 120),
  delivery_phone   text CHECK (delivery_phone ~ '^\+233[0-9]{9}$'),
  delivery_address text CHECK (char_length(delivery_address) <= 300),
  expected_date    date,
  notes            text CHECK (char_length(notes) <= 1000),
  status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','confirmed','processing','delivered','cancelled')),
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX supply_requests_user_idx ON supply_requests (user_id, created_at DESC);
CREATE INDEX supply_requests_status_idx ON supply_requests (status, created_at);

CREATE TABLE supply_request_items (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  supply_request_id uuid NOT NULL REFERENCES supply_requests(id) ON DELETE CASCADE,
  category_id       uuid NOT NULL REFERENCES categories(id),   -- a parent category
  product_name      text NOT NULL CHECK (char_length(product_name) BETWEEN 2 AND 100),
  quantity          int  NOT NULL CHECK (quantity >= 1),
  unit              text NOT NULL,
  sort_order        int  NOT NULL
);
CREATE TABLE supply_request_events (
  id                bigserial PRIMARY KEY,
  supply_request_id uuid NOT NULL REFERENCES supply_requests(id),
  from_status       text,
  to_status         text NOT NULL,
  actor_id          uuid,
  note              text CHECK (char_length(note) <= 500),
  created_at        timestamptz NOT NULL DEFAULT now()
);
```

## API

**Reports**

| Method & path | Auth | Request | Responses |
|---|---|---|---|
| `POST /v1/reports` | bearer, `sensitive` | `{listingId? \| userId?, reason, description?}` | 201; 400 (both or neither target); 409 `already_reported` |
| `GET /v1/admin/reports` | admin | `?status&page&limit` | 200 list, with a target summary |
| `POST /v1/admin/reports/{id}/resolve` | admin | `{status: actioned\|dismissed, action: none\|suspend_listing\|hide_review, note}` | 200; `suspend_listing` sets the listing `suspended` (Phase 11 rules) in the same tx; audited |

**Supply requests**

| Method & path | Auth | Request | Responses |
|---|---|---|---|
| `POST /v1/supply-requests` | bearer, `sensitive` | `{items: [{categorySlug, productName, quantity, unit}] (1–20), deliveryName?, deliveryPhone?, deliveryAddress?, expectedDate?, notes?}` | 201 `SupplyRequest` |
| `GET /v1/supply-requests` | bearer | `?status&page&limit` | 200 own list (paginated, unlike the legacy app) |
| `GET /v1/supply-requests/{id}` | bearer | none | 200 own (with events); others' → 404 |
| `POST /v1/supply-requests/{id}/cancel` | bearer | `{reason?}` | 200 (owner, `pending` only); 409 otherwise |
| `GET /v1/admin/supply-requests` | admin | `?status&page&limit` | 200 all |
| `POST /v1/admin/supply-requests/{id}/status` | admin | `{status, note?}` | 200; 409 illegal transition; audited |

## Rules

- **Supply request status machine:**
  - `pending→confirmed→processing→delivered`
  - `pending|confirmed|processing → cancelled`
  - admin: any legal transition; owner: only `pending → cancelled`
- Every transition writes a `supply_request_events` row and enqueues `notify.sms supply_request_<status>` to the requester.
- **Items:** `categorySlug` must be a **parent** category. `unit` must be in the union of the §8 unit sets (WEIGHT, VOLUME, COUNT, AREA, TIME, plus `heads`). `deliveryPhone` is normalised through `geo.NormalizeGhanaPhone`. It defaults to the user's phone and display name when omitted, as in the legacy app. `expectedDate` ≥ today (Africa/Accra).
- **`request_number`:** `SUP-` + today's date `YYYYMMDD` (Africa/Accra) + `-` + 6 random uppercase base36 characters. Retry up to 5 times on a unique violation.

## Tests

| Test | Proves |
|---|---|
| `TestReport_OneOpenPerTarget` | Second open report → 409; after dismissal a new one is allowed |
| `TestReport_ExactlyOneTarget` | Both or neither → 400 |
| `TestReport_ResolveSuspendsListing` | Listing `suspended`, disappears from search, owner can't edit (Phase 11 test reused), audit row |
| `TestSupplyRequest_CreateValidation` | Table: 0 items, 21 items, child category slug, bad unit, past date, bad phone |
| `TestSupplyRequest_NumberFormatAndRetry` | Regex; a forced collision retries |
| `TestSupplyRequest_StatusMachine` | Full table: admin legal/illegal; owner cancel only from pending; others' → 404 |
| `TestSupplyRequest_NotifiesOnTransition` | |
| `TestUniqueness_Constraints` | The plan's Done-when: review/favorite/report uniqueness (20a + 20b) are all tested |
