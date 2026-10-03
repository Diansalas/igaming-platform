_Reviewer: `code-reviewer`. Branch `prh2-d1-poll-amount` @ `788b873`. Recorded verbatim by the orchestrator._

> **Orchestrator decision (2026-10-03):** CR-5 (the forced poll-vs-callback test passes when its forced callback never happens, shown by mutant `T-FORCED-HOOK-OFF`) is made **pre-merge**. It contradicts the QA confirmation (`d1-qa-confirmation.md`), which stated the test "cannot pass vacuously"; for the hook-off case it can, so that statement is corrected here. Z4b (reschedule into the past survives; pin `next_action_at > now()`) is optional and not pre-merge; it is added to PAY-POLL-ECHO-HARDENING-1.

# Code re-review: PRH-2 D1 final (`prh2-d1-poll-amount` @ 788b873, delta ea17d68..788b873)

**Verdict: READY WITH CONDITIONS.** D1-CR-1, D1-CR-2 and D1-CR-3 are all closed. The production code is unchanged apart from comments, and the final `-race` run is green.

There is one new condition, in the QA F1 additions. The "forced" poll-vs-callback test passes even when its forced callback never happens (CR-5). The fix is a one-line assertion in that test.

I reviewed a `git archive` export of 788b873 and did not edit the worktree; the export matched the archive byte for byte after the mutant runs. Private DB `cr_d1_rv2` was built fresh with `priv_db.sh` and dropped at the end (`dropped cr_d1_rv2`). There were no role or credential changes, and I touched nothing shared.

## Production code: comments only (confirmed)

`git diff ea17d68..788b873 -- '*.go' ':!*_test.go'` touches only `poll_evidence.go:5-8` and `sweeper.go:535-537`. Both hunks are comment text, now citing ADR 0095 §36.1. No executable line changed.

## Earlier conditions

| Item | Status | How I checked |
|---|---|---|
| D1-CR-1 (the Missing reschedule was not asserted) | **Closed** | The test now asserts `poll_count == before+1` (`poll_amount_integration_test.go:~232-239`). I re-applied **Z4** (Missing is not rescheduled) and it is now **KILLED** by `TestPollAmount_Missing_NeverPosts_StaysLiveAndAudited`. |
| D1-CR-2 (N2-sweeper checked only "disputed") | **Closed** | `rvlf_i1_regression_integration_test.go:~2000` now asserts `TerminalReasonTombstonePrecedesSuccess`. My variant D-N2-V changes the fixture echo from 5000 to 4000, so the attempt would park as `poll_amount_mismatch`. It is **KILLED** by the new reason assertion. |
| D1-CR-3 (comments cited §34.8) | **Closed** | Both comments now cite §36.1 and say the order is "not literally" §34.8's. |

**Is poll_count enough for D1-CR-1?** Yes, for what CR-1 was about. It proves `RescheduleNonTerminal` ran, and it is the counter that backoff growth and the registered PAY-DEPOSIT-ESCALATION-1 depend on. The implementer's reasoning holds: the default backoff (30 s) is earlier than the 60 s lease, so comparing `next_action_at` against the lease value would not work.

There is one residual, Low and optional. My mutant Z4b reschedules into the past (`s.backoff(...).AddDate(0,0,-1)`), and it **survived**. The delay value itself is still not pinned. A cheap ordering assertion would kill it without a wall-clock bound: `next_action_at > now()` read in the same query.

## QA F1 additions (2bf6ca4)

**CR-5 (Low–Medium, test vacuity, condition).** `TestPollSuccess_CallbackCommitsDuringTheProviderCall_PollIsADuplicateNoOp` (`poll_amount_integration_test.go:~479`) never checks that the callback it forces actually happened.
- **Mutant:** T-FORCED-HOOK-OFF makes `deliverCallbackOnce` return before installing the `onQuery` hook, so no callback is delivered.
- **Result:** this test still **PASSES**, because the poll then succeeds and posts on its own. Every assertion in the test holds for "poll alone" too: `succeeded`, one posting, balance 5000, no contradiction audits, zero unresolved receipts. The mutant was killed only through other tests that share the helper (`TestF3SM_*`, `TestPollDeclinedAttempt_*`, `TestSweepCASNoise_*`).
- **Failure scenario:** a later change that stops `QueryStatus` from being called, or that breaks the hook, leaves this test green. The succeeded×succeeded cell is then not exercised, while the test's name says it is.
- **Fix:** assert evidence that the callback was applied first. For example, check that the attempt's `last_evidence_kind == 'callback'`. Or check that exactly one `payment_provider_events` row exists for `ref` with the applied disposition/resolution. Or both.

**Otherwise correct and simple:**
- The forced test injects the callback inside `QueryStatus`, where no transaction is held, so the interleaving is deterministic.
- The reverse-order test (poll succeeds, then the callback arrives) is sequential and asserts `duplicate_effect` with exactly one posting.
- The smoke relabel to `TestPollSuccess_RacingCallback_Smoke_SamplesSchedulerPostsExactlyOnce` is honest: the comment says it samples the scheduler and points to the deterministic tests.
- The F2a note on the fault-injection order (the audit and projection writes come after the dispute CAS) is accurate.

## Commands and results

**Mutants** (`crd1_mut.py`, one at a time, each reverted). `-run 'TestPoll|TestFC4|TestF3SM|TestSweepCASNoise|TestDeferredReceipt|TestParkFaultInjection|TestDepRef|TestDepSync|TestRVLF_N2|TestReceipt_|TestA7_3_'`, `-p 1`.

| Mutant | Result | Notes |
|---|---|---|
| Z4 | KILLED | |
| D-N2-V | KILLED | |
| Z4b | SURVIVED | optional residual above |
| T-FORCED-HOOK-OFF | killed by other tests | the forced test itself passed (CR-5) |

**Final race run:** `set -o pipefail`; `-race -tags integration -count=1 -p 1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./internal/payments/...`.
- payments ok (464 s), exit 0, 0 FAIL lines, no DATA RACE.

Logs are in the scratchpad: `crd1b_race.log` and `crd1_mut_{Z4-MISSING-NORESCHED,Z4b-RESCHED-NOW,D-N2-V,T-FORCED-HOOK-OFF}.log`.

## Files

All paths are under `/home/user/igaming-platform/.claude/worktrees/agent-a1924f400143860c9/`:
- `internal/payments/poll_amount_integration_test.go`: the forced test at around line 479 (CR-5); the Missing test at around 229-239.
- `internal/payments/rvlf_i1_regression_integration_test.go`: around line 2000.
- `internal/payments/poll_evidence.go`: lines 5-8 (comment) and line 108 (the reschedule).
- `internal/payments/sweeper.go`: lines 535-537 (comment).
