_Reviewer: `code-reviewer`. Recorded verbatim by the orchestrator._

# Code review: PRH-2 C (PAY-DEP-REF-VALIDATE-1, INVDEP1-BACKSTOP-BRANCH-TEST-1, KS-CAS-DISCRIM-TEST-1)

Branch `prh2-c-dep-ref-validate`, diff `c458bb8..fc0d18b`. I reviewed from a `git archive` export and did not edit the worktree.

**Verdict: NOT READY. One fix is needed (F1), plus a small test for it.** Everything else is sound:
- The M2 and K1 tests are real and deterministic.
- The rvlf fixture edit keeps N2 and N4 on their original subject.
- `CompareProviderAmount` suits D.

## Findings, most severe first

| # | Severity | Finding | Concrete scenario | Required fix |
|---|---|---|---|---|
| F1 | **High (confirmed by a probe)** | On the adapter error path the reference is never validated. `internal/payments/drive.go:278-279` returns `res` untouched with `ErrorClassAmbiguous` **before** the new validation. `gate.go:195` passes the value through even when `callErr` is set. Phase C then runs the binding query with the raw reference (`drive.go:436`). The default branch (`drive.go:575-580`) then writes it to `deposit_intents.provider_reference` through `finalizeAmbiguous`. This contradicts plan C ("an invalid reference on **any** outcome → park"), ADR 0095 §34's "any" row, and the scrub promise in the `depositAdapterCall` comment. | An adapter returns `(DepositResult{Outcome: Ambiguous, ProviderReference: <256 bytes or "ref\x07…">}, errors.New("read timeout"))`. My probe drove this through the real `InitiateDepositAttempt`. The result is `deposit_intents_provider_reference_ref_bound` (SQLSTATE 23514), returned as an untyped error, and phase C rolls back. The attempt stays `submitting` with no reference. After I aged `lease_until`/`next_action_at` in the fixture, the sweeper claimed it on every pass (`poll=1`, then `poll=2`), and `processViaQueryStatus` (`sweeper.go:328-334`) just reschedules it forever. The intent stays `pending` with no route to resolution. This is exactly the stuck attempt that §34.3 says C removes. A further case: a *valid* reference on the error path also returned the adapter's `RedirectURL` (`https://evil.invalid/x`) to the player, because nothing on that path is scrubbed. | In `depositAdapterCall`, apply `providerref.ValidateOptional` to a non-empty `res.ProviderReference` **before** the `err != nil` return. If it is invalid, return the scrubbed `DepositResult{Outcome: res.Outcome}` with `ErrorClassProviderRefInvalid`, so the call parks like every other outcome. Add an integration case to `TestDepRef_InvalidReferenceOnAnyOutcomeParks_…`: an error together with an oversize or control-character reference must park with no persistence. `depRefProvider.Deposit` (`dep_ref_validate_integration_test.go:34-43`) cannot express this today: it drops the script whenever `err != nil` and cannot return an error itself. Whether a redirect or token may be handed back on an error-path ambiguous result is a security question. **I am surfacing it to `security`, not deciding it.** `payoutAdapterCall` (`payout.go:389-391`) has the same error-path gap; it is outside C's scope, so flag it to `security` and the payout owner. |
| F2 | Low (test gap, a mutant survived) | The `ProviderRefInvalid` park returns the updated intent, but no test checks the returned value. | My mutant CR-B made `drive.go:430-431` return the stale `intent` instead of `updated`. It **survived** the targeted set. `InitiateDepositAttempt` would then report the intent as `pending` to its caller while the database says `ambiguous`. `assertParkedNoMoney` only reads the database. | In `assertParkedNoMoney` (`dep_ref_validate_integration_test.go:147`), also assert `res.Intent.Status == DepositIntentAmbiguous`. |
| F3 | Low (fixture fragility) | `TestRVLF_N2_DriveGo` (`rvlf_i1_regression_integration_test.go:1853-1856`) asserts only `State == disputed`, not the terminal reason. | The fixture edit is correct today: rvInit is 5000 EUR and the echo is 5000 EUR, and my mutant CR-G (tombstone check disabled) is killed by N2. But if that echo ever drifts, N2 passes again through `sync_amount_mismatch`. That is the very wrong-reason pass the evidence file warns about. | Assert `TerminalReason == "reversal_tombstone_precedes_success"` in N2. |
| F4 | Info (order of checks) | The code's order is **reference-invalid → binding → amount mismatch → tombstone → INV-DEP-1 → post**. The brief said mismatch comes before binding. | Each of these steps parks with no money and no error, so swapping binding and mismatch only changes which terminal reason is recorded. Running binding first is defensible, because it applies to every outcome and mismatch applies only to Succeeded. | No code change needed. ADR 0095 §34.2/§34.3 should state the full order in one place, so D can mirror it deliberately. |
| F5 | Info (for D) | `CompareProviderAmount` takes plain values, so it fits `StatusResult.Amount`/`StatusResult.AssetCode` (`types.go:541-542`) without change. Partial evidence (a contradicting amount with an empty asset) is classed **Missing**, not Mismatch. | On the sync path, Missing becomes Ambiguous and the poll decides, which is safe. On the poll path (D) there is no later step to defer to. | D must decide explicitly what Missing means on the poll. It must never post on Missing. |
| F6 | Info | `parkDepositAttempt` uses `ApplyDisputeFromNonTerminal`, which fails with a CAS conflict if the attempt is not live. | My mutant CR-A (no self-exclusion on the intent) was killed only by N4, which runs `applyDepositCallResult` on a *declined* attempt; the park then failed with a CAS conflict. Production callers always reach phase C from `submitting`, which is the same assumption the existing tombstone branch makes. So this is not a regression. | None. Noting it in case any future path re-drives phase C on a terminal attempt. |

## Focus items

- **Park paths:** the three parks share one helper. That helper never posts, always commits, writes an audit with a closed reason (`ref_reason`, length, hash prefix; no raw value), recomputes the intent projection, and leaves `next_action_at` NULL. The only exception is F1.
- **N2/N4 fixture edit:** it does not weaken them, and both stay on their original subject. CR-G (tombstone disabled) is killed by N2; N4 still exercises the T13 path.
- **Backstop race test** (`invdep1_backstop_branch_integration_test.go`): it is deterministic.
  - It forces the race with a held projection-row lock.
  - It waits on `pg_stat_activity` blocking-PID conditions (`loWaitBlocked`, `loWaitBlockedByAnyOf`), not on timing.
  - There is no `time.Sleep` in the test itself; the helpers' 10 ms poll interval is condition polling, not a timing assumption.
  - There is no wall-clock assertion, so T-1 and T-2 are satisfied.
  - `loWaitBlockedByAnyOf` also returns true if racer B finishes early. That is covered by the assertions that B was disputed, that the reason is `multiple_success_for_intent`, and that the backstop P1 was logged exactly once.
- **Simplicity:** the change is proportionate. `foreignReferenceBinding` makes two small indexed lookups, and the explicit `tenant_id` predicate is redundant under FORCE RLS but harmless.
- **Constant-time comparison checklist item:** not applicable; there is no webhook scheme code in this diff.

## Commands and results

The private DB `cr_c_rv` was built fresh with `priv_db.sh` from the exported source and dropped at the end (`dropped cr_c_rv`). There were no role or credential changes.

**F1 probe** (temporary `zz_crc_probe_integration_test.go`, deleted afterwards):

| Reference returned with the error | Result |
|---|---|
| 256 bytes | 23514 error; attempt `submitting`, no reference; sweeper claims it every pass, `poll_count` 1 → 2 |
| control character | same as above |
| valid `valid-err-ref` | attempt and intent `ambiguous`, intent reference stored, adapter redirect returned to the caller |

**Mutants** I re-applied myself (`crc_mut.py`). Each was applied once and reverted, and afterwards the source matched the archive byte for byte. The targeted set was the evidence set plus `TestRVLF_N2|TestRVLF_N4|TestX5_`.

| Mutant | What it changes | Result |
|---|---|---|
| **M2** | `orchestrator.go:716`, backstop branch disabled | **KILLED** by `TestINVDEP1_BackstopBranch_…`, for the right reason: racer B gets the propagated sentinel error |
| **K1** | `drive.go:159`, never return on `!engaged` | **KILLED** by `TestKSCASDiscrim_…`, which got `<nil>` instead of `ErrAttemptStateConflict` |
| CR-D | binding check runs only on Succeeded | KILLED (`…ParksEveryOutcome` pending/declined/ambiguous) |
| CR-A | no self-exclusion in the intent binding query | KILLED (N4) |
| CR-F | mock sync success echoes nothing | KILLED (`TestMockDeposit_EchoesAmountAndAsset`) |
| CR-G | tombstone check disabled | KILLED (N2) |
| CR-B | the invalid-reference park returns the stale intent | **SURVIVED** (F2) |

**Suites and static checks:**
- `set -o pipefail`; `-tags integration -race -count=1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_'` on `./internal/payments/` and `./internal/providerref/`: both `ok` (payments took 452 s); 0 FAIL lines and no DATA RACE.
- Unit `go test ./internal/payments/`: ok.
- `go vet -tags integration`: clean.
- golangci-lint 2.9.0 (`--build-tags integration`): 0 issues.
- `gofmt -l`: clean.

Logs are in the scratchpad (`/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad`): `crc_race.log` and `crc_mut_<name>.log`.

## Files
- /home/user/igaming-platform/.claude/worktrees/agent-a368b6ad544bcaee1/internal/payments/drive.go (lines 278-279 for F1; 430-431 for F2; 436 for the binding call; 575-580 for the ambiguous persistence)
- /home/user/igaming-platform/.claude/worktrees/agent-a368b6ad544bcaee1/internal/payments/gate.go:195
- /home/user/igaming-platform/.claude/worktrees/agent-a368b6ad544bcaee1/internal/payments/sweeper.go:328-334
- /home/user/igaming-platform/.claude/worktrees/agent-a368b6ad544bcaee1/internal/payments/payout.go:389-391 (same gap, out of scope, for `security`)
- /home/user/igaming-platform/.claude/worktrees/agent-a368b6ad544bcaee1/internal/payments/dep_ref_validate_integration_test.go (lines 34-43 for the probe limitation; 147 for F2)
- /home/user/igaming-platform/.claude/worktrees/agent-a368b6ad544bcaee1/internal/payments/rvlf_i1_regression_integration_test.go:1853-1856 (F3)
- /home/user/igaming-platform/.claude/worktrees/agent-a368b6ad544bcaee1/internal/payments/invdep1_backstop_branch_integration_test.go
- /home/user/igaming-platform/.claude/worktrees/agent-a368b6ad544bcaee1/internal/payments/ks_cas_discrim_integration_test.go
- /home/user/igaming-platform/.claude/worktrees/agent-a368b6ad544bcaee1/internal/payments/amount_evidence.go
