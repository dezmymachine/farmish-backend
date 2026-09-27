# ADR-0029: Payout account resolution, name check and error mapping

- **Status:** Accepted
- **Date:** 2026-09-27
- **Phase:** 18a

## Context

Sellers register a MoMo or GhIPSS account that 18b will pay into. Paystack owns bank truth (codes, holder names, recipient handles); Farmish owns the name check, the secret and the cooldown.

## Decisions

1. **Any ResolveAccount error is 422 `account_unresolvable`.** The spec's rule 2 maps every resolve failure to 422, including transport failures: from the seller's view the number cannot be resolved right now, and the message says to check the number and bank. Provider outages earlier in the flow (bank list, recipient creation) are 502 instead, since no account data was judged.
2. **Name check is token overlap, not equality.** Both names are uppercased, de-punctuated and split; one shared token of ≥ 3 characters against the business name or display name verifies. Ghanaian holder names rarely equal business names exactly (personal name vs shop name), so equality would push every seller into manual review.
3. **MoMo numbers normalize to local `0XXXXXXXXX`.** `+233`/`233`/`0` prefixes and spaces/dashes all land on the 10-digit local form the spec expects Paystack to resolve. **Live verification open:** Transfers are not enabled on the Paystack account (plan §11), so the local-vs-international format and the `basilisk` recipient type for GhIPSS await a live test-mode check before 18b runs real money.
4. **Re-submitting an existing row restarts the 48h cooldown.** "On change (a row already existed)" is read literally: any PUT over an existing row sets `cooldown_until = now + 48h`, with old/new masks in the audit. Identical re-submits are not special-cased.
5. **Banks cache per type in memory for one hour** with an injectable clock. No Redis: the list is tiny, per-process staleness of ≤ 1h is harmless, and it keeps the metered Upstash budget for rate limits.
