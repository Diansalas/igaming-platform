_Reviewer: `code-reviewer`. Branch `prh2-fpay-kyc-gate` @ `3c049ed`. Recorded verbatim by the orchestrator (the F-pay part of a combined hand-back; the D2 part is in `d2-code-final-review.md`)._

> **Orchestrator decision (2026-10-03):** FP-1 (the deposit `allow` decision row commits even when the deposit is then declined for no routable provider, or by the kill switch at cascade T2) is made **pre-merge** as a documentation-and-comment fix only. Moving the row after routing would require edits to `deposit_v2.go` and `drive.go`, which are NOT F-pay's critical files (I-wire edits them). So: state in ADR 0096 §24 that a deposit `allow` row records the evaluation, not the claim, and correct the `kycgate.go` comment. FP-2 and FP-3 are informational.

# Code reviews: PRH-2 F-pay (prh2-fpay-kyc-gate @ 3c049ed) and PRH-2 D2 final (prh2-d2-recon-parked-capture @ adcde1e)

Both reviews are below.
- **F-pay: READY WITH CONDITIONS.** One low-severity inconsistency: deposit "allow" decision rows commit even when the deposit is then declined. Fix the code or document it.
- **D2 final: READY WITH CONDITIONS, with a pre-merge condition.** A probe confirms that a disputed attempt with a "bound" reason but no stored reference produces a `pay_captured_unposted` finding that never clears, even after the PSP refunds the payment. The fix is one line plus a test. The classification table belongs to ledger-finance, so please route the fix through them.

Environment:
- I used only `git archive` exports and edited no worktree. I re-verified each export against its archive after the mutant runs.
- I used two private DBs, `cr_fpay_rv` and `cr_d2f_rv`. Both were built fresh with `priv_db.sh` and both are dropped.
- I deleted my exports, plus my old D1/D2 exports, to save disk. Free space went from 5.8 G at the start to a low of 4.8 G, never below 3 G.
- There were no role or credential changes, and I touched no shared cache, directory or other agents' databases.

## Part 1: F-pay (diff 6836319..3c049ed)

**withdrawal.go is comment-only (confirmed).** Every changed line in `internal/withdrawal/withdrawal.go:172-183` is a comment.

### Findings

| # | Severity | Finding | Scenario | Fix |
|---|---|---|---|---|
| FP-1 | Low (inconsistent with the payout rule and with its own comment) | The deposit gate writes the decision row inside `EvaluateDeposit` (`kycgate.go:~67-77`). That runs **before** routing in `InitiateDepositAttempt` (`deposit_v2.go:210-231`), and before the parent lock and claim in the cascade T2 (`drive.go:102`). If routing then fails, `finalizeDeclined(…"no_routable_provider")` returns nil and the transaction commits. The same happens with a kill-switch decline at T2. | Result: an `allow`/`passed` `kyc_enforcement_decisions` row, plus its audit, commits for a deposit that was declined with no claim. The payout path deliberately avoids exactly this: `payout.go:~361` writes the allow row only after routing, and `TestClaimForDispatch_NoRoutableProvider_NoAllowDecisionRow` pins it. The comment at `kycgate.go:~67` says the row "commits with exactly the effect it authorizes", which is not true for an allow followed by a no-route decline. No money is affected; this is about audit and compliance accuracy. | Either write the deposit allow row after routing (the payout pattern), or state in ADR 0096 §24 that a deposit allow row records the evaluation and not the claim, and fix the comment. |
| FP-2 | Info | At T12, an outage reschedules **twice** per tick. `resubmitPayoutAmbiguous` first calls `PollPayoutStatus`, which reschedules (`payout_sweep.go:~318`). The `unavailable` branch of `gateAndEscalateOnDeny` then reschedules again with the same `nextPoll`. | `poll_count` goes up by 2 per T12 outage tick, so backoff grows twice as fast. That is harmless. It is also why the T12 test cannot catch a missing reschedule (MU-6b is caught only at T2). I agree with the disclosure: T2 and T12 share one function, the T2 test pins it, and at T12 the extra reschedule is redundant anyway. | None needed. |
| FP-3 | Info (loop hazard is bounded) | During an outage, each tick writes one `unavailable` decision row and one audit per due attempt. `RescheduleNonTerminal` increments `poll_count`, and `backoff` (`sweeper.go:219`) is exponential with a hard cap. | Steady state is about one row and one audit per attempt per cap interval, or two at T12. Acceptable. A `RecordDecision` failure during the outage is the registered LF-I3-5. | None. |

### Focus answers

- **Commit-then-error in `ClaimForDispatch`** (`payout.go:~292-425`) is clean.
  - The `unavailable` branch writes the decision and the denied submit audit, sets `kycUnavailable`, and returns nil, so `WithTenant` commits normally.
  - The typed `ErrPayoutKYCUnavailable` is returned only after `WithTenant` has returned with no error. If the commit itself fails, the existing `err != nil` path returns that real error, so nothing is masked and no transaction is left half-open.
  - `WithTenant` has no retry loop, so a stale `kycUnavailable` flag cannot survive a retried closure.
  - The branch runs before `DenyForCompliance` and before routing and the kill switch, as ADR 0096 §24 requires.
- **Sweeper reschedule** (`payout_sweep.go:~131-138`):
  - It records the decision first, then calls `RescheduleNonTerminal(nextActionAt)`. That increments `poll_count` and sets `next_action_at` to the backoff.
  - There is no state change, no Escalate, no deny audit and no resend; `allowed=false` keeps the caller away from the provider.
  - `RescheduleNonTerminal` requires a non-null `next_action_at`, which the batch claim always sets.
- **PAY-PAYOUT-ERRREF-1** (`payout.go:~442-460`): the reference is validated before the error return, and an invalid result is scrubbed down to the outcome alone. Correct.
- **Tests and the D1-style weakness.**
  - Each outage test asserts **exactly one `unavailable` decision row**. That is impossible on the deny or allow paths, so every test would fail if the outage path did not run.
  - Fault injection holds `ACCESS EXCLUSIVE` on `kyc_verifications` for the whole call, with `lock_timeout` on the victim only, so it is deterministic.
  - The HTTP test is deterministic too. Only the submit call goes through the `lock_timeout` pool. A 503 from the other `ErrPayoutKYCUnavailable` shape (the gate's own Go error) would roll back and leave no decision row, so the test's row assertion tells the two apart.
  - There are no sleeps. The only time comparison is `next_action_at` after versus before, read from the DB, which is an ordering check.
  - No existing test file is modified, so none is weakened.
- **Deposit `not_required` rows.** One insert plus one audit per deposit evaluation, including the cascade T2. That is proportionate, because deposits are rare compared with bets; casino and sportsbook play paths correctly skip it. FP-1 is the only concern.
- **Simplicity.** The changes are small and follow the established F-kyc pattern. The only duplication is the two `RescheduleNonTerminal` calls in `gateAndEscalateOnDeny`, which is acceptable.

### Commands (F-pay)

**Baseline:** targeted `-run 'TestClaimForDispatch_|TestSweeper_T2|TestSweeper_T12|TestDepositGate_|TestPayoutAdapterCall_ErrorPath|TestPayoutDispatch_ErrorPlusHostile|TestPayoutDispatch_OversizeProviderReference_Parks'` with `-p 1`: ok.

**Mutants** (`crfp_mut.py`, each reverted):

| Mutant | Result | Killed by |
|---|---|---|
| MU-1 | KILLED | T1p outage test |
| MU-4 | KILLED | T1p outage test |
| **MU-6b** | KILLED | T2 outage test |
| MU-9 as a compilable variant (validate only when `err == nil`) | KILLED | `ErrorPathReferenceValidation`, `ErrorPlusHostile` |
| Y4 (mine): no error after the commit | KILLED | T1p outage test |
| Y6 (mine): deposit row on deny only | KILLED | `TestDepositGate_RecordsOneDecisionRowPerEvaluation` |
| Y8 (mine): unavailable reschedules only if already escalated | KILLED | T2 and T12 outage tests |

My first MU-9 form did not compile and is not counted.

**Race run:** `set -o pipefail`; `-race -tags integration -count=1 -p 1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./internal/payments/ ./internal/withdrawal/`.
- payments ok (564 s), withdrawal ok (10 s), exit 0, 0 FAIL lines, no DATA RACE.

**HTTP outage tests:** `TestSubmitWithdrawalHandler_KYCStoreOutageReturns503`, `TestRequestWithdrawalHandler_KYCStoreOutageReturns503` and `TestSubmitWithdrawal*`, run with `-race -count=3 -p 1`: ok.
