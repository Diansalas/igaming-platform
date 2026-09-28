# RV-FH3: independent code review of FH-3 (PAY-DOUBLE-CREDIT-1 / INV-DEP-1) and the FH-5 follow-ups

- Reviewer: `code-reviewer` (independent)
- Date: 2026-09-28
- Branch: `worktree-agent-adce273a3a5339f77` at `a927aed`
- Commits reviewed:
  - `8ce538c` (FH-3)
  - `109ef04`, `bdb9085`, `f43025c`, `5541b23`, `9e107ed` (FH-5 follow-ups)
  - `a927aed` (A7-TESTS-1 #1a)
- Spec: ADR 0095 §28
- Method:
  - I read the source and test diffs directly.
  - All probes and mutants ran in a detached worktree with a uniquely named private DB (`crfh3_95`),
    built with `priv_db.sh`. No sudo, role or password changes were made.
  - Every mutant used a byte-exact anchor (verified to occur exactly once) and was reverted after its
    run; `git status` was clean after every batch.
  - Survivors were re-run against the full package(s).
  - An `internal/httpserver` failure seen in the first pass (X1/X3) did not reproduce on re-run. It is
    treated as the known `TestResolutionIsolation_*` flake, not a kill.
- Baseline: `internal/payments`, `internal/reconciliation`, `internal/ledger` and `internal/idempotency`
  all pass under `-tags=integration`. That includes `TestINVDEP1_D/K`, `TestA7_1a` and all
  `TestMigration0107_*`.
- Environment note: at first, another agent's worktree was already occupying my usual path
  (`scratchpad/rvfh3`), so my `git worktree add` failed silently. My first baseline therefore ran in
  that agent's worktree. It was read-only on my side, but its result is discarded. I re-ran everything
  in my own worktree, `crfh3_95-wt`. Nothing of the other agent's was modified by me. I dropped my own
  stray DB, `rv_fh3_scratch`; the other agent's DBs are `rv_lf_fh3_*` and were not touched.

## Verdict: NOT READY (money path correct; spec deviations and test gaps must close first)

INV-DEP-1 itself holds.

- No mutant I applied, and no probe I ran, produced a second credit for an intent.
- The layered defence works, but only as a whole: the pre-check, the re-check and the ledger index each
  mask the others' removal.

What blocks is:

- two reconciliation deviations from §28.9 (for `ledger-finance` to rule on);
- missing tests that §28.12 requires;
- one C-condition claimed closed that is not (C5);
- a guaranteed compile conflict with the FH-6 branch.

---

## Findings, most severe first

### R1 (Medium, confirmed by reading; for `ledger-finance`): `pay_captured_unposted` fires when the statement line itself says the capture was reversed

`matchPayment` gates the new case on `providerSucceeded`, which is
`l.status == Succeeded || l.status == Reversed` (`payment_statement.go`). §28.9 says the kind is emitted
when "the statement line for it (if any) is `succeeded`", and that it clears when the PSP refunds.

Failure scenario:

1. The PSP captures twice.
2. The platform disputes the second capture (`multiple_success_for_intent`).
3. The PSP refunds that second capture and reports it on the statement as `reversed`. It does not send
   a separate `deposit_reversal` line, and no callback arrives, so no tombstone exists.

Recon then reports a standing captured-unposted exposure that has already been refunded. That is a
false positive on a P1-class report, and it can never clear. Mutant X15 (counting only `succeeded`)
**survives** the full reconciliation package, so neither behaviour is pinned.

### R2 (Medium; for `ledger-finance`): the exposure is only reported on runs whose statement contains the attempt's line

The emission lives inside `matchPayment`, which only runs for statement lines present in the current
run. §28.9 says the kind is "reported on **every** run until it clears (ageing …)", and "for each
attempt … the statement line for it (**if any**)".

Failure scenario: a daily statement carries the second capture's `succeeded` line once. On day 1 it is
reported. From day 2 onward it silently disappears from reconciliation, although nothing was refunded
or allocated, because no statement line for it exists in those runs. Nothing iterates the disputed
`multiple_success_for_intent` attempts independently of the statement.

### R3 (Medium): test gaps on the §28 money path and its reporting (§28.12 required items missing)

Each item below is a surviving mutant (full package):

- **X1 / X2 / X3 / X5 / X6: the three defence layers are each individually unpinned.**

  | Mutant | Layer removed |
  |---|---|
  | X1 | the wrapper pre-check |
  | X2 | the re-check in `postDepositSuccess` |
  | X3 | both application checks, leaving only the ledger index |
  | X5 | the `ledger.ErrDepositAlreadyPostedForIntent` → sentinel mapping in `postDepositSuccess` |
  | X6 | the ledger half of `resolvedForOtherDeposit` |

  All five survive `internal/payments` **and** `internal/httpserver`. The only kills anywhere in the
  suite come from direct-call tests, not from any behavioural test:
  - X7 (the attempts half of the predicate) is killed by `TestINVDEP1_Mutation1_*`'s truth table;
  - X4 (ledger sentinel routing) is killed by `TestMigration0107_LedgerBackstop_*` at ledger level.

  The P1 line `payments_deposit_intent_index_backstop_fired` is never asserted, neither its absence
  in normal T10/T13d nor its presence when the backstop fires. So "the choke point was bypassed" (a
  defect signal per §28.3 rule 3) has no test. §28.12's "drop the choke-point check" and "drop the
  re-check" kills are only met by the unit truth table.

  Fix:
  - capture logs in one T10 and one T13d behavioural test and assert that `…backstop_fired` is absent
    (this kills X1 and X3);
  - add one test that reaches the ledger index through `postDepositSuccessOrDispute`, for example with a
    pre-seeded deposit posting that carries a different key and has no succeeded attempt, and asserts
    `disputed` plus the backstop P1 (this kills X5 and X6).
- **MG4: trigger accepts ANY deposit `declined → disputed` reason.** It **survives**. §28.12 requires
  "a deposit `declined → disputed` with any other `terminal_reason` is refused (run against HEAD)". No
  such test exists. MG3 (refusing the T13d reason) is killed.
- **§28.12 "down refuses while a `pay_captured_unposted` row exists" has no test.** Also, the down
  migration restores the 0102 CHECK with a bare `ALTER TABLE … ADD CONSTRAINT`. §28.8 asks for it to be
  "wrapped like the index builds" with a clear runbook message. It will still fail closed, with a raw
  CHECK error, but not as specified.
- **Pre-flight refusal tests accept any `23505/23514/P0001`.** They do not assert which index refused
  or the runbook text, so removing the `DO … EXCEPTION` wrapper, or swapping the messages, would pass.
  MG1 and MG2 (dropping each index) are killed by these tests.
- **Reconciliation:**
  - X13 (reversal-line clearing removed): **survives**.
  - X14 (tombstone clearing removed): **survives**.
  - X16 (terminal_reason filter dropped, so every disputed attempt, including
    `reversal_tombstone_precedes_success`, is reported): **survives**. This is §28.12's "Precedence …
    no `pay_captured_unposted`" requirement at the recon layer.
  - Only X12 (emission removed) is killed.
- **§28.6.3 "a reversed deposit still occupies the INV-DEP-1 slot" has no test.** My probe
  (child succeeds, child is reversed, the declined parent's late success arrives) shows correct
  behaviour: parent `disputed`, balance stays 0, disposition `anomaly`. It should be pinned.

### R4 (Low): §28.3 rule 3's legacy refusal audit is not wired

`RecordDepositMultipleSuccessRefusal` has **zero callers**. The legacy `resolveAmbiguous` returns
`ErrDepositIntentAlreadyResolved` and posts nothing, which is correct. But the specified
"P1 plus a `deposit.multiple_success_refused` audit row in a separate tx" never happens. The legacy
chain is now labelled TEST-ONLY and has no production caller, so the impact is nil today. Either wire
it into the one legacy caller or delete it and amend §28.3.

### R5 (Low): the sweeper chooses T10 vs T13d from a stale attempt copy

`processViaQueryStatus` passes the attempt read *before* the provider call and the intent lock into
`applyStatusEvidence` → `postDepositSuccessOrDispute` → `applyMultipleSuccessDispute`, which picks T13d
or T10 by `attempt.State`. If a callback moves the attempt between the read and the lock, the CAS
fails and the tick errors, then retries. That fails safe, with no money effect, but it is a
self-resolving error loop. Re-read the attempt under the lock, as `ApplyReceiptEvidence` now does (H2).

### R6 (Low): the inverted P6 test dropped two adversarial assertions

`TestINVDEP1_Inverted_RVLF_P6_*` keeps the fixture, but drops two assertions from the original:

- the PAY-REV-1 "second distinct reversal of the same original → `ErrDepositAlreadyReversed`" check;
- the `reverses_transaction_id` identity check on the one real capture's reversal. It now checks only
  the balance.

PAY-REV-1 is still pinned by the `payrev1_*` tests, so coverage is not lost overall, but the inversion
is weaker than "same fixture, inverted assertion". The other inverted tests look sound:

- `Inverted_T13SecondCapture`: T13d, one posting, balanced;
- the idempotency fixture now uses distinct correlation ids, and its key-composition assertions are
  unchanged.

---

## Correctness review of the §28 implementation (what is right)

- **Choke point (§28.3):**
  - `postDepositSuccessOrDispute` evaluates `resolved_for_other(I, A, K)` under the caller's intent
    lock, with the §28.2 predicate verbatim.
  - The exact-redelivery exclusion works: X8 (own key counted) is killed by `TestINVDEP1_E/F`.
  - Check order is tombstone → INV-DEP-1 in the receipt, drive and sweeper paths. The swap is killed by
    `TestINVDEP1_Mutation6`.
  - T10 vs T13d selection is correct: X9 (inverted) is killed.
  - The receipt disposition is `anomaly`: X10 is killed.
  - The audit record is written: X11 is killed.
  - `deposit.second_capture_posted` is deleted.
- **Callers:**
  - receipt T7 and T13 cells, `drive.go` phase C and the sweeper poll all go through the wrapper;
  - T17 is `Touch` (next_action_at only, NULL for terminal rows) and so reaches only the sweeper path;
  - legacy calls `postDepositSuccess(nil attempt)` and gets the sentinel (see R4).
  - `grep` finds no other `TxDeposit` poster.
- **Migration 0107:**
  - Both indexes are built inside `DO … EXCEPTION`, which is RLS-safe.
  - I diffed the `payment_attempts_guard()` body against 0101 mechanically. The only non-comment changes
    are the T13d reason, the error text and the whitelist comment. No later migration had replaced the
    function, so nothing is silently reverted.
  - The down migration restores 0101's body verbatim (only comments differ) and restores 0102's CHECK.
    0107's CHECK is an exact superset plus `pay_captured_unposted`, and 0102 was the last redefinition.
- **Ledger sentinel:** it follows the 0092 pattern exactly: idempotency-key lookup first, and the
  sentinel only on `ErrNoRows` plus the named constraint. X4 is killed.
- **Reconciliation kind:** `MismatchKindPayCapturedUnposted` exists, the CHECK allows it, and emission
  is killed (X12). R1 and R2 cover its semantics.

## Status of my prior conditions C1–C6 (from re-review 2, `ad476d6`)

| C | Status | Evidence |
|---|---|---|
| **C1** (N5b race-only branch) | **CLOSED** | The misnamed test is renamed. `testHookBeforeReferenceConflictRecheck` is an **unexported** package var (`receipt.go:37`): nil by default, assigned only in `rvlf_i1_regression_integration_test.go` (with a `t.Cleanup` reset), never assigned in non-test code (`git grep` confirms), and not reachable from outside package `payments`. The package has no `t.Parallel()` tests, so the global carries no cross-test race today. Optional hardening: a static test asserting that no non-`_test.go` file assigns it. |
| **C2** (M7/N12) | **CLOSED** | N12 is killed by `TestRVLF_C2N12_DeferredAmbiguousBackstopAlsoRecomputesIntentProjection`. |
| **C3** (H2 / N15 / H2r) | **CLOSED** | H2r (re-read removed) is killed in this round's run. N15 is claimed killed by `TestRVLF_C2_DFR_*`. |
| **C4** (S-H1 allow-list / SH1a) | **CLOSED** | SH1a is killed. |
| **C5** (H1 rule 3 / H1b) | **NOT CLOSED, despite being claimed** | See below. |
| **C6** (F7 remainder) | **CLOSED** | The dead mapper 404 branch is removed, the stale simulate comment is fixed, the duplicated doc comment is merged, and the legacy chain is labelled TEST-ONLY. The one remaining `receiveDepositCallback` mention (`orchestrator.go:1288`) is historical context. |

On C5: removing `ev.RawOutcome = rawOutcome` from `applyReversalReceiptEvidence` (H1b) **survives the
full `internal/payments` package**. `TestRVLF_SecGapA3_ReversalFingerprintUsesRawWireOutcome` calls
`computeEventFingerprint` directly and compares against a delivery whose wire outcome was already
`succeeded`, so the normalized value and the raw value coincide. It never proves that the reversal path
sets `RawOutcome`.

Required test: deliver the same reversal twice, once `succeeded` and once `declined`-as-carrier.
Assert two distinct receipt rows, exactly one ledger reversal, and both wire outcomes in the audit
metadata.

## A7 helper duplication (`a7_1a_integration_test.go` vs the unmerged FH-6 branch)

`a927aed` (not yet merged) defines `a7HoldRow` and `a7WaitAnyLockWaiter` in
`internal/payments/a7_1a_integration_test.go`. FH-6 (`worktree-agent-a5b19b46582e95b75`, also not yet
merged) defines the same two names in `internal/payments/a7_lockorder_integration_test.go`, with
**different signatures**:

| Helper | `a927aed` | FH-6 |
|---|---|---|
| `a7HoldRow` | `(t, pool, tenantID, rowID uuid.UUID, table, name string)` | `(t, pool, tenantID uuid.UUID, table string, rowID uuid.UUID, name string)` |
| `a7WaitAnyLockWaiter` | polls `pg_stat_activity WHERE wait_event_type='Lock'` cluster-wide, then filters by `pg_blocking_pids` | polls `pg_locks NOT granted` joined to `pg_stat_activity WHERE datname = current_database()` |

Merging both branches produces a compile error (`a7HoldRow redeclared in this block`) in the package's
integration build. The argument order also differs, so this is not a mechanical rename.

Proposed reconciliation:

1. Treat FH-6's versions as canonical. Its waiter query is scoped to the current database, which
   matters on this shared multi-agent cluster.
2. Fold in `a927aed`'s one improvement: the explicit `ErrNoRows` "no %s row %s to lock" error in
   `a7HoldRow`.
3. Move both helpers into a neutral `internal/payments/a7_helpers_integration_test.go`, on whichever
   branch merges first (FH-6 is further along its review).
4. Rebase the other branch to delete its copies and adapt its call sites to the canonical parameter
   order (`tenantID, table, rowID, name`).
5. Run `go vet -tags=integration ./internal/payments/` after the second merge. Its compile failure is
   the gate.

## Required before FH-3 can be marked IMPLEMENTED

1. `ledger-finance` rules on R1 (the statement `reversed` status) and R2 (every-run reporting that does
   not depend on a statement line). Fix accordingly and pin with tests (kill X13–X16).
2. Close R3:
   - behavioural kills for X1, X3, X5 and X6 via backstop P1 log assertions;
   - the trigger any-reason test (kill MG4);
   - a down-refusal test, with the down migration's CHECK restore wrapped with the runbook message;
   - pre-flight tests that assert which index refused and the runbook text;
   - a §28.6.3 reversed-slot test.
3. C5: add the two-delivery reversal test (kill H1b).
4. R4: wire `RecordDepositMultipleSuccessRefusal` or delete it and amend §28.3. R5 (re-read under
   lock) and R6 are recommended.
5. Reconcile the A7 helpers before either branch merges.

Surviving and killed mutants, for the evidence file:

| Result | Mutants |
|---|---|
| Killed | X4, X7, X8, X9, X10, X11, X12, SH1a, H2r, N12, MG1, MG2, MG3 |
| Survived | X1, X2, X3, X5, X6, X13, X14, X15, X16, H1b, MG4 |

X1 and X3 survived both `internal/payments` and `internal/httpserver`. The ADR explicitly allows X2 and
X6 to survive as defence in depth only if each layer is pinned somewhere; today none is pinned
behaviourally.

---

## Re-review 1: FH-3c at `92f5889` (FH-3b underneath at `cb03868`), 2026-09-28

- Range reviewed: `a927aed..92f5889`, read as a direct diff of `internal/` and `migrations/`.
- Environment:
  - A detached worktree with a unique name (`crfh3c_5688-wt`) and a private DB (`crfh3c_5688`), built
    with `priv_db.sh`.
  - No sudo, role or password changes.
  - The worktree and DB were removed afterwards, and `git status` was clean after every mutant batch.
- Baseline: `internal/payments`, `internal/reconciliation`, `internal/ledger` and `internal/idempotency`
  all pass under `-tags=integration`.
- Every mutant below ran against the **full** package(s). MG4 rebuilt the private DB from the mutated
  `0107…up.sql`, then rebuilt it again clean afterwards.

### Verdict: READY WITH CONDITIONS (one Low, fail-safe residual for `ledger-finance` to rule on; nothing blocking)

Every item from my `95a1c34` review is fixed and pinned. Every requested mutant is now killed. The A7
helper collision is resolved, and I proved it compiles by performing the merge.

### Mutants (all requested, re-run)

| ID | Mutation | Result | Killed by |
|---|---|---|---|
| X1 | wrapper pre-check dropped | **KILLED** | `TestINVDEP1_FL1_ApplicationChokePointCatchesItBeforeTheDBBackstop` (backstop P1 must not fire) |
| X2 | re-check in `postDepositSuccess` dropped | **KILLED** | `TestINVDEP1_MC2_PostDepositSuccessInternalRecheck` (asserts the application sentinel, not the ledger one) |
| X3 | both application checks dropped | **KILLED** | `TestINVDEP1_FL1_*` |
| X4 | ledger sentinel routing removed | **KILLED** | ledger/0107 backstop tests |
| X5 | backstop → sentinel mapping removed | **KILLED** | `TestX5_LedgerBackstopMapping` |
| X6 | ledger half of `resolvedForOtherDeposit` dropped | **KILLED** | `TestX6_ResolvedForOtherDepositPredicate_LedgerHalfAlone` (a direct predicate test, which is acceptable because X1/X3 pin the behavioural layer) |
| X13 | recon reversal-line clearing removed | **KILLED** | `TestINVDEP1_C5_*` |
| X14 | recon tombstone clearing removed | **KILLED** | `TestINVDEP1_C5_*` |
| X15 | recon counts statement `reversed` as captured | **KILLED** | `TestINVDEP1_C5_*/own_line_reported_reversed` |
| X16a / X16b | terminal_reason filter dropped (with a statement line / with no line) | **KILLED** | `TestINVDEP1_X16_*` |
| X17 (new) | the every-run, no-statement-line emission removed | **KILLED** | `TestINVDEP1_C2_CapturedUnposted_StandsAcrossStatementWindow` |
| H1b | `ev.RawOutcome = rawOutcome` removed | **KILLED** | `TestINVDEP1_H1b_ReversalFingerprintEndToEnd_RawWireOutcomePreserved` |
| MG4 | the 0107 trigger accepts ANY deposit `declined → disputed` reason | **KILLED** | `TestMigration0107_T13tT13d_TerminalReasonTrigger_HEAD` and `TestMigration0101_T13t_*` (both now run at HEAD) |

The H1b test drives a real `declined`-carrier reversal through the callback path. It asserts that the
stored fingerprint equals the one computed from the raw wire outcome, and that it differs from the
fingerprint a `succeeded` delivery would produce. **C5 is closed.**

### Status of my previous findings

| Item | Status |
|---|---|
| R1 (statement status `reversed` counted as captured) | **Fixed.** The matched-line case now requires `PaymentStatusSucceeded`. A line whose own status is `reversed` clears the report for that run (X15 killed). A residual remains; see N-1. |
| R2 (only reported when a statement line exists) | **Fixed.** `checkUnmatchedAttempts` now reports disputed `multiple_success_for_intent` attempts on every run, deliberately without a date window (X17 killed). |
| R3 (test gaps) | **Fixed.** See the X-kills above. Also: the down migration wraps the CHECK restore in `DO … EXCEPTION` with runbook text and has a refusal test, and the pre-flight tests now assert the runbook message. |
| R4 (legacy refusal audit unwired) | **Fixed.** `InitiateDepositAudited` catches the typed `DepositIntentAlreadyResolvedRefusal` and writes the audit in a separate transaction. It is labelled TEST-ONLY. Minor: `TestC4_*` calls `RecordDepositMultipleSuccessRefusal` directly rather than going through `InitiateDepositAudited`'s own unwrapping. |
| R5 (stale attempt copy in the sweeper) | **Fixed.** The attempt is re-read under the parent lock in `sweeper.go` and in both `deposit_v2.go` call sites (`84af3d4`). |
| R6 (inverted P6 lost assertions) | **Fixed.** The PAY-REV-1 second-reversal and `reverses_transaction_id` assertions are restored (`1ffc904`). |
| C5 / H1b | **Closed** (see above). |

### A7 helper collision

- `internal/payments/a7_helpers_integration_test.go` at `92f5889` has **byte-identical** signatures to
  FH-6 `b7f84ec` for both `a7HoldRow(t, pool, tenantID, table, rowID, name)` and
  `a7WaitAnyLockWaiter(t, pool, exclude, timeout)`.
- `a7WaitAnyLockWaiter`'s body is identical, including the `current_database()` scope.
- `a7HoldRow`'s body differs only by the folded-in `ErrNoRows` "no %s row %s to lock" error and
  `fmt.Sprintf`, as proposed. This is a behaviour-preserving superset.
- **Compile proof:** in a throwaway detached worktree I merged `b7f84ec` into `92f5889` (no textual
  conflicts). The first `go vet` failed on `a7WaitAnyLockWaiter redeclared`. After deleting only FH-6's
  two copies from `a7_lockorder_integration_test.go`, `go vet -tags=integration ./internal/payments/` is
  **clean**, with no unused imports and no other collision. So the payout/FH-6 branch deleting its
  copies will compile. The throwaway worktree was removed and nothing was committed.

### New finding

**N-1 (Low, fail-safe; for `ledger-finance` to rule, non-blocking): a refund reported only as the
capture's own statement line with status `reversed` clears `pay_captured_unposted` for that run only.**

The clearing rule, `capturedUnposted` (no `deposit_reversal` line in this run, and no ledger tombstone),
has no durable memory of a statement-reported refund. So the next run, whose statement no longer
carries the line, re-reports the exposure. I confirmed this with a scratch probe using the recon test
harness:

```
run1 (own line reversed) captured_unposted=false; run2 (no line) captured_unposted=true
```

This errs toward over-reporting: a refunded capture keeps reappearing as an exposure. It never hides
real unposted money, so it is not a money-safety defect. Ops, however, would see a standing false
positive that can never clear, and that erodes the report's value.

Options, for `ledger-finance` to choose:
- (a) accept and document that a PSP must also send a reversal callback or line to clear the exposure;
- (b) persist the statement-reported refund durably, for example as a resolved receipt or reconciliation
  clearance row keyed by `(provider_id, provider_reference)`, and include it in `capturedUnposted`.

### Required to close the conditions

1. `ledger-finance` rules on N-1, and it is either fixed with a two-run test (the probe above) or
   documented as an accepted residual.
2. Optional: a test that drives `InitiateDepositAudited` itself end to end.
