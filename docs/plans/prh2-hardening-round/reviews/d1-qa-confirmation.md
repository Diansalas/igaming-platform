_Reviewer: `qa`. Branch `prh2-d1-poll-amount` @ `788b873`. Recorded verbatim by the orchestrator._

D1 QA CONFIRMATION at 788b873: PASS. My earlier F1 and F2(a) are closed, and I have no conditions left on D1. The F3, F4 and F5 informational notes from my ea17d68 review stand unchanged and are non-blocking.

Delta ea17d68..788b873 (`git diff --stat`): 5 files. Production changes are comment-only: sweeper.go and poll_evidence.go are comment rewording. The other changes are in poll_amount_integration_test.go, rvlf_i1_regression_integration_test.go and the evidence file. This confirms your own check.

Commands run (S=/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad; PRIV_DB=qa_d1b_gate PRIV_SRC=$S/qa_d1b from `git archive 788b873`; DB dropped afterwards)
1. D-FORCED-1 re-kill: I mutated `sweeper.go:488` (`case AttemptSucceeded:` replaced by an unreachable case, so the succeeded x succeeded poll handling is dropped) and ran `-run 'TestPollSuccess_|TestF3SM'`.
   - Result: FAIL in TestPollSuccess_CallbackCommitsDuringTheProviderCall_PollIsADuplicateNoOp, TestF3SM_MismatchedPollAgainstAnAlreadySucceededAttempt_AuditsTheContradiction and TestF3SM_TerminalMismatchAudit_NeverStoresARawEcho.
   - It was killed by the new forced test. The file was restored and verified byte-identical.
2. `-race -tags integration -count=10 -timeout 40m -p 1 -run 'TestPollSuccess_|TestPollAmount|TestRVLF_N2|TestParkFaultInjection|TestF3SM|TestDeferredReceipt|TestDepRef' ./internal/payments/`
   - Result: `ok 97.694s`, zero FAIL, panic or DATA RACE.
3. Full `-race -tags integration -count=1 -timeout 40m -p 1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./internal/payments/...`
   - Result: `ok 464.056s`, exit=0, zero FAIL, panic or DATA RACE.

Checks
- F1, forced test: TestPollSuccess_CallbackCommitsDuringTheProviderCall_PollIsADuplicateNoOp is a genuinely forced interleaving.
  - `deliverCallbackOnce` runs the verified callback inside the poll's QueryStatus (via the onQuery hook, `sync.Once`, no goroutines). The callback therefore commits after the poll's provider call and before its phase C. The poll must then land on a succeeded attempt.
  - If the sweeper never polled, the callback would never fire and the state assertion would fail. So the test cannot pass vacuously.
  - It asserts everything you listed:
    - no sweep errors;
    - state succeeded;
    - exactly 1 deposit tx for the intent and balance 5000;
    - zero audits for `payment.attempt_disputed`, `payments.poll_evidence_contradicts_terminal_attempt` and `payments.callback_amount_asset_mismatch_terminal`;
    - zero unresolved receipts;
    - ledger balanced and `loAssertProjectionMatchesRebuild`.
  - The re-kill above shows it detects removal of the handling.
- Reverse order: TestPollSuccess_ThenCallback_IsDuplicateEffectOnePosting is sequential. That is sufficient here, because a poll that commits before the callback leaves no state a concurrent timing could change.
  - Phase C commits under the intent lock, and the callback's own lock then queues behind it.
  - It asserts the poll succeeded, the callback disposition is duplicate_effect, 1 posting with balance 5000, no dispute, and a balanced ledger.
  - It lacks the projection-rebuild assertion that the forced test has. That is optional.
- Goroutine test: it is kept as TestPollSuccess_RacingCallback_Smoke_SamplesSchedulerPostsExactlyOnce. Its comment now says plainly that it only samples the scheduler and forces nothing, and it points to the forced tests. That is honest. It is stable under -race with count=10.
- F2(a): the NOTE comment sits directly above the fault-injection helpers. It states the dependency on parkDepositAttempt writing the audit and intent update after the dispute CAS, and tells maintainers to re-check the injection points if the order changes.
- CR-1: the Missing test now reads `poll_count` before and after the poll and asserts exactly +1. This strengthens the earlier next_action_at check, which the batch lease alone could have satisfied. The test still asserts the live ambiguous state, no posting, no dispute audit, one unconfirmed audit, and the liveness re-poll that posts once.
- CR-2: N2 (TestRVLF_N2_SweeperGo) additionally asserts `TerminalReason == TerminalReasonTombstonePrecedesSuccess`. This is a strict addition.
- None of these tests is weakened. All changes are additions or comment edits.

Relevant paths: /home/user/igaming-platform (untouched; branch prh2-d1-poll-amount). The reviewed archive is $S/qa_d1b. The files are internal/payments/poll_amount_integration_test.go (the forced test, reverse-order test, smoke test and NOTE comment), internal/payments/rvlf_i1_regression_integration_test.go, internal/payments/sweeper.go, internal/payments/poll_evidence.go and docs/plans/payment-readiness/evidence/prh2-d1-mutation-kill.txt.
