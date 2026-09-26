# ADR-0007: Firebase phone sign-in replaces the custom OTP flow

- **Status:** Accepted. Supersedes the Auth and SMS rows of ADR-0001.
- **Date:** 2026-09-26

## Context

The original plan built phone login ourselves (Phase 7):
- the backend generated and hashed 6-digit codes
- mNotify delivered them
- attempts and cooldowns were tracked in `phone_verifications`
- the backend minted a Firebase custom token

Firebase Authentication offers phone sign-in as a first-class provider. Firebase sends the SMS, verifies the code, and protects the flow with reCAPTCHA or App Check. That provider is now enabled on the project.

## Decision

- **All sign-in methods are Firebase providers:** social, email/password and phone. The client SDK runs the phone flow (`signInWithPhoneNumber` + `RecaptchaVerifier`).
- **The backend only verifies Firebase ID tokens,** whatever the provider. `users.signup_method` comes from the token's `firebase.sign_in_provider`. There are no custom tokens and no OTP endpoints or tables.
- **Phase 7 is repurposed** as *phone sign-in hardening & step-up re-auth*:
  - console hardening: SMS region policy limited to Ghana, App Check, budget alert
  - a `RequireRecentAuth(maxAge)` guard on the token's `auth_time`

  Payouts (Phase 18) use that guard instead of an OTP `step_up` purpose.
- **mNotify is kept only for transactional SMS** (order and payout alerts), behind a `notify.Notifier` interface. It's introduced in Phase 16, where those alerts start. `MNOTIFY_*` env vars move there, and `OTP_TTL_MINUTES` / `OTP_MAX_ATTEMPTS` are dropped.
- **Turnstile stays in Phase 6.** It guards our own anonymous public endpoints. It can't replace the reCAPTCHA that Firebase runs inside its phone flow.

## Consequences

- **Less security-critical code:** no code generation, hashing, brute-force limits or custom-token minting for us to get wrong.
- **Cost and dependency:** phone sign-in needs the Firebase **Blaze** plan, and every SMS is billed by Google. SMS-pumping abuse is a cost risk, mitigated by the region policy, App Check and a budget alert (Phase 7, §11).
- **Less control over SMS delivery:** we give up the sender ID, message text and delivery path to Firebase. Delivery quality to Ghanaian networks is Google's responsibility. If it proves poor, a custom provider could come back behind the same ID-token contract.
- **Step-up is re-authentication, not an OTP purpose.** The client must re-authenticate, and the backend checks `auth_time`.
