_Reviewer: `qa`. Branch `prh2-d2-recon-parked-capture` @ `682fd5b`. Recorded verbatim by the orchestrator._

> **Orchestrator note (2026-10-04): this QA confirmation is PARTIAL.** QA stopped at 2.9G free disk (below the 3G floor) after the `-count=10` run of `TestD2_15|TestD2_P1_RuntimeRule` (ok) and the static review. NOT RUN: the full `TestD2_` pass, the D2F1 mutant re-kill and one P1-table mutant re-kill. Mitigation already on record: ledger-finance independently re-killed the D2F1 mutant at `c09d82d` (5 failing tests, `d2-ledger-finance-d2f1-confirmation.md`), QA re-killed five P1-table mutants at `adcde1e` (`d2-qa-final.md`, table unchanged since), and the whole-repo `-race` run at merge covers the full reconciliation package. QA's verdict is therefore treated as PASS WITH CONDITIONS, condition 1 to be satisfied by the merge-time whole-repo run and, if disk allows, a QA re-run.

D2 DELTA CONFIRMATION at 682fd5b: PASS ON THE EVIDENCE I GATHERED, BUT PARTIAL. I stopped as instructed when free space fell below 3G, so the items marked NOT RUN below are not verified. Treat the verdict as PASS WITH CONDITIONS until those are run once disk allows.

Disk: it started at 3.3G, fell to 3.1G after the archive export and DB build, and to 2.9G after the count=10 run. At that point I stopped, dropped `qa_d2g_gate`, deleted my export, and ran no further tests. Free space is still about 2.9G. No role or credential changes.

What I ran (S=/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad; `git archive 682fd5b`, private DB qa_d2g_gate)
- `priv_test.sh -race -tags integration -count=10 -p 1 -run 'TestD2_15|TestD2_P1_RuntimeRule' ./internal/reconciliation/`: `ok 7.514s`, no FAIL, panic or DATA RACE. Done.

NOT RUN (disk):
- The `-race -tags integration -count=1 -p 1 -run 'TestD2_'` pass over ./internal/reconciliation/.
- The D2F1-BOUND-EMPTY-REF-STAYS-BOUND re-kill and the P1-table mutant re-kill.
- Please run those when space is available. They are small (the whole TestD2_ run took about 84s and a few GB of cache earlier).

Static review

Delta adcde1e..682fd5b (`git diff --stat`): 5 files, 209 insertions and 11 deletions. They are the ADR, the evidence file, payment_statement.go (+23/-11, logic and comments), payment_reason_classification_test.go (+34) and prh2_d2_postd1_integration_test.go (+84). The test-file diff shows 118 insertions and 0 deletions, so NO existing test was changed or weakened. That confirms the implementer.

The logic change is exactly the one condition you described: in `captureClass`, `if c == reasonBound || c == reasonBoundIfReferenced`, so a bound reason with an empty stored reference resolves to unbound. The table is unchanged.
- This is a correct and conservative fix. Without it, a ref-less callback_amount_asset_mismatch park was "bound" on an empty reference. Its standing finding could never clear on a reversal or tombstone, and it was keyed on an empty reference.
- Now it is reported in-run by merchant reference and cleared on the line's reference, like the other unbound parks.
- The side effect is that an attempt with a bound reason but no stored reference loses its standing finding. That is the documented unbound limit tracked as STANDING-1, so it adds no new gap class.

New tests
- TestD2_P1_RuntimeRule is a unit test, with no database.
  - It covers every reason from `d2ReasonsToCheck` (the payments list, expanded for the invalid-reference prefix and bare form), each with and without a reference.
  - It asserts bound or bound-if-referenced resolves to bound only with a reference, and to unbound without one, while unbound and excluded reasons are unaffected.
  - The expected value is computed from the table class independently of `captureClass`, so reverting the condition fails it.
- TestD2_15 is built through the real path. A scripted Ambiguous result with no reference gives an ambiguous attempt with a NULL reference. Then a verified callback names R plus the attempt's merchant reference with a different amount. The receipt path resolves it by merchant reference and parks it callback_amount_asset_mismatch without binding R.
  - Subtests: (a) an in-run succeeded line for R gives exactly one finding (pay_captured_unposted plus pay_amount_mismatch). It must be the unbound form keyed on the line's reference R, with no standing finding afterwards and no ledger tx, and it asserts D2 balance and drift; (b) a tombstone on R clears it (with the tombstone count asserted to be 1); (b2) a reversal line naming R clears it.
- Vacuity probe: the precondition is real.
  - The setup `Fatalf`s unless the pre-callback attempt is ambiguous with `ProviderReference == nil`.
  - It then runs `mustParked(callback_amount_asset_mismatch)`, which fails unless the attempt is disputed with that terminal reason and no ledger link.
  - It `Fatalf`s again unless the parked attempt still has no stored reference.
  - If the park or the missing reference did not happen, TestD2_15 fails in setup before any reconciliation assertion, so it cannot pass vacuously.
  - I did not re-run the setup-disabling probe this time (disk), but the checks above are explicit on the state and reference.
- Determinism: no sleeps and no wall-clock assertions in the delta. `time.Now()` appears only in the existing d2 helpers' coverage and drift windows. The tests are sequential and each subtest builds its own world.

Conditions
1. Run the three NOT RUN items above (full TestD2_ pass, D2F1-BOUND-EMPTY-REF-STAYS-BOUND re-kill, and one P1-table mutant) before treating this as final. The expected outcome is that reverting `captureClass` to `c == reasonBoundIfReferenced` fails TestD2_P1_RuntimeRule and TestD2_15 (the latter at its unbound-key assertion or in the standing `d2NoCU` check).
2. Carried: STANDING-1 for unbound parks stays a prerequisite before any real PSP or statement source is enabled. This delta makes one more shape (a ref-less bound-reason park) fall under that limit, and it is documented in the new test comments.

Relevant paths: /home/user/igaming-platform (untouched; branch prh2-d2-recon-parked-capture). Files: internal/reconciliation/payment_statement.go (`captureClass`), internal/reconciliation/payment_reason_classification_test.go (TestD2_P1_RuntimeRule), internal/reconciliation/prh2_d2_postd1_integration_test.go (TestD2_15).
