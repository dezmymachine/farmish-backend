# ADR-0014: Step-up revocation test vs the Auth emulator

- **Status:** Accepted (Phase 7).
- **Date:** 2026-09-26

## Context

Phase 7's spec for `TestStepUp_RevokedTokenRejected` says: revoke with the
Admin SDK, then the same token gets 401 on a step-up operation while it
still returns 200 on a normal operation until expiry. That describes
production Firebase exactly: `Verify` never checks revocation, only
`VerifyStrict` (`VerifyIDTokenAndCheckRevoked`) does.

## Decision

The test as written cannot pass against the Firebase Auth emulator, for two
emulator-specific reasons found while implementing:

1. The Admin SDK checks revocation on **both** paths when
   `FIREBASE_AUTH_EMULATOR_HOST` is set (`if c.isEmulator ||
   checkRevokedOrDisabled` in `auth.go`), so after a revocation the normal
   operation also returns 401 in tests. Production behaviour is unchanged.
2. The emulator tracks revocation at one-second granularity (`validSince`),
   so revoking in the same second as sign-in is a no-op. The test sleeps
   1.2s (past a second boundary) before revoking. That sleep works around
   emulator timestamp granularity, not a business timer.

So the committed test asserts: after a real `RevokeRefreshTokens`, the same
token gets 401 on the step-up operation. It does not assert 200 on the
normal operation. The production half of the spec's expectation (the normal
path never consults revocation) is proved instead by
`TestStepUp_NonStepUpUsesFastVerify`, which counts verifier calls with a
fake: normal operations call only `Verify`, step-up operations only
`VerifyStrict`.

## Consequences

- The revocation path (`VerifyStrict` → generic 401; freshness → 401
  `reauth_required`) is tested end to end against the emulator.
- If the SDK ever stops checking revocation in emulator mode, the normal-op
  assertion from the spec can be reinstated; the fake-based test already
  guards the routing.
