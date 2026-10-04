_Reviewer: `security`. Branch `prh2-d2-recon-parked-capture` @ `682fd5b`. Recorded verbatim by the orchestrator. Request noted: STANDING-1's scope must name the ref-less BOUND shapes explicitly; PM-3 in ADR 0095 §35.4 already does._

Security confirmation — PRH-2 D2 final deltas (prh2-d2-recon-parked-capture @ 682fd5b897da0e3e40df598c2939fb05f68fb9d8; delta since the accepted adcde1e: c3ec56d, c09d82d, 682fd5b)

VERDICT: ACCEPT.

Method:
- Disk was 3.3G at the start. It touched 3.0G (not below) during the race compile and ended at 3.1G, so I continued; no STOP was triggered.
- Minimal git archive (internal, migrations, cmd, go.mod/go.sum and deploy only; 17M). Fresh private DB sec_d2d_rv_20261003, then DROPPED WITH (FORCE); verified 0 left. Export deleted.
- No role or credential changes, no shared cache, directory or other agent's DB touched, pipefail on, grepped for FAIL.
- I confirmed the only logic change is payment_statement.go captureClass: `if c == reasonBound || c == reasonBoundIfReferenced`. The table is unchanged; the rest is comments, tests, ADR and evidence.

Tests:
- `-race -tags integration -count=1 -p 1 -run 'TestD2_15|TestD2_P1_RuntimeRule|TestD2_14|TestD2_11' ./internal/reconciliation/`: ok, no DATA RACE.
- D2F1-BOUND-EMPTY-REF-STAYS-BOUND (the condition reverted to `c == reasonBoundIfReferenced`): KILLED by TestD2_P1_RuntimeRule and TestD2_15_RefLessCallbackMismatchPark_IsUnboundAndClearsOnLineReference (subtests a_in_run_succeeded_line_for_R_gives_exactly_one_finding, b_tombstone_on_R_clears and b_reversal_line_naming_R_clears). Reverted and cmp-identical.
- I ran this mutant under -race too, to reuse the compiled dependencies and stay within disk.

1. False negatives:
   - The change affects only attempts with a bound reason and an EMPTY stored reference. Any attempt holding a reference still resolves to bound, so its in-run and standing behaviour is unchanged.
   - For the ref-less case, the old behaviour checked clearing on "", which could never clear, and the standing rule reported it forever. Now it is reported in-run when a succeeded line resolves to it (by merchant reference), clearing on that line's own reference. So it can only change the clearing key, and it removes the standing re-report for ref-less parks.
   - The silent drop-out: ACCEPTED behind the §35.4 gate. Security does not need a stronger statement, on two conditions of fact that I verified:
     - (a) The finding is NOT lost when it drops out. Mismatch rows persist, and only a staff ResolveMismatch sets resolved_at / investigation_status='resolved' (reconciliation.go ~290-310). The run that saw the succeeded line leaves an open pay_captured_unposted row until a human resolves it; later runs simply do not re-report it.
     - (b) Real-PSP use is gated on PAY-RECON-PARKED-CAPTURE-STANDING-1, which per PM-3 now states this plainly.
   - Request, not a merge condition: STANDING-1's scope should name the ref-less BOUND shapes explicitly (callback_amount_asset_mismatch resolved by merchant reference, and multiple_success_for_intent with no stored reference), not only the unbound parks, so the standing work covers both.
2. No tenant or provider cross-over: classification is per attempt row inside the unchanged per-tenant, per-provider matcher (tenant snapshot transaction, FORCE RLS). The detail text adds only the fixed phrase "on this line's reference"; the line's reference is already providerref-validated and was already rendered. No raw provider value is added, and there is no new write path (the change only selects which existing finding branch applies).

Relevant paths (commit 682fd5b):
- internal/reconciliation/payment_statement.go (captureClass, ~235-255)
- internal/reconciliation/prh2_d2_*_integration_test.go (TestD2_15, TestD2_P1_RuntimeRule)
- internal/reconciliation/reconciliation.go (ResolveMismatch, ~290-310)
