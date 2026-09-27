# RV A7-TESTS-1: ADR 0082 Amendment A7 §(7) required tests vs. HEAD

- Reviewer: `ledger-finance` (owner of A7 closure, with `payments`)
- Trigger: architect review `rv-prh-architect.md` §4.1, finding A7-TESTS-1 (`fbb779e`)
- HEAD inspected: `fbb779e`, read-only inventory. No tests were written or run for this note.
- Source of the requirement: `docs/decisions/0082-canonical-financial-lock-ordering.md`,
  Amendment A7 §(7). §(7) requires every harness test to assert the outcome, assert where the waiter
  blocks, and end with the ledger balance invariant.

## Status line

**Confirmed: ADR 0082 Amendment A7 is `PARTIALLY IMPLEMENTED`. Owner: `ledger-finance` (with
`payments`).** The ADR's own line still reads "`NOT IMPLEMENTED`" and should be corrected to that.

- **Implemented:** the lock order in production code for the payout paths. My payout re-review 2 in
  `rv-prh-i1-payout-ledger.md` verified T1p, T2/T12, phase C, status apply and the receipt path, and
  the 40P01 probe no longer reproduces. The sweeper lease uses `SKIP LOCKED`.
- **Not implemented:**
  - most of the §(7) harness (below);
  - the §1.6/§1.7 inventory update (neither section mentions `payment_attempts`, T1p or R0 yet);
  - conformance of the deposit reversal tombstone branch (architect finding A7-TOMB-1: R0 inserted
    after an L1 lock).

A7's own closure rule ("`IMPLEMENTED` requires the tests in (7) and a `ledger-finance` gate review
of the as-built lock sequence") is therefore not met.

## Required tests (A7 §(7)) against HEAD

| # | Required test | Status | Evidence |
|---|---|---|---|
| 1a | Sweeper lease + per-item claim racing a callback **and** phase C on the same **deposit intent** | **MISSING** | No test runs `Sweeper.RunOnce`, `claimBatch` or a deposit T2 per-item claim concurrently with `ApplyReceiptEvidence` or a phase C. All sweeper tests (`sweeper_integration_test.go`, `payout_dispatch_*_test.go`) are single-threaded. |
| 1b | The same on the same **withdrawal** | **MISSING** (partial analogue only) | `payout_dispatch_round3_test.go:TestPayoutDispatch_R3_LockOrderConsistentAcrossConcurrentEvidence_NoDeadlock` races phase C against a raw `withdrawal FOR UPDATE → attempt FOR UPDATE` transaction that imitates the receipt-path order. It involves no sweeper lease or claim, no real callback, and does not assert where the waiter blocks. |
| 2 | Two concurrent deliveries of one event serializing on **R0** | **PARTIAL** (outcome only) | `adversarial_test.go:TestReceiveCallback_ConcurrentDuplicateCallbacksOnlyOnePosts`, `webhook_replay_duplicate_integration_test.go:TestWebhook_ConcurrentDuplicates_ExactlyOnePosting`, and `replay_f7_integration_test.go:TestF7Payments_ConcurrentIdenticalReversalRedelivery` assert exactly one posting. None asserts that the loser blocks on the R0 `payment_provider_events` key (the `lockorder_harness_test.go` helpers `loWaitBlocked`/`loBlockingPIDs` exist but are unused here), and none ends with the balance invariant. |
| 3 | Deferred-receipt backstop racing a callback for the same attempt (parent → attempt → receipts, ascending id) | **MISSING** | `receipt_integration_test.go:TestReceipt_Unresolved_DeferredThenAppliedOnceReferenceKnown` and `rvlf_i1_regression_integration_test.go:TestRVLF_P8_DeferredReceiptAppliedByPhaseC` are sequential, with no concurrency. |
| 4 | **N1:** sweeper deposit T2 re-claim racing an RG self-exclusion write for the same person (serializes on L0.4, no deadlock) | **MISSING** | No payments test combines the sweeper T2 path with `rg.CreateSelfExclusion`. `rg_integration_test.go:TestConcurrentSelfExclusionAndEligibilityCheck_Serializes` covers RG only, not the deposit claim tx. `rg_enforcement_integration_test.go` is sequential. |
| 5a | Mutant: RG gate moved after the parent lock turns a test red | **MISSING** | No test would detect it (it depends on #4), and there is no evidence entry in `evidence/prh-i1-mutation-kill.txt` or any other evidence file. |
| 5b | Mutant: `SKIP LOCKED` dropped from the lease turns a test red | **MISSING** | No test holds an attempt row lock and asserts `claimBatch` skips it rather than waiting. No evidence entry. |
| 5c | Mutant: receipt insert after an L1 lock turns a test red | **MISSING** | No test and no evidence entry. A7-TOMB-1 shows the production tombstone branch already does this undetected, which confirms the gap is real. |

**Related tests present but not on the §(7) list:** `lockorder_integration_test.go:TestLockOrder_ConcurrentDepositAndDepositReversal_NoDeadlock`
(deposit vs. reversal callbacks); `payout_dispatch_fixround_test.go:TestClaimForDispatch_GateRunsUnderOuterLock`
(T1p L1-before-gate, which does assert blocking); `payout_dispatch_fixround_test.go:TestConcurrentClaimForDispatch_ExactlyOneWithdraws`;
`payout_dispatch_integration_test.go:TestConcurrent_DenyForComplianceVsClaimForDispatch_ExactlyOneWins`.

## Missing list (for the implementer; ledger-finance re-verifies)

1. #1a: sweeper lease/claim vs. callback vs. phase C, same deposit intent.
2. #1b: sweeper lease/claim vs. callback vs. phase C, same withdrawal.
3. #2 upgrade: add an R0 waiter assertion and a closing balance invariant to an existing
   duplicate-delivery test.
4. #3: deferred-receipt backstop vs. callback, same attempt.
5. #4 (N1): sweeper deposit T2 re-claim vs. RG self-exclusion, same person.
6. #5a, #5b, #5c: the three mutants, each recorded in `evidence/prh-i1-mutation-kill.txt`. 5c
   should be run together with the A7-TOMB-1 fix and its race test.
7. Non-test closure items: the §1.6/§1.7 inventory rows for the as-built payment/withdrawal
   sequences; fix A7-TOMB-1; then a `ledger-finance` gate review of the deposit-side sequence.
   The payout side is already reviewed.

Each new harness test must assert the outcome, assert where the waiter blocks (`loWaitBlocked`),
and end with `loAssertBalanced` + `loAssertProjectionMatchesRebuild`.
