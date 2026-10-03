_Reviewer: `code-reviewer`. Branch `prh2-d1-poll-amount` @ `dc8130d`. Recorded verbatim by the orchestrator._

# Code re-review: PRH-2 D1 CR-5 (`prh2-d1-poll-amount` @ dc8130d, delta 788b873..dc8130d)

**Verdict: READY.** CR-5 is closed: the forced test now fails by itself when its callback does not happen. The new cleanup guard caused no false failures in 70 runs under `-race`, and the final full `-race` pass is green. I accept skipping Z4b.

I reviewed a `git archive` export of dc8130d and did not edit the worktree; the export matched the archive byte for byte after the mutant runs. Private DB `cr_d1_rv3` was built fresh with `priv_db.sh` and dropped at the end (`dropped cr_d1_rv3`). There were no role or credential changes.

## Delta scope (confirmed)

The delta changes only `poll_amount_integration_test.go` and the evidence file. No production `.go` file changed.

## Your checks

1. **Forced test assertions** (`TestPollSuccess_CallbackCommitsDuringTheProviderCall_PollIsADuplicateNoOp`, `poll_amount_integration_test.go:~492-520`). All three are present:
   - It fails if the `fired` channel is not closed.
   - It requires `last_evidence_kind == 'callback'`. A poll that posted by itself leaves `query_status`, and a matching poll on an already-succeeded attempt writes no evidence kind, so `'callback'` survives only when the callback really came first.
   - It requires exactly one `payment_provider_events` row for the reference, and that row must have `disposition_at_receipt = 'applied'`.
2. **Shared guard.** `deliverCallbackOnce` (around lines 86-110) registers a `t.Cleanup` that fails the test if `fired` was never closed. The channel is closed with `defer close(fired)` inside `once.Do`, so it is closed exactly once, and only after `rvCallback` returns. The guard covers all 8 call sites with no edits at the call sites.
3. **T-FORCED-HOOK-OFF re-applied, in two forms.** The forced test **itself** now fails under both:
   - **HOOK-OFF-A:** the hook fires and closes `fired`, but never sends the callback. The forced test fails on `last_evidence_kind="query_status", want "callback"` (line 516), so its own assertions catch it even when the channel is closed.
   - **HOOK-OFF-B:** the hook is never installed. The forced test fails on "the forced callback never fired" (line 507), and the cleanup guard fails every other test that uses the helper (line 98).

   Both are KILLED. This matches the implementer's D-FORCED-HOOK-1.
4. **No false failures from the guard.**
   - Each of the 8 call sites runs in its own test or `t.Run` subtest with its own environment. So each hook is installed once per `t`, and the cleanup is scoped to that subtest; no second call replaces an unfired hook.
   - Every call site goes through `QueryStatus`, either `e.poll` or `RunOnce`.
   - `depRefProvider.QueryStatus` runs the hook **before** it chooses between an override and the MOCK. So the hook also fires in the "transport failure" case of `TestSweepCASNoise_*`, where the MOCK returns an error.
   - No current caller legitimately skips `QueryStatus`.
   - **Stability:** I ran all 7 tests that contain the 8 call sites with `-race -count=10 -p 1`. Result: 70 PASS, 0 FAIL, no "never fired", no DATA RACE.
5. **Z4b skip: accepted.** The `poll_count == before+1` assertion kills Z4, the "no reschedule" mutant that CR-1 was about. The exact delay is a stricter, optional property, and leaving it unpinned is fine.

## Commands and results

- **Mutants** (`crd1_mut.py`, each reverted): HOOK-OFF-A and HOOK-OFF-B, both KILLED, with the forced test failing for the intended reasons.
- **Stability run:** `-race -count=10 -p 1` on the 7 tests that use the hook: 70 PASS, exit 0.
- **Final pass:** `set -o pipefail`; `-race -tags integration -count=1 -p 1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./internal/payments/...`.
  - payments ok (453 s), exit 0, 0 FAIL lines, no DATA RACE.

Logs are in the scratchpad: `crd1c_count10.log`, `crd1c_race.log` and `crd1_mut_HOOK-OFF-{A,B}.log`.

## Files

- `/home/user/igaming-platform/.claude/worktrees/agent-a1924f400143860c9/internal/payments/poll_amount_integration_test.go`: `deliverCallbackOnce` at around lines 86-110; the forced test at around 492-530.
- `/home/user/igaming-platform/.claude/worktrees/agent-a1924f400143860c9/docs/plans/payment-readiness/evidence/prh2-d1-mutation-kill.txt`
