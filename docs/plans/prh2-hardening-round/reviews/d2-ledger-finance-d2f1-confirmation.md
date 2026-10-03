_Reviewer: `ledger-finance`. Branch `prh2-d2-recon-parked-capture` @ `c09d82d`. Recorded verbatim by the orchestrator. The test skips used elsewhere (`TestStoreOutage_DoesNotPinPool`, `TestResolutionIsolation_`) are the timing-lane exception, run separately at the final gate. This run was deliberately targeted, not the whole reconciliation suite, because of low disk._

# Ledger-finance confirmation: PRH-2 D2F-1 at `c09d82d8151df6f935a8a2612c2bff3b4428290d`

## Verdict: ACCEPT WITH CONDITIONS. One pre-merge item, and it is ADR text only (PM-3)

The runtime rule is correct, the targeted tests pass under `-race`, and my re-kill of the mutant was killed.

## What I ran
- **Setup:** disk was 3334M free at the start, so I ran only the targeted set. I exported `c09d82d` with `git archive` and built a fresh private DB, `lf_d2r5_20261004`. No role or credential changes; I did not touch any shared cache, directory or other agent's DB. The lowest free space was 3258M, above the 3G line.
- **Targeted run** (`-race -tags integration -count=1 -p 1 -run 'TestD2_15|TestD2_P1_RuntimeRule|TestD2_14|TestD2_11' ./internal/reconciliation/`): ok.
  - TestD2_P1_RuntimeRule, TestD2_11, TestD2_14 and TestD2_15 all PASS;
  - FAIL 0, DATA RACE 0.
- **D2F1-BOUND-EMPTY-REF-STAYS-BOUND** (the condition reverted to `c == reasonBoundIfReferenced`, under `-race`): **KILLED**, 5 fails:
  - `TestD2_15/a_in_run_succeeded_line_for_R_gives_exactly_one_finding`;
  - `TestD2_15/b_reversal_line_naming_R_clears`;
  - `TestD2_15/b_tombstone_on_R_clears`;
  - `TestD2_P1_RuntimeRule`.

  The file was restored byte-identical (`cmp` against `git show c09d82d`).
- **Cleanup:** DB dropped (`dropped lf_d2r5_20261004`, first try); export deleted; 3294M free at the end.
- **Diff check:** the only logic change is in `captureClass` (`if c == reasonBound || c == reasonBoundIfReferenced`), plus comments. `disputeReasonClasses` is unchanged.

## Rulings

### 1. The reclassification: CONFIRMED
At run time, every bound reason now behaves as bound-if-referenced. That is what I ruled for the conflict park in PM-1, and it is the only consistent reading:
- a standing finding needs a reference to be keyed on and to clear on;
- checking on the empty reference produced a false positive that no reversal or tombstone could ever clear, and M1 only acknowledges, so nothing could resolve it either.

A finding that can never resolve is not acceptable evidence. The table keeps recording what each reason *is*, and the attempt row decides how it behaves. That is the right split.

### 2. The consequence (a ref-less bound-reason park has no finding once its statement period has passed)
- **ACCEPTABLE, on the MOCK only, under the B1 gate, until STANDING-1.** Neither behaviour is correct:
  - before the fix, the finding stood forever and could never clear, even after the PSP reversed;
  - after the fix, it drops out silently once the period passes.

  The before-state was not loud in any useful way: it was permanently wrong, and once true and false findings are indistinguishable, an operator stops reading them. The fix gives a correct finding in every run whose statement carries the line, and it clears correctly. The silent drop-out afterwards is exactly what STANDING-1 exists to close, and the B1 gate already bars any real PSP or non-MOCK statement source until STANDING-1 lands.
- **PM-3 (PRE-MERGE, ADR text only): extra wording is needed in §35.4.** The B1 gate's item 1 currently describes STANDING-1 as "standing coverage for unbound parks". It must say explicitly that STANDING-1 covers every disputed attempt the runtime rule treats as unbound, including ref-less parks with a bound reason (`callback_amount_asset_mismatch` and `multiple_success_for_intent` resolved by merchant reference). Until then, the exposure of such a park drops out of reconciliation silently once its statement period has passed. Its only remaining trace is the `payment.attempt_disputed` audit row, because the I-wire P1 alert does not exist yet. One or two sentences are enough. The new text in the runtime-rule section ("no standing finding until STANDING-1") does not reach the gate itself, which is the text that governs launch.
- **New binding item, PAY-CALLBACK-MISMATCH-BIND-1 (payments, before the first real PSP or non-MOCK source):** the better root fix is on the payments side.
  - The callback T10 that writes `callback_amount_asset_mismatch` should bind the callback's reference when it passes `providerref.Validate` and the LF-6/F-C4 binding check, exactly as the `sync_amount_mismatch` park already does with `bindRef`. The park then holds R and gets the bound rule, including the standing finding.
  - `applyMultipleSuccessDispute` should get the same treatment. That gap predates D2.
  - Either this item or STANDING-1 closes the hole. Both remain behind the B1 gate.
- **B4 (MA020 widening):** must apply the same runtime rule. MA020's tombstone-only clearing keyed on the attempt's reference has the same "empty reference never clears" defect for ref-less parks. Count them by the attempt row, as `captureClass` does, so `player_open_payment_exposure` does not report something that can never clear.

### 3. Tests: ACCEPTED
- **`TestD2_P1_RuntimeRule`:** for every reason in `payments.DepositDisputeTerminalReasons()`, including the expanded prefix, it checks the run-time class with and without a reference. Bound and bound-if-referenced resolve to bound only with a reference. Unbound and excluded are unaffected.
- **`TestD2_15`:**
  - the setup is the real callback path: a ref-less ambiguous deposit, then a verified callback resolved by merchant reference with 4999 against 5000, giving `disputed`/`callback_amount_asset_mismatch` with no stored reference;
  - it asserts exactly one CU plus one `pay_amount_mismatch` in-run, and no finding without a line;
  - a real tombstone on R clears it, and so does a reversal line naming R;
  - the ledger invariants hold.

  The mutant above kills its key subtests.
- TestD2_11 and TestD2_14 still pass. The bound rule for reference-holding parks is therefore unaffected, including PM-1's standing finding keyed on X.

## Conditions

| ID | Item | When |
|---|---|---|
| PM-3 | ADR 0095 §35.4, B1 gate item 1: STANDING-1 covers every attempt the runtime rule treats as unbound, including ref-less bound-reason parks; until then their exposure drops out silently after the statement period | **PRE-MERGE** (text only) |
| PAY-CALLBACK-MISMATCH-BIND-1 | Payments binds the validated, non-conflicting callback reference at the `callback_amount_asset_mismatch` park, and at `applyMultipleSuccessDispute` | binding, before the first real PSP or non-MOCK source |
| B4 | MA020 widening applies the same runtime rule | binding, as registered |
| Unchanged | STANDING-1, PAY-RECON-POLL-REF-CLEAR-1, the I-wire P1 alert (does not exist yet), H1, E1–E3, T4, the T15 integration test | as registered |

## Labels
- The D2F-1 runtime rule: **IMPLEMENTED**.
- Standing coverage for unbound and ref-less parks: **NOT IMPLEMENTED** (STANDING-1).
- PAY-CALLBACK-MISMATCH-BIND-1: **NOT IMPLEMENTED**.
- Statement source: **MOCK**.

## Relevant paths
- /home/user/igaming-platform/internal/reconciliation/payment_statement.go (`captureClass`)
- /home/user/igaming-platform/internal/reconciliation/payment_reason_classification_test.go (`TestD2_P1_RuntimeRule`)
- /home/user/igaming-platform/internal/reconciliation/prh2_d2_postd1_integration_test.go (`TestD2_15`)
- /home/user/igaming-platform/docs/decisions/0095-provider-io-transaction-boundary-and-payment-contract.md (§35.4, where PM-3 goes)
- The run script and logs: /tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad/lfd2r5.sh, with `lfd2r5_out.txt`, `lfd2r5_base.log` and `lfd2r5_MUT.log` next to it.
