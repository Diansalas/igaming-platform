# Ledger-finance re-check — PRH-2 K1 (2026-09-28)

**Reviewer:** `ledger-finance`. The orchestrator recorded this review.

**Scope:** `prh2-k1-int` @ `5a27be6`, reviewed via `git archive`. Private DB, dropped afterwards.

## Verdict: ACCEPT

**Acting-session money probe (re-run, valid G-P2 session):**

| Attempt | Result |
|---|---|
| INSERT `ledger_transactions`, `ledger_accounts`, `wallet_balance_projection` | 42501 |
| INSERT `ledger_entries` | P0001 |
| UPDATE `wallet_balance_projection` | 0 rows |
| SELECT on all 8 money tables | 0 visible |

- **C-K1-1: CLOSED.** A re-revoke case is in A-13. **Mutant 10 re-killed:** A13 fails with "re-revoke: expected CG012, got <nil>".
- **C-K1-2: CLOSED.** ADR 0099 carries the final allocation, and §6.6 states the same-migration fence-ownership rule (K2 = 0113 branch (a); K3 = 0115 (a)+(b)+(c)).
- **The R-12/R-14 model is sound.**
  - Unrevoked rows never overlap, so at most one grant is in force at `now()`, and the evaluation stays unambiguous.
  - The approval guard refuses `valid_until <= now()` in the same transaction as the clamped insert, so no inverted grant can be created.
  - The overlap check uses the unclamped window (a superset), which is conservative.
  - §7.4's "lock the grant in force at `now()`" is correct.

**Notes:**
- **N-3 (optional):** a table `CHECK (valid_until IS NULL OR valid_until > valid_from)` on `staff_capability_grants`, as defence in depth.
- Carried to K2/K3, unchanged: C-K1-3 (a)–(c), N-1 and N-2.
