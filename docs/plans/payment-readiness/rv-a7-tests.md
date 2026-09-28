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

---

# Re-review: FH-6 A7 §(7) suite (`worktree-agent-a5b19b46582e95b75` @ `4b544f5`)

- Environment: a detached worktree and a private DB `igaming_lf_fh6` (admin create, runtime-role
  grants, migrated 1→106). Both have been removed. No sudo, no role or password changes.
- Results on `4b544f5`:
  - full `internal/payments` and `internal/withdrawal` suites, and the `internal/httpserver`
    withdrawal tests: **pass**;
  - `TestA7_*`, the `…SecondBlocksOnReceiptKey` R0 tests and the payout security-round tests,
    `-race -count=3`: **pass**;
  - pinned golangci-lint 2.9.0: 0 issues (untagged and `integration`).

## Required tests: status now

| # | Required (A7 §(7)) | Test at `4b544f5` | Outcome | Blocking point | Balance / rebuild | Verdict |
|---|---|---|---|---|---|---|
| 1a | Sweeper vs. callback vs. phase C, same **deposit intent** | none. Excluded from FH-6 by the orchestrator (owned by the double-credit fix) | n/a | n/a | n/a | **MISSING (assigned elsewhere)** |
| 1b | Same, same **withdrawal** | `a7_lockorder_integration_test.go:TestA7_1b_SweeperClaimVsCallbackPhaseC_SameWithdrawal` | yes: `succeeded`/`completed` exactly once | yes: both racers blocked by the withdrawal-row holder (`pg_blocking_pids`); no deadlock | yes / yes | **PRESENT.** The phase-C leg is the sweeper's own status apply, not a dispatch phase C. That is acceptable: both use the same `LockForPayoutEvidence` path. |
| 2 | Two deliveries of one event serializing on R0 | `adversarial_test.go:TestReceiveCallback_ConcurrentDuplicateCallbacks_SecondBlocksOnReceiptKey`; `webhook_replay_duplicate_integration_test.go:TestWebhook_ConcurrentDuplicates_SecondBlocksOnReceiptKey`; `replay_f7_integration_test.go:TestF7Payments_ConcurrentIdenticalReversalRedelivery_SecondBlocksOnReceiptKey` | yes: exactly one credit | **partial**: proves the second delivery waits on the *first delivery's* pid, but not that it waits at R0 rather than at the intent's L1 row lock (both appear as a transaction-id wait) | balance yes / rebuild **no** | **PRESENT, with a precision gap.** Condition A7-C1 below. |
| 3 | Deferred-receipt backstop vs. callback, same attempt | `a7_lockorder_integration_test.go:TestA7_3_DeferredReceiptAppliedVsFreshCallback_SameAttempt` | yes: `succeeded`, exactly one ledger posting | yes: both racers block on the intent holder | yes / yes | **PRESENT** |
| 4 | N1: sweeper deposit T2 vs. RG self-exclusion, same person | `a7_lockorder_integration_test.go:TestA7_4_N1_SweeperDepositReclaimVsSelfExclusion_SamePerson` | yes: the self-exclusion commits, and a rejected attempt has 0 provider calls | yes: both block on L0.4. The blocker key matches `rg.lockPerson` exactly (`hashtext('player_restrictions'), hashtext(person)`) | yes / yes | **PRESENT** |
| 5a | Mutant: RG gate moved after the parent lock turns a test red | `TestA7_5a_SweeperDepositClaim_RGGateBlocksBeforeParentLock`. It holds L0.4 and the intent row, and fails if the claim queues on the intent. | n/a | yes, this is the assertion itself | yes / yes | **PRESENT (permanent test)** |
| 5b | Mutant: `SKIP LOCKED` dropped turns a test red | `TestA7_5b_ClaimBatch_SkipLockedNeverWaitsOnALockedAttemptRow`. `claimBatch` must return within 500 ms, skip the locked row and claim the free one. | yes | yes (a timeout means it waited) | not applicable (no posting) | **PRESENT (permanent test)** |
| 5c | Mutant: receipt insert after an L1 lock turns a test red | none | n/a | n/a | n/a | **MISSING.** It belongs with the A7-TOMB-1 fix. |

## A7 status

**Still `PARTIALLY IMPLEMENTED`, owner `ledger-finance` + `payments`.** 5 of 8 required tests are
now present and substantive (1b, 3, 4, 5a, 5b), plus #2 with a precision gap. What remains before
`IMPLEMENTED`:
1. #1a, deposit-intent race, owned by the double-credit fix.
2. #5c together with the A7-TOMB-1 tombstone-branch fix and its race test.
3. **A7-C1:** #2 must pin the blocking point to R0. For example, assert that the waiter's
   `pg_stat_activity.query` is the `payment_provider_events` insert. Otherwise a regression that
   moved R0 after the intent lock (the 5c shape) would still pass #2. Also add
   `loAssertProjectionMatchesRebuild`.
4. ADR 0082 §1.6/§1.7 inventory rows for the as-built payment/withdrawal sequences: still owed. At
   `cb1330f` neither section mentions `payment_attempts`, T1p or R0.
5. A `ledger-finance` gate review of the deposit-side lock sequence, once items 1 and 2 land. The
   payout side is already reviewed.
6. Correct the ADR's status line from "NOT IMPLEMENTED" to "PARTIALLY IMPLEMENTED".

---

# FH-6 round 2 confirmation (`b7f84ec`)

Same environment as the payout confirmation in `rv-prh-i1-payout-ledger.md`.

- **A7-C1: closed.** All three #2 tests now assert that the blocked backend's query contains
  `INSERT INTO payment_provider_events` (`loBackendQuery`), and each ends with
  `loAssertProjectionMatchesRebuild`. Mutation check: moving the deposit receipt insert after the
  intent `FOR UPDATE` fails
  `TestReceiveCallback_ConcurrentDuplicateCallbacks_SecondBlocksOnReceiptKey` and
  `TestWebhook_ConcurrentDuplicates_SecondBlocksOnReceiptKey`. The F7 reversal test covers the
  separate reversal path, which that mutant does not touch.
- **§1.6/§1.7 as-built rows: present.** They cover `InitiateDepositAttempt`,
  `driveCreatedAttempt`, `ApplyReceiptEvidence`, `ClaimForDispatch` (T1p), `claimBatch` (with V1)
  and the withdrawal-side locks. I checked the `ApplyReceiptEvidence` row against the code at
  `b7f84ec`: R0 is inserted before the parent `FOR UPDATE`, which matches the row.
- **Status line: now `PARTIALLY IMPLEMENTED`. Confirmed.**
- **Still open before `IMPLEMENTED`:**
  1. #1a (deposit-intent sweeper/callback/phase-C race), owned by the double-credit fix.
  2. #5c for the tombstone branch, together with the A7-TOMB-1 fix. The mutation check above
     covers the main deposit receipt path only, not the tombstone branch.
  3. The `ledger-finance` gate review of the deposit-side sequence, once 1 and 2 land.

---

# A7 closure review (FH-3c, `92f5889`), by `ledger-finance`, 2026-09-28

The environment and runs are as recorded in `rv-fh3-ledger.md`, section "Re-review 1: FH-3c".

## Items that were still open

- **#1a** (`TestA7_1a_SweeperClaimVsCallbackPhaseC_SameDepositIntent`): **PRESENT, meets §(7).**
  - **Race:** the sweeper lease plus per-item claim, and a late callback, both blocked on the held
    `deposit_intents` row (`a7WaitAnyLockWaiter`, `loBlockingPIDs`, `loWaitBlocked`). The
    sweeper's own phase C follows the release.
  - **Outcome:** exactly one attempt succeeded and one disputed. The **unconditional**
    `ledgerDepositTxCount == 1` (C7) is now counted by ledger rows, keyed by `correlation_id`.
  - **Ends with:** `assertLedgerBalanced`, `loAssertBalanced` and
    `loAssertProjectionMatchesRebuild`.
  - The FIFO lock-queue assumption is documented in the test.
  - It passes `-race -count=3`. Mutants AID, PRE and RECK all fail it or its siblings.
- **#5c** (`TestA7_5c_TombstoneBranch_SecondIdenticalReversalWaitsOnReceiptInsert`):
  **PRESENT, and the mutant is killed.**
  - My mutant M5C takes `deposit_intents FOR UPDATE` before the R0 insert in the reversal
    tombstone branch.
  - It is killed by this test, and not by any unrelated failure.
  - The test also asserts no deadlock, one `applied` plus one `duplicate_effect`, exactly one
    tombstone, balanced, and rebuild.
  - Low L2 (non-blocking): the waiter is identified by query text only, not also by
    `wait_event_type = 'Lock'`.
- **Deposit-side gate review:** done in `rv-fh3-ledger.md` ("Ruling: deposit-side lock order",
  `076e42e`). The as-built sequence is conformant, and the 0107 index adds no cycle. FH-3c's
  changes add no lock:
  - F3's re-read is a plain `SELECT` under the already-held intent lock.
  - C2 is reconciliation-only, running under the stream's own snapshot and advisory lock.

## A7 status ruling: **IMPLEMENTED**

- All eight §(7) required tests are present and substantive, and each asserts outcome, blocking
  point and the balance/rebuild invariants: #1a, #1b, #2 (with A7-C1), #3, #4, #5a, #5b, #5c.
- The three mutants §(7) names (5a, 5b, 5c) are each killed by a permanent test.
- The §1.6/§1.7 as-built inventory rows are present.
- Both halves have their `ledger-finance` gate review: payout in `rv-prh-i1-payout-ledger.md`,
  deposit in `rv-fh3-ledger.md`.

A7's own closure rule is therefore met. **Owner action:** the ADR 0082 status line must be updated
from "PARTIALLY IMPLEMENTED" to "IMPLEMENTED", citing this section. That edit belongs to the ADR's
editor (`architect`/`payments`); I have not made it here.

The residuals do not affect A7 status:
- L2, above;
- FH-3 F3b, which is a CAS-noise issue and not a lock-order issue.
