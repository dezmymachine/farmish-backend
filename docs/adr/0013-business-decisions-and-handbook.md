# ADR-0013: Owner business decisions and the implementer handbook

- **Status:** Accepted
- **Date:** 2026-09-26

## Context

Development continues with a different coding model, and a senior engineer reviews each phase. Money and marketplace rules must be decided up front and written down precisely, not inferred phase by phase. Several decisions were still open in §11 of the plan.

## Decisions

1. **Owner business decisions** (full rules in `docs/DOMAIN.md` §2–3):
   - Commission: **5%** (500 bps) of the item subtotal, delivery excluded, snapshotted per order, with per-category overrides.
   - The Paystack charge fee is **paid by the buyer** as a visible, non-refundable processing-fee line, grossed up so the platform nets the base: `charge = ceil(base × 10000 / (10000 − PAYSTACK_FEE_BPS))`, default 195 bps. The platform absorbs transfer fees.
   - Sellers accept within **48h**, or the order auto-cancels with a refund. Escrow auto-releases **3 days** after delivery unless disputed.
   - Payouts run daily at 10:00 Africa/Accra, minimum **GHS 20**, with a 48h cooldown after a payout-account change and step-up re-auth to change it.
2. **The order state machine drops `fulfilling`.** `accepted` covers preparation, and `shipped` doubles as "ready for pickup". The single table is in DOMAIN §4.
3. **Pre-split** of the large phases into one-task sub-phases: 13a/13b, 15a/15b, 17a/17b, 18a/18b, 20a/20b.
4. **The handbook is canonical for implementers:**
   - `AGENTS.md`: rules; `CLAUDE.md` points to it
   - `docs/ENGINEERING_GUIDE.md`: patterns
   - `docs/DOMAIN.md`: the business source of truth
   - `docs/phases/*`: specs with named tests
   - `docs/REVIEW_PROTOCOL.md`: the review packet
5. **Legacy bugs are deliberately not ported** (DOMAIN §12): decimal cedis, the webhook race and missing amount check, expired promotions ranking forever, credits going negative, `&` in slugs, and others.

## Consequences

- The Paystack fee rate and GH transfer fee must be confirmed from the live dashboard before go-live (Phase 22). They're config, not code.
- Any change to a money rule means editing DOMAIN.md **first**, then adding an ADR and changing code and tests.
- Phase numbering changed from 13 onwards (sub-phases). §9 progress rows use the new ids.
