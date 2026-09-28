# Code review — PRH-2 E2 (2026-09-28)

**Reviewer:** `code-reviewer`. The orchestrator recorded this review.

**Scope:** commit `9a25453` (`e2-prov-outbound-cred-1-legacy`, based on `cabca27`).

## Verdict: READY WITH CONDITIONS

The production deletion is correct and reduces risk. But the deleted chain's behaviour survives as a
near-verbatim test-only copy. Four tests that covered production `InitiateDeposit` now silently cover
that copy, and the ADR amendment describes the bridge inaccurately.

## Findings

| # | Sev | Finding | Required / recommended |
|---|---|---|---|
| E2-1 | **Medium (condition)** | **Four tests' subject changed silently to test code.** They are in `orchestrator_integration_test.go`: `TestInitiateDeposit_SynchronousDeclineNoCascade`, `_CascadesToSecondProviderOnCascadableDecline`, `_CascadeExhaustedEndsDeclined` and `_AmbiguousOutcomeIsNotCascaded`. They call `initiateDepositWithAttempt`, which now wraps `legacyShapeInitiateDeposit`. So a regression in the production cascade (`drive.go:396`) cannot fail them. This is based on reading the source; the MA/MB mutants could not be run. | Delete these four, or migrate them to `InitiateDepositAttempt`. Live coverage already exists (`TestInitiateDepositAttempt_DeclinedOutcome_T8`, `_AmbiguousOutcome_T6`, `TestDriveCreatedAttemptCascade_*`, `TestRVLF_H3_DriveGo_*`, `TestSweeper_PendingDeclineCascades_*`). Relabel `TestRVLF_L2_BridgeCascadeThenT13SecondCapture`: its assertion targets the live T13 path, but its setup depends on the copy's cascade. |
| E2-2 | Medium (condition, docs) | The ADR 0095 §29 amendment and the bridge's doc comment are inaccurate:<br>(a) "the only deposit-creation entry point … test or production". The test-tree `legacyShapeInitiateDeposit` is a second one, and it calls `provider.Deposit`/`QueryStatus` inside the caller's tx.<br>(b) "where the test's subject was the receipt/callback path". E2-1 contradicts this.<br>(c) "never a resurrected copy". It is line-for-line `cabca27:orchestrator.go:555-826`.<br>A stray "open." remains before "[CLOSED]". | Correct the wording. |
| E2-3 | Low (condition) | `RouteProvider` is orphaned in production; only tests call it. It still calls `HealthStatus` under the tx (the §9.6 gap), and its NOTE says it exists only for the deleted call sites. | Delete it and move the tests and the bridge to `ListRoutingCandidates` + `RankRoutingCandidates`. Or keep it with a corrected comment and a test-only guard. |
| E2-4 | Low | The bridge is not minimal. It has about 33 call sites in 13 files that mostly need only fixture state, yet it carries the full cascade and QueryStatus machinery, and txscope skips test files. | After E2-1, reduce it to the fixture shape the callback tests need. |
| E2-5 | Low | Stale production comments still describe the deleted chain as live:<br>- `deposit_v2.go:8-13`, `:69`, `:181`<br>- `drive.go:37`<br>- `attempt.go:4`<br>- `orchestrator.go:177`, `:313`, `:326`, `:370`, `:514`, `:612`, `:789`<br>- `types.go:7`, `:206`<br>- `httpserver/deposit_handlers.go:94`, `:102`<br>- `payment_deposit_simulation_handlers.go:58`, `:164`<br>- `idempotency/doc.go:14`<br>- `kyc/enforcement.go:16`<br>- `withdrawal/withdrawal.go:982` | Sweep them. |

## What checks out

- **Migrated sites** keep identical assertions, now against the live `InitiateDepositAttempt`. `TestInitiateDepositAttempt_AmbiguousOutcome_T6` is strengthened.
- **The three deleted tests** are justified and covered on the live path: X5, `INVDEP1_MC2`, `FL1`/`FL2`.
- **No production orphans** among the bridge's other primitives.
- `DepositIntentAlreadyResolvedRefusal` and `RecordDepositMultipleSuccessRefusal` have zero references.

## Verification (local, private DB `cr_e2_0928`)

- build, vet, gofmt, and lint with 0 issues;
- payments filtered `-race` integration, reconciliation `-race`, and httpserver `Financial|Deposit` `-race`: all ok.

**Not run:**
- the full payments suite;
- `resolution_isolation` (the timing lane);
- the MA/MB mutants, because the tool safety check was unavailable.
