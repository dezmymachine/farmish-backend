# Review packet: Phase 16 fulfilment state machine & notifications

## Summary

`internal/orders` is now one state machine over DOMAIN §4: a single
`Transition` validates (from, to, actor) under the row lock, moves exactly one
status timestamp, and writes the order event; actions enforce role before
state (other party 403, stranger 404) and commit status, escrow column and
River enqueues in one transaction. Seven contract-first action endpoints
expose the moves with role-shaped responses, the 48-hour accept and 3-day
auto-complete sweeps run hourly through River with an injectable clock, and
every transition notifies the other party by SMS (mNotify client verified
against the live endpoint, log-only with masked numbers while
`NOTIFY_SMS_ENABLED=false`).

## Commits

```
76b679a Phase 16: seven fulfilment action endpoints, contract-first
9be2fd9 Phase 16: fulfilment state machine, timers and notify package
cf5919b Phase 16: disputes schema, sweep queries and timer/notify config
```

(`git log --oneline cbbeb01..HEAD`; cbbeb01 is the Phase 15b packet commit.)

## Done-when checklist

From `REDEVELOPMENT_PLAN.md` §6 and the spec's test table:

- [x] illegal transitions return 409: `TestEndpoints_IllegalTransition409`
  (`internal/http/fulfilment_test.go`; ship a paid order → 409 with
  from/to details), plus `TestTransition_FullTable` rejects every
  non-table combination.
- [x] each actor can do only their own transitions (the full-table test):
  `TestTransition_FullTable` (`internal/orders/table_test.go`, generated
  from the code's table **and** a hand-written DOMAIN §4 copy) and
  `TestEndpoints_ActorEnforcement` (buyer-accept 403, seller-confirm 403,
  stranger 404 on both surfaces).
- [x] the timers are tested with a fake clock: `TestAutoCancel_FakeClock`
  and `TestAutoComplete_FakeClock` (`internal/orders/timers_test.go`;
  nothing at 47h59m/day 2, cancel/complete at the deadline with stock,
  escrow, event actor and queued refund/release asserted), plus
  `TestSweeps_RiverExecution` (`internal/orders/river_timers_test.go`)
  proving both sweeps move overdue orders when executed by real River
  workers.
- [x] every transition enqueues an SMS:
  `TestNotify_EveryTransitionEnqueuesSMS`
  (`internal/orders/notify_walk_test.go`, inspects the River table for
  template + recipient on the happy path and the reject path),
  `TestMNotify_Client` + `TestRender` + `TestLogOnly_MasksNumber`
  (`internal/notify/notify_test.go`), and
  `TestNotifyJob_SkipsUserWithoutPhone`
  (`internal/orders/notify_job_test.go`; phone resolved at send time).
- [x] dispute stops the clock: `TestDispute_CreatesRowAndStopsTimer`
  (disputes row, escrow stays held, sweep skips, second dispute 409).
- [x] extra endpoint cover beyond the spec table:
  `TestEndpoints_FulfilmentHappyPath` (accept → ship → mark-delivered →
  confirm-receipt, contract-valid, commission hidden from the buyer),
  `TestEndpoints_RejectRefunds` (cancelled/refund_pending + refund job +
  rejected-buyer SMS), `TestEndpoints_DisputeAndCancel` (dispute row,
  validation 400s, buyer cancel, stranger 404, reject-after-cancel 409),
  `TestEndpoints_DisputeRateLimit` (sensitive budget → 429 with
  Retry-After).

## make ci

Tail of the green run on the code-final tree (docs commit after it touches
only `.md` files):

```
#12 [build 4/6] RUN --mount=type=cache,target=/go/pkg/mod go mod download
#12 CACHED
#13 [build 5/6] COPY . .
#13 DONE 0.1s
#14 [build 6/6] RUN ... CGO_ENABLED=0 go build ... -o /out/ ./cmd/api ./cmd/migrate
#14 DONE 4.0s
#15 [stage-1 2/2] COPY --from=build /out/api /out/migrate /
#15 CACHED
#16 exporting to image
...
#16 naming to docker.io/library/farmish-backend:dev done
#16 unpacking to docker.io/library/farmish-backend:dev done
#16 DONE 0.1s
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

## Manual QA

Live walk against `:8081` (local containers, fake Paystack stand-in for
initialization, real signed webhook path, `NOTIFY_SMS_ENABLED=false`). The
Auth emulator cannot put a phone claim on password users, so the two QA
numbers were seeded into `users.phone_e164`: seller before settle (paid SMS),
buyer before accept (three buyer SMS), seller again before confirm-receipt
(every request re-mirrors the phoneless token). No real SMS sent.

```
$ python3 /tmp/opencode/qa/qa16.py
seller profile: 200
listing: qa16-heifer-1790518207
quote: base 850000 charge 866905
checkout: 510f79a1-... FMS-26aw4lecoozl3j4gxe7a
phones set: {'qa16seller@example.com': '+233240000001'}
webhook: 200
poll after pay: paid order 2cd62f2d-...
phones set: {'qa16buyer@example.com': '+233240000002'}
accept: accepted commission 42500
ship: shipped tracking QA16-TRK-1
mark-delivered: delivered
phones set: {'qa16seller@example.com': '+233240000001'}
confirm-receipt: completed commission visible to buyer: None
ship after complete: 409 invalid_transition
buyer accept: 403 forbidden
stranger read: 404 not_found
```

The five masked log lines (one more than the spec's four — the paid SMS too):

```
sms (log-only) to="+233*****0001" message="Farmish: you have a new paid order 2cd62f2d. Accept it within 48 hours or it is cancelled and refunded."
sms (log-only) to="+233*****0002" message="Farmish: order 2cd62f2d was accepted. We will message you when it ships."
sms (log-only) to="+233*****0002" message="Farmish: order 2cd62f2d is on its way."
sms (log-only) to="+233*****0002" message="Farmish: order 2cd62f2d was delivered. Confirm receipt within 3 days or open a dispute."
sms (log-only) to="+233*****0001" message="Farmish: order 2cd62f2d is complete. Your earnings were released to your balance."
```

`grep -c "240000001\|240000002" api16.log` → 0: no raw number reaches the logs.
`grep -rn "UPDATE orders SET status"` outside `internal/db` (generated) and
tests → only `internal/orders/transition.go:100`: one status writer.

## Files changed

`git diff --stat cbbeb01..HEAD` — 43 files, +5,714/−487. New:

- `migrations/000013_disputes.up.sql` / `.down.sql`: disputes table +
  `set_updated_at` trigger; down drops the table and restores
  `forbid_mutation()`.
- `internal/orders/actions.go`: the seven actions + the role-before-state
  guard + single-transaction `act`.
- `internal/orders/jobs.go`: refund/release log-only workers (Phase 17a
  replaces them), the two sweep workers, schedules.
- `internal/orders/transition.go` (rewrite): table, `Transition`,
  `ApplyEffects`, refund/release arg types.
- `internal/notify/`: `notify.go` (SMS interface), `templates.go` (9
  templates), `mnotify.go` (client), `logonly.go`, `job.go` (`notify.sms`
  worker resolving the phone at send time).
- `internal/http/handlers/orders_actions.go`: the seven handlers +
  `classifyOrderError`.
- `internal/http/fulfilment_test.go`: endpoint suite + dump-on-timeout
  settle wait.
- `internal/orders/table_test.go`, `timers_test.go`,
  `river_timers_test.go`, `notify_job_test.go`, `notify_walk_test.go`,
  `internal/notify/notify_test.go`: the spec's test table plus River
  execution and rate-limit cover.

## Schema changes

Migration `000013_disputes` (table + trigger, see above).
`migrations_test` (up → down → up) passes under `make ci`.

## API changes

Seven operations, all bearer-auth, no Turnstile; only dispute carries
`x-farmish-rate-limit: sensitive`:

- `POST /v1/seller/orders/{id}/accept` (acceptOrder)
- `POST /v1/seller/orders/{id}/reject` (rejectOrder, `{reason 1–500}`)
- `POST /v1/seller/orders/{id}/ship` (shipOrder, `{trackingRef? ≤120}`)
- `POST /v1/seller/orders/{id}/mark-delivered` (markOrderDelivered)
- `POST /v1/orders/{id}/cancel` (cancelOrder, `{reason? ≤500}`)
- `POST /v1/orders/{id}/confirm-receipt` (confirmOrderReceipt)
- `POST /v1/orders/{id}/dispute` (disputeOrder, `{reason enum,
  description 10–2000}`)

`make api-lint` and `generate-check` pass under `make ci`.

## Deviations from the spec

None. Two implementation points the reviewer should know are spec-compliant,
not deviations:

- Role is checked before state in `act()`, because the spec's own actor
  table demands 403 for the wrong party and 404 for a stranger even when
  the state would also refuse. The guard reads the bare order row
  (`GetOrderByID`), not the detail read — the detail joins the seller
  profile, which is a read concern, not a guard.
- `Transition` maps a missing row to `ErrNotFound` (was a bare lock error).

See ADR-0026 for the mNotify verification the spec required.

## Open questions / risks

- Test River clients must register the **complete** worker set their flows
  enqueue. While debugging, the checkout fixtures (HTTP + service) were
  missing the `notify.sms` worker: `payments.succeeded` sat `available`
  with attempt 0 for the full 30 s wait, and registering the worker fixed
  it instantly (River v0.47.0). Production `main.go` registers everything,
  so this is test-harness only — but any new fixture that settles a
  checkout needs the notify worker too.
- Webhook `eventKey` is `<event>:<provider id>`: reusing the hardcoded
  provider id `424242` across settles in one DB makes the second settle a
  silent replay (200, payment stays pending). The fulfilment helper now
  mints a fresh id per settle; `checkout_test.go` still uses one id per
  test, which is fine only because each test gets a fresh DB.
- The manual QA's SQL phone seeding (above) is a stand-in for phone-auth
  users; the automated tests prove enqueue + masking, not delivery. One
  real SMS with owner credentials is still untested (spec-optional).

## Backlog additions

- Document the River test-fixture full-registry rule in
  `docs/ENGINEERING_GUIDE.md` (test-harness gotcha found this phase).
