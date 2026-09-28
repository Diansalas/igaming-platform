# Ledger-finance review — PRH-2 K1 (2026-09-28)

**Reviewer:** `ledger-finance`. The orchestrator recorded this review.

**Scope:** `40647bc..0f34d36`, reviewed via `git archive`. Private DB, dropped afterwards.

## Verdict: ACCEPT WITH CONDITIONS

K1 introduces **no** path to any money table.

**The acting-session money probe** used a valid G-P2 acting session on tenant X, whose money rows were seeded:

| Attempt | Result |
|---|---|
| INSERT `ledger_transactions` | Refused, 42501 |
| INSERT `ledger_entries` | Refused, P0001: the entries trigger cannot see the account |
| INSERT `ledger_accounts` | Refused, 42501 |
| UPDATE `wallet_balance_projection` | 0 rows affected |
| SELECT X's money rows | 0 visible |

Every policy on the ledger, wallet, projection, deposit, attempt and withdrawal tables keys on `app.tenant_id` or `app.player_account_id`, so an acting session is denied by default. 0112 adds no acting policy on any of them. The §6.6/§6.7 fences are correctly owned by K2 and K3, not K1.

**Focus items:**
- **Grant in-force:** evaluated at `now()`, which is correct for execution.
- **FOR SHARE support:** present.
- **The G-T/G-P2 flows, R-1..R-13 and `decided_txid`:** correct.
- **The `grantee_person_id` snapshot:** sound, because it is immutable per 0034 and required non-NULL. It is **grant-approval evidence only**.

**The four "masked" mutants:**
- 5 and 9-Go are genuinely masked.
- 7: LF considers it masked by RLS. **Security ruled otherwise (K1-C3); the stricter ruling stands.**
- **10 is a live survivor.** A re-revoke that rewrites the revoke record was accepted with the mutant applied, and A13 still passed.

## Conditions

| ID | Sev | Condition | When |
|---|---|---|---|
| C-K1-1 | Medium | Add a re-revoke case to A-13 (CG012, record unchanged), and reclassify mutant 10 as killed by it. This is the same finding as security's K1-C4. | Before K2 (already required pre-merge by security) |
| C-K1-2 | Low | Fix ADR 0099's migration numbers (K1 = 0112, K2 = 0113, K3 = 0115). State the rule: the migration that adds any acting permissive policy on `ledger_transactions`, `ledger_entries`, `ledger_accounts` or `wallet_balance_projection` must, in the same migration, first create the §6.6 and §6.7 fences. K2 carries branch (a); K3 carries (b)/(c), with the `withdrawal.go:1442`/`:1539` keys. | Before K2 |
| C-K1-3 | Binding on K2/K3 | (a) The execution-time LF-11 floor uses live `staff_users.person_id`, read `FOR SHARE`, never the snapshot. (b) K2's B-7 and K3's C-11 prove `FOR SHARE` on grants and staff rows works from tenant **and** acting sessions inside the real executor, and that a revoke committed first is re-read as revoked. (c) In-force is evaluated at `now()`; any "as of" use needs `(revoked_at IS NULL OR at < revoked_at)`. | K2/K3 |

**Notes:**
- **N-1:** the "as of" revoke semantics (above).
- **N-2:** once K2 adds acting SELECT on `ledger_accounts`, the entries trigger's account lookup will succeed. K2's A-4/B-16 must then re-assert that a non-governed entry insert is refused.
