# ADR-0028: Dispute resolution mechanics, release deferral, retry enqueue and the daily reconciliation schedule

- **Status:** Accepted
- **Date:** 2026-09-27
- **Phase:** 17b

## Context

Phase 17b resolves disputed orders through the same `orders.Transition` all other moves use, and adds a daily detector for the DOMAIN §5.4 invariants. Four mechanics needed decisions the spec leaves open.

## Decisions

1. **The release waits on in-flight refunds, not just on escrow state.** The spec says the release job treats `refund_pending` as "retry later". A partial resolution keeps the order's escrow `held` (nothing has settled yet), so escrow state alone cannot express "a refund is on its way". `ReleaseEscrow` therefore counts the order's `queued`/`pending` refunds under the row lock and returns `ErrReleaseDeferred` while any exist; the worker maps that to `river.JobSnooze(10m)`. A completed order with no live refund releases exactly as before. This also closed a real hole the new guard exposed: the Phase 16 sweep test rewound a cancelled order (with its cancel refund still pending) back to delivered, and the release would previously have paid the full base alongside the pending full refund.

2. **Admin retries enqueue without job uniqueness.** `RetryRefund` inserts `RefundArgs` with no `UniqueOpts`. River uniqueness is scoped to existing job rows, so a unique retry insert is swallowed as a duplicate of the first attempt's (completed or failed) row: the refund would sit `queued` forever with no worker coming. The refund's own `queued` check is the idempotency mechanism (a second job for the same refund reconciles instead of calling twice), which the Phase 17a ambiguous-outcome machinery already assumes. Pinned by the river_job row count in `TestRetryRefund`.

3. **Daily 03:00 Africa/Accra via a custom River schedule.** River v0.47 has no cron helper, only `PeriodicInterval`. `jobs.DailyAt{Hour, Min, Loc}` implements `river.PeriodicSchedule` (next occurrence after now) plus a `Registry.Schedule` method, and `ledger.reconcile` uses it for 03:00 Africa/Accra with `runOnStart: false` (a detector has no backlog to clear at deploy). Accra is UTC+0 with no DST, so a fixed zone is exact where tzdata is missing; `LoadLocation` wins when it exists. Pinned by `TestDailyAt_Next`.

4. **Contract shapes.** `DisputeAdmin` embeds the full `OrderDetail` (seller-shaped: commission, recipient, events — what a resolver needs) rather than a slimmed copy, and `AdminOrderDetail` wraps that same `OrderDetail` with both parties' contacts plus the order's ledger entries (account, GHS amount, transaction kind/reference). The dispute's order-event note is `"dispute resolved: <outcome>"`, not the admin's free-text note, because `order_events.note` caps at 500 chars while the resolution note allows 2000.
