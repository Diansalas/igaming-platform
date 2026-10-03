_Reviewer: `code-reviewer`. Recorded verbatim by the orchestrator._

# Code review: PRH-2 D2 (PAY-RECON-PARKED-CAPTURE-1)

Branch `prh2-d2-recon-parked-capture`, diff `564c515..0005aa7`. I reviewed from a `git archive` export and did not edit the worktree. After the mutant runs, the export matched the archive byte for byte.

**Verdict: READY WITH CONDITIONS.** The code matches the ledger-finance ruling (a)/(c), and every mutant I re-applied is killed. The one gap I found is a silent case in the "conflict with another deposit" scenario. ADR 0095 §35.2 currently says that case is covered, and the doc is only true when two lines are present. The conditions are to correct that doc statement and to send the matcher question to `ledger-finance`. It is a financial-attribution call, so I am raising it for them to decide, not deciding it myself.

## Findings, most severe first

| # | Severity | Finding | Concrete scenario | Required action |
|---|---|---|---|---|
| D2-1 | **Medium. Confirmed by a probe. The matcher gap predates D2; the ADR's coverage claim is new.** | When a deposit has a reference conflict with another *deposit* attempt, and the statement has only **one** line for the reference R, the run is silent. `matchPayment` (`payment_statement.go:937`) resolves a line by provider reference first. The line's merchant reference is cross-checked only when the line resolved *by merchant* (`:964`). A line with R and the parked attempt's merchant reference therefore lands on the holder. | Probe: holder A succeeded and bound to R. Attempt B was parked as `provider_reference_conflict` on R. The statement has one line: R, merchant = B, succeeded. **The run produced 0 mismatches**: no `pay_captured_unposted`, no reference mismatch. The PSP is saying R's capture belongs to B's player, while the platform credited A's player. With the holder still `pending` instead, it produced one `pay_status_mismatch` against the *holder*, which is loud but attributed to the wrong attempt. §35.2 says this case gives `pay_duplicate` "Verified by test". `TestD2_4/deposit_bound_conflict_second_succeeded_line_is_pay_duplicate` checks that only with **two** lines. | (1) Correct §35.2 to state that the single-line case is silent today. (2) `ledger-finance` decides whether a line resolved by reference whose non-empty merchant reference names a *different* attempt should raise `pay_reference_mismatch` (for example `check=merchant`). If so, register it as a follow-up next to PAY-RECON-PARKED-CAPTURE-STANDING-1. Two notes for that decision: the change touches every deposit line, not only parks; and D2's unbound rule would then also fire for these parks. |
| D2-2 | Low (drift guard) | Reason literals are hard-coded and not imported from `payments`. Keeping the dependency one-way is the right coupling: production reconciliation never imports `internal/payments` today. The drift risk is mostly covered already. The tests produce `sync_amount_mismatch`, `provider_reference_conflict` and `invalid_provider_reference:*` through the **real** `payments` code, so a renamed constant fails the tests. The two poll reasons are inserted by a fixture using the plan's strings. I checked D1 at `47777cb`: `TerminalReasonPollAmountMismatch = "poll_amount_mismatch"` and `TerminalReasonPollReferenceMismatch = "poll_reference_mismatch"`, so they match today. | If D1 renames a poll reason, D2 silently stops flagging it, and only §35.1's "change in the same merge" note protects against that. | When D1 merges, add a small test in `internal/reconciliation` that compares `capturedUnpostedBoundReasons` and `isUnboundParkReason` against the `payments` constants. Reconciliation tests already import `payments`, so this adds no production coupling. Alternatively, have the D2 fixture use the D1 constants. |

## Focus items

- **Widened bound predicates.** In-run (`:990`) and standing (`:1075`) both use `capturedUnpostedBoundReasons`.
  - Leaving `reversal_tombstone_precedes_success` out is correct: a tombstone on the same reference always clears the flag, so including it would change nothing. `TestD2_6` pins this.
  - The in-run case still requires `l.status == succeeded`, so a `reversed` line clears it (R1).
- **Unbound in-run rule** (`:999`). It requires a disputed attempt with an unbound reason and a succeeded line, and it clears on the **line's** reference. That is correct, because the attempt has no reference of its own.
  - Not gating on `byMerchant` is the right call. When the attempt holds no reference the gate makes no difference, and when it does hold one, dropping the gate is the more conservative choice. Pinned by `…unbound_reason_on_a_reference_holding_attempt_is_still_flagged`.
  - Standing is correctly excluded and disclosed as NOT IMPLEMENTED in §35.4.
  - `isUnboundParkReason` also accepts the bare `invalid_provider_reference` with no suffix. Payments writes that form when `providerref.AsError` fails, so this is correct.
- **Clearing.** `capturedUnpostedRef` covers exactly the two signals: a reversal line in this run naming the reference, or a tombstone on that reference. Both widening mutants are killed.
- **Detail strings.** The `ReconciliationKey` is unchanged (`… check=captured_unposted`); only the expected and actual text changed.
  - Nothing parses those values. The only consumers are the HTTP DTO, which passes them through, and a 0107-down fixture INSERT that does not depend on the text.
  - The full reconciliation suite, including `TestINVDEP1_C5`, passes.
  - The F13 wording ("M1 only acknowledges") is used only on the unbound rule. The bound rules keep the §28.9 text, and the tests pin both (mutant Y3).
- **Determinism and simplicity.** There are no sleeps, no goroutines and no `t.Parallel`. The coverage windows are fixed offsets from now (±47–48 h), with no wall-clock assertions. Fixtures go through real phase C parks, with `ApplyDisputeFromNonTerminal` used only for the D1 reasons. `d2Run` takes a before/after snapshot, so each run is checked to write only run and mismatch rows. The production change is small: one set, one helper, one extra case.

## Commands and results

The private DB `cr_d2_rv` was built fresh with `priv_db.sh` and dropped at the end (`dropped cr_d2_rv`). There were no role or credential changes.

- **Baseline:** `-run 'TestD2_|TestINVDEP1_|TestPaymentStatement_'` on `./internal/reconciliation/`: ok.
- **Mutants** (`crd2_mut.py`, anchors that must match exactly once, same `-run` set). All 9 KILLED:

| Mutant | Killed by |
|---|---|
| **M-PRED** | all `TestD2_1` subtests |
| **M-CLEAR-W1-ANY-REVERSAL** | `TestD2_3/*/reversal_line_or_tombstone_on_another_reference_does_not_clear` |
| **M-CLEAR-W2-ANY-TOMBSTONE** | same as above |
| M-UNBOUND-BYMERCHANT-GATE | `TestD2_4` |
| M-STANDING-UNBOUND-WIDEN | `TestD2_4` |
| Y1 (mine): exact `invalid_provider_reference` only | `TestD2_5` |
| Y2 (mine): `terminal_reason=` dropped from the bound detail | `TestD2_1` |
| Y3 (mine): unbound rule using the old M1 wording | `TestD2_4` |

- **D2-1 probe:** a temporary `zz_crd2_probe_integration_test.go`, deleted afterwards. With the holder succeeded the run had no mismatches; with the holder pending it had one `pay_status_mismatch`, against the holder.
- **Full suite:** `set -o pipefail`; `-tags integration -race -count=1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_'` on `./internal/reconciliation/...`.
  - Both packages `ok` (69 s), exit 0, 0 FAIL lines, no DATA RACE.
- **Static checks:** `go vet -tags integration` clean; golangci-lint 2.9.0: 0 issues; `gofmt -l`: clean.

Logs are in the scratchpad: `crd2_race.log` and `crd2_mut_<name>.log`.

## Files

All paths are under `/home/user/igaming-platform/.claude/worktrees/agent-a4e8fbc8d827abc87/`:
- `internal/reconciliation/payment_statement.go`: line 937 (resolution by reference), 964 (merchant cross-check only when resolved by merchant), 990 and 999 (in-run cases), 1075 (standing).
- `internal/reconciliation/prh2_d2_parked_capture_integration_test.go`: `TestD2_4`, around line 573.
- `docs/decisions/0095-provider-io-transaction-boundary-and-payment-contract.md`: §35.2 needs the correction.
- `docs/plans/payment-readiness/evidence/prh2-d2-mutation-kill.txt`
