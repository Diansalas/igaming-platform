_Reviewer: `code-reviewer`. Branch `prh2-d1-poll-amount` @ `54a55e3`. Recorded verbatim by the orchestrator._

# Code re-review: PRH-2 D1 final (`prh2-d1-poll-amount` @ 54a55e3, delta dc8130d..54a55e3)

**Verdict: READY.** The new check that the forced callback actually took effect is correct and simple. It cannot deadlock, hold a transaction, or fail a test that is working correctly. Mutant B is now killed by every test that uses the helper. With the in-hook check also removed, the T13 matching subtest still fails on its own new assertion. Stability (`-race -count=10`) and the final full `-race` pass are both green.

I reviewed a `git archive` export of 54a55e3 and did not edit the worktree; the export matched the archive byte for byte after the mutant runs. Private DB `cr_d1_rv4` was built fresh with `priv_db.sh` and dropped at the end (`dropped cr_d1_rv4`). There were no role or credential changes.

**Delta scope (confirmed):** only `poll_amount_integration_test.go` (+15 lines) and the evidence file changed. No production code changed.

## The helper change (`deliverCallbackOnce`, `poll_amount_integration_test.go:~107-116`)

- **No deadlock and no transaction held.**
  - The hook runs inside the adapter's `QueryStatus`, which the gate calls during phase B, when no transaction is open (the sweeper opens its phase C transaction only after the call returns).
  - The new read is a plain `SELECT state` through `depScan`, in its own short `WithTenant` transaction. It runs only after `rvCallback`'s own transaction has committed.
  - The previous hook already opened `rvCallback`'s transaction from the same place, so this adds one more short, lock-free read and no new ordering risk.
- **`t.Errorf`/`t.Fatalf` from inside the hook is safe.** `sweeper.go` and `gate.go` contain no goroutines (no `go func`, `WaitGroup` or `errgroup`). So for all 8 call sites the hook runs on the test goroutine, and `depScan`'s `t.Fatalf` on a query error is legal. The goroutine smoke test does not use this helper.
- **No false failures.**
  - The read filters by `tenant_id` and the `ref` that was queried, which is the attempt's bound reference, so it finds that one attempt.
  - The expected state follows directly from the callback outcome: Succeeded gives `succeeded`, Declined gives `declined`.
  - In the CAS-noise "transport failure" case, the hook still runs before the MOCK returns its error, the callback is applied, and the attempt is `succeeded` as expected.
  - The stability run below confirms this.

## Mutants (`crd1d_mut.py`, each reverted)

| Mutant | What it changes | Result |
|---|---|---|
| **B** | The hook fires but `rvCallback` is skipped. | **KILLED** by all 7 hook-based test functions, including every subtest: the forced test, both `TestF3SM_*` succeeded-state tests, `TestF3SM_TerminalMismatchAudit…` (both subtests), `TestPollDeclinedAttempt_ContradictionAuditedMatchingPostsT13` (both subtests), `TestPollDeclinedAttempt_MissingEvidence…` and `TestSweepCASNoise_*` (all 4 cases, transport failure included). All fail on the in-hook check (line 115): "the callback was not applied before the poll's phase C". |
| **B2** | B, plus the in-hook check disabled. Isolates the T13 subtest's own assertion. | **KILLED.** `…ContradictionAuditedMatchingPostsT13/matching_success_posts_under_the_bound_reference` fails on its new assertion (line 697): "applied declined-callback receipts = 0, want exactly 1". |

This confirms the implementer's D-FORCED-HOOK-2.

## Commands and results

- **Stability:** `-race -count=10 -p 1 -run 'TestPollSuccess_CallbackCommitsDuringTheProviderCall_PollIsADuplicateNoOp|TestF3SM_|TestPollDeclinedAttempt_|TestSweepCASNoise_'`: 70 top-level PASS, 0 FAIL lines, no DATA RACE, no "not applied" or "never fired" messages.
- **Final pass:** `set -o pipefail`; `-race -tags integration -count=1 -p 1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./internal/payments/...`.
  - payments ok (468 s), exit 0, 0 FAIL lines, no DATA RACE.

Logs are in the scratchpad: `crd1d_count10.log`, `crd1d_race.log` and `crd1d_mut_{B,B2-no-inhook-check}.log`.

## Files

- `/home/user/igaming-platform/.claude/worktrees/agent-a1924f400143860c9/internal/payments/poll_amount_integration_test.go`: lines ~107-116 (helper) and ~694-698 (T13 subtest).
- `/home/user/igaming-platform/.claude/worktrees/agent-a1924f400143860c9/docs/plans/payment-readiness/evidence/prh2-d1-mutation-kill.txt`
