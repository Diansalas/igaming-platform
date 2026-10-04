_Reviewer: `code-reviewer`. Branch `prh2-d2-recon-parked-capture` @ `682fd5b`. Recorded verbatim by the orchestrator. The reviewer did not re-kill the D2F-1 mutant (disk stop); security (`d2-security-final-deltas.md`) and ledger-finance (`d2-ledger-finance-d2f1-confirmation.md`) each re-killed it independently._

# Code re-review: PRH-2 D2 D2F-1 confirmation (prh2-d2-recon-parked-capture @ 682fd5b, delta adcde1e..682fd5b)

**Verdict: READY.** D2F-1 is fixed the way I asked: a "bound" reason on an attempt with no stored reference is now treated as unbound at run time. My probe now clears after the refund, and all requested targeted tests pass under `-race`.

**Disk stop: I did not re-kill the D2F-1 mutant.** Free space dipped below the 3 G limit during the one test build (lowest sample 2,981,684 KB, about 2.84 GiB), so I followed your rule and stopped there. I ran no further build, dropped `cr_d2g_rv` (`dropped cr_d2g_rv`), and deleted my export; free space is back to 3.1 G. Instead I checked statically that the reverted rule cannot pass `TestD2_P1_RuntimeRule` (item 4). Everything else was run.

I reviewed a `git archive` export of 682fd5b and did not edit the worktree. There were no role or credential changes, and I touched nothing shared.

## Your checks

1. **The fix is correct, simple and consistent.**
   - The only logic change is in `captureClass` (`internal/reconciliation/payment_statement.go`, about line 247): `if c == reasonBound || c == reasonBoundIfReferenced`. With a stored reference the attempt is bound; without one it is unbound.
   - `disputeReasonClasses` is unchanged, so the table still records what each reason is, and only the run-time behaviour changes.
   - The doc comments on `reasonBound` and `captureClass` describe exactly this rule. The classification pin (`TestD2_P1_EveryPaymentsDepositDisputeReasonIsClassified`) still pins the table classes, and the new `TestD2_P1_RuntimeRule` pins the run-time class for every reason in `payments.DepositDisputeTerminalReasons()`, with and without a reference.
2. **Probe re-run.** Setup: a ref-less ambiguous deposit, then a callback by merchant reference with amount 4999 versus 5000.

   | Step | Result | At adcde1e |
   |---|---|---|
   | After the callback | `disputed/callback_amount_asset_mismatch`, `ref=<nil>` | same |
   | Standing run, no line | **no findings** (unbound parks have no standing finding) | 1 `pay_captured_unposted` |
   | In-run, a succeeded line for R resolved by merchant reference | `pay_captured_unposted` + `pay_amount_mismatch` | n/a |
   | After the tombstone on R, with the same succeeded line | **only `pay_amount_mismatch`; the captured-unposted finding cleared** on the line's reference | did not clear |
   | After the tombstone, no line | no findings | 1 `pay_captured_unposted`, forever |

   `TestD2_15_RefLessCallbackMismatchPark_IsUnboundAndClearsOnLineReference` passes and covers the same path through the real callback.
3. **Existing tests are unchanged and pass.**
   - `git diff adcde1e..682fd5b` does not touch `prh2_d2_parked_capture_integration_test.go` or `prh2_d2_merchant_crosscheck_integration_test.go`.
   - `prh2_d2_postd1_integration_test.go` and `payment_reason_classification_test.go` only gained lines; nothing was removed.
   - The tests whose attempts hold a reference all pass under `-race`: TestD2_1, TestD2_2, TestD2_3, TestD2_10, TestD2_11 and TestD2_14.
4. **Mutants.**
   - **D2F1-BOUND-EMPTY-REF-STAYS-BOUND** (rule reverted): not re-run because of the disk stop. By reading, it cannot survive. `TestD2_P1_RuntimeRule` is a pure unit test, with no DB and no timing. For every `reasonBound` reason with `providerRef == ""` it expects `reasonUnbound`, and the reverted condition returns `reasonBound`. So it fails deterministically on five reasons: `multiple_success_for_intent`, `sync_amount_mismatch`, `poll_amount_mismatch`, `poll_reference_mismatch` and `callback_amount_asset_mismatch`. On the integration side, `TestD2_15` asserts the same no-standing-finding and clear-after-tombstone outcomes that my probe showed, and those differ under the old rule (column "At adcde1e" above).
   - **PM1-CONFLICT-BOUND killed only by the classification pin: acceptable.** With the run-time rule, `reasonBound` and `reasonBoundIfReferenced` behave identically, so the mutant is equivalent at run time and no behavioural test could catch it. The pin is the right guard: it keeps the documented classification, which ledger-finance owns, from drifting.

## Commands and results

- `priv_db.sh` built `cr_d2g_rv` fresh.
- **Targeted run:** `set -o pipefail`; `-race -tags integration -count=1 -p 1 -run 'TestCRD2G_Probe|TestD2_15|TestD2_P1_RuntimeRule|TestD2_14|TestD2_11|TestD2_1_|TestD2_3|TestD2_2_|TestD2_10' ./internal/reconciliation/`.
  - Exit 0: TestD2_P1_RuntimeRule, TestD2_1, TestD2_2, TestD2_3, TestD2_10, TestD2_11, TestD2_14, TestD2_15 and the probe all PASS.
  - No DATA RACE.
  - This is your requested set plus TestD2_2, TestD2_10 and my temporary probe, all in a single build.
- **Disk:** sampled every 3 s during the run. The lowest reading was 2,981,684 KB, below the 3 G limit, so I stopped.
- The probe file was temporary and is deleted with the export. `cr_d2g_rv` is dropped. Free space is now 3.1 G.

## Files

- `internal/reconciliation/payment_statement.go`: `captureClass`, about line 247.
- `internal/reconciliation/payment_reason_classification_test.go`: `TestD2_P1_RuntimeRule`.
- `internal/reconciliation/prh2_d2_postd1_integration_test.go`: `TestD2_15`.

These paths are relative to the repo root at `/home/user/igaming-platform`, at branch `prh2-d2-recon-parked-capture` @ 682fd5b; I reviewed them from a `git archive` of that commit, not from a worktree.
