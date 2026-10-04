_Reviewer: `security`. Branch `prh2-d2-recon-parked-capture` @ `adcde1e`. Recorded verbatim by the orchestrator (the D2 part of a combined hand-back; the F-pay part is in `fpay-security.md`)._

Two reviews in this hand-back: (A) Security review — PRH-2 F-pay, and (B) Security confirmation — PRH-2 D2 delta (PM-1).

Common method:
- Disk was 5.8G free at the start and 4.9G at the end; it never approached the 3G floor.
- Each review used a fresh private DB (sec_fp_rv_20261003 and sec_d2c_rv_20261003), both DROPPED WITH (FORCE); verified 0 left. DB access worked.
- One git archive per review, built once. Exports, logs and scripts deleted.
- `-p 1` throughout. No role or credential changes, no shared cache, directory or other agent's DB touched, pipefail on, grepped for FAIL.
- Every mutant was reverted and cmp-verified identical.

======================================================================

(B) Security confirmation — PRH-2 D2 delta (prh2-d2-recon-parked-capture @ adcde1ec0fa564d85ac532b50d76c3c89c57a40f; delta since the accepted 61524df: 3a92cb2, 6c44c24, adcde1e)

VERDICT: ACCEPT.

I confirmed that the only production change is the one row in payment_statement.go:214: `provider_reference_conflict`, which moves from reasonUnbound to reasonBoundIfReferenced.

1. Additive, with no tenant or provider cross-over and no raw values:
   - A conflict park that HOLDS a reference (the D1 poll F-C4 park on X) now classifies as bound. It is reported in-run on a succeeded line and STANDING when unmatched, clearing only on capturedUnposted(a): a reversal line naming X, or a tombstone on X.
   - A park without a reference resolves to unbound, exactly as before.
   - No tenant or provider cross-over: classification is per attempt row, and the matcher scoping I reviewed at 61524df (tenant snapshot transaction, FORCE RLS, per-provider maps) is unchanged.
   - Finding details add nothing new: terminal_reason (closed vocabulary), platform ids, and validated line fields.
   - Residual narrowing, disclosed for completeness, not a merge condition. For a reference-holding conflict park b, a succeeded line resolved by reference to a different attempt a, naming b's merchant reference, no longer produces the cross-check's pay_captured_unposted against b, because b.unboundPark() is now false. That line still produces pay_reference_mismatch check=merchant against a. b itself is still reported by the standing rule whenever no other line matched it. The only fully narrowed case is b also having its own non-succeeded line in the same run, a contrived shape. The clearing-on-Y family is already registered as PAY-RECON-POLL-REF-CLEAR-1. Not a new false negative class.
2. A phase C conflict park with no reference stays unbound and has no standing finding: PINNED by prh2_d2_postd1_integration_test.go ~230-235 (`d2NoCU(… "a phase C conflict park with no reference, no line")`).
3. Tests and mutant:
   - `-race -tags integration -count=1 -p 1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./internal/reconciliation/...`: ok (reconciliation 75s, statement 1s), no DATA RACE.
   - PM-1 mutant (row reverted to reasonUnbound): KILLED by TestD2_P1_EveryPaymentsDepositDisputeReasonIsClassified and TestD2_14_PollFC4ConflictPark_HoldingReference_IsBoundAndStanding (subtests standing_without_a_deposit_line_keyed_on_X and reversal_line_naming_X_clears).

Relevant paths:
- F-pay (3c049ed): internal/payments/payout.go (~306-332 unavailable branch; ~361-368 allow decision row; ~442-456 adapter validation; payoutStatusQuery ~1235-1257; applyPayoutStatusEvidenceInTx); internal/payments/payout_sweep.go (gateAndEscalateOnDeny ~108-140); internal/payments/kycgate.go (EvaluateDeposit); internal/payments/deposit_v2.go (~210-223); internal/payments/fpay_kyc_integration_test.go; docs/plans/payment-readiness/evidence/prh2-fpay-mutation-kill.txt.
- D2 (adcde1e): internal/reconciliation/payment_statement.go:214; internal/reconciliation/prh2_d2_postd1_integration_test.go.
