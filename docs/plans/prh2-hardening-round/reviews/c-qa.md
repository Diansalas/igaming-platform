# PRH-2 C — QA review

_Reviewer: `qa`. Branch `prh2-c-dep-ref-validate` @ `fc0d18b`. Recorded verbatim by the orchestrator._

VERDICT: PASS WITH CONDITIONS (branch prh2-c-dep-ref-validate @ fc0d18b, read-only; scratch archive at $S/qa_c)

Short version: all tests are green, race-clean and stable at count=10. The M2 and K1 kill claims are real. One pre-merge defect: the callback-vs-sync race test is vacuous. It never exercises the race it claims to test.

Commands run (S=/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad; PRIV_DB=qa_c_gate PRIV_SRC=$S/qa_c; all with set -o pipefail)
1. `git archive fc0d18b | tar -x -C $S/qa_c`, then `priv_db.sh`. It rebuilt the DB fine.
2. `priv_test.sh -race -tags integration -count=1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./internal/payments/...`
   - Result: `ok internal/payments 484.773s`, exit=0.
   - The log has no FAIL, panic or DATA RACE.
3. `priv_test.sh -race -tags integration -count=10 -run 'TestDepRef|TestDepSync|TestDepositAdapterCall|TestCompareProviderAmount|TestMockDeposit_Echoes|TestINVDEP1_BackstopBranch|TestKSCASDiscrim|TestRVLF_N2|TestRVLF_N4' ./internal/payments/`
   - Result: `ok 170.068s`, no FAIL, panic or DATA RACE.
4. Probe, scratch copy only: I logged the callback's result inside the race test.
   - In all 5 iterations the callback returned `Disposition:deferred_unresolved`, with Status empty and DepositIntentID Nil.
5. Mutation spot-checks, scratch copy only, restored afterwards:
   - K1 (`drive.go:159`, `if false && !engaged`) makes TestKSCASDiscrim FAIL.
   - M2 (`orchestrator.go:716`, backstop branch disabled) makes TestINVDEP1_BackstopBranch FAIL.
   - Both claimed kills are reproduced.
6. Dropped the private DB. No roles, credentials or the repo were touched.

Findings, ranked

F1 (HIGH, PRE-MERGE): the callback-vs-sync race test is vacuous.
- Location: dep_ref_validate_integration_test.go:584-621, TestDepRef_CallbackVersusSyncSuccessRace_PostsExactlyOnce.
- The callback goroutine is launched from inside the adapter call (phase B). At that point the reference is not yet bound on deposit_intents, and `loadDepositIntentByProviderRef` (orchestrator.go:~430) looks up by that column. So the callback is always `deferred_unresolved` and does nothing.
- The test then only re-proves that a lone sync success posts once, which TestDepSyncAmount_MatchPostsOnce already covers. The comment "whatever the interleaving" overstates it.
- The 5 iterations add no coverage. The test also does not assert the callback's disposition, so it cannot notice this.
- The plan lists "the callback-vs-sync race" as a required test for C, so this is a coverage gap.
- The test is stable and race-detector clean. Repeating it will not force the interleaving, so the answer to your question is: this needs a forced interleaving.
- Required fix, either of:
  a. Force it deterministically, as the backstop test does. Hold the intent row lock (or a psp_clearing projection row) from a second transaction. Run phase C in a goroutine and wait until pg_stat_activity shows it blocked. Then deliver the callback so it queues behind the lock, release, and assert one posting and no dispute. Reuse `loHoldProjectionRow`, `loStartRacer` and `loWaitBlockedByAnyOf`.
  b. Or at minimum, deliver the callback after the reference is bound, assert `Disposition==applied` and a single posting, and relabel the test honestly as sequential.
- Also assert the callback disposition in whichever form is kept.

F2 (MEDIUM, pre-merge or a recorded deferral decision): no reconciliation-impact test, and a possible reconciliation blind spot.
- Neither new T10 reason has a test for the payment_statement recon stream.
- `reconciliation/payment_statement.go:941-951` special-cases only `multiple_success_for_intent` (pay_captured_unposted). Every other disputed reason is silently excluded from status comparison, so the three new reasons are also excluded.
- Consequence for `sync_amount_mismatch`: the provider reports success but the platform parks without posting. Recon will never flag it, and the only record is the audit row. That is the same "captured, not posted" shape that §28.9 treats as a P1.
- Zero ledger drift is trivially true, because no ledger tx is written (the tests assert this). It is asserted only as SUM(debit)==SUM(credit), via `assertLedgerBalanced`. No test runs the projection-vs-ledger diff.
- A test is needed:
  - Park via each new reason.
  - Run the payment_statement stream with a succeeded line.
  - Assert the explicit expected result: either excluded by design, or flagged.
  - Run the drift recompute and assert zero.
- The plan puts this QA F5 reconciliation test in D (PAY-POLL-AMOUNT-1), not C. If it is deferred to D, the orchestrator must record that decision. The blind spot for sync_amount_mismatch should go to ledger-finance and the orchestrator as a routed item, because the plan's own C text does not mention it.

F3 (MEDIUM): provider-callback and retry coverage for parked attempts is thin.
- There is no test of a late or retried verified callback after a park, for either the (unbound) reference or the conflicting one.
- Only the invalid-reference park has a sweeper-invisibility test (dep_ref_validate_integration_test.go:231). The sync_amount_mismatch and provider_reference_conflict parks have none.
- There is no fault-injection test for partial failure or rollback. "tx commits" is asserted only indirectly, by reading committed state.
- No idempotent re-drive test exists for a parked attempt.

F4 (LOW): the "no T10 produces a ledger tx" assertion is complete for two reasons and thin for the third.
- `assertParkedNoMoney` asserts: no ledger tx at all, balance 0, ledger balanced, intent ambiguous, no cascade, and no next_action_at.
- It covers invalid_provider_reference (7 cases, all outcomes, plus the S-9 empty case at :177) and sync_amount_mismatch (5 cases, :344).
- provider_reference_conflict is asserted weaker. Deposit-attempt conflict (:401) and payout-attempt conflict (:458) check `depLedgerTxCount==0` and balance==0 plus ledger balanced; the payout-attempt test checks only the first two. The intent conflict (:492) checks only the ledger tx count.
- The conflict tests do not use assertParkedNoMoney, so they skip the next_action_at, no-cascade and intent-status checks (except in :401).
- Suggest routing :458 and :492 through assertParkedNoMoney. Not blocking.

F5 (LOW): test-hygiene issues.
- `close(started)` in the race-test script panics if the script runs twice (for example a cascade). Use sync.Once.
- invdep1_backstop_branch_integration_test.go calls `slog.SetDefault` and restores it in Cleanup. That is fine unless the test is made parallel.
- `time.Now().Add(time.Minute)` appears at dep_ref_validate_integration_test.go:391,394 only as lease values written into a fixture. That is acceptable under T-1 and is not a wall-clock assertion.

Answers to the questions
1. Financial categories:
   - Normal: covered (TestDepSyncAmount_MatchPostsOnce).
   - Duplicates and idempotency: covered mainly via the conflict tests and the backstop test; exactly one posting is asserted.
   - Concurrency: the backstop test is real, the callback race test is NOT (F1).
   - Retries: partial (sweeper invisibility for one reason, F3).
   - Partial failure and rollback: no fault injection (F3). Commit-on-park is asserted.
   - Settlement: sync posting is covered.
   - Reconciliation: missing (F2).
   - Provider callbacks: weak (F1, F3).
   - Authorization and tenant isolation: covered (TestDepRefConflict_CrossTenantSameStringHasNoEffectOnTheOtherTenant, which follows the S-9/LF-6 wording).
   - Auditability: covered (exactly one `payment.attempt_disputed` audit per park, with reason, provider_amount and bound_to_operation metadata, plus a no-leak probe on the raw reference).
2. Determinism:
   - No time.Sleep anywhere in the three new files.
   - No wall-clock assertions.
   - The backstop test IS a forced interleaving: it uses a held projection row, waits on pg_stat blocking, and asserts the P1 log lines.
   - The callback race test is not forced and needs forcing (F1).
3. Weakened tests: no. The RVLF N2/N4 fixture edit only adds `Amount: 5000, AssetCode: "EUR"`, which matches the 5000-EUR intent. Without it, those tests would have hit T10 sync_amount_mismatch before reaching the tombstone/T13 branch. Both tests are green in the count=10 run (included via TestRVLF_N2 and TestRVLF_N4) and still exercise their original branch.
4. Reconciliation: no such test exists, and one is needed or must be formally deferred to D (F2).
5. "No T10 produces a ledger tx": asserted for every reason, but only fully for two of the three (F4).

Pre-merge: F1 (required). F2: add the test, or get an orchestrator-recorded deferral plus routing of the sync_amount_mismatch recon blind spot to ledger-finance. F3, F4 and F5 can follow in the same branch or in PRH-2 D.

Files: /home/user/igaming-platform (the branch was not modified). Reviewed in $S/qa_c/internal/payments/{dep_ref_validate_integration_test.go,invdep1_backstop_branch_integration_test.go,ks_cas_discrim_integration_test.go,amount_evidence_test.go,rvlf_i1_regression_integration_test.go,drive.go}.
