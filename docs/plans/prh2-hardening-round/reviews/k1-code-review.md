# Code review — PRH-2 K1 (2026-09-28)

**Reviewer:** `code-reviewer`. The orchestrator recorded this review.

**Scope:** `0f34d36`, excluding the merged main content. Tests ran on a private DB (`cr_k1_rv`), dropped afterwards.

## Verdict: NOT READY

**What is solid:** the DB core, including:
- the forced-actor resolver;
- the three-way distinct-Person check with the `grantee_person_id` capture;
- the restrictive fence;
- the sole setter;
- the down guard;
- the scratch-through-0112 helper;
- the static walkers, which skip dot-directories.

**What is missing:** the HTTP API has **no tests**. A 100-line HTTP probe found an audit-integrity bug and error-mapping defects.

## Findings

| ID | Sev | Finding | Required change |
|---|---|---|---|
| F-1 | High | This is security's K1-C1. A platform approve via tenant A's path on tenant B's request gave 200; the grant was created in B but audited with `subject_tenant_id = A`. The same applies to reject, cancel and revoke. | Scope every by-id statement to the path tenant (404 on mismatch), with HTTP tests. |
| F-2 | High | The forced actor columns are untested. Mutants MF-a (`requested_by_person_id`) and MF-b (`grantee_person_id`) **survived**, and those are exactly the columns R-4/R-8 read. | Add a DB test per forced column (mismatching supplied value → the resolver's value persists). |
| F-3 | Medium | 23505 (R-11/R-12) and 23514 map to 500. A duplicate pending request gives 201, then 500. | Map them to 409/422 with a refusal audit, or pre-check in the triggers. |
| F-4 | Medium | An expired, unrevoked grant blocks re-granting (the R-12 index is on `revoked_at IS NULL`), and a backdated `valid_from` is accepted. | **Architect decision** requested. |
| F-5 | Medium | The platform list endpoints ignore the path tenant. | Filter by the path tenant and test it. |
| F-6 | Medium | A-4's refusal subtests accept any error. `write_risk_rules` **passes with the fence dropped** (it hits a CHECK, 23514), and `read_login_attempts` depends on incidental rows. | Assert 42501, use a CHECK-valid row, and seed in the test. Re-run the fence mutants per table. |
| F-7 | Medium | The lifecycle audit rows lack the grantee, capability, reason and before/after. Reject writes `capability_grant.rejectd`. The setter lacks the request id, operation, refusal log and metric. | Fix all of these and cover them in A-12. |
| F-8 | Medium | The HTTP layer has no tests, and §19.1 overstates "end to end". | Add the A-1 K1 subset over HTTP, and reword. |
| F-9 | Low | `FOR ALL` policies and the missing service exclusion. This is security's K1-C2. | Per-command policies, plus a `pg_policies` pin. |
| F-10 | Low | The evidence counts are wrong (really 7 killed, 3 masked plus 1 Go variant), and several ADR mutants were not attempted. R-7 **can** be independently killed (CG011 vs 23514 with a temporary permissive policy). | Correct the counts and run or disclose the rest. |
| F-11 | Low | A-17 asserts only `err != nil`; A-14b is narrow; the A-16 argument pin is deferred to K2. | Assert CG099, widen A-14b, carry the pin to K2. |
| F-12 | Info | Duplicate scan wrappers. | Optional. |

**Reviewer's answers:**
- **R-1 masked:** accurate. An independent kill would need its own error code, which is optional.
- **R-7 masked:** accurate, but it is independently killable, so "masked" should become "killed".
- **Scratch helper:** correct.
- **Static walkers:** skip dot-directories correctly.

**Local runs (not CI):**
- `-race -tags integration` for db, capability, identity, audit, alerting, auth, httpserver and `cmd/platform-api`: all ok.
- lint: 0 issues.
