# Review packet: Phase 13b Ledger foundation

## Summary

Implemented the append-only, double-entry ledger required by DOMAIN §5: ledger
accounts, transactions and entries, an idempotent `Post` that validates every
entry before inserting anything, derived balances, lazy creation of dynamic
seller and promotion-credit accounts, fixed accounts seeded from DOMAIN §5.2,
and constructors for all eight DOMAIN §5.3 posting shapes. The database also
rejects unbalanced transactions at commit time and rejects every UPDATE or
DELETE on ledger tables.

## Commits

```
1ace954 Phase 13b: ledger posting service and invariant tests
7235746 Phase 13b: ledger schema and queries
```

## Done-when checklist

Plan §6 Phase 13b:

- [x] A ledger post with a non-zero sum errors, per currency: `TestPost_UnbalancedRejected` (`internal/ledger/ledger_test.go`)
- [x] Posts are idempotent on (kind, reference): `TestPost_Idempotent` (`internal/ledger/ledger_test.go`)
- [x] The tables are append-only: `TestLedger_AppendOnly` (`internal/ledger/ledger_test.go`)
- [x] A property test holds balances consistent: `TestLedger_PropertyBalancesConsistent` (`internal/ledger/ledger_property_test.go`)

Spec test table:

- [x] `TestPost_Balanced` (`internal/ledger/ledger_test.go`)
- [x] `TestPost_UnbalancedRejected` (`internal/ledger/ledger_test.go`)
- [x] `TestPost_PerCurrencyBalance` (`internal/ledger/ledger_test.go`)
- [x] `TestPost_CurrencyMismatchWithAccount` (`internal/ledger/ledger_test.go`)
- [x] `TestPost_Idempotent` (`internal/ledger/ledger_test.go`)
- [x] `TestPost_RollsBackWithCallerTx` (`internal/ledger/ledger_test.go`)
- [x] `TestLedger_AppendOnly` (`internal/ledger/ledger_test.go`)
- [x] `TestLedger_DeferredBalanceTrigger` (`internal/ledger/ledger_test.go`)
- [x] `TestLedger_PropertyBalancesConsistent` (`internal/ledger/ledger_property_test.go`)
- [x] `TestPostingTemplates_CheckoutPaidAndEscrowRelease`, `TestPostingTemplates_RefundsAndPromotions`, and `TestPostingTemplates_Payouts` (`internal/ledger/postings_test.go`)

Additional coverage used by the packet:

- [x] `TestLedger_SeededFixedAccounts` verifies all nine DOMAIN §5.2 accounts.
- [x] `TestPost_UnknownAccountRejected` verifies that an unknown code writes nothing.

## make ci

```
#13 DONE 0.2s

#14 [build 6/6] RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build     CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/api ./cmd/migrate
#14 DONE 10.7s

#15 [stage-1 2/2] COPY --from=build /out/api /out/migrate /
#15 DONE 0.4s

#16 exporting to image
#16 exporting layers
#16 exporting layers 9.4s done
#16 exporting manifest sha256:22b99b069e4560999562a119ccafb0c4057d1e54e11339d4dfcbcb7947f92986 0.0s done
#16 exporting config sha256:27f67e5fcdeb4445b17edf14726a3d06708baeec1d9677269d66dad1d6d53518 0.0s done
#16 exporting attestation manifest sha256:79a04fd21565e05e0cb607b410171009e32d13b6e80c7a7a1f7b7d3039eaabb3 0.0s done
#16 exporting manifest list sha256:10dd365749066b966ba54d49ca428b6437ab2512f6d0c7a9bd537a9bc8dd6c16 0.0s done
#16 naming to docker.io/library/farmish-backend:dev done
#16 unpacking to docker.io/library/farmish-backend:dev
#16 unpacking to docker.io/library/farmish-backend:dev 0.0s done
#16 DONE 10.4s
./scripts/smoke.sh farmish-backend:dev
smoke: migrate ok, /healthz ok, redis limits ok, /v1/me 401 -> 200 with token, /readyz 200 -> 503 on DB loss, graceful shutdown ok
ci: all checks passed
```

## Manual QA

After the focused tests, `make migrate-up` reported the local database at
version 10. The fixed-account query returned:

```
code                  | type      | currency
----------------------+-----------+----------
escrow                | liability | GHS
payout_clearing       | liability | GHS
paystack_clearing     | asset     | GHS
paystack_fees         | expense   | GHS
platform_commission   | revenue   | GHS
processing_fee_income | revenue   | GHS
promo_credits_issued  | equity    | CRD
promotion_revenue     | revenue   | GHS
transfer_fees         | expense   | GHS
```

An `UPDATE ledger_entries SET amount=1` against the empty development table
returned `UPDATE 0`, so it did not exercise the trigger. A balanced
paystack_clearing/escrow pair was therefore inserted in one transaction,
followed by the same update:

```
BEGIN
INSERT 0 1
INSERT 0 1
ERROR:  UPDATE on ledger_entries is not allowed (append-only)
CONTEXT:  PL/pgSQL function forbid_mutation() line 2 at RAISE
```

The aborted transaction left no row behind:

```
select count(*) from ledger_transactions
  where kind='manual-qa' and reference='trigger-check';
 count
-------
     0
```

## Files changed

`git diff --stat c898f8d..1ace954` — 11 files, +1313:

- `db/queries/ledger.sql` — account, transaction, entry and balance queries.
- `internal/db/ledger.sql.go`, `internal/db/models.go`, `internal/db/querier.go` — generated sqlc accessors; never hand-edited.
- `internal/ledger/ledger.go` — `Entry`, fixed-account constants, `Post`, `Balance`, dynamic-account resolution and validation.
- `internal/ledger/postings.go` — the eight DOMAIN §5.3 posting constructors.
- `internal/ledger/ledger_test.go` — balance, idempotency, rollback, append-only and deferred-trigger tests.
- `internal/ledger/postings_test.go` — exact-amount posting tests.
- `internal/ledger/ledger_property_test.go` — the seeded 500-posting invariant test.
- `migrations/000010_ledger.{up,down}.sql` — ledger schema and rollback.

## Schema changes

- `000010_ledger` — `ledger_accounts`, `ledger_transactions` and
  `ledger_entries`; the nine DOMAIN §5.2 fixed accounts; append-only triggers
  on all three tables; and a deferred `ledger_check_balanced()` trigger.
- `TestUpDownUp` passes. `make migrate-version` on the development database
  reports `version 10 (dirty: false)`.

## API changes

none. Phase 13b is a foundation package with no HTTP surface, so `api-lint`
and `generate-check` confirm no contract drift.

## Deviations from the spec

none.

## Open questions / risks

- `Balance` reports the raw signed sum of entries, as the phase API specifies.
  DOMAIN §5.1 separately says liability, revenue and equity balances should be
  displayed as `-Σ`; that presentation choice belongs with the future API that
  displays money, not with the storage-level balance query.
- Dynamic account creation sets `owner_id` from the account code's UUID.
  Tests therefore use real users for those owners; an owner UUID without a
  `users` row fails on the foreign key rather than silently creating an
  orphaned account.

## Backlog additions

none.
