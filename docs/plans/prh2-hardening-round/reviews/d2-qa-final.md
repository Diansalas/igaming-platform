_Reviewer: `qa`. Branch `prh2-d2-recon-parked-capture` @ `adcde1e`. Recorded verbatim by the orchestrator. The two test skips used (`TestStoreOutage_DoesNotPinPool`, `TestResolutionIsolation_`) are the timing-lane exception; they are run separately at the final gate._

D2 FINAL QA GATE VERDICT: PASS (prh2-d2-recon-parked-capture @ adcde1ec0fa564d85ac532b50d76c3c89c57a40f, "docs(prh2-d2): PM-1 mutant evidence (35/35 killed at 6c44c24)"). All three conditions from my 0005aa7 review (d2-qa.md) are closed. The few findings below are advisory only and none is pre-merge.

Disk and cleanup: free space stayed at 5.1-5.5G on the shared disk and never fell below 3G. I exported the archive once, used -p 1, then deleted the export and dropped the DB (`dropped qa_d2f_gate`). The "pwd: error retrieving current directory" line at the end of my last command is only my shell standing in the directory I had just deleted. It is harmless.

Commands run (S=/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad; PRIV_DB=qa_d2f_gate PRIV_SRC=$S/qa_d2f; pipefail)
1. `priv_test.sh -race -tags integration -count=1 -timeout 40m -p 1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./internal/reconciliation/...`
   - Result: `ok reconciliation 83.8s`, `ok statement`, exit=0, zero FAIL, panic or DATA RACE.
2. `priv_test.sh -race -tags integration -count=10 -timeout 40m -p 1 -run 'TestD2_' ./internal/reconciliation/`
   - Result: `ok 96.6s`, no FAIL, panic or DATA RACE.
3. Mutants re-killed (scratch copy only, restored and `cmp`-verified; each run was `-run TestD2_`). All 5 are killed:
   - PM1-CONFLICT-UNBOUND (`provider_reference_conflict` reverted to reasonUnbound): TestD2_P1_Every... and TestD2_14 FAIL.
   - M-CALLBACK-DROP: TestD2_P1_Every... and TestD2_11 FAIL.
   - M-UNCLASSIFIED-AS-BOUND: TestD2_P1_UnknownReasonIsUnclassified and TestD2_6 FAIL.
   - M-T15-ALWAYS-BOUND: TestD2_P1_Every... and TestD2_P1_BoundIfReferenced FAIL.
   - M-TOMB-BOUND: TestD2_P1_Every..., TestD2_7 (R-1) and TestD2_6 FAIL.
4. "Would the test fail if the park did not happen?" probe:
   - I disabled the scripted `setStatus` in TestD2_10 and TestD2_14 (so the real poll sees the MOCK's pending answer and no park occurs).
   - All five affected subtests FAIL at their setup `mustParked` assertion, with the message `state=pending ..., want disputed/<reason>`. These are TestD2_10/poll_amount_mismatch and /poll_reference_mismatch, and TestD2_14's standing_without_a_deposit_line_keyed_on_X, in_run_line_on_X_is_flagged_once and reversal_line_naming_X_clears.
   - Neither test can pass vacuously. This is the D1 weakness check you asked for.

Check 1, my earlier conditions
- D2-F1 is closed. TestD2_10 drives a real poll park (the scripted QueryStatus plus a fixture `next_action_at`, then a real `Sweeper.RunOnce`) for both poll reasons.
  - It asserts the park, the bound reference kept, exactly one dispute audit with that reason, then the in-run and standing pay_captured_unposted, plus pay_amount_mismatch where the amounts differ, and `d2AssertNoMoney`.
  - Real callback_amount_asset_mismatch is covered by TestD2_11. Declined-attempt plus succeeded line gives pay_status_mismatch in TestD2_12, with a declined-line control.
- Reason pin: payment_reason_classification_test.go iterates `payments.DepositDisputeTerminalReasons()`.
  - The test expands prefix entries with all four closed providerref reasons plus the bare form, and compares each against an explicit ledger-finance table.
  - Its negative test `UnknownReasonIsUnclassified` proves the pin can fail, and `NoStaleClassification` guards against dead table rows.
  - D2-F1's string-drift worry is now closed by two layers: this pin and D1's exact-string and write-site tests.
- R-1: TestD2_7/r1 has three cases, poll_amount_mismatch, sync_amount_mismatch and tombstone. Each uses `parkPoll`, whose `mustParked` fails if the park did not occur.
  - B gets only the check=merchant mismatch against A, plus (for the bound reasons) its own standing finding keyed on its own reference, and no pay_captured_unposted from the line naming it.
  - It is a fixture park, not a real one, which is fine for a matcher unit-of-behaviour. Y-B-ANYDISPUTED is recorded as killed.

Check 2, new tests
- TestD2_14 uses a real shape: a pending deposit bound to X, a payout whose Step B settlement reference is X, then a sweeper poll giving the F-C4 park.
  - The setup asserts the park, the bound reference X kept, and `bound_to_operation=ledger_withdrawal_completed`.
  - Cases: standing (payout line only, plus a past window), in-run flagged once, a reversal line clears it, and a phase C conflict park with no reference has no standing finding.
  - It correctly notes that a tombstone clear is structurally impossible for this shape (the ledger unique key already holds X).
- The P1 pin, its negative test and the stale-row test are as described above. All are unit tests, with no database.

Check 3, determinism
- No sleeps and no wall-clock assertions. `time.Now()` appears only in coverage windows and the drift window.
- Sweeper timing is driven through fixture `next_action_at` writes (T-1).
- Each subtest builds its own world and tenant. The only map iterations are over independent cases.

Check 4, no weakened tests
- TestD2_4's "unbound reason on a reference-holding attempt" subtest now runs two reasons with the same assertions (`d2Expect` exactly one pay_captured_unposted, `d2CUFor`, `d2AssertNoMoney`).
- The conflict reason now goes through the bound rule, which is real D1's F-C4 shape, covered end to end by TestD2_14.
- The invalid_provider_reference:control_char case keeps the "unbound rule is not gated on how the line resolved" pin. Net coverage is preserved.
- Other changes to existing D2 tests are strict additions (constants taken from payments, the F13 wording assertion in `d2CUFor`).

Check 5, T15 and L1 (my view)
- T15 (`success_for_never_sent_attempt`) with unit coverage only is acceptable.
  - Migration 0101's CHECK blocks a realistic fixture. The unit test pins both the table row (bound-if-referenced) and the row-level resolution (bound with a reference, unbound without, never for non-disputed), and the P1 pin forces the row to exist.
  - The M-T15 mutants are killed.
- Not doing L1 (a loud default for an unclassified reason) is acceptable now. The pin test makes a missing classification a CI failure, and the stale-row test prevents drift.
  - The residual risk is a reason written through a path that bypasses `DepositDisputeTerminalReasons()`. D1's write-site coverage test covers that.
  - A runtime loud default would be defense in depth but also noise on legacy rows. I would register it as a low-priority follow-up, not a condition.

Advisory findings (none pre-merge)
- A1 (LOW): R-1 and TestD2_1..3 use fixture parks. The real parks are covered by TestD2_10/11/14, so the matcher is covered end to end for the reasons that matter.
- A2 (LOW): the drift check (`RunLedgerVsProjection`) is explicit in `d2AssertNoMoney` and `d2AssertBalanced`. For parked-only worlds, drift is trivially zero because no ledger rows are written. This was already noted.
- A3 (INFO, carried from my 0005aa7 review): unbound-park standing coverage remains NOT IMPLEMENTED (PAY-RECON-PARKED-CAPTURE-STANDING-1). The ADR §35.4 gate (LF PM-2) is in place. My earlier condition stands: this must be done before any real PSP or statement source is enabled.

Relevant paths: /home/user/igaming-platform (untouched; branch prh2-d2-recon-parked-capture). The files reviewed were internal/reconciliation/{prh2_d2_postd1_integration_test.go, prh2_d2_parked_capture_integration_test.go, prh2_d2_merchant_crosscheck_integration_test.go, payment_reason_classification_test.go, payment_statement.go} and docs/plans/payment-readiness/evidence/prh2-d2-mutation-kill.txt.
