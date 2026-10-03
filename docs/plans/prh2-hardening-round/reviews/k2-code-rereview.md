# PRH-2 K2 — Code re-review

_Reviewer: `code-reviewer`. Branch `prh2-k2-manual-adjustments` @ `309f33b`. Recorded verbatim by the orchestrator._

Code re-review — PRH-2 K2 (R-1..R-4), 2026-10-03

Scope: `prh2-k2-manual-adjustments` at `309f33b`, reviewed as `18c357a..309f33b`:
- the fix commit `99d5a8f`;
- the evidence commits `d17b382` and `309f33b`;
- the main merges `8a7feb3` (49cf696) and `24cb753` (c458bb8), whose content I did not review.

I exported with `git archive 309f33b` and used a fresh private DB, `cr_k2r_rv`, which I dropped afterwards. I made no role, password or credential changes and never used the shared DB.

Per your instruction, the K2-C1, C2 and C3 security fixes were reviewed only for correctness and simplicity.

## Verdict per R-item: all four CLOSED

| Item | Verdict | Verification |
|---|---|---|
| **R-1** (a non-executed exit could leave an unlinked governed posting) | **CLOSED** | `migrations/0113_governed_manual_adjustments.up.sql:1047`: every exit to `refused_insufficient_funds`, `refused_at_execution`, `rejected`, `cancelled` or `expired` raises **MA040** if a `ledger_transactions` row with key `'manual_adjustment:'\|\|id` exists, in any session type. **My mutant MR1** (`IF false AND …`, with the DB rebuilt from the mutant migration): `TestK2R1_TenantSessionNonExecutedExitRefusedAfterGovernedPosting` **and** `TestK2C1_ActingFenceEnforcesClosedShape` **FAIL**, so it is **KILLED**. Together with the deferred no-commit-in-`executing` trigger and `verify_link` on the executed exit, a governed posting can no longer survive except as a verified link. |
| **R-2** (decide on an expired request returned 200 without recording the decision) | **CLOSED** | `execute.go` sets `Outcome.Expired`, so the expiry transition and its audit row commit. `Service.Decide` then returns `ErrRequestExpired`, which maps to **409 "request expired"** plus a denied audit. **Re-applied MEX** (`if expired && false`): `TestManualAdjustmentAPI_K2R2_ExpiredDecisionIs409` **FAILS**, so it is **KILLED** (previously it survived). |
| **R-3** (error mapping) | **CLOSED** | MA021, MA022 and MA025 map to **400** with a closed-set token. 40001 and 40P01 map to a **retryable 409**. Every class writes a denied audit. `TestClassifyError_K2Review` and `TestRefusalToken_ClosedSet` (unit) pass. Crucially, `manual_adjustment_api_integration_test.go:425` pins a **real** DB-raised token (`direction_not_allowed_for_reason`) end to end as an HTTP 400, so the SQL message format and the Go parser are coupled by a test. **My mutant MR3** (closed-set lookup disabled, so every response becomes a generic token): `TestRefusalToken_ClosedSet` **FAILS**, so it is **KILLED**. |
| **R-4** (the clean-except-unlinked helper could pass vacuously) | **CLOSED** | `cleanExceptUnlinkedFixtures(run, ms)` decides in memory. A clean run must have zero mismatches; a non-clean run must have at least one, each carrying `RunID == run.ID` and of kind `ledger_unlinked_manual_adjustment`. A nil run id gives false. `persistedCleanExceptUnlinkedFixtures` (used for the sweep, where only the Run is returned) reads every stored row and applies the same rule. **Re-applied MW** (a real `balance_mismatch` injected into every `RunLedgerVsProjection`): `TestCasinoConsistency_CleanWorldReconcilesToZero`, `TestSportsbookSettlementRecon_CleanAfterEveryLifecycleFlow` and `TestSportsbookSettlementRecon_DriftInjectionPerKind` all **FAIL**. Not vacuous. |

## The security fixes (correctness and simplicity only; security owns the verdict)

- **K2-C1(ii) closed-shape entry fence** (`0113…up.sql` around `:1475-1500`). Each acting-inserted entry must be one leg of the request's §4 shape: wallet `player_cash` in the player direction, or the tenant `manual_adjustment` house account in the opposite direction, with the request's asset and amount. At most one leg per direction and at most two entries are allowed. This is logically correct. It relies on row-level BEFORE triggers seeing earlier rows from the same multi-row INSERT, which Postgres provides for VOLATILE trigger functions (the default); `TestK2C1_ActingFenceEnforcesClosedShape` passes.
- **K2-C2 and K2-C3:** `TestK2C2_FenceStateArmAfterExit` and `TestK2C3_DBRecountOnExecuting` pass. MR1 is also killed by the K2-C1 test, so the two layers reinforce each other.

## New observations (non-blocking)

| ID | Severity | Observation | Suggested fix |
|---|---|---|---|
| N-1 | Low (simplicity / robustness) | `RefusalToken` (`internal/adjustment/adjustment.go:273-277`) extracts the token by parsing `pgErr.Message` (`LastIndex "refused: "`). The closed-set check plus the per-code fallback make this safe, and the HTTP test at `:425` pins the coupling. It is still the one place in K2 that classifies by message text rather than SQLSTATE or structured fields. | Raise the token in a structured field in SQL, for example `USING ERRCODE = …, DETAIL = <token>` or `HINT`, and read `pgErr.Detail`, dropping the string search. |
| N-2 | Info (simplicity) | The duplicate-leg check at `0113…up.sql:1499` nests `(SELECT count(*) …) >= 2` inside the `EXISTS` subquery's `OR`, so the count is re-evaluated per existing entry. | Split it into two plain checks: `IF (SELECT count(*) …) >= 2 OR EXISTS (… AND e.direction = NEW.direction) THEN`. This has the same semantics and reads clearer. |
| N-3 | Info | The 0113 down file now documents that it must run in a single transaction, because a MA099 refusal under psql autocommit would leave FORCE RLS lifted. That is correct and consistent with `db.MigrateDown`. | None. |

## Commands run and results

- `git log --first-parent 18c357a..309f33b`, and `git show 99d5a8f` for the migrations, `internal/adjustment`, the httpserver routes and the reconciliation/wallet tests.
- `git archive 309f33b` to `$S/cr-k2r/src`. `PRIV_DB=cr_k2r_rv priv_db.sh` built a fresh DB.
- On the real code: `-run 'TestK2R1_|TestManualAdjustmentAPI_K2R2_|TestClassify|RefusalToken|TestK2C'` on adjustment and httpserver, all PASS (K2R1, K2R2, K2C1, K2C2, K2C3, ClassifyError, RefusalToken).
- **MR1** (DB rebuilt from the mutant migration): K2R1 and K2C1 FAIL, so it is **KILLED**. The DB was then rebuilt clean.
- **MEX:** K2R2 FAIL, so it is **KILLED**. **MR3** (unit): `TestRefusalToken_ClosedSet` FAIL, so it is **KILLED**. **MW:** all three reconciliation tests FAIL, so it is **KILLED**.
- `priv_test.sh -race -tags integration -count=1 -p 1 -skip 'TestStoreOutage_DoesNotPinPool|TestResolutionIsolation_' ./internal/adjustment/... ./internal/db/... ./internal/reconciliation/... ./internal/wallet/... ./internal/ledger/... ./internal/audit/... ./internal/httpserver/ ./cmd/platform-api/`: all ok (httpserver 465s), exit 0, no FAIL, no DATA RACE. The two `subject_tenant_id` reconciliation failures from the previous round are gone after the main merge.
- `go vet -tags integration` on adjustment, httpserver and reconciliation: clean. `golangci-lint run --allow-parallel-runners ./...`: 0 issues.
- Cleanup: `PRIV_DB=cr_k2r_rv drop_priv_db.sh` → "dropped cr_k2r_rv". I deleted all mutant copies.

## Relevant paths (worktree `/home/user/igaming-platform/.claude/worktrees/agent-a6572bf833858955c`)
- `migrations/0113_governed_manual_adjustments.up.sql:1047` (R-1 guard); `:1475-1500` (the K2-C1(ii) entry fence; N-2 at `:1499`)
- `internal/adjustment/execute.go` (`Outcome.Expired`, `Service.Decide`: R-2)
- `internal/adjustment/adjustment.go:273-277` (`RefusalToken`, N-1); `ClassifyError` (R-3)
- `internal/httpserver/manual_adjustment_routes.go` (`writeAdjustmentError`: the R-2 and R-3 mappings)
- `internal/httpserver/manual_adjustment_api_integration_test.go:425` (the real-token HTTP pin)
- `internal/reconciliation/unlinked_manual_adjustment_helpers_integration_test.go` (R-4 helpers)
