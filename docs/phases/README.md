# Phase specs

One file per remaining phase. `REDEVELOPMENT_PLAN.md` §6 is the checklist; these files hold the detail. Large phases are pre-split into sub-phases, each delivered as one task.

| Phase | Spec | Depends on |
|---|---|---|
| 7 | [Phone sign-in hardening & step-up re-auth](phase-07.md) | 4 |
| 8 | [Profiles & seller onboarding](phase-08.md) | 4 |
| 9 | [Catalog (categories, attributes, locations)](phase-09.md) | 2, 3 |
| 10 | [Media uploads (R2)](phase-10.md) | 4 |
| 11 | [Listings CRUD](phase-11.md) | 8, 9, 10 |
| 12 | [Search & browse](phase-12.md) | 11 |
| 13a | [Payments core (Paystack, webhooks)](phase-13a.md) | 4, 5, 8 |
| 13b | [Ledger foundation](phase-13b.md) | 13a |
| 14 | [Promotions](phase-14.md) | 12, 13b |
| 15a | [Checkout foundations (pricing, orders schema)](phase-15a.md) | 12, 13b |
| 15b | [Checkout payment & escrow hold](phase-15b.md) | 15a |
| 16 | [Fulfilment state machine & notifications](phase-16.md) | 15b |
| 17a | [Escrow release & refunds](phase-17a.md) | 16 |
| 17b | [Disputes & ledger reconciliation](phase-17b.md) | 17a |
| 18a | [Seller payout accounts](phase-18a.md) | 7, 8, 17b |
| 18b | [Payout execution](phase-18b.md) | 18a |
| 19 | [Messaging](phase-19.md) | 11, 16 |
| 20a | [Reviews & favorites](phase-20a.md) | 16 |
| 20b | [Reports & supply requests](phase-20b.md) | 16, 9 |
| 21 | [Observability & hardening](phase-21.md) | 18b, 19, 20a, 20b |
| 22 | [Deploy & contract freeze](phase-22.md) | 21 |
| F0–F9 | [Frontend phases](frontend.md) | 22 (F0–F2 may start after 12) |

Every spec uses the same sections: **Goal · Scope / Out of scope · Schema · Queries · API · Rules · Jobs · Config · Files · Tests · Manual QA · Pitfalls**. Read `AGENTS.md`, `docs/ENGINEERING_GUIDE.md` and `docs/DOMAIN.md` first.

## Migration numbers

The `NNNNNN_` numbers in the specs are **indicative**: they assume phases are done in table order. Always create migrations with `make migrate-new name=<name>`, which picks the next free number, and keep the name from the spec (e.g. `seller_profiles_audit`). If you do phases in a different order (e.g. 9 before 8), your numbers will differ from the spec. That's expected; mention it in the review packet.
