# Runbook: Firebase phone sign-in

Phone sign-in is a Firebase provider (ADR-0007). The backend only verifies
Firebase ID tokens; it never sends or checks SMS codes. This checklist
hardens the Firebase console side against SMS-pumping abuse and surprise
bills. Walk it in the real Firebase console for dev, staging and production.

## 1. Enable the Phone provider

1. Open Firebase console → your project → Build → Authentication → Sign-in method.
2. Under Additional providers, open **Phone** and click **Enable**.
3. Save.

## 2. Restrict the SMS region policy to Ghana

1. Authentication → Settings → **SMS region policy**.
2. Choose **Allow** only, and add **Ghana (+233)**.
3. Save. This blocks SMS-pumping to premium foreign destinations.

## 3. Set authorized domains

1. Authentication → Settings → **Authorized domains**.
2. Keep: `localhost`, the staging domain and the production domain.
3. Remove any defaults you don't use (e.g. the demo firebaseapp.com domain
   if nothing needs it).

## 4. App Check (reCAPTCHA Enterprise)

1. Register the web app for App Check with reCAPTCHA Enterprise
   (Firebase console → Build → App Check).
2. Enforce App Check for Authentication once the frontend ships (F2).
   Until then, keep it in monitoring mode so local dev isn't blocked.

## 5. Billing budget alert

1. Google Cloud console → Billing → Budgets & alerts on the Firebase project.
2. Create a monthly budget (e.g. the GHS-equivalent of $20) with alert
   thresholds at 50%, 90% and 100%.
3. Point the alerts at the on-call email.

## 6. Confirm the Blaze plan

Phone-auth SMS is billed: the project must be on the **Blaze (pay-as-you-go)**
plan. Firebase console → Project overview → plan badge. Each SMS is charged;
the budget alert above is the guardrail.

## 7. Optional: test phone numbers (no real SMS)

For manual QA without spending SMS:

1. Authentication → Sign-in method → Phone → **Phone numbers for testing**.
2. Add a Ghana test number (e.g. `+233241234567`) with a fixed 6-digit code.
3. Sign in with that number + code in the app; no SMS is sent and no charge
   is incurred.
