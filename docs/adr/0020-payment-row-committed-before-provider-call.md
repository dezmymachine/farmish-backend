# ADR-0020: The payment row is committed before Paystack is called

- **Status:** Accepted (Phase 13a).
- **Date:** 2026-09-26

## Context

The Phase 13a spec writes the service entry point as
`Initialize(ctx, tx?, {UserID, Email, Purpose, PurposeRef, BasePesewas})` and
then says, in order:

1. compute the gross-up,
2. insert the `pending` payment row (**commit**),
3. call `InitializeTransaction` **outside** the tx,
4. store `authorization_url`.

The optional `tx?` is ambiguous next to an explicit "commit" in step 2. The
two readings differ in what a caller in Phase 14 or 15b can do: under one, the
caller wraps Initialize in its own transaction and the payment row is not
visible until the caller's work commits; under the other, Initialize manages
its own transactions and the payment is visible immediately.

## Decision

`Initialize(ctx, in CreateInput)` takes no transaction and manages its own:

- its own transaction to insert the `pending` row and commit it,
- the provider call with no transaction open,
- its own transaction to store the authorization URL.

A provider failure marks the row `failed` and returns `ErrProviderUnavailable`,
so the attempt survives as an audit trail rather than as a stuck transaction.

## Consequences

- A network call never holds a transaction or a connection. That matters most
  in `payments.succeeded`, which runs inside the worker's transaction, and in
  Initialize itself, which is called from a checkout handler.
- A buyer who abandons checkout leaves a `pending` row that is visible to the
  verify fallback and, later, to Phase 17's expiry sweep. It is never a
  half-inserted row inside somebody else's transaction.
- The cost is that the payment row is not atomic with whatever the caller was
  doing. For a checkout, the cart is confirmed by `payments.succeeded` in its
  own transaction anyway (Phase 15b), so there is nothing to lose. Phase 14's
  promotion purchase is the case to watch: the promotion must not be granted
  before the payment settles, and it is not — the grant happens in the
  purpose handler, after settlement.
