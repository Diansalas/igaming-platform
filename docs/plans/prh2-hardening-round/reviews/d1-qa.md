_Reviewer: `qa`. Branch `prh2-d1-poll-amount` @ `ea17d68`. Recorded verbatim by the orchestrator._

> **Orchestrator decision (2026-10-03):** F1 (the poll-versus-callback race test is only probabilistic) is made **pre-merge**, consistent with the C review (QA C F1). It is a test-only addition using the existing `deliverCallbackOnce` helper. F2(a) (note the CAS-before-injection dependency in a comment) is folded into the same commit as a comment-only change. F3 and F4 are not pre-merge. This QA review did not re-run the mutants.

D1 QA GATE VERDICT: PASS WITH CONDITIONS (prh2-d1-poll-amount @ ea17d68, base 564c515). All tests are green, stable and race-clean. There is one MEDIUM finding, the same class as my C F1, and no hard blockers.

Commands run (S=/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad; PRIV_DB=qa_d1_gate PRIV_SRC=$S/qa_d1, built from `git archive ea17d68`; pipefail; DB dropped afterwards)
1. `priv_test.sh -race -tags integration -count=1 -timeout 40m -p 1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./internal/payments/...`
   - Result: `ok 415.588s`, exit=0, zero FAIL, panic or DATA RACE.
2. `priv_test.sh -race -tags integration -count=10 -timeout 40m -p 1 -run 'TestPoll|TestFC4|TestF3SM|TestSweepCASNoise|TestDeferredReceipt|TestParkFaultInjection|TestDepRef|TestDepSync|TestRVLF_N2|TestReceipt_|TestA7_3_|TestDepositDisputeTerminalReasons|TestPost_EmptyProviderTxID' ./internal/payments/... ./internal/ledger/...`
   - Result: payments `ok 466.9s`, ledger `ok`, zero FAIL, panic or DATA RACE.
3. `-race -count=50 -run TestPollSuccess_RacingCallback ./internal/payments/`
   - Result: `ok 8.574s`, clean.
   - Passing is not evidence of interleaving coverage (F1 below).
4. I read the evidence file (36 mutants, 34 killed, 2 classified survivors). I did not re-run the mutants this time.

Findings, ranked

F1 (MEDIUM, recommended pre-merge; it can be a recorded follow-up if the orchestrator accepts the residual): the headline "poll vs callback, matching success" race is probabilistic only.
- Location: poll_amount_integration_test.go:466-511, TestPollSuccess_RacingCallback_PostsExactlyOnce.
- It starts two goroutines released by `close(start)` and asserts the outcome.
- It does not force an interleaving or record which order occurred. -count=50 only samples the scheduler. In C, my F1 was a vacuous race test; this one is not vacuous (both operations can resolve a bound reference), but it is not forced.
- The deterministic building block already exists in this branch: `deliverCallbackOnce` (poll_amount_integration_test.go:85-97) runs a verified callback inside the poll's own QueryStatus, so the callback commits between the poll's provider call and its phase C. It is used for mismatched, omitted-amount, declined and noise cases (lines 522, 546, 564, 580, 618, 1036, 1070).
- Gap: no test combines `deliverCallbackOnce(Succeeded, 5000)` with a matching `pollSuccess(ref, 5000, "EUR")`, which is the exact "poll then lands on an already-succeeded attempt" cell. The reverse order, poll first then callback giving duplicate_effect, is at most covered sequentially elsewhere.
- Fix (small): add one forced test using `deliverCallbackOnce` plus a matching pollSuccess, asserting no sweep errors, state succeeded, exactly 1 posting, no dispute audit, and the deferred and receipt state. Keep the goroutine race test as a smoke test and relabel it as such.
- The concurrent both-queued-at-the-intent-lock case needs a lock-held blocker plus pg_stat waiting, as in the INVDEP1 backstop test. It is optional, because either order resolves to a sequential cell already covered.

F2 (LOW): the fault injection is deterministic, with two caveats.
- Location: poll_amount_integration_test.go:700-949.
- Mechanism: `LOCK TABLE <audit_log or deposit_intents> IN SHARE MODE` is held by a separate tx. `loHoldWith` blocks until the lock is acquired and ready. The victim sets `SET LOCAL lock_timeout='1ms'`.
- The victim's next write needs ROW EXCLUSIVE, which conflicts with SHARE. The lock is held for the entire attempt, so 55P03 always fires. The test asserts the SQLSTATE explicitly, then asserts that the pre-failure snapshot is identical after the rollback, then re-drives cleanly.
- It is deterministic. There are no sleeps. The timeout is a trigger, not a time assertion, so T-1/T-2 are fine.
- Caveat (a): the test asserts that 55P03 occurred but not that the dispute CAS had already run when it did.
  - By construction the audit insert and the intent update come after the CAS in `parkDepositAttempt`, so that is true today.
  - If the order is ever changed, the rollback assertion could become trivial. Mutants D-FI-1 and D-FI-2 prove the error is not swallowed, not the order.
  - Suggestion: assert the CAS ran, for example via a SAVEPOINT-free probe, or note the dependency in a comment.
- Caveat (b): `pollVictim` and `phaseCVictim` re-implement the lock, re-read, then `applyStatusEvidence` / `applyDepositCallResult` sequence of `RunOnce` and phase C in a test-owned tx. They test the same functions but not the real wrapper transaction. Acceptable.
- This closes my C3: partial failure and rollback are now covered for all four C reasons, at two injection points each, and for both poll reasons.

F3 (LOW): "audited on every poll" for Missing is checked with one Missing poll only (poll_amount_integration_test.go:217-265). A second consecutive Missing poll producing a second `payment.attempt_poll_amount_unconfirmed` audit, and a bounded audit rate, is not asserted. The liveness half is covered (see answer 5). Minor.

F4 (LOW): the drift check is `loAssertProjectionMatchesRebuild` plus `assertLedgerBalanced`, not `RunLedgerVsProjection`. It is the same invariant and is asserted explicitly in `assertPollParked`. D2 asserts `RunLedgerVsProjection` directly. Acceptable.

F5 (LOW, informational): D-DRAIN-4 (the receipt.go callback-path T4/T9 drain) is a pre-existing surviving mutant, tracked by PAY-RECEIPT-T4-DRAIN-TEST-1. Confirm that registry item is open. D-ECHO-2 is correctly classified as equivalent (the COALESCE and the earlier park make the argument unobservable).

Answers to your questions
1. Financial categories for D:
   - Normal: poll success posts under the bound reference (TestPollReference_EmptyOrMatchingEcho...).
   - Duplicates and idempotency: exactly one posting asserted everywhere; redelivery gives duplicate_effect; the F-C4 ledger-key conflict parks without an error loop.
   - Concurrency: only the probabilistic race (F1). Forced callback-during-poll cells cover mismatch, declined and noise cases.
   - Retries: re-drive after rollback (park fault injection); the Missing-liveness re-poll.
   - Partial failure and rollback: fault injection (F2).
   - Settlement: poll success T6-bound and T9 Pending drain post exactly once.
   - Provider callbacks: the deferred receipt tests, callback-during-poll, and the redelivery check.
   - Authorization: no new surface in D, and the earlier tenant isolation tests are unchanged.
   - Auditability: audit counts, adapter_outcome, and no-raw-echo (D-F2).
2. Determinism: no `time.Sleep` and no wall-clock assertions. The fault injection is deterministic (F2). The race test is only probabilistic, so a forced variant is needed (F1).
3. Changed existing tests: none is weakened.
   - TestRVLF_N2_SweeperGo: the fixture only gains `Amount: 5000, AssetCode: "EUR"`. An echo-less poll success is now "Missing" and never reaches the tombstone branch. The test comment says so, and the assertions are unchanged. This is the same fixture-adaptation pattern I accepted in C. It still exercises the tombstone T10 on the sweeper's success path.
   - TestDepRef_SyncSuccess_CallbackDuringPhaseB: the `t.Logf` became assertions plus a redelivery check. This strengthens it, and it still exercises the phase B deferred callback then sync success path.
   - depRefProvider `setStatus` and `onQuery`: additive test-double hooks. Existing callers that don't set them keep the same behaviour (nil hook, nil map).
4. QA C1 follow-through: done.
   - `assertDeferredReceiptResolved` asserts exactly one stored receipt with disposition deferred_unresolved at receipt.
   - It asserts `resolved_at IS NOT NULL` (via `unresolved == 0`), `resolution == applied`, and `attempt_id == the attempt`.
   - It is used in the phase-B sync test and in the two sweeper tests (T6-bound poll success, T9 Pending).
   - Exactly one posting and balance 5000 are asserted in each, with redelivery giving duplicate_effect in the sync case.
5. Missing: yes. TestPollAmount_Missing_NeverPosts_StaysLiveAndAudited covers three variants: no amount, no asset, neither.
   - It asserts the attempt stays ambiguous, there is no terminal_reason, no dispute audit, one unconfirmed audit, and the attempt stays scheduled.
   - A later poll with full evidence posts exactly once (1 deposit tx, balance 5000).
   - The declined-attempt Missing path is covered too, with a single audit that is not repeated on a further sweep.
6. No T10 produces a ledger tx: `assertPollParked` asserts no ledger tx of any kind, balance 0, ledger balanced, projection equals its rebuild, intent ambiguous, no cascade, no next_action_at, and a sweeper no-op.
   - It runs for poll_amount_mismatch (7 variants), poll_reference_mismatch and the poll tombstone.
   - The phase C reasons are covered by the fault-injection re-drive (ledger tx count unchanged).
   - Recon impact (my C F2) is now handled in D2, which I reviewed as PASS WITH CONDITIONS.
   - D1 pins the exact reason strings D2 matches on, in TestDepositDisputeTerminalReasons_ExactStringsAreAContract, with the D-REASON-2 mutant killing drift. That closes my D2-F1 concern except for a single end-to-end "real poll park then recon flags" test. That test is still worth adding once D1 and D2 are on the same branch.

Pre-merge: F1 recommended. If it is not done, record it as a follow-up with the forced-interleaving test described above. All other items are non-blocking.

Relevant paths: /home/user/igaming-platform (untouched; branch prh2-d1-poll-amount). The reviewed archive is $S/qa_d1. The files are internal/payments/poll_amount_integration_test.go, internal/payments/dep_ref_validate_integration_test.go, internal/payments/rvlf_i1_regression_integration_test.go, internal/payments/deposit_terminal_reasons_test.go and docs/plans/payment-readiness/evidence/prh2-d1-mutation-kill.txt.
