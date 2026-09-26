# Review protocol

Every phase ends with a **review packet**, `docs/reviews/phase-NN.md` (e.g. `phase-07.md`, `phase-13a.md`), committed with the phase. The reviewer (a senior engineer or Claude) reviews the commits plus the packet. A phase isn't accepted until the review passes; fixes go in follow-up commits on the same phase.

## Review packet template

Copy this into `docs/reviews/phase-NN.md` and fill every section. Write "none" rather than deleting a section.

```markdown
# Review packet: Phase NN <name>

## Summary
Two to five sentences: what was built and how it satisfies the spec.

## Commits
`git log --oneline <first>^..<last>` output.

## Done-when checklist
For each "Done when" item in REDEVELOPMENT_PLAN.md §6 and each test in the phase spec:
- [x] <item>: <test name(s) that prove it> (<file>)

## make ci
The last 25 lines of `make ci` output, ending in `ci: all checks passed`.

## Manual QA
The QA script from the phase spec, with the actual commands run and their actual output (trimmed). Mask tokens.

## Files changed
`git diff --stat <base>..HEAD` plus one line per new file on its purpose.

## Schema changes
New migrations (numbers + one-line purpose). Confirm up→down→up passes (`migrations_test`).

## API changes
New/changed operations (method, path, operationId, auth, rate-limit/turnstile extensions). Confirm `make api-lint` and `generate-check` pass.

## Deviations from the spec
Each deviation, why, and the ADR number. "none" if none.

## Open questions / risks
Anything the reviewer should look at closely, or that you were unsure about.

## Backlog additions
Items added to §10.
```

## What the reviewer checks

Blocking issues fail the review.

**Correctness and money (blocking)**
- Amounts are `int64` pesewas everywhere. Rounding follows `docs/DOMAIN.md` exactly. No floats.
- Server-side pricing: nothing money-related is read from the request body.
- The state change, ledger post and job enqueue happen in **one** transaction. No enqueue after commit, and no ledger post in a separate transaction.
- Idempotency: replaying the webhook, job or request produces no double effect. It's **tested**.
- Ledger entries balance per currency. Invariant tests exist.
- The state machine rejects illegal transitions (409) and is tested against the full table.

**Security (blocking)**
- The user comes only from `users.FromContext`. Ownership is checked in the service layer, with a 403 test for another user's resource.
- Public endpoints return safe projections (a test asserts forbidden fields are absent).
- No secrets, tokens, full phones or account numbers in logs or errors.
- Webhook signatures are verified on the raw body before parsing, and a bad signature gets a 401.

**Contract and structure**
- The spec came first. Generated code isn't hand-edited, and the drift checks pass.
- Handlers are thin (parse → service → map errors). Business rules live in the domain package, not in handlers or SQL-only.
- sqlc for all SQL. Migrations are forward-only, with working downs.
- Package layout matches `docs/ENGINEERING_GUIDE.md`, and new code reads like the surrounding code.

**Tests and QA**
- Every Done-when item maps to a named test.
- Real Postgres, emulator and Redis are used. No DB mocks. External HTTP services are faked behind interfaces.
- Time uses an injectable clock, and nothing sleeps for business timers.
- The manual QA output is real, not described.

**Docs**
- The plan checkbox is ticked, and §8/§9 have rows. An ADR exists per deviation, and `.env.example` and §7 cover new env vars.
