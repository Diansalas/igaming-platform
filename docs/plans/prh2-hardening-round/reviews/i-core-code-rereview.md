# Code re-review — PRH-2 I-core (2026-09-28)

**Reviewer:** `code-reviewer`. The orchestrator recorded this review.

**Scope:** `bc73c24..9f5970c` (`90b4bc3`, `8a8412e`, `9f5970c`), excluding the merged main content. Tests ran on a private DB (`cr_ic_rr`), dropped afterwards.

## Verdict: READY WITH CONDITIONS

**Both High durability bugs are fixed and re-probed:**
- **StrandedClaim:** 0 deliveries within the lease, 1 delivery after it (`attempt_no=1`).
- **CancelledCtx:** `err=nil`, and 1 alert row written.

**The reviewer's mutants, re-applied against the fixed code:**

| Mutant | Result |
|---|---|
| MA: `failed` is always due | KILLED by `BackoffHoldsRetryUntilElapsed` |
| MB: escalation predicate reduced | KILLED by the 2 escalation tests |
| MC: `claim` returns true on conflict | KILLED 10/10 by `ClaimRaceIsDeterministic` (the old `TwoDispatchersOneClaim` still catches it only 1/10; keep it only as a smoke test) |
| **MB2**: escalation disabled entirely | **SURVIVED** (C-1) |
| **MG**: `freshReadCommittedRunner` platform-admin case removed | **SURVIVED** (C-2) |
| **MF**: principal case mapped to the tenant runner | **SURVIVED** (C-3) |

F-4 to F-13 are fixed. The reviewer agrees that IC-1 being tested only negatively is acceptable, with the follow-up already recorded under ALERT-DELIVERY-1.

## Conditions

| ID | Sev | Finding | Required change |
|---|---|---|---|
| C-1 | Medium | Escalation has no positive test, so escalation disabled entirely passes the suite. | Add an un-acked alert, advance past `escalate_after`, run a pass, and assert step 1 is `sent` (or `unrouted` when there is no step-1 route). |
| C-2 | Medium | The platform-admin branch of `freshReadCommittedRunner` is untested. Under MG a platform alert that was swallowed in-tx is silently dropped on Flush, with no `raise_failed`. | Add a Flush test through `NewPlatformAdminRunner` asserting `raised_by_scope = 'platform_admin'`. Make the cases explicit, and make an unknown `ScopeKind` an error. |
| C-3 | Low | The principal branch is untested. MF loses the acting-principal attribution (SR-1). | Add a Flush test through `NewPrincipalRunner` asserting `raised_by_scope = 'tenant_principal'`. |
| C-4 | Low | Stale-reclaim residuals: (a) the lease is measured from DB time against the app clock; (b) a `Deliver` slower than `ClaimLease` is re-delivered under a different idempotency key; (c) a stale claim at the final attempt gets one extra delivery. | Bound `Deliver` with a context timeout shorter than `ClaimLease`. Consider a stable per-step idempotency key. Record `dead` without delivering when a reclaim would exceed `MaxAttempts`. These can be I-wire or real-channel preconditions (HD-PRH2-4-OPS). |

**Local runs (not CI):**
- `-race -tags integration` for alerting, db and auth: ok, no DATA RACE.
- lint: 0 issues.
