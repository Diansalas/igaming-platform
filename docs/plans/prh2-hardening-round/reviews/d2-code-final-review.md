_Reviewer: `code-reviewer`. Branch `prh2-d2-recon-parked-capture` @ `adcde1e`. Recorded verbatim by the orchestrator (the D2 part of a combined hand-back; the F-pay part is in `fpay-code-review.md`)._

> **Orchestrator decision (2026-10-03):** D2F-1 (a disputed attempt with a `bound` reason but NO stored reference, e.g. `callback_amount_asset_mismatch` resolved by merchant reference, produces a `pay_captured_unposted` finding that never clears, even after the PSP's own reversal) is **pre-merge**. Fix: in `captureClass`, a bound reason on an attempt with no stored reference resolves to unbound (equivalently, treat every bound reason as bound-if-referenced), with an end-to-end test from the reviewer's probe. The classification table is ledger-finance's, so ledger-finance must confirm the reclassification. `multiple_success_for_intent` has the same shape by code reading (the gap predates D2). D2F-2 (T15 integration test) stays optional under PAY-RECON-PARKED-CAPTURE-STANDING-1.

# Code reviews: PRH-2 F-pay (prh2-fpay-kyc-gate @ 3c049ed) and PRH-2 D2 final (prh2-d2-recon-parked-capture @ adcde1e)

Both reviews are below.
- **F-pay: READY WITH CONDITIONS.** One low-severity inconsistency: deposit "allow" decision rows commit even when the deposit is then declined. Fix the code or document it.
- **D2 final: READY WITH CONDITIONS, with a pre-merge condition.** A probe confirms that a disputed attempt with a "bound" reason but no stored reference produces a `pay_captured_unposted` finding that never clears, even after the PSP refunds the payment. The fix is one line plus a test. The classification table belongs to ledger-finance, so please route the fix through them.

Environment:
- I used only `git archive` exports and edited no worktree. I re-verified each export against its archive after the mutant runs.
- I used two private DBs, `cr_fpay_rv` and `cr_d2f_rv`. Both were built fresh with `priv_db.sh` and both are dropped.
- I deleted my exports, plus my old D1/D2 exports, to save disk. Free space went from 5.8 G at the start to a low of 4.8 G, never below 3 G.
- There were no role or credential changes, and I touched no shared cache, directory or other agents' databases.

## Part 2: D2 final (diff 6836319..adcde1e)

### Findings

| # | Severity | Finding | Concrete scenario (probe) | Fix |
|---|---|---|---|---|
| D2F-1 | **Medium (confirmed by a probe; pre-merge)** | `callback_amount_asset_mismatch` is classed `reasonBound` (`payment_statement.go:~212`). But the callback T10 that writes it (`receipt.go:~823`, `ApplyDisputeFromNonTerminal`) does **not** bind the reference. When the callback resolves the attempt **by merchant reference** (a live attempt with no reference, such as a timed-out ambiguous one), the disputed attempt has `provider_reference = NULL`. As "bound", it is checked on the empty string: `capturedUnposted(a)` calls `capturedUnpostedRef("")`, which can never match a reversal line or a tombstone. | Probe:<br>1. A ref-less ambiguous deposit receives a verified callback (ref R, merchant = the attempt, amount 4999 versus 5000). Result: `disputed/callback_amount_asset_mismatch`, `ref=<nil>`.<br>2. Standing run with no line: 1 `pay_captured_unposted`.<br>3. **After the PSP's own reversal (tombstone on R): still 1 `pay_captured_unposted`.** It never clears, on every run, forever. M1 only acknowledges it.<br>4. A reversed line for R suppresses it in that one run only.<br><br>`multiple_success_for_intent` has the same shape by code reading: `applyMultipleSuccessDispute` (`orchestrator.go:622`) does not bind either. That gap predates D2, but D2's table is where it gets decided. | Make every bound reason "bound only if referenced". In `captureClass` (`payment_statement.go:~232`), return `reasonUnbound` when `c == reasonBound && a.providerRef == ""`. Equivalently, reclassify these rows as `reasonBoundIfReferenced`. A ref-less park then reports in-run, by merchant reference, and clears on the line's reference. Add an end-to-end test from my probe: a ref-less ambiguous attempt, a callback by merchant reference with a mismatched amount, then a tombstone on R must clear the finding. **This is ledger-finance's table, so they should confirm the reclassification.** |
| D2F-2 | Info | `success_for_never_sent_attempt` (T15) has no integration test. Mutant Y-T15-EXCLUDED is killed only by the unit pins (`TestD2_P1_*`). | The classification is pinned; the end-to-end behaviour is not. | Optional. |

### Focus answers

- **`captureClass` and the bound-if-referenced path** (`payment_statement.go:~220-255`).
  - It resolves correctly against the attempt's own row, and `boundCapture` and `unboundPark` are simple predicates.
  - The PM-1 change is right: a phase C conflict park with no reference counts as unbound, and the poll F-C4 park that holds X counts as bound and standing.
  - The only gap is D2F-1: "bound" assumes a reference that some write sites never bind.
- **Pin and literal consistency.**
  - `payment_reason_classification_test.go` iterates `payments.DepositDisputeTerminalReasons()`, expanding the `invalid_provider_reference:` prefix with every `providerref` reason and the bare form.
  - It compares each reason against a ledger-finance expectation table keyed by the **payments constants**, and it rejects stale or unclassified rows.
  - Any drift in a string or class fails the pin, as Y-CALLBACK-MISMATCH-EXCLUDED, Y-T15-EXCLUDED and both PM1 mutants show.
- **Alternative-path weakness.**
  - The forced scenarios check their precondition: `mustParked` checks the terminal reason, and the D2_7 r1 subtest checks what it relies on.
  - My mutants that remove a path all fail for the intended reason.
  - D2_11 covers only an attempt that holds a reference, which is why D2F-1 slipped through.
- **Determinism and simplicity.** There are no sleeps; the coverage windows are fixed offsets. The single table replaces two sets cleanly, and there is no duplication beyond the three detail strings.

### Commands (D2)

**Baseline:** `-run 'TestD2_|TestINVDEP1_|TestPaymentStatement_'` with `-p 1`: ok.

**Mutants** (`crd2f_mut.py`, each reverted). All 7 KILLED:

| Mutant | Killed by |
|---|---|
| **Y-B-ANYDISPUTED (R-1)** | `TestD2_7` |
| **PM1-CONFLICT-UNBOUND** | `TestD2_P1_Every…`, `TestD2_14` |
| PM1-CONFLICT-BOUND | `TestD2_P1`, `TestD2_7`, `TestD2_4`, `TestD2_14` |
| M-CLEAR-W2-ANY-TOMBSTONE | `TestD2_3` |
| Y-BIR-ALWAYS-BOUND (mine) | `TestD2_P1_BoundIfReferenced`, `TestD2_7`, `TestD2_4`, `TestD2_14` |
| Y-CALLBACK-MISMATCH-EXCLUDED (mine) | `TestD2_P1`, `TestD2_11` |
| Y-T15-EXCLUDED (mine) | unit pins only (D2F-2) |

**D2F-1 probe:** a temporary `zz_crd2f_probe_integration_test.go`, deleted afterwards. Results are in the table above.

**Race run:** `set -o pipefail`; `-race -tags integration -count=1 -p 1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./internal/reconciliation/...`.
- reconciliation ok (73 s), statement ok, exit 0, 0 FAIL lines, no DATA RACE.
- `go vet` printed nothing. The command's overall exit code was 1, which I put down to `pwd` failing after I deleted the export directory, but I did not re-run vet alone to confirm.

Logs are in the scratchpad: `crfp_race.log`, `crfp_mut_*.log`, `crd2f_race.log` and `crd2f_mut_*.log`.

## Files

**F-pay** (`/home/user/igaming-platform/.claude/worktrees/agent-a9ad199c50e9b929c/`):
- `internal/payments/kycgate.go`, around lines 67-77 (FP-1).
- `internal/payments/deposit_v2.go`, lines 210-231, and `internal/payments/drive.go`, line 102 (FP-1).
- `internal/payments/payout.go`, around lines 292-425 and 442-460.
- `internal/payments/payout_sweep.go`, around lines 107-160 and 316-330.
- `internal/payments/fpay_kyc_integration_test.go` and `internal/httpserver/kyc_payout_outage_503_integration_test.go`.
- `internal/withdrawal/withdrawal.go`, lines 172-183 (comment-only).

**D2** (branch `prh2-d2-recon-parked-capture` @ adcde1e; I reviewed it from a `git archive` of the repo at `/home/user/igaming-platform`, so these paths are relative to the repo root):
- `internal/reconciliation/payment_statement.go`: around line 212 (table) and lines 232-255 (`captureClass`).
- `internal/payments/receipt.go`, around line 823, and `internal/payments/orchestrator.go`, line 622: the write sites that do not bind.
- `internal/reconciliation/prh2_d2_postd1_integration_test.go`: `TestD2_11`.
- `internal/reconciliation/payment_reason_classification_test.go`
