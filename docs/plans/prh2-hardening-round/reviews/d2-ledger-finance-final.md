_Reviewer: `ledger-finance`. Branch `prh2-d2-recon-parked-capture` @ `61524df`. Recorded verbatim by the orchestrator._

# Ledger-finance review: PRH-2 D2 final (`61524df`, includes the D1 merge `7adb0c5`)

## Verdict: ACCEPT WITH CONDITIONS. Two small pre-merge items

V1 is done, the classification table matches my ruling, and the new tests are sound. Both pre-merge items come from the implementer's questions:
- **PM-1:** a conflict park that holds a reference should be bound-if-referenced, not unbound. This is a one-row change plus one test.
- **PM-2:** widen the wording of the B1 gate. Text only.

## V1 (my earlier pre-merge condition): DONE
- **Setup:** I exported `61524df` with `git archive` and built a fresh private DB, `lf_d2r3_20261003`. No role or credential changes, and I did not touch any shared cache, directory or other agent's DB. Free disk was 7180M at the start and never below 6560M, so it stayed above the 3G stop line.
- **Baseline** (`-race -tags integration -count=1 -p 1 ./internal/reconciliation/...`, pipefail):
  - `internal/reconciliation`: ok (99.5s);
  - `statement`: ok;
  - FAIL 0, DATA RACE 0.
  - This also covers the second half of (c)4: the existing suite produces no new findings.
- **Mutants:** I re-anchored them on the new code and ran each with `-run 'TestD2_|TestINVDEP1_|TestPaymentStatement_'`. The file was restored byte-identical (`cmp` against `git show 61524df`).

| Mutant | Change | Result | Killed by |
|---|---|---|---|
| X1 | cross-check removed (`if false && !byMerchant ...`) | KILLED (15 fails) | TestD2_7, TestD2_8, TestD2_9, TestD2_9b |
| X4 | B recorded in `matchedBy` before the `b.unboundPark()` step | KILLED (6) | TestD2_7, TestD2_9 |
| X6 | B cleared on `a.providerRef` instead of `l.ref` | KILLED (1) | TestD2_9b |
| LF own: CB-UNB | `callback_amount_asset_mismatch` classified unbound | KILLED (2) | TestD2_11_RealCallbackAmountAssetMismatch_IsBound, TestD2_P1_EveryPaymentsDepositDisputeReasonIsClassified |

- **Cleanup:** DB dropped (`dropped lf_d2r3_20261003`, first try); no `lf_%` DB remains; export and pristine copy deleted.

## Item 2: the new work since D1 (`a0a6325`)

### P1, the `disputeReasonClasses` table: MATCHES my ruling
Every row agrees with what I asked for:
- **bound:** `multiple_success_for_intent`, `sync_amount_mismatch`, `poll_amount_mismatch`, `poll_reference_mismatch`, `callback_amount_asset_mismatch`;
- **unbound:** `provider_reference_conflict`, the bare `invalid_provider_reference`, and the `invalid_provider_reference:*` prefix;
- **bound-if-referenced:** `success_for_never_sent_attempt`;
- **excluded:** `reversal_tombstone_precedes_success`.

The one exception is `provider_reference_conflict`; see 3(a) below.

The pin tests are what I asked for:
- `TestD2_P1_EveryPaymentsDepositDisputeReasonIsClassified` iterates `payments.DepositDisputeTerminalReasons()`, expanding the prefix to every closed `providerref` reason;
- `NoStaleClassification` catches a reason that payments has renamed or removed;
- `UnknownReasonIsUnclassified` and `BoundIfReferenced` cover the edges.

The behaviour change is **accepted and required**. `callback_amount_asset_mismatch` parks are now reported as `pay_captured_unposted`: the money was captured and not posted, which is the same exposure as `sync_amount_mismatch`.

**LOW, not blocking:** at run time an unclassified reason yields no finding, which fails quiet. The pin makes this unreachable from the payments package. If the table is touched again, consider defaulting an unclassified disputed deposit to bound-if-referenced, so it fails loud.

### TestD2_10, TestD2_11 and TestD2_12: ACCEPTED
- **TestD2_10** drives D1's real sweeper to produce `poll_amount_mismatch` and `poll_reference_mismatch` parks. It asserts that the bound reference is kept (B2), that there is exactly one dispute audit, and that both the in-run and standing findings appear. This closes the D1/D2 end-to-end gap.
- **TestD2_11** covers a real `callback_amount_asset_mismatch` park, giving CU plus `pay_amount_mismatch` in-run and CU when standing. CB-UNB kills it.
- **TestD2_12** gives `pay_status_mismatch` for a declined attempt plus a succeeded line, and a clean control with a declined line. That meets my D1 re-review item (b), with no matcher change needed.

### R-1 (the TestD2_7 `r1` subtest): ACCEPTED
When B has a bound reason (`poll_amount_mismatch` or `sync_amount_mismatch`) or an excluded one (tombstone), the line that names B produces no CU for B. A bound B gets only its own standing finding, keyed by its own reference RB. That is the right split: bound clearing is keyed on the attempt's own reference, and unbound clearing on the line's reference.

## Item 3: the implementer's two questions

### (a) A poll conflict park that holds a reference should be BOUND-IF-REFERENCED, not unbound. PRE-MERGE (PM-1)
I ruled "unbound" for `provider_reference_conflict` on the assumption that such a park never binds the reference, which is true of phase C's conflict park. D1's poll path (the F-C4 ledger binding) breaks that assumption:
- the attempt has held X since T6, and the PSP has just confirmed success on X;
- classifying it unbound means only in-run reporting, so the exposure drops out silently once the statement period passes.

Behaviour in-run does not change: the X line resolves by reference to this attempt, and clearing on `l.ref` is the same as clearing on `a.providerRef`. The only gap is the standing finding, and silent loss of a captured, unposted exposure is exactly what I must not accept. The B1 gate does keep it MOCK-only for now, but the fix is one row and the table is being introduced in this branch, so it should be right from the start.

Required:
1. In `disputeReasonClasses`, set `"provider_reference_conflict": reasonBoundIfReferenced`, and update the same row in `d2ExpectedClasses`. Phase C conflict parks have no reference, so they stay unbound with no change.
2. Add an integration test using the real poll F-C4 park shape (as in `TestFC4_PollSuccessWhoseBoundReferenceBecameAPayoutStepBKey_ParksNotErrorLoop`). It must assert:
   - with no statement line: a standing `pay_captured_unposted` keyed on X;
   - a tombstone on X, or a reversal line naming X, clears it;
   - a phase C conflict park with no reference still produces no standing finding;
   - SUM(D) = SUM(C), and `RunLedgerVsProjection` reports 0.
3. Add a mutant (revert the row to unbound) and record it in the evidence file.

This is noisy, not silent: when X really belongs to the holder, the finding stays standing until someone investigates. For a PSP confirming a deposit capture on a key that is also a payout settlement key, that is the right outcome. As usual, M1 only acknowledges it.

**Follow-up:** B4 (the MA020 widening) should use the same bound-if-referenced rule.

### (b) Widen the B1 gate wording: YES. PRE-MERGE, text only (PM-2)
In ADR 0095 §35.4, replace the hand-written list of reasons with a reference to the table: every deposit dispute reason classified bound, unbound or bound-if-referenced in `disputeReasonClasses`. That covers `callback_amount_asset_mismatch` and `success_for_never_sent_attempt`, and tracks future additions automatically. Also name `payments.poll_evidence_contradicts_terminal_attempt` in the I-wire P1 alert set, per my D1 re-review item (a).

## Item 4: deferring B3 to PAY-RECON-POLL-REF-CLEAR-1: ACCEPTED
- **Why it is acceptable:** it fails loud. A PSP reversal under Y leaves the X finding standing; it is noisy, never silent.
- **Deadline:** binding before the first real PSP or the first non-MOCK statement source, whichever comes first. It ships together with PAY-RECON-PARKED-CAPTURE-STANDING-1, because both need persisted structured evidence. Allocate one schema change for both, and ledger-finance signs off on it.
- **Until then:** §35 must say that a standing `poll_reference_mismatch` finding whose reversal came under Y needs manual verification, and that M1 only acknowledges it.

## Item 5: T15 with unit-only coverage: ACCEPTED
- A realistic T15 attempt was never sent, so it normally holds no reference: it is unbound, reported in-run through the merchant path.
- The run-time predicates (`boundCapture` and `unboundPark`) are the same ones the integration tests exercise for other reasons. The only thing specific to T15 is its classification, and `TestD2_P1_BoundIfReferenced` pins both branches.
- Migration 0101's CHECK legitimately prevents building a realistic fixture.
- **Binding follow-up:** add a T15 integration test in STANDING-1, or as soon as a fixture is possible.

## Conditions

| ID | Item | When |
|---|---|---|
| PM-1 | `provider_reference_conflict` becomes bound-if-referenced: table row, pin row, integration test and mutant (3(a)) | **PRE-MERGE** |
| PM-2 | §35.4: the B1 gate covers every non-excluded class in the table, and the I-wire set names `payments.poll_evidence_contradicts_terminal_attempt` (3(b)) | **PRE-MERGE** (text only) |
| V1 | baseline plus X1, X4 and X6 | **DONE** |
| B3 | PAY-RECON-POLL-REF-CLEAR-1, shipped with STANDING-1 before the first real PSP or non-MOCK source | binding |
| T15i | T15 integration test | with STANDING-1 |
| L1 | default an unclassified disputed reason to fail loud | LOW, optional |
| Still binding | STANDING-1 (B1), B4 (MA020, using the bound-if-referenced rule for conflict parks), H1 (PAY-PAYOUT-UNBOUND-HOLD-1), E1–E3, T4 | as registered |

## Labels
- PAY-RECON-PARKED-CAPTURE-1, the D2-1 cross-check and the P1 classification: **IMPLEMENTED** (once PM-1 lands).
- Standing detection for unbound parks: **NOT IMPLEMENTED** (STANDING-1).
- B3, PAY-RECON-POLL-REF-CLEAR-1: **NOT IMPLEMENTED**.
- Statement source: **MOCK**.

## Relevant paths
- /home/user/igaming-platform/internal/reconciliation/payment_statement.go (`disputeReasonClasses`, `captureClass`)
- /home/user/igaming-platform/internal/reconciliation/payment_reason_classification_test.go (`d2ExpectedClasses`)
- /home/user/igaming-platform/internal/reconciliation/prh2_d2_postd1_integration_test.go
- /home/user/igaming-platform/internal/reconciliation/prh2_d2_merchant_crosscheck_integration_test.go
- /home/user/igaming-platform/docs/decisions/0095-provider-io-transaction-boundary-and-payment-contract.md (§35.4)
- The run script and logs: /tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad/lfd2r3.sh, with `lfd2r3_out.txt` and `lfd2r3_*.log` next to it.
