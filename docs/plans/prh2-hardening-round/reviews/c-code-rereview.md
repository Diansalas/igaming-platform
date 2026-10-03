_Reviewer: `code-reviewer`. Recorded verbatim by the orchestrator._

# Code re-review: PRH-2 C at c2d1fc1 (fix 34779b3, fixtures c2d1fc1, merge e8f56e0)

**Verdict: READY.** All four earlier findings are fixed, and my F1 probe now shows the correct behaviour. Every mutant I re-applied, including M2, K1 and CR-B (now C-INTENT-1), is killed. The fixture change in c2d1fc1 is the right approach, and those tests keep their original subject. I have no new blocking findings.

I reviewed a `git archive` export of c2d1fc1 and did not edit the worktree. After the mutant runs, the export matched the archive byte for byte. Private DB `cr_c_rv2` was built fresh with `priv_db.sh` and dropped at the end (`dropped cr_c_rv2`). There were no role or credential changes.

## Earlier findings

| # | Status | How I checked |
|---|---|---|
| F1 (error-path reference not validated) | **Fixed** | `drive.go:296-311` now validates the reference before the error return. Pending requires a reference only when `err == nil`; otherwise the optional validator applies. An invalid reference returns a scrubbed result with `ErrorClassProviderRefInvalid` and the attempt parks. The probe results are below. Mutant X1 (error return moved back above the validation) is killed by `TestDepRef_ErrorPathWithInvalidReference_ParksNothingPersisted`. |
| F2 (park returned the stale intent) | **Fixed** | `assertParkedNoMoney` now checks `res.Intent.Status` (`dep_ref_validate_integration_test.go:194`). I re-applied CR-B myself and it is **KILLED** by `…InvalidReferenceOnAnyOutcomeParks…`, `…ParkedAttemptIsNotAnErrorLoop…` and `…ErrorPathWithInvalidReference…`. |
| F3 (N2 checked only "disputed") | **Fixed** | N2 now asserts `TerminalReasonTombstonePrecedesSuccess`, one dispute audit, and the intent recomputed to `ambiguous` (`rvlf_i1_regression_integration_test.go:1857-1877`). Mutant X7 (tombstone T10 without the shared park, so no audit or recompute) is killed by N2. |
| F4 (order of checks undocumented) | **Fixed** | ADR 0095 §34.8 lists the order: reference invalid (including the error path), binding conflict, amount mismatch, tombstone, INV-DEP-1, post. This matches `applyDepositCallResult` line for line. It also states that all four T10s go through `parkDepositAttempt`. |

## F1 probe re-run against c2d1fc1

The adapter returns an `Ambiguous` result with an error, a redirect, and the reference below. I aged the leases in the fixture before each sweep.

| Reference returned with the error | Result | Sweeper |
|---|---|---|
| 256 bytes | No error. Attempt `disputed/invalid_provider_reference:too_long`, no reference on the attempt or the intent. Intent `ambiguous`; the returned `res.Intent` is also `ambiguous`. Redirect `""`. 0 ledger transactions, 1 dispute audit, 0 audit rows containing the raw value. | claimed=0 on both passes, unchanged |
| control character | Same, with reason `control_char` | claimed=0 on both passes, unchanged |
| valid `valid-err-ref` | No error. Attempt `ambiguous` with the reference bound on T6. Intent `ambiguous` with the same reference. Redirect `""`, no dispute. | claimed=1 each pass; it is polled by reference (`poll_count` 1, 2, 3) instead of being rescheduled forever as at fc0d18b |

## Review of the fixture change (c2d1fc1)

- **Why it is needed.** T6 now binds a returned reference (`MarkAmbiguousFromSubmittingBindingRef`). The MOCK's `MockAmountAmbiguous` returns one, so the four tests' premise ("ambiguous, reference not known yet") was false. I removed the wrapper in two of them to show this:
  - X8: `TestReceipt_Unresolved_…` fails with "expected exactly 1 deferred receipt applied, got 0". Its later `MarkAccepted(unknownRef)` cannot replace the reference that is already bound.
  - X9: `TestINVDEP1_I_…` fails with "expected a cascade child: no rows".

  Both are killed, so the wrapper is necessary and not decoration.
- **Wrapper versus changing the MOCK.** The wrapper is the simpler correct choice.
  - "Ambiguous with a reference" is a legitimate provider answer, and the default MOCK still exercises it, which is the new T6 binding path.
  - Changing the MOCK would silently change every other `MockAmountAmbiguous` user, such as `TestDepositV2…v2-ambiguous`, and would lose the case where an ambiguous result carries a reference.
  - `refLessAmbiguousProvider` strips the reference only for `Ambiguous` with no error. That exactly models the timeout these tests describe ("timeout surrogate", "this attempt has no provider_reference yet").
- **Original subject kept.** All four (`TestA7_3_`, `TestINVDEP1_I_`, `TestReceipt_Unresolved_…`, `TestReceipt_DeferredReceipt_PredatesSubmission…`) still test what their comments say: a deferred receipt applied once the reference becomes known, and a timeout followed by a fallback and a late original. Their assertions are unchanged.
- **Nit, optional:** `refLessAmbiguousProvider` is defined at the bottom of `dep_ref_validate_integration_test.go` but used by three other files. A shared helper file would be easier to find.

## playerFacingRedirect

- **Correct.** It is keyed on the committed state re-read after phase C, and only `pending` returns a redirect or token. It is applied at both exits (`deposit_v2.go:337`, `drive.go:248`). The cascade loop's own `driveCreatedAttempt` output is scrubbed too.
- **The only race fails closed.** A callback that moves the attempt between the phase C commit and the re-read can only suppress a redirect for an attempt that is already decided. It can never expose one for a parked attempt, because those states are terminal.
- **Simple:** five lines and one rule.
- **Covered:**
  - X3 (always return the redirect) is killed by four conflict and mismatch tests.
  - X4 (no scrub in `driveCreatedAttempt`) is killed by `…CascadeDrivenParkReturnsNoRedirect`.
  - X5 (no scrub in `InitiateDepositAttempt`) is killed by four tests.
  - `TestDepRef_BoundaryAndNonParkingPaths` asserts that a pending attempt keeps its redirect and token.
- One behaviour change: an `Ambiguous` result that carries a redirect no longer returns it. That matches the §34.8 statement. Whether any real PSP needs a redirect on an ambiguous result belongs to the `PROVIDER DEPENDENT` adapter contract, not this change.

## Other code added in the fix commit

- **T6 binding** uses `COALESCE(provider_reference, NULLIF($4,''))`, so an already-bound reference cannot be overwritten. It runs only after validation and the binding pre-check. X2 (no binding) is killed by `…ErrorPathWithValidReference…` and `…MissingEcho_ReferenceBound_SweepPollsAndPostsOnce`.
- **Binding the reference at the `sync_amount_mismatch` park** (`drive.go` `parkDepositAttempt` `bindRef`) runs a plain UPDATE restricted to live states before the dispute CAS. X6 (do not bind) is killed by `…LateCallbackAfterMismatchPark…` and `…MismatchPark_BindsReferenceAndAuditsAdapterOutcome`.
- The tombstone branch now goes through the shared park, with the audit and the intent recompute. This is a ledger-finance item (LF F-C2); the code is correct and covered by N2.

## Commands and results

- **F1 probe:** a temporary `zz_crc_probe_integration_test.go`, deleted afterwards. Results are in the table above.
- **Mutants:** `crc2_mut.py`, one at a time, each reverted.
  - Targeted set: `TestDepRef|TestDepSync|TestDepositAdapterCall|TestCompareProviderAmount|TestMockDeposit_Echoes|TestINVDEP1_|TestKSCASDiscrim|TestRVLF_N2|TestRVLF_N4|TestX5_|TestA7_3_|TestReceipt_|TestDepositV2`.
  - **All 12 KILLED:** M2, K1, CR-B/C-INTENT-1, X1 to X7, X8 and X9.
  - M2 is killed by `TestINVDEP1_BackstopBranch_…`; K1 by `TestKSCASDiscrim_…`.
- **Full suite:** `set -o pipefail`; `-tags integration -race -count=1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_'` on `./internal/payments/` and `./internal/providerref/`.
  - Both `ok` (payments 501 s), exit 0, 0 FAIL lines, no DATA RACE.
- **Static checks and unit tests:**
  - Unit `go test ./internal/payments/`: ok.
  - `go vet -tags integration`: clean.
  - golangci-lint 2.9.0 (`--build-tags integration`): 0 issues.
  - `gofmt -l`: clean.

The logs are in the scratchpad (`/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad`): `crc2_race.log` and `crc2_mut_<name>.log`.

## Files

All paths are under `/home/user/igaming-platform/.claude/worktrees/agent-a368b6ad544bcaee1/`:
- `internal/payments/drive.go`: `playerFacingRedirect` at line 258; error-path validation at 296-311; the shared park; T6 call at 618.
- `internal/payments/deposit_v2.go:337`
- `internal/payments/attempt.go`: `MarkAmbiguousFromSubmittingBindingRef`
- `internal/payments/dep_ref_validate_integration_test.go`: `refLessAmbiguousProvider` at the end of the file.
- `internal/payments/inv_dep1_matrix_integration_test.go`: `newInvDep1SetupWith`
- `internal/payments/receipt_integration_test.go`
- `internal/payments/a7_lockorder_integration_test.go`
- `internal/payments/rvlf_i1_regression_integration_test.go`: lines 1857-1877.
- `docs/decisions/0095-*.md`: §34.8, at line 6384.
