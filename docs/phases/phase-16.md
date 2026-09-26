# Phase 16: Fulfilment state machine & notifications

**Depends on:** 15b · **Size:** large

## Goal

- The complete order state machine from DOMAIN §4, enforced in **one** function.
- Seller and buyer action endpoints.
- The timer jobs (auto-cancel after 48h unaccepted, auto-complete 3 days after delivery).
- A `disputes` table (resolution comes in 17b).
- The `notify` package with the mNotify SMS client. Every transition notifies the other party.

## `internal/orders`

```go
type Actor struct{ Type string; ID *uuid.UUID } // buyer|seller|admin|system
var ErrInvalidTransition = errors.New("invalid order transition")
var ErrForbidden = errors.New("actor may not perform this transition")

// Transition locks the order, validates (from,to,actor) against the table,
// updates status/timestamps, writes order_events, and returns the side effects
// the caller must perform in the same tx.
func (s *Service) Transition(ctx context.Context, tx pgx.Tx, orderID uuid.UUID, to Status, actor Actor, note string) (Order, []SideEffect, error)
```

- **Transition table:** a Go map literal `map[Status]map[Status][]ActorType`, mirroring DOMAIN §4 **exactly**. Also check the actor's relation to the order: the seller actor must be `order.seller_id`, the buyer actor must be `order.buyer_id`.
- **Side effects:** a small enum the caller executes in the same tx: `RestoreStock`, `EnqueueRefund`, `EnqueueRelease`, `SetAutoComplete`, `Notify(template, recipient)`.
  - `EnqueueRefund` / `EnqueueRelease` enqueue **17a** jobs. Until 17a lands, enqueue job kinds whose workers only log, so 17a can replace the worker bodies. **Say so in the review packet.**
- **Timestamps:** `accepted_at`, `shipped_at`, `delivered_at`, `completed_at`, `cancelled_at`. On `delivered`: `auto_complete_at = delivered_at + ESCROW_AUTO_COMPLETE_DAYS`.

## Schema: `migrations/000013_disputes.up.sql`

```sql
CREATE TABLE disputes (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  order_id        uuid NOT NULL UNIQUE REFERENCES orders(id),
  opened_by       uuid NOT NULL REFERENCES users(id),
  reason          text NOT NULL CHECK (reason IN ('not_received','not_as_described','damaged','wrong_item','other')),
  description     text NOT NULL CHECK (char_length(description) BETWEEN 10 AND 2000),
  status          text NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved')),
  outcome         text CHECK (outcome IN ('refund_buyer','release_seller','partial')),
  refund_pesewas  bigint CHECK (refund_pesewas >= 0),
  resolution_note text CHECK (char_length(resolution_note) <= 2000),
  resolved_by     uuid REFERENCES users(id),
  resolved_at     timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
```

## API (tag `orders`)

All endpoints are bearer. The actor role comes from the URL namespace **and** is checked against the order.

| Method & path | Actor | Body | Transition |
|---|---|---|---|
| `POST /v1/seller/orders/{id}/accept` | seller | none | paid → accepted |
| `POST /v1/seller/orders/{id}/reject` | seller | `{reason: 1–500}` | paid\|accepted → cancelled |
| `POST /v1/seller/orders/{id}/ship` | seller | `{trackingRef?: ≤120}` | accepted → shipped |
| `POST /v1/seller/orders/{id}/mark-delivered` | seller | none | shipped → delivered |
| `POST /v1/orders/{id}/cancel` | buyer | `{reason?}` | paid → cancelled |
| `POST /v1/orders/{id}/confirm-receipt` | buyer | none | shipped\|delivered → completed |
| `POST /v1/orders/{id}/dispute` | buyer | `{reason enum, description 10–2000}` | shipped\|delivered → disputed (+ disputes row) |

- Responses: 200 `OrderDetail`; 403 (wrong party); 404 (not a party); 409 `invalid_transition` (includes `details` with `from` and `to`).
- The dispute endpoint has `x-farmish-rate-limit: sensitive`.

## Jobs (periodic, hourly, both idempotent; use `FOR UPDATE SKIP LOCKED` batches of 100)

- `orders.auto_cancel_unaccepted`: `status='paid' AND paid_at <= now - SELLER_ACCEPT_TIMEOUT_HOURS` (`paid_at` is set by the 15b paid transition) → cancelled (actor system, note `seller_timeout`), with its side effects.
- `orders.auto_complete`: `status='delivered' AND auto_complete_at <= now` and no open dispute → completed, which enqueues the release.

## `internal/notify`

```go
type SMS interface{ Send(ctx context.Context, toE164, message string) error }
```

- **`MNotify` implementation** (BMS API):
  - `POST https://api.mnotify.com/api/sms/quick?key=<MNOTIFY_API_KEY>`
  - JSON body `{recipient: ["0241234567"], sender: MNOTIFY_SENDER, message, is_schedule: false, schedule_date: ""}`
  - mNotify expects local format (`0XXXXXXXXX`): convert from E.164
  - success = HTTP 200 with `"status":"success"`, otherwise an error with the body's message
  - timeout 10s
  - **Verify these details against mNotify's current docs** and record the verified endpoint in the ADR.
- **`LogOnly` implementation** for dev and tests when `NOTIFY_SMS_ENABLED=false`, the default in dev. It logs a *masked* phone and the template name.
- **Job** `notify.sms {UserID, Template, Params map[string]string}`:
  - resolve the user's phone at **send** time; no phone → skip, and log at info
  - render the template (Go `text/template`, stored in `internal/notify/templates.go`)
  - keep messages ≤ 160 characters, with no URLs except the site domain
- **Templates:** `order_paid_seller`, `order_accepted_buyer`, `order_rejected_buyer`, `order_shipped_buyer`, `order_delivered_buyer` ("confirm or dispute within 3 days"), `order_completed_seller`, `order_cancelled_buyer`, `order_cancelled_seller`, `order_disputed_seller`.
- **Wiring:** every transition's `Notify` side effect enqueues `notify.sms` in the same tx. Implement the Phase 15b "notify seller" call site.

## Config

- `SELLER_ACCEPT_TIMEOUT_HOURS` (48)
- `ESCROW_AUTO_COMPLETE_DAYS` (3)
- `NOTIFY_SMS_ENABLED` (false in development)
- `MNOTIFY_API_KEY`, `MNOTIFY_SENDER`: required when `NOTIFY_SMS_ENABLED=true`

## Tests

| Test | Proves |
|---|---|
| `TestTransition_FullTable` | **Every** (from, to, actorType) combination over all statuses × actor types: allowed per DOMAIN §4 → success, all others → `ErrInvalidTransition`/`ErrForbidden`. Generated from the same table used by the code **and** a hand-written copy of DOMAIN §4 (so a typo in one is caught) |
| `TestEndpoints_IllegalTransition409` | e.g. ship a `paid` order → 409 with details |
| `TestEndpoints_ActorEnforcement` | The buyer calls the seller accept on their own purchase → 403; the seller calls confirm-receipt → 403; a stranger → 404 |
| `TestAutoCancel_FakeClock` | Paid at T; at T+47h59m nothing happens; at T+48h → cancelled, stock restored, refund enqueued, `order_events` actor=system |
| `TestAutoComplete_FakeClock` | Delivered at T; at T+3d → completed + release enqueued; an open dispute blocks it |
| `TestDispute_CreatesRowAndStopsTimer` | |
| `TestNotify_EveryTransitionEnqueuesSMS` | Each transition enqueues the expected template for the expected recipient (inspect the River jobs table) |
| `TestMNotify_Client` | httptest: URL, key param, body shape, local phone format, error parsing |
| `TestNotifyJob_SkipsUserWithoutPhone` | |

## Manual QA

Run a full happy path with the fake clock (a test), plus a manual run: accept → ship → mark-delivered → confirm-receipt, with `NOTIFY_SMS_ENABLED=false`. The logs show the four masked SMS entries. Optionally send **one** real SMS to your own number with real mNotify credentials.

## Pitfalls

- One transition function. **No** status updates anywhere else (grep for `UPDATE orders SET status` in the review).
- Timer jobs must re-check the state under the row lock. The order may have moved since selection.
