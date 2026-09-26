# ADR-0023: Snapshot purchased promotion credits in payments.metadata

- **Status:** Accepted (Phase 14).
- **Date:** 2026-09-26

## Context

Phase 14 must grant the credit count a tier had at purchase time, even if
promotion_configs is edited before the webhook or job runs. The spec allows
either a `credits` field in Paystack metadata or a `payments.metadata` JSONB
column.

Paystack metadata is useful for reconciliation, but it is not durable inside
Farmish: the purpose handler receives the stored payment, not the provider's
echo of the checkout request. Prices and charges are already payment columns,
while the purchased credit count had no durable home.

## Decision

Add `payments.metadata jsonb NOT NULL DEFAULT '{}'` and store the purchased
tier's credits there under `promotion_credits`. The same snapshot is also sent
to Paystack alongside the payment's own identifiers. The purpose handler reads
only the stored payment snapshot when posting `promotion_paid`.

## Consequences

- Changing a tier's credits affects future purchases, not payments that have
  already been created.
- A payment whose snapshot is missing, non-integral or out of range fails
  loudly in the purpose handler instead of granting an assumed credit count.
- Future purposes, including checkout snapshots in Phase 15b, can reuse the
  same column rather than adding purpose-specific payment columns.
