_Reviewer: `code-reviewer`. Recorded verbatim by the orchestrator._

> **Orchestrator note (2026-10-03):** R-1 (add a negative subtest to `TestD2_7`: B disputed with a bound or other reason, line resolved to A with merchant B, succeeded; expect only the `check=merchant` mismatch against A and no `pay_captured_unposted` for B from this line; re-kill `Y-B-ANYDISPUTED`) is held for the D2 implementer's post-D1 batch, because D2's remaining implementation must not start before D1 has a merge SHA. The disk-full incident described below is reported to the user; no shared cache or directory has been cleaned.

# Code re-review: PRH-2 D2 at 0c1e8ac (diff 0005aa7..0c1e8ac)

**Verdict: READY.** `checkMerchantAttribution` follows ledger-finance ruling (a) exactly. My D2-1 probe now gives the expected findings. It raises nothing on correct lines: the full existing reconciliation suite passes unchanged. X1, X2, X4 and X6 are killed when I re-applied them myself. One non-blocking test gap remains (R-1).

I reviewed a `git archive` export and did not edit the worktree; the export matched the archive byte for byte after the mutant runs. Private DB `cr_d2_rv3` was built fresh with `priv_db.sh` and dropped at the end (`dropped cr_d2_rv3`). There were no role or credential changes.

**Incident to note, not caused by a privileged action.** Partway through, the root filesystem filled up (20 MB free, "no space left on device"). The shared Postgres at 127.0.0.1:5432 briefly reported "the database system is in recovery mode".
- I freed about 400 MB by deleting only my own earlier review exports in the scratchpad.
- I touched nothing shared: not the 14 GB `/root/.cache/go-build`, and not the stale `/tmp/go-build*` directories from Sep 25–28 that other processes left behind.
- Postgres recovered by itself within about 10 s, after which I rebuilt my DB and continued.
- Free space was 1.4 GB at the end. Someone should reclaim space (the go-build cache or the stale `/tmp/go-build*` directories) before more parallel suites run. That is a decision about shared infrastructure for you, not for me.

## Findings

| # | Severity | Finding | Scenario | Fix |
|---|---|---|---|---|
| R-1 | Low (test gap, a mutant survived) | The guard that limits the B captured-unposted step to `isUnboundParkReason(b.terminalReason)` (`payment_statement.go`, in `checkMerchantAttribution`) is not covered by any test. | My mutant Y-B-ANYDISPUTED dropped that guard, so the step fires for **any** disputed B. It **survived** `TestD2_|TestINVDEP1_|TestPaymentStatement_`. Consequence: a line resolved to A whose merchant reference names a B disputed for `reversal_tombstone_precedes_success`, or another non-capture reason, would raise a false `pay_captured_unposted` against B. If B is a bound-reason park (for example `sync_amount_mismatch`), it would be reported twice: once from this line and once from B's own in-run or standing rule. | Add a negative subtest to `TestD2_7`: B is disputed with a bound or other reason, the line has ref R and merchant B, status succeeded. Expect only the `check=merchant` mismatch against A, and no `pay_captured_unposted` for B from this line. |

## Focus items

- **Attempt identity is compared, not strings.** It resolves `b := m.byMerchant[l.merchant]` and then compares `b == a`.
  - Mutant Y-SELF removed the `b == a` early return. It was killed by five existing clean-run tests (`TestPaymentStatement_CleanMockRunOverMixedHistory`, `…NoFinancialEffect`, `TestINVDEP1_Recon_M_…`, among others). This also shows that the existing suite would catch any new finding on a correct line.
- **"No platform attempt" and other-operation cases.**
  - `b == nil` gives `check=merchant` with "names no platform attempt of this provider".
  - `b.operation != op` gives `check=merchant` naming B and its operation.
  - Both return before the B step, which is correct: B is either absent or of the wrong operation.
  - My mutant Y-OTHEROP-SILENT was killed by `TestD2_8/names_an_attempt_of_the_other_operation`.
  - `byMerchant` is loaded per tenant and per provider (`loadPlatform`, `a.provider_id = $2`), so "of this provider" is accurate.
- **B is not consumed.** The new code never writes `matchedBy`. X4 (record B) is killed by `TestD2_7/c7_B_not_consumed` and `TestD2_9`.
- **A's own checks are unchanged.** The call sits after the existing `byMerchant` check and before the settlement, asset, amount and status checks, and it only adds findings. Probe case 2 confirms that A's `pay_status_mismatch` still fires next to `check=merchant`.
- **No findings on correct lines.** The full `./internal/reconciliation/...` suite with `-race` passed with 0 FAIL lines, and that suite includes exact-set assertions such as `d2Expect`. The new check runs only when `!byMerchant && l.merchant != ""`, so lines with an empty merchant reference are unaffected.
- **D2-1 probe re-run.** Fixture: holder A bound to R, B parked as `provider_reference_conflict` on R, one line with R, merchant B, succeeded.
  - Holder succeeded: exactly `pay_reference_mismatch … attempt=A check=merchant` plus `pay_captured_unposted … attempt=B`. This was 0 findings at 0005aa7.
  - Holder pending: the same two findings plus `pay_status_mismatch` against A.
- **X6 claim: confirmed.** `byRef` is keyed on `a.operation + a.providerRef`, so for any line resolved by provider reference, `l.ref == a.providerRef` by construction. Only a payout line resolved through `bySettlement` has `l.ref != a.providerRef`. TestD2_9b's shape is reachable in production, not fixture-only: payouts do write `invalid_provider_reference[:reason]` (`payout.go:495-497`, `:938-940`), so a payout B with an unbound reason is real. X6 is killed only by `TestD2_9b`.
- **§35.2** is accurate now. It covers the one-line versus two-line cases, the cross-check rule, B not consumed, additivity and the holder-pending result. The P2 wording ("allocation (LEDGER-SUSPENSE-B-1); M1 only acknowledges") is consistent at all three sites and pinned in `d2CUFor`.
- **Simplicity.** It is one function of about 40 lines with a four-way switch, reuses the existing helpers (`byMerchant`, `capturedUnpostedRef`, `isUnboundParkReason`), and needs no schema change. It is appropriately small.

## Commands and results

**D2-1 probe:** a temporary `zz_crd2_probe_integration_test.go`, deleted afterwards. Results are in the probe item above.

**Mutants** (`crd2_mut.py`, anchors that must match exactly once; `-run 'TestD2_|TestINVDEP1_|TestPaymentStatement_'`):

| Mutant | Result | Killed by |
|---|---|---|
| **X1** cross-check removed | KILLED | TestD2_7 (c1, c2 ×2, c3), … |
| **X2** gated back on `byMerchant` | KILLED | TestD2_7, … |
| **X4** B recorded in `matchedBy` | KILLED | TestD2_7/c7, TestD2_9 |
| **X6** clear B on `a.providerRef` | KILLED | TestD2_9b |
| Y-SELF (mine): no `b == a` return | KILLED | 5 existing clean-run tests |
| Y-OTHEROP-SILENT (mine) | KILLED | TestD2_8 |
| Y-B-ANYDISPUTED (mine) | **SURVIVED** | (R-1) |

**Full suite:** `set -o pipefail`; `-tags integration -race -count=1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_'` on `./internal/reconciliation/...`.
- Both packages `ok` (56 s), exit 0, 0 FAIL lines, no DATA RACE.

**Static checks:** `go vet -tags integration` clean; golangci-lint 2.9.0: 0 issues; `gofmt -l`: clean.

Logs are in the scratchpad: `crd2r_race.log` and `crd2_mut_<name>.log` (the Y and X logs overwrote the earlier ones of the same name).

## Files

All paths are under `/home/user/igaming-platform/.claude/worktrees/agent-a4e8fbc8d827abc87/`:
- `internal/reconciliation/payment_statement.go`: the call in `matchPayment`, `checkMerchantAttribution`, and `loadPlatform` (byMerchant, around line 756).
- `internal/reconciliation/prh2_d2_merchant_crosscheck_integration_test.go`: R-1 belongs in `TestD2_7`.
- `docs/decisions/0095-provider-io-transaction-boundary-and-payment-contract.md`: §35.2.
- `docs/plans/payment-readiness/evidence/prh2-d2-mutation-kill.txt`

The ruling is at `/home/user/igaming-platform/docs/plans/prh2-hardening-round/reviews/d2-1-ledger-finance-ruling.md`.
