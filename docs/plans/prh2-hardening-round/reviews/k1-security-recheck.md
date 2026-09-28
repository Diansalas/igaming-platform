# Security re-check — PRH-2 K1 (integrated head `5a27be6`, 2026-09-28)

**Reviewer:** `security` (the gate). The orchestrator recorded this review.

**Scope:** the K1 delta `4ebe923..5a27be6`, excluding merged main content. Private DB, dropped afterwards.

## Verdict: ACCEPT WITH CONDITIONS

- 2 pre-merge conditions: RC-1 and RC-2.
- 4 hard gates before K2: K2-G1..G4.
- No launch blocker beyond TM-7 and TM-10.

## Mutants

| Mutant | Result | Killed by |
|---|---|---|
| M1: advisory-lock call removed | KILLED | A23 |
| M2: lock taken after the check | KILLED | A23 |
| M3: isolation guard removed | KILLED | R12a |
| M4: clamp removed | KILLED | A22, A10Ext and others |
| M5: I-5 re-read removed | KILLED | I5 |
| M6: R-7 trigger check removed | KILLED | A2, and the K1C3 layered test |
| M7: `OLD.revoked_at` check removed | KILLED | A13 |
| M8: overlap check removed | KILLED | A23 |
| **M9: grants `acting_read` tenant predicate removed** | **SURVIVED** | see K2-G2 |
| **M10: `financial_acting_session_valid()` status check removed** | **SURVIVED** | see K2-G3 |
| M11: tenant actor status check removed | KILLED | A8 |
| **M12: platform actor status check removed** | **SURVIVED** | see RC-2 |

**Closed:**
- K1-C1..C4;
- L-1 and L-2;
- the CG020 exactness;
- R12-b, R12-c and R12-d;
- clock consistency (everything on `now()`; Go no longer defaults the time);
- A-10 after the clamp;
- I-5;
- A-18 is built (with gaps; see K2-G1).

## Pre-merge

- **RC-1 (Medium; security corrects its own R12-a text):** the isolation guard must accept **only** READ COMMITTED.
  - Probe: a SERIALIZABLE approver took its snapshot, then a READ COMMITTED approver committed; the SERIALIZABLE approver then took the lock, missed that grant, and committed. Result: two overlapping unrevoked grants. SSI only tracks SERIALIZABLE transactions against each other.
  - This is unreachable in production today (the R-11 one-pending index), but the trigger is R-12's named binding control.
  - Fix: refuse anything other than `read committed`, and add a SERIALIZABLE-refused test.
- **RC-2 (Medium, test only):** M12 survived. A suspended platform_admin requesting, approving or revoking through `WithPlatformAdmin` is not tested. Add A8 cases expecting CG001.

## Before K2 merges (hard gates)

- **K2-G1:** A-18 has false negatives, proved by plants:
  - (a) a negative `… IS NULL` mention counts as a guard;
  - (b) `FOR ALL USING (true) WITH CHECK (true)` is not detected;
  - (c) every SELECT NULL arm is blanket-exempt.

  Fix: guards must be positive; flag `true` arms; SELECT policies are allowed only for the §6.2 allowlist. Add a dynamic complement: count the rows visible to a valid acting session on every public table, against the allowlist.
- **K2-G2:** acting-tenant scoping masks itself. M9 survives, and "widen acting to any tenant" is masked by it. Add an A-4 case where an acting session reads another tenant's grants and sees 0. Add a rolled-back-DDL layered test that widens `acting_read` and asserts the setter still refuses another tenant's grant.
- **K2-G3:** M10 survives. Add an A-8 acting case: suspend the grantee after the grant, then `WithPlatformActingInTenant` must give CG020.
- **K2-G4:** K2-P2..P4 carried forward: A-1 and A-12 against K2 operations; the call-site argument pin; use-time re-checks with a mutant each; lock the grant in force at `now()`.

## Rulings

- **Scratch-DB methodology: SOUND**, under two rules:
  1. Remove only what makes the state unreachable, and pin the removed control independently. R-11 is pinned by A11, and 0034 by the I4 tests.
  2. Restore everything else before exercising the path. I-5 cases 2 and 3 run the approval with `staff_users` RLS still disabled (L-b).

  The technique proves the mechanism, not reachability; reachability rests on R-11 and 0034, both of which are pinned.
- **Evidence:**
  - "widen acting to any tenant", disclosed as masked, is not acceptable as disclosure only (K2-G2).
  - "Drop the in-tx status re-read": M11 killed, M12 is RC-2, M10 is K2-G3.
  - "Drop the grant function from one acting policy" is vacuous in K1 and mandatory in K2.

## Low

- **L-a:** the grant-time R-13 re-check should bound by the requested `v_req.valid_from`, not the clamped value.
- **L-b:** I-5 cases 2 and 3 should re-enable RLS before the approval.

**Housekeeping:** about 10 orphaned `cap0112*` scratch DBs with no sessions were found. They are not the reviewer's; cleanup is to be done by their owner.
