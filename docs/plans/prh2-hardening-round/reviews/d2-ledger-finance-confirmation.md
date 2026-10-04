_Reviewer: `ledger-finance`. Branch `prh2-d2-recon-parked-capture` @ `adcde1e`. Recorded verbatim by the orchestrator._

> **Orchestrator note (2026-10-03):** the two test skips used in this run (`TestStoreOutage_DoesNotPinPool` and `TestResolutionIsolation_`) are the project's long-standing timing-lane exception; they are run SEPARATELY and are an exception, not something this review verified. They must be run explicitly at the final gate (plan W5), and a failure there is a real failure.

# Ledger-finance confirmation: PRH-2 D2 final (`adcde1ec0fa564d85ac532b50d76c3c89c57a40f`)

## Verdict: ACCEPT. No pre-merge items remain
PM-1 and PM-2 are both met, my own re-kill of the PM-1 mutant was killed, and the race baseline is clean.

## What I ran
- **Setup:** `git archive` export of `adcde1e`, and a fresh private DB, `lf_d2r4_20261004`. No role or credential changes; I did not touch any shared cache, directory or other agent's DB. Free disk was 5365M at the start and 5217M after the baseline, so it never got near the 3G stop line.
- **Baseline** (`-race -tags integration -count=1 -p 1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./internal/reconciliation/...`):
  - `internal/reconciliation`: ok (84.1s);
  - `statement`: ok;
  - FAIL 0, DATA RACE 0.
  - I applied the two skips because you asked me to. They are your exception, not something I verified.
- **PM1-CONFLICT-UNBOUND** (the row reverted to `reasonUnbound`, run with `-run 'TestD2_'`): **KILLED** by:
  - `TestD2_14/standing_without_a_deposit_line_keyed_on_X`;
  - `TestD2_14/reversal_line_naming_X_clears`;
  - `TestD2_P1_EveryPaymentsDepositDisputeReasonIsClassified`.

  The file was restored byte-identical (`cmp` against `git show adcde1e`).
- **Cleanup:** DB dropped (`dropped lf_d2r4_20261004`, first try); no `lf_%` DB remains; export deleted.

## The five points

### 1. PM-1: MET
- **The change:** in `payment_statement.go`, the `disputeReasonClasses` row is now `"provider_reference_conflict": reasonBoundIfReferenced`, and the matching `d2ExpectedClasses` row is updated. I checked the diff: no other production line changed.
- **TestD2_14 builds the real shape:**
  - a pending deposit bound to X;
  - a payout whose Step B settles on X;
  - a real sweeper poll success on X, parked as `provider_reference_conflict`;
  - the setup asserts the park keeps X and that the audit's `bound_to_operation` is `ledger_withdrawal_completed`.
- **TestD2_14 asserts:**
  - a standing CU keyed on X with "no statement line", both with only the payout line present and past the window, with exact counts;
  - the in-run line on X is flagged exactly once;
  - a reversal line naming X clears it;
  - a phase C conflict park with no reference still has no standing finding;
  - `d2AssertBalanced` (SUM(D) = SUM(C), and `RunLedgerVsProjection` reports 0) in every subtest.
- **No tombstone clearing in this shape: ACCEPTED.**
  - Why: `ledger_transactions` is unique on `(tenant_id, provider_id, provider_tx_id)`, and X is already the `withdrawal_completed` key, so a tombstone on X cannot exist.
  - Consequence: the only thing that clears it is a reversal line in the same run. On the next run without that line, the standing finding comes back. Until allocation (LEDGER-SUSPENSE-B-1) or a persisted clearing signal exists, it is effectively permanent: noisy, never silent, which is what this ambiguous capture deserves.
  - **New STANDING-1 scope (binding, not pre-merge):** STANDING-1's persisted line evidence must let a persisted reversal line naming X clear a bound standing finding. That covers this shape, and it also gives every bound park a clearing signal that survives across runs.

### 2. TestD2_4 adjustment: NOTHING LOOSENED
The subtest now loops over `provider_reference_conflict` (bound rule) and `invalid_provider_reference:control_char` (unbound rule). Each runs the original assertions unchanged: `d2Expect({CU: 1})`, `d2CUFor(attempt)` and `d2AssertNoMoney`. Coverage went up: the unbound rule on a reference-holding attempt is still pinned (it kills M-UNBOUND-BYMERCHANT-GATE), and the bound path for a reference-holding conflict park is now pinned too.

### 3. PM-2, ADR 0095 §35.4: MET
- **The gate list:** the B1 gate's item 2 now refers to `disputeReasonClasses` (every class except excluded), with `callback_amount_asset_mismatch` and `success_for_never_sent_attempt` named. A future reason joins the gate through the P1 pin.
- **The I-wire P1 set** names `payments.poll_evidence_contradicts_terminal_attempt`.
- **The missing alert is stated plainly:** "**Status at D2: neither holds.** The I-wire P1 alert does **not** exist yet."
- **The operator rule is there:** a standing `poll_reference_mismatch` finding whose reversal came under Y needs manual verification against the PSP, and M1 only acknowledges it.
- **The PAY-RECON-POLL-REF-CLEAR-1 deadline is binding:** before the first real PSP or non-MOCK statement source, shipped together with STANDING-1 under one schema change that ledger-finance signs off. STANDING-1 is also tied into the gate's item 1.
- PM-1 is documented, including the tombstone impossibility, and points to TestD2_14.

### 4. L1 not done: ACCEPTED as optional (PAY-RECON-D2-HARDENING-1)
- The P1 pin (`TestD2_P1_EveryPaymentsDepositDisputeReasonIsClassified` together with `NoStaleClassification`) refuses any unclassified reason the payments package can write. So the quiet behaviour at run time is unreachable from code that is actually shipped.
- Changing it would mean reversing TestD2_6's pinned behaviour for an unknown reason. That is a design choice worth its own small change, not a merge blocker.

### 5. Baseline: CLEAN
Results are above: 0 FAIL and 0 DATA RACE, with the two tests you specified skipped.

## Items still open, none pre-merge for D2
- **STANDING-1, binding before the first real PSP or non-MOCK source:** standing coverage for unbound parks, plus the new requirement that a persisted reversal line clears a bound standing finding (the TestD2_14 shape).
- **PAY-RECON-POLL-REF-CLEAR-1:** ships with STANDING-1.
- **The I-wire P1 alert:** does not exist yet, and is a gate condition.
- **B4 (MA020 widening):** must use the bound-if-referenced rule for `provider_reference_conflict`.
- **H1, E1–E3, T4 and the T15 integration test:** as registered.

## Labels
- PAY-RECON-PARKED-CAPTURE-1, the D2-1 merchant cross-check, the P1 classification and PM-1: **IMPLEMENTED**.
- Standing detection for unbound parks: **NOT IMPLEMENTED**.
- B3 (PAY-RECON-POLL-REF-CLEAR-1): **NOT IMPLEMENTED**.
- I-wire P1 alert: **NOT IMPLEMENTED**.
- Statement source: **MOCK**.

## Relevant paths
- /home/user/igaming-platform/internal/reconciliation/payment_statement.go (`disputeReasonClasses`)
- /home/user/igaming-platform/internal/reconciliation/payment_reason_classification_test.go
- /home/user/igaming-platform/internal/reconciliation/prh2_d2_postd1_integration_test.go (TestD2_14)
- /home/user/igaming-platform/internal/reconciliation/prh2_d2_parked_capture_integration_test.go (TestD2_4)
- /home/user/igaming-platform/docs/decisions/0095-provider-io-transaction-boundary-and-payment-contract.md (§35.4)
- The run script and logs: /tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad/lfd2r4.sh, with `lfd2r4_out.txt`, `lfd2r4_base.log` and `lfd2r4_PM1.log` next to it.
