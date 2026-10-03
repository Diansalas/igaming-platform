# PRH-2 C — QA re-review

_Reviewer: `qa`. Branch `prh2-c-dep-ref-validate` @ `c2d1fc1`. Recorded verbatim by the orchestrator._

RE-REVIEW VERDICT: PASS WITH CONDITIONS (prh2-c-dep-ref-validate @ c2d1fc1, read-only). There are no pre-merge blockers. I also did not run a mutation of my own on the new tests. This time I did not write the review file reviews/c-qa.md, and I did not touch the repo or any branch.

Commands run (S=/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad; PRIV_DB=qa_c2_gate PRIV_SRC=$S/qa_c2, built from `git archive c2d1fc1`; set -o pipefail; DB dropped afterwards)
1. `priv_test.sh -race -tags integration -count=1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./internal/payments/...`
   - Result: `ok 593.240s`, exit=0, zero FAIL, panic or DATA RACE.
2. First count=10 attempt with the default timeout: `-race -count=10 -run 'TestDepRef|TestDepSync|TestDepositAdapterCall|TestCompareProviderAmount|TestMockDeposit_Echoes|TestINVDEP1|TestKSCASDiscrim|TestRVLF_N2|TestRVLF_N4|TestA7_3_|TestReceipt_'`.
   - It hit the Go 10-minute default test timeout (a panic from the timeout, not a test failure).
   - My selection was broader than the C tests and included the INVDEP1 matrix and Receipt tests. Re-run below with a longer timeout.
3. The same command with `-timeout 40m`:
   - Result: `ok 902.073s`, exit=0, zero FAIL, panic or DATA RACE.
   - No flakiness across 10 iterations under -race.

Findings, in the order you asked

F1 (race test): resolved. Coverage is now honest.
- The implementer's reasoning holds, and it matches what I measured last time. The probe showed the callback is `deferred_unresolved` when it runs inside phase B.
- For a sync success, the reference is bound on payment_attempts and deposit_intents only in phase C's own commit (ApplySuccess).
- Under READ COMMITTED, a callback that starts while phase C's tx is open still sees no row. It would then be deferred and could not queue behind a lock.
- So the only reachable orderings are phase B (deferred) and after phase C (duplicate_effect). A lock-held forced interleaving, as in the backstop test, would add nothing here.
- The two tests drive those orderings sequentially and deterministically (no goroutines, no sleeps) and assert the disposition explicitly: `DispositionDeferredUnresolved`, then `DispositionDuplicateEffect`.
- Each also asserts exactly 1 deposit posting, balance 5000, no dispute audit and a balanced ledger.
- The test comment states the limit openly, which is correct. The earlier misleading "whatever the interleaving" claim is gone.
- The real concurrent double-credit hazard stays covered elsewhere: the INV-DEP-1 backstop test (killed by M2, which I verified last round) and TestRVLF_P8 for the Pending race.
- The mutant claim: I accept it.
  - Workstream C adds no code on the callback or posting path. The double-post guard is the ledger idempotency key plus the 0107 index, and the succeeded-attempt check.
  - C-CB-1 mutates the callback disposition, which is the nearest C-owned behaviour. The postings==1 assertions would also fail on a real double post.
  - It is not a mutant of the idempotency guard itself. That guard's own mutation-kill record belongs to the earlier workstreams (RVLF/INVDEP1).
  - I did not independently re-run C-CB-1. I only confirmed the record names two tests that assert disposition.

F3 (late callbacks, sweeper): resolved.
- `assertParkedNoMoney` now runs `Sweeper.RunOnce`, asserts no errors, and asserts the attempt is untouched (still disputed, UpdatedAt unchanged).
- Late callback after the conflict park: `applied` to the owning first attempt only, with 0 postings on the parked intent.
- Late callback after the mismatch park: `duplicate_effect`, with nothing posted and the attempt still disputed.
- A cascade-driven park returns no redirect or token.
- New tests, all green, now cover several earlier gaps:
  - the error path with an invalid reference;
  - the error path with a valid reference, which binds the reference and returns no redirect;
  - the missing-echo case, where the sweeper's next poll calls QueryStatus with the bound reference and posts once;
  - the mismatch park binds the reference and its audit carries adapter_outcome.

F4: resolved.
- The payout-bound and intent-bound conflict tests now route through `assertParkedNoMoney`.
- This adds the intent-status, no-cascade, next_action_at, sweeper-invisibility and no-redirect checks to both.

F5: resolved.
- `close(started)` and the goroutine are gone with the replaced test.
- No time.Sleep anywhere. `time.Now` remains only as lease values in a fixture.

Fixtures (c2d1fc1): none of the four tests is weakened.
- The change is test-only: a `refLessAmbiguousProvider` wrapper that blanks the reference on an Ambiguous result only. Nothing else in those tests changed.
- Affected tests: TestA7_3_DeferredReceiptAppliedVsFreshCallback_SameAttempt, TestINVDEP1_I_TimeoutThenFallbackThenLateOriginal, TestReceipt_Unresolved_DeferredThenAppliedOnceReferenceKnown, and TestReceipt_DeferredReceipt_PredatesSubmission_NeverApplied.
- Their premise is that the attempt is ambiguous and its reference is not known yet. T6 now binds a returned reference (`MarkAmbiguousFromSubmittingBindingRef`, LF F-C1), so the MOCK's ref-returning ambiguous result would have invalidated that premise. The wrapper restores the true real-timeout shape. That is the same state those tests had before C.
- The wrapper only strips the reference. The MOCK's internal attempt map still holds it, so the later callbacks in those tests still verify.
- All four are in the green full-suite run and in the count=10 run, which includes TestA7_3_, TestINVDEP1 and TestReceipt_.
- The N2/N4 fixtures from the first review are strengthened this round: N2 now also asserts the tombstone terminal_reason, exactly one dispute audit and the intent recomputed to ambiguous.

Conditions (non-blocking):
- C1 (LOW): the phase-B callback test only logs, via t.Logf, that the deferred receipt stays unresolved (`resolved_at IS NULL`) after the sync success. It does not assert it.
  - That receipt may become a false `pay_unresolved` in recon after the 24h horizon.
  - Route it with the D / PAY-RECON-PARKED-CAPTURE-1 work. If the receipt is meant to be resolved, add a test there. If not, record it as intended.
- C2 (carried): F2 (recon impact, plus the sync_amount_mismatch "captured, not posted" blind spot) is deferred to D as PAY-RECON-PARKED-CAPTURE-1, per ledger-finance's ruling. The deferral must be recorded as a decision.
- C3 (LOW): fault-injection tests for partial failure and rollback still do not exist for C. Commit-on-park is verified only indirectly, by reading committed state. It is acceptable to carry this into D.

Relevant paths: /home/user/igaming-platform (branch untouched). The reviewed archive was in $S/qa_c2/internal/payments/. The files are dep_ref_validate_integration_test.go, inv_dep1_matrix_integration_test.go, a7_lockorder_integration_test.go, receipt_integration_test.go, rvlf_i1_regression_integration_test.go, drive.go and attempt.go.
