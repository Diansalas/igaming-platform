_Reviewer: `code-reviewer`. Branch `prh2-d1-poll-amount` @ `ea17d68`. Recorded verbatim by the orchestrator._

> **Orchestrator decision (2026-10-03):** D1-CR-1 (assert the Missing reschedule: `poll_count` +1 and `next_action_at` past the lease; mutant Z4 survived) and D1-CR-2 (N2-sweeper asserts `TerminalReason == TerminalReasonTombstonePrecedesSuccess`) are made **pre-merge** and folded into the QA-F1 commit round, together with D1-CR-3 (comment-only: cite ADR §36.1 rather than §34.8). D1-CR-4 (add the bare `invalid_provider_reference` form to the exported list) is optional and not pre-merge; it is added to PAY-POLL-ECHO-HARDENING-1.

# Code review: PRH-2 D1 (`prh2-d1-poll-amount` @ ea17d68, diff 564c515..ea17d68)

**Verdict: READY WITH CONDITIONS.** There is no correctness bug. Every financial path posts only under the bound reference. Every contradiction parks or audits without posting, and Missing never posts. All six of the implementer's mutants I re-applied are killed, as are four of my own. The full `-race` run is green.

The conditions are two low-severity test gaps, D1-CR-1 and D1-CR-2. Each is a one- or two-line assertion and can land before merge or as an immediate follow-up. D1-CR-3 and D1-CR-4 are informational.

I reviewed a `git archive` export of ea17d68 and did not edit the worktree; the export matched the archive byte for byte after the mutant runs. Private DB `cr_d1_rv` was built fresh with `priv_db.sh` and dropped at the end (`dropped cr_d1_rv`). There were no role or credential changes, and I touched no shared cache, directory or other database.

## Findings, most severe first

| # | Severity | Finding | Concrete scenario | Fix |
|---|---|---|---|---|
| D1-CR-1 | Low (test gap, a mutant survived) | When a poll on a live attempt is Missing, the attempt is rescheduled with backoff (`poll_evidence.go:108`, `RescheduleNonTerminal`), but no test checks that reschedule. My mutant Z4 replaced it with `return true, nil` and **survived** the D1 targeted set. `TestPollAmount_Missing_NeverPosts_StaysLiveAndAudited` (`poll_amount_integration_test.go:248-250`) asserts only `next_action_at IS NOT NULL`. That holds anyway, because `claimBatch` (`sweeper.go:~199`) sets `next_action_at = lease_until`. | If the reschedule is lost, an adapter that never echoes an amount is re-polled on every lease interval forever. `poll_count` never increments, so there is no backoff growth and nothing for the registered PAY-DEPOSIT-ESCALATION-1 to count, and one `payment.attempt_poll_amount_unconfirmed` audit row is written per lease. The PSP sees a flat polling rate, and the audit volume grows without bound. | In that test, also assert that `poll_count` went up by one and that `next_action_at` moved past the lease value, or matches `s.backoff(prev)`. |
| D1-CR-2 | Low (fixture fragility) | `TestRVLF_N2_SweeperGo_SuccessAfterTombstoneDisputesNotIndexError` (`rvlf_i1_regression_integration_test.go:~1997`) asserts only `State == disputed`. D1 added `Amount: 5000, AssetCode: "EUR"` to the fixture, which correctly matches `rvInit(…, 5000, …)`, so the test currently exercises its original tombstone subject and is **not weakened**. | Poll mismatches are now checked before the tombstone. If the echo in this fixture ever drifts to a different amount, N2 still passes, but through `poll_amount_mismatch`. This is the same wrong-reason pass that C's F3 fixed for the drive.go twin. | Assert `TerminalReason == TerminalReasonTombstonePrecedesSuccess`, as `TestRVLF_N2_DriveGo` now does. |
| D1-CR-3 | Info (order and comment) | The poll path checks amount, then echo, then binding, then tombstone. §34.8 (callback/sync) checks binding before amount. `poll_evidence.go:5-7` and `sweeper.go:~535` say the poll path "mirrors the callback path's order (§34.8)", which is not literally true. ADR §36.1's own table states the actual order correctly. | The order matters only when two checks apply. One consequence: a Missing poll whose bound reference is also held elsewhere (F-C4, for example a payout Step B key) stays live and is rescheduled, instead of parking as `provider_reference_conflict`. Nothing posts, so there is no money risk. The conflict is decided on the first poll that carries an amount. | Change the two comments to cite §36.1 rather than §34.8. No code change. |
| D1-CR-4 | Info | `DepositDisputeTerminalReasons()` includes only the prefix `invalid_provider_reference:`. `IsDepositDisputeTerminalReason("invalid_provider_reference")` returns false, yet `drive.go` (and `payout.go`) write the bare form when `providerref.AsError` fails. In practice that never happens: the gate wraps a `providerref.Error` with `%w`, so `AsError` always succeeds. | D2's `isUnboundParkReason` already accepts the bare form, so reconciliation is safe. Only a consumer that pins against this list would misclassify the bare form. | Optional: add the bare constant to the list. |

## Focus items

1. **Plan, the D1/D2 split, and ADR §36.** The implementation matches:
   - All four bound-reference uses take `boundRef` from `*attempt.ProviderReference`: tombstone, binding, `postDepositSuccessOrDispute` and `ApplySuccess`.
   - The branch returns an error rather than falling back to the echo if no reference is bound (`sweeper.go:~537`).
   - `ledger.Post` rejects an empty `ProviderTxID` with `ErrInvalidEntry` (`lockorder.go:355`). I grepped every non-ledger caller that sets `ProviderTxID` (casino, withdrawal, payments). None passes `""` deliberately, and nothing classifies the 23514 that used to come back, so the typed sentinel changes no behaviour.
2. **Order.** It is amount/asset, echo, binding, tombstone, INV-DEP-1, post, as ADR §36.1 states. The note about §34.8 is in D1-CR-3.
3. **Shared helpers.**
   - `pollAmountEvidence` wraps `CompareProviderAmount` with the poll-only tightening for partial contradictions, and the succeeded-state audit (`sweeper.go:~501`) reuses it, so there is one rule.
   - `echoAuditMeta` is the single validate-or-hash point, used at three sites.
   - All live contradictions go through `parkDepositAttempt` via the `contradict` closure. Declined attempts get `auditPollTerminalContradiction`.
   - There is no meaningful duplication.
4. **Existing-test changes.** Neither is weakened. N2-sweeper's fixture echo matches the attempt; its reason assertion is D1-CR-2. `TestDepRef_SyncSuccess_CallbackDuringPhaseB…` is strengthened: the `t.Logf` became assertions that the receipt was resolved, that a redelivered callback is `duplicate_effect`, and that there is exactly one posting.
5. **receipt.go.** The only change is the variadic `extra ...map[string]any` on `auditTerminalAmountAssetMismatch`, merged into the metadata. Existing callers are unchanged.
6. **Determinism.** There is no `time.Sleep` and no wall-clock assertion.
   - Fault injection holds `LOCK TABLE … IN SHARE MODE` in another transaction for the whole attempt, with `SET LOCAL lock_timeout='1ms'` in the victim. 55P03 is therefore guaranteed and needs no clock.
   - Lease and `next_action_at` are driven through fixtures (`setNextActionNow`).
   - The only goroutine test (`TestPollSuccess_RacingCallback_PostsExactlyOnce`) asserts an outcome invariant that holds under any interleaving: one posting, `succeeded`, no dispute, and the projection equals a rebuild. That satisfies T-2.
   - The deferred-receipt drains are safe with the stale `attempt.State` passed in, because `ApplyDeferredReceiptsForAttempt` re-reads the attempt per receipt (`receipt.go:~1598`).
7. **Mutants** (`crd1_mut.py`, one at a time, each reverted). Targeted set: the implementer's `-run`, with `-p 1`. Baseline ok.

   | Mutant | Result | Killed by |
   |---|---|---|
   | **D-F1-1** decline uses the echo | KILLED | `TestPollDecline_EchoNeverOverwritesTheBoundReference` |
   | **D-F2-1** raw echo stored as `provider_reference` | KILLED | `TestF3SM_TerminalMismatchAudit_NeverStoresARawEcho` |
   | **D-M1-1** declined + Missing silent | KILLED | `TestPollDeclinedAttempt_MissingEvidenceIsAuditedOnce…` |
   | **D-FC4-1** ledger check never matches | KILLED | both `TestFC4_*` |
   | D-TOMB-2 tombstone skipped on empty echo | KILLED | `TestPollTombstone_…` |
   | Z1 (mine): F-C4 tombstone filter dropped | KILLED | `TestPollTombstone`, `TestParkFaultInjection_PhaseC`, `TestRVLF_N2_DriveGo` |
   | Z3 (mine): noise short-circuit also swallows success | KILLED | `TestF3SM_*`, `TestPollDeclinedAttempt_*` |
   | Z5 (mine): tightening on amount only | KILLED | `TestPollAmount_Mismatch_ParksNoPosting` |
   | Z6 (mine): poll-success drain dropped | KILLED | `TestDeferredReceipt_PollSuccessOnT6Bound…` |
   | **Z4 (mine): Missing not rescheduled** | **SURVIVED** | (D1-CR-1) |

   I agree with the evidence file's two classifications. D-ECHO-2 is unreachable because of the COALESCE plus the earlier echo park. D-DRAIN-4 is the pre-existing site covered by PAY-RECEIPT-T4-DRAIN-TEST-1.
8. **Race run.** `set -o pipefail`; `-tags integration -race -count=1 -p 1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_'` on `./internal/payments/... ./internal/ledger/... ./internal/txscope/...`.
   - payments ok (457 s), ledger ok (25 s), txscope ok (2 s).
   - Exit 0, 0 FAIL lines, no DATA RACE.

**Static checks.**
- `go vet -tags integration`: clean. `gofmt -l`: clean.
- The static reason test `TestDepositDisputeTerminalReasons_*` passes.
- golangci-lint reports one issue: `internal/ledger/lockorder_harness_test.go:214 loHoldWithRollback is unused`. That file is unchanged between 564c515 and ea17d68, so it is not D1's.

Logs are in the scratchpad: `crd1_race.log` and `crd1_mut_<name>.log`.

## Files

All paths are under `/home/user/igaming-platform/.claude/worktrees/agent-a1924f400143860c9/`:
- `internal/payments/poll_evidence.go`: lines 5-7 for D1-CR-3; line 108 for D1-CR-1.
- `internal/payments/sweeper.go`: the noise short-circuit at around line 427; the Pending, success and decline branches between about 440 and 600.
- `internal/payments/drive.go`: F-C4 at around lines 443-461; the phase C drain at around 605.
- `internal/payments/deposit_terminal_reasons.go` (D1-CR-4).
- `internal/payments/poll_amount_integration_test.go`: lines 217-265 for D1-CR-1.
- `internal/payments/rvlf_i1_regression_integration_test.go`: around line 1997 for D1-CR-2.
- `internal/ledger/lockorder.go`: line 355.
- `docs/decisions/0095-provider-io-transaction-boundary-and-payment-contract.md`: §36.
- `docs/plans/payment-readiness/evidence/prh2-d1-mutation-kill.txt`
