# Security review — PRH-2 K1 (2026-09-28)

**Reviewer:** `security` (the gate). The orchestrator recorded this review.

**Scope:** `40647bc..0f34d36`, excluding merged main content. The I-1..I-5 follow-ups were not reviewed. Tests ran on private DBs, dropped afterwards.

## Verdict: ACCEPT WITH CONDITIONS

- 4 conditions are pre-merge (K1-C1..C4).
- 4 must land before K2 (K2-P1..P4).
- 2 are Low.
- The launch flags TM-7 and TM-10 remain open.

| Mutant | Result |
|---|---|
| M1: C-1 exact shape, `platform_admin_principal_id IS NULL` dropped | KILLED |
| M2: `persons` removed from the fence loop | KILLED |
| M3: `staff_users` fence INSERT `WITH CHECK true` | KILLED |
| **M4: the R-7 trigger scope check removed** | **SURVIVED** (K1-C3) |
| **M5: the `OLD.revoked_at` check removed** | **SURVIVED** (K1-C4); the implementer's "masked" claim is wrong |

## Pre-merge

| ID | Sev | Finding | Fix |
|---|---|---|---|
| K1-C1 | Medium | The platform-scope routes are not bound to the path tenant. `CancelRequest`, `DecideAndGrant` and `RevokeGrant` select by id only, and the list queries are unfiltered. A platform approve of tenant B's request via `/tenants/A/...` succeeds, and is audited to tenant A's tenant-visible audit. That breaks audit attribution; it does not grant extra authority. | Add `AND tenant_id = $target` to all five queries (0 rows → 404). Add an HTTP test per verb. |
| K1-C2 | Medium | The RLS shape deviates from ADR §10: `FOR ALL` policies where per-command sets are required. The grants and approvals T/P predicates omit `app.platform_service_id IS NULL`, so a mixed platform_admin + service session reads 9 grant rows. A tenant INSERT into grants is refused only by the trigger. | Use per-command policies exactly per §10.3–§10.5, add the missing arm, and update the down migration. |
| K1-C3 | Medium | R-7 (HD-PRH2-2's core control) is not independently pinned. The layers are trigger (CG011), then RLS (42501), then CHECK (23514); tests fail only when all three are gone. **"Masked by RLS" is not acceptable for R-7.** | A2 asserts exactly CG011. Add a rolled-back layered test (widen RLS, then drop the CHECK; CG011 each time), plus a catalogue assertion on the RLS and the CHECK. |
| K1-C4 | Medium | The revocation record can be rewritten: with M5 applied, a re-revoke rewrote the reason, the timestamp and the actor, and A13 still passed. | Add a re-revoke case expecting CG012 and an unchanged row. Correct the mutant 7 and 10 write-ups. |

## Before K2 merges

- **K2-P1: A-18.** K1 may merge without it. The acting shape is unreachable in production at K1: the setter has zero callers. A probe of a valid acting session saw only its own staff and grant rows plus §6.2 reference data (add `alert_kinds` to the list). A-18 is a **hard gate on K2**.
- **K2-P2:** A-12 (audit content) and A-1 (the role × family × capability × action matrix at HTTP and DB level, including the K1-C1 cases).
- **K2-P3:** the §6.1 call-site static test for the first caller.
- **K2-P4:** use-time re-checks in the K2 transaction: grantee active, role eligible, tenant unchanged, live `person_id` = snapshot, each with its own mutant.

## Low

- **L-1:** add an in-migration guarded REVOKE/GRANT block to 0112.
- **L-2:** the setter refusal test asserts that the error comes from the open step (CG020).

## Rulings

- **`acting_grant_max_lifetime = 4h`:** ACCEPTED as a technical security default. It is a restrictive ceiling, fail-closed, changeable only by migration, and not a business value.
- **R-1 masked by R-4:** ACCEPTED as a genuinely equivalent mutant.
- **The `subject_tenant_rls` test change:** legitimate, not a weakening. Suggestion: assert exactly CG020 for cases (5) and (5e).
- **S-a (a tenant grantee's status and role cannot be verified at approval):** CONFIRMED as a residual. G-T grants may be indefinite, so the use-time re-checks (K2-P4) are the binding control.
- **S-b:** ACCEPTED, and to be made stronger:
  - enforce it in the DB (a `staff_users` trigger or guard), not only in the API;
  - pin that reactivation never restores a revoked grant;
  - include a role change to `platform_admin` and deletion or deactivation.
- **S-c:**
  - The snapshot is SOUND: 0034 makes `person_id` immutable, and the re-link probe was refused.
  - The G-P1 refusal is FAIL-CLOSED CONFIRMED (CG010, 409, no row).
  - The "view" option must not be adopted without a human decision and a security review.

**Scope limits:** code- and DB-level only. The HTTP findings come from code reading, because no HTTP harness exists yet for these routes.
