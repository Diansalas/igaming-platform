# Code re-review — PRH-2 K1 (integrated head `5a27be6`, 2026-09-28)

**Reviewer:** `code-reviewer`. The orchestrator recorded this review.

**Scope:** `0f34d36..5a27be6`, excluding merged main content. Private DB, dropped afterwards.

## Verdict: READY WITH CONDITIONS

Every High finding is fixed and independently re-verified:
- **F-1:** the path-tenant binding, with an HTTP test.
- **F-2:** the forced columns. MF-a and MF-b are **KILLED**.
- **F-6:** a fence-drop mutant on each of the 5 loop tables is **KILLED**.

| Finding | Status |
|---|---|
| F-3 | Code fixed; the **test is missing** (C-1) |
| F-4 | Fixed by the architect's ruling (R-12/R-14) |
| F-5 | Fixed |
| F-7 | Fixed for K1 scope (`rejectd` pinned by A-12). The setter's refusal log and metric carry to K2 (the setter has no callers). |
| F-8 | Fixed; HTTP coverage is adequate for K1 |
| F-9 | Fixed |
| F-10 | Mostly fixed (C-3) |
| F-11 | Fixed |

**A-20..A-23 are non-vacuous:**
- A-23's real race: MA-1 (lock removed) is **KILLED**, and the test passes `-race -count=10`.
- The R-11 index is dropped only in scratch DBs. That is sound, and the approval-side path is also reachable in production.
- A-20 and A-22 use real sleeps (1.5 s and 0.9 s). This is a minor conflict with the §14 clock guidance; the flake risk is low.

## Conditions

| ID | Sev | Finding | Required change |
|---|---|---|---|
| C-1 | Medium | The 23505→409 and 23514→400 mappings have no test. Mutant MC-1 **SURVIVED**. | HTTP tests: a double-submit gives 201 then 409, with a refusal audit; one 23514 case gives 400. |
| C-2 | Medium | ADR §8.3/§2/§11/A-12 say K1 provides `grant.reattested`, but no code writes it, and §19 says it is out of scope. | **Orchestrator decision:** defer it to STAFF-LIFECYCLE-1, amend the ADR, and label it NOT IMPLEMENTED. |
| C-3 | Low | A stray `=======` in the evidence file; the mutant count should read "10 required-class: 9 killed, 1 accepted equivalent (R-1)". | Fix both. |

**Local runs (not CI):**
- `-race`: db, capability, identity, audit, auth, httpserver (362 s) and `cmd/platform-api` all ok.
- lint: 0 issues.
