# Phase 18b: Payout execution

**Depends on:** 18a · **Size:** large

## Goal

A daily batched payout per seller from `seller_payable`, sent as Paystack Transfers.
- Webhooks finalise each payout.
- A failure or reversal re-credits the payable.
- A daily reconciliation catches stuck transfers.
- Sellers see their balance and payout history; admins can retry.

Ledger rules are DOMAIN §5.3.6–8.

## Schema: `migrations/000016_payouts.up.sql`

```sql
CREATE TABLE payouts (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  seller_id      uuid NOT NULL REFERENCES users(id),
  amount_pesewas bigint NOT NULL CHECK (amount_pesewas > 0),
  reference      text NOT NULL UNIQUE,              -- 'PO-' || 20 base32 chars
  recipient_code text NOT NULL,
  transfer_code  text UNIQUE,
  status         text NOT NULL DEFAULT 'queued'
                 CHECK (status IN ('queued','pending','success','failed','reversed')),
  failure_reason text,
  sent_at        timestamptz,
  completed_at   timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payouts_seller_idx ON payouts (seller_id, created_at DESC);
CREATE INDEX payouts_status_idx ON payouts (status, sent_at);
-- At most one in-flight payout per seller:
CREATE UNIQUE INDEX payouts_one_inflight ON payouts (seller_id) WHERE status IN ('queued','pending');
```

## Jobs

**`payouts.execute_all`**: periodic **daily at 10:00 Africa/Accra**.
- River periodic jobs take a schedule: implement a `river.PeriodicSchedule` for "next 10:00 UTC", since Ghana is UTC+0. Unit-test it.
- For each seller with a `verified` account, `cooldown_until` null or past, no in-flight payout, and `Balance(seller_payable)` ≥ `PAYOUT_MIN_PESEWAS`. In one tx per seller:
  1. `pg_advisory_xact_lock(hashtext('payout:'||seller))`.
  2. Re-read the balance.
  3. Insert the payout (`queued`, amount = the full payable balance).
  4. Post `payout_initiated` (ref = payout reference).
  5. Enqueue `payouts.send {PayoutID}` (unique).

**`payouts.send {PayoutID}`:**
1. Lock the payout. If it isn't `queued`, no-op.
2. Paystack `InitiateTransfer(amount, recipient_code, reference, reason="Farmish payout")`, **outside** the tx.
3. Record `transfer_code` and `status=pending`, `sent_at`.
4. Paystack error:
   - **4xx** (e.g. insufficient balance, invalid recipient): mark `failed` and post `payout_failed` (re-credit), in one tx, with an Error log.
   - **5xx / timeout:** keep `queued` and retry. Before retrying, call `VerifyTransfer(reference)`: if Paystack already has the transfer, adopt its state. The reference makes a double send impossible, since Paystack rejects duplicate references.

**`payouts.reconcile`** (daily 11:00): payouts `pending` for more than 24h → `VerifyTransfer(reference)` → apply the same handler as the webhooks.

## Webhooks (13a registry), matched by `data.reference`; each in one tx, idempotent on status

- **`transfer.success`:** pending|queued → success; post `payout_succeeded` (including the transfer fee `PAYSTACK_TRANSFER_FEE_PESEWAS` if > 0); SMS to the seller "GHS X sent to your account".
- **`transfer.failed` / `transfer.reversed`:** → failed/reversed; post `payout_failed` (re-credit `seller_payable`); SMS to the seller; Error log.
- A late `success` after we marked `failed` (it can happen) → log an Error and write an audit event. **Don't** auto-post: it goes to an admin. Document this.

## API

| Method & path | Auth | Responses |
|---|---|---|
| `GET /v1/seller/balance` | bearer (seller) | 200 `{available: Money (seller_payable), inEscrow: Money (Σ held remainder of their orders), inFlight: Money (queued+pending payouts), paidOut: Money (Σ success)}` |
| `GET /v1/seller/payouts` | bearer (seller) | 200 `{items: [{id, amount, status, reference, createdAt, completedAt, failureReason?}], meta}` |
| `POST /v1/admin/payouts/{id}/retry` | admin | 200: only for `failed`/`reversed`. It triggers a fresh `execute` for that seller: a **new** payout row with a new reference for the (re-credited) balance. 409 otherwise. Audited |
| `GET /v1/admin/payouts` | admin | 200 list with filters `status`, `sellerId` |

## Config

- `PAYOUT_MIN_PESEWAS` (2000)
- `PAYSTACK_TRANSFER_FEE_PESEWAS` (0 until confirmed)

## Tests (the plan's Done-when first)

| Test | Proves |
|---|---|
| `TestPayout_EndToEnd` | A completed order → `seller_payable` → `execute_all` → payout queued → send (fake Paystack) → pending → `transfer.success` webhook → success; ledger: `payable → payout_clearing → paystack_clearing`, all balanced |
| `TestPayout_FailedRestoresBalance` | `transfer.failed` → `seller_payable` back to its pre-payout value; the payout `failed` |
| `TestPayout_NoPayoutDuringCooldown` | Account changed 1h ago → no payout; after 48h → payout |
| `TestPayout_BelowMinimumSkipped` | A balance of 1999 → none; 2000 → payout |
| `TestPayout_OneInFlight` | `execute_all` twice concurrently → one payout |
| `TestPayout_NeedsReviewAccountSkipped` | |
| `TestPayout_SendRetryAdoptsExisting` | The first send times out but Paystack created the transfer → the retry adopts it via verify, with no duplicate |
| `TestPayout_ReconcileStuck` | Pending 25h + fake verify success → success |
| `TestSchedule_Next10amAccra` | 09:59 → the same day 10:00; 10:00:01 → the next day |
| `TestPayoutAccount_NeverUnmasked` | Balance and payout responses never include the account number |

## Pitfalls

- The whole payable balance at execution time is paid. New releases after that wait for the next day.
- **Money leaves only through a transfer the webhook confirms.** Never mark success from the initiate response alone.
