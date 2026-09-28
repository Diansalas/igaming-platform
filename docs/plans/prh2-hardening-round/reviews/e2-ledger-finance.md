# Ledger-finance review — PRH-2 E2 (LF-16), 2026-09-28

**Reviewer:** `ledger-finance`. The orchestrator recorded this review.

**Scope:** commit `9a25453` (`e2-prov-outbound-cred-1-legacy`, based on `cabca27`), exported with `git archive`.
The author's uncommitted worktree edits were not reviewed.

**Method:**
- Private DBs for E2 and for a `cabca27` control, both dropped afterwards.
- `pipefail`, with every log grepped for FAIL/panic.

## Verdict: ACCEPT WITH CONDITIONS

The E2 deletion is safe for INV-DEP-1, A7 and reconciliation.
- `orchestrator.go` changes by pure deletion (352 lines, 0 added).
- None of the deleted symbols had a live caller.
- Mutant parity holds.

The one pre-existing gap on the live path (C-1) does not block the E2 merge.

## Local runs (not CI)

At `9a25453`: `-tags integration` for `./internal/payments/...` and `./internal/reconciliation/...` were all ok.

| Mutant (`orchestrator.go` at 9a25453) | Result | Killed by |
|---|---|---|
| M1: INV-DEP-1 choke-point pre-check bypassed (`:728`) | KILLED | `TestINVDEP1_FL1_ApplicationChokePointCatchesItBeforeTheDBBackstop` |
| M3: X5 ledger-sentinel mapping removed (`:839`) | KILLED | `TestX5_LedgerBackstopMapping` |
| M2: the choke point's backstop branch no longer maps to a T10/T13d dispute (`:738`) | **SURVIVES** at both `9a25453` and `cabca27` (pre-existing) | none (C-1) |

## The three questions

1. **Mutant parity: holds.** The evidence's "still passes ≡ still kills" argument is sound. The reviewer's re-kills of M1 and M3 supply real kills for the two load-bearing sentinels. M2 behaves the same before and after.
2. **The deleted C4 tests leave nothing live uncovered.**
   - They exercised only the legacy chain, including the `deposit.multiple_success_refused` audit, which no longer exists in non-test code.
   - The live equivalent (a T10/T13d dispute plus `payment.attempt_disputed` in the same transaction) is covered by `inv_dep1_matrix_integration_test.go:404`, F-L1, C3, MC2 and X5.
   - The deleted QueryStatus test is replaced by a strictly stronger T6.
3. **The test-only bridge cannot mask a production regression.**
   - It writes money only through production primitives: `postDepositSuccess`, `finalizeDeclined`, `finalizeAmbiguous`, `setIntentAttempt`, `IdempotentInsert` and `audit.Record`.
   - There is no raw `ledger.Post`, no projection write and no trigger bypass.
   - Residual: it drives a shape that production no longer has. See the code review's E2-1.

## Conditions

| ID | Sev | Condition | When |
|---|---|---|---|
| **C-1** | MEDIUM (pre-existing, not E2) | Add a test that drives the choke point's backstop branch (`postDepositSuccessOrDispute` `:738-746`): the pre-check passes, then `postDepositSuccess` returns `ErrDepositIntentAlreadyResolved`, from its internal re-check or from the ledger index via X5, under a real race or a test seam. Assert: the T10/T13d dispute; exactly one `payment.attempt_disputed`; the `payments_deposit_intent_index_backstop_fired` P1 is logged; no second posting; the transaction commits. M2 must then be killed. Today a regression turning this race into a propagated error (a 500 and a retry loop) would pass every test. The 0107 unique index still prevents a double credit, so this is a liveness and evidence gap, not a money-safety gap. | Before I-wire wires ADR 0102 §8 row 2; recommended in C/D. Registry: INVDEP1-BACKSTOP-BRANCH-TEST-1. |
| C-2 | LOW | Stale doc comments naming deleted functions (`deposit_v2.go:9,69`; `drive.go:37`; `payment_deposit_simulation_handlers.go:58,164`). | With E2 (it overlaps code-review E2-5). |
| C-3 | INFO | Record in the E2 evidence file: LF re-killed M1 and M3, and M2 survives before and after (pre-existing, see C-1). | Orchestrator, at merge. |
