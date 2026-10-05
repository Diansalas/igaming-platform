# PRH-2 final gate: timing-lane run on an idle environment (LOCAL EVIDENCE, not GitHub CI)

Date 2026-10-05, tree `cfa08d5` (E1 + K3 merged). Environment: one container, 4 vCPU, 16 GB RAM, Go 1.26.8, PostgreSQL 16 local; no agents or other test runs active (1-minute load average 0.5-1.7, the upper part being the previous repetition of this same lane); private orchestrator DB migrated to 0115. Commands are exactly the CI timing-lane commands (`-race -tags=integration -v -count=1`, one name guard per test), thresholds UNCHANGED (isoBound 500 ms etc.), five repetitions, run 1 on the long-lived orchestrator DB and run 2 on a freshly rebuilt DB.

## Result: NOT GREEN. The final-gate condition for this lane is NOT satisfied.

| Test | 4 CPU, 5 reps (stale DB) | 4 CPU, 5 reps (fresh DB) | `taskset -c 0-1`, 3 reps (fresh DB) |
|---|---|---|---|
| `TestStoreOutage_DoesNotPinPool` | 5/5 PASS | 5/5 PASS | not run |
| `TestStoreOutage_DoesNotPinPool_ProductionPoolSize` | 5/5 PASS | 5/5 PASS | not run |
| `TestResolutionIsolation_OneTenantStoreOutage` | 5/5 PASS | 5/5 PASS | 3/3 PASS |
| `TestResolutionIsolation_MultipleTenantsOutage` | 5/5 PASS | 5/5 PASS | 3/3 PASS |
| `TestResolutionIsolation_SimultaneousOnset_Bounded` | 5/5 PASS | 5/5 PASS | 3/3 PASS |
| `TestResolutionIsolation_ConnectionExhaustion` | 5/5 PASS | 5/5 PASS | 3/3 PASS |
| `TestResolutionIsolation_FinancialDuringOutage` | 5/5 PASS | 5/5 PASS | 3/3 PASS |
| `TestResolutionIsolation_NormalOperation` | **0/5 FAIL** | **0/5 FAIL** | **0/3 FAIL** |

`TestResolutionIsolation_FinancialDuringOutage` (the test that failed under a heavy run) passed 13/13 idle repetitions.

`NormalOperation` failure: only the 500 ms per-callback bound is violated, always on deposit callbacks (observed 501-555 ms; the test's own comment records a historical -race p100 of 400-590 ms at pool size 10). No other assertion in the test fails.

## Is it a PRH-2 / E1 / K3 regression? Not shown to be.
Same test, same idle box, same flags, different source trees (migrated DB is a superset of each tree's schema):
- `ecd2b74` (before E1/K3): 0/3 PASS.
- `94b4ae5` (pre-PRH-2 baseline, the FH-7 completion report): 1/3 PASS (one run 481.9 ms worst, two runs 501-511 ms).
- On 2026-09-28 the same lane passed 40/40 (`fh7-timing-lane-idle.txt`). The same code now fails on this box, so the margin is environment-dependent (the box was not characterized beyond the above). No test or threshold was modified; the cause of the change from 40/40 is not established.

## Consequences
- TEST-RESISO-RACE-1 stays OPEN: the idle-machine half was satisfied on 2026-09-28 but is NOT reproduced today for `NormalOperation`. It must not be reported as green.
- The bound must NOT be loosened to make this pass. A decision is needed (human/QA): characterize the environment, or review whether a 500 ms p100 admission bound is a sound design given a measured -race p100 of 400-590 ms (ADR 0094 owns the bound).
- GitHub CI half remains BLOCKED by CI-BILLING-1.
