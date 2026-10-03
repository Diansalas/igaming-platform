# PRH-2 K2 — Code review

_Reviewer: `code-reviewer`. Branch `prh2-k2-manual-adjustments` @ `18c357a`. Recorded verbatim by the orchestrator._

Code review — PRH-2 K2 (resumed 2026-10-03)

Scope: governed manual adjustments (ADR 0100, including §19/§20) on `prh2-k2-manual-adjustments` at `18c357a`, reviewed as `9c6c705..18c357a` (commits `1f45882`, `0bccb58`, `18c357a`). I exported with `git archive` and did not edit the worktree.

All test and mutant measurements below were taken before the container restart. Nothing has changed on `18c357a` since, so I did not re-run them.
- After the restart I confirmed my private DB `cr_k2_rv` still held the unmutated function bodies (`mutant=false` for all three mutated functions), then dropped it.
- I made no role, password or credential changes and never used the shared DB.

**Verdict: ACCEPT WITH CONDITIONS.**

The core is sound and well tested:
- the single counting function `ledger_adjustment_execution_status()`;
- execution in the final approval's transaction, with the A8 lock order;
- the DB-derived idempotency key and link verification;
- the payload hash;
- the migrate-through-0113 helper;
- the dot-directory-skipping walkers.

All four of my mutants on the counting function, payload hash and idempotency key are killed. The four modified pre-K2 assertions are **not** a weakening (verified by mutant). The conditions are one preventive-control gap related to K2-C1, one untested state transition with a misleading 200, and error-mapping polish.

Per your instruction, I excluded the two pre-existing `subject_tenant_id` failures (`TestINVDEP1_Recon_M_DuplicateDetector_LegacyDataShape`, `TestCasinoConsistency_C5_TombstoneConflict`), which are fixed on main by `9fcd08e`. I also did not duplicate security's K2-C1, K2-C2 or K2-C3.

## Findings (most severe first)

| ID | Severity | Finding | Concrete scenario | Fix |
|---|---|---|---|---|
| R-1 | **Medium**, related to and beyond K2-C1 | **The `executing → refused_insufficient_funds` exit does not check that nothing was posted.** `migrations/0113_governed_manual_adjustments.up.sql:1101-1109`: the branch only requires `NEW.ledger_transaction_id IS NULL`. Meanwhile the §6.6 fence (`ledger_governed_fence_allows`, `:1415`) admits a `manual_adjustment` insert whenever the request is `executing` with `executed_txid = txid_current()`. | A session that has legitimately reached `executing` (acting, or any tenant session, where the fence is a no-op) can post a `manual_adjustment` with the request's key (any amount or shape, per K2-C1), then set `refused_insufficient_funds`. The guard allows it, the deferred `no_executing_commit` is satisfied, and the posting commits **unlinked**, bypassing `ledger_adjustment_verify_link` (MA040) entirely. The Go executor never does this (`execute.go` returns before `Post` on that path), so exploiting it needs non-executor SQL in a governed transaction. The §12 detector would flag it after the fact as `ledger_unlinked_manual_adjustment` (P1), so it is detective-only. **Code-reading finding; I did not execute it.** | In the `refused_insufficient_funds` (and `refused_at_execution`) branches, raise MA040 if `EXISTS (SELECT 1 FROM ledger_transactions WHERE tenant_id = OLD.tenant_id AND idempotency_key = 'manual_adjustment:' \|\| OLD.id)`. Add a B-12-style test that posts and then refuses. Hand this to whoever closes K2-C1, because the closed-shape fence fix and this exit-path check should land together. |
| R-2 | **Medium** | **The request-expiry branch of `DecideInTx` is untested, and it returns HTTP 200 for a decision that was not recorded.** At `internal/adjustment/execute.go:105-112`, an expired pending request is moved to `expired`, audited, and returned with a nil error and **no approval row**. `internal/httpserver/manual_adjustment_routes.go:399` then writes **200** `{executed:false}`. **My mutant MEX** (`if expired && false`) **SURVIVED** the full `internal/adjustment` integration suite and `-run ManualAdjustment` in httpserver. The only "expired" tests cover grant expiry (`b6_usetime_integration_test.go:124`). | An approver submitting a late approval gets a 2xx and could reasonably believe the approval counted. With MEX in place, behaviour silently changes (the approvals guard would instead raise MA031 → 409), and no test notices. | Return a typed error (for example `ErrExpired` → 409 "request expired") after committing the `expired` transition, which needs the state write to commit while still returning an error. Alternatively, return `Outcome` with an explicit flag that the handler maps to 409. Add a test: create a request, force `expires_at` past on a scratch DB, decide, and assert 409, state `expired`, a `ledger_adjustment.expired` audit row, and no approval row. |
| R-3 | Low | **Error mapping.** `internal/adjustment/adjustment.go:230` maps every `MA0xx` code to 409 `"conflict"` with no reason. That includes client-input payload refusals such as `MA022` (unknown reason code, direction not allowed for the reason, evidence required, causation forbidden or required, compensation cap) and `MA021` (wallet or asset). Serialization and deadlock codes (40001, 40P01) fall to 500. | A back-office user who submits `credit_player` for a debit-only reason code gets an opaque 409 with no hint, and support cannot tell it apart from a concurrency conflict. | Map MA021/MA022/MA025 to 400 (`CodeValidation`) with the closed refusal-code enum (it is already the `split_part(…, ':', 2)` token and safe to expose). Map 40001/40P01 to 409 retryable. Keep the denied audit for each. |
| R-4 | Low | `runCleanExceptUnlinkedFixtures` (`internal/reconciliation/unlinked_manual_adjustment_helpers_integration_test.go:39`) decides "clean" by re-reading **persisted** mismatches by `run.ID` in a **new** transaction. If the run were not yet committed (the `CleanAfterEveryLifecycleFlow` call site runs inside the open `WithTenant` transaction), it reads zero rows and returns true. That call site is still safe only because it also checks the in-memory `ignoringUnlinkedFixtureAdjustments(ms)`. | A future caller using only the helper inside an uncommitted transaction would pass vacuously. | Make the helper take `ms []Mismatch` and decide in memory (`len(ignoringUnlinkedFixtureAdjustments(ms)) == 0`), dropping the DB read. |

## Your focus items

**`ledger_adjustment_execution_status()`, the single counting implementation (`0113…up.sql:1349-1407`).** Correct against ADR §6.2:
- `required = GREATEST(pinned, current)`, per §3.6;
- an approval counts only with: approve decision, equal payload hash, not the initiator, an eligible in-force grant (or the §7.6 invisible-platform residual), a non-NULL live Person distinct from the initiator and the beneficiary, not a contributing-policy author, and `DISTINCT ON (person)`;
- the initiator is re-validated the same way.

The same function gates both the executor and the `→ executing` guard (`:1083`), so there is no Go copy.

My mutants:
- **MK1** (drop `payload_hash` equality) and **MK2** (drop the initiator-Person exclusion): **KILLED** by `TestLayered_ExecutionCountIgnoresBadApprovals` on its file-migrated scratch DB, at `layered_counting_integration_test.go:76` and `:88` respectively. My first attempt applied them as live SQL on the private DB, which that scratch-DB test does not see, so it was a false survivor. Re-applied as migration-file mutants, both are killed. This matches the evidence file's S02/S04.

**Execution in the final approval's transaction (A8).** `execute.go:81-286` takes, in order: request `FOR UPDATE` (L1); the approval insert (no lock class); staff, then grants `FOR SHARE` in ascending id order; a recount under the locks; causation `FOR UPDATE` (L2); `GetOrCreateAccounts` before `LockProjectionsForPosting` (L3); `ledger.Post` (L4); then the link. This is consistent with ADR 0082 A8. K1's revoke locks only the grant row, so there is no cycle. The deferred `no_executing_commit` trigger forbids committing in `executing`, which is good, except for R-1's escape.

**Idempotency.**
- The key is DB-generated (`'manual_adjustment:' || id`, `:782`) and has a UNIQUE constraint.
- The executor builds the same key, and `AlreadyPosted` on a pending request fails closed.
- `verify_link` checks key, correlation, causation, reason, no provider fields, and exactly the §4 two-entry shape.
- **My mutant MK4** (the link's `idempotency_key` check dropped): **KILLED** by `TestB12_ShapeAndForgedLinkRefused`.

**Payload hash.** It uses collision-free length-prefixed canonical encoding (`:131`), and approvals pin it. **My mutant MK3** (`amount` removed from the hash, which would let an approval reviewed for one amount bind to a request with another): **KILLED** by `TestB4_PayloadBindingRefusals`.

**The four modified pre-K2 assertions: not a weakening.** **My mutant MW** made `RunLedgerVsProjection` append one genuine `balance_mismatch` to every run. All four modified tests still **FAIL**: `TestCasinoConsistency_CleanWorldReconcilesToZero`, `TestSportsbookSettlementRecon_CleanAfterEveryLifecycleFlow`, `TestSportsbookSettlementRecon_DriftInjectionPerKind` and `TestReconciliationSweep_UnchangedOverLockedSplitAccounts`. They exempt only `ledger_unlinked_manual_adjustment`. The robustness caveat is R-4.

**Migrations through 0113 only.** `scratchPoolThroughDir` (`internal/adjustment/b6_usetime_integration_test.go:184`) copies only files numbered ≤ the version and asserts the last one applied. B-19 does up/down/up with a whole-schema snapshot diff. Correct.

**Static walkers skip dot-directories.** `internal/adjustment/static_test.go:47` skips `.`- and `_`-prefixed directories and `testdata`. `null_arm_replay_static_test.go` reads only the migrations directory (`:310`), with no tree walk. Correct.

**Error mapping and audit over HTTP.** Forbidden, disabled, exposure, conflict and session-invalid each write a denied audit and a fixed body; not-found and invalid do not. See R-2 and R-3. HTTP coverage is `TestManualAdjustmentAPI_A1_Matrix` and `TestManualAdjustmentAPI_A12_AuditContent`, which is adequate apart from R-2.

**Evidence file.** "68 mutants, 68 KILLED" is consistent with what I could reproduce (S02, S04, plus my MK3 and MK4). The disclosed run history (S32 first-run survival, G1 hang, S11 invalid run) is honest.

**Simplification (Info).**
- `ledger_adjustment_live_person` and `ledger_adjustment_invisible_platform_grant` each probe `staff_users` with the same EXISTS check, and both run once per candidate inside the counting CTE. Folding them into one lookup per candidate would halve the probes. Not a correctness issue.

## Commands run and results

All run before the restart, on `cr_k2_rv`, a private DB rebuilt from the `18c357a` export:
- Baseline `priv_test.sh -tags integration ./internal/adjustment/`: ok (61s).
- Live SQL **MK1** and **MK2**: false survivors (the killing test uses a scratch DB). Re-run as migration-file mutants with `-run TestLayered_ExecutionCountIgnoresBadApprovals`: **KILLED** both.
- **MK3** (live SQL payload hash): **KILLED** (`TestB4_PayloadBindingRefusals`). **MK4** (live SQL `verify_link`): **KILLED** (`TestB12_ShapeAndForgedLinkRefused`). Each was restored and verified.
- **MW** (Go copy, injected `balance_mismatch`): all 4 modified tests FAIL. Not a weakening.
- **MEX** (Go copy, request-expiry branch disabled): full `internal/adjustment` plus `-run ManualAdjustment` httpserver → ok, so it **SURVIVED** (R-2).
- `-race -tags integration -count=1 -p 1` across adjustment, db, reconciliation, wallet, ledger, capability, auth, httpserver and cmd/platform-api: all ok except the 2 pre-existing `subject_tenant_id` reconciliation failures, which are excluded as instructed.
- `go vet -tags integration` on adjustment, httpserver, reconciliation and db: clean. `golangci-lint run --allow-parallel-runners ./...`: 0 issues. Unit `go test ./internal/adjustment/ ./internal/db/` (static tests): ok.

After the restart:
- I confirmed that `cr_k2_rv` held the original function bodies (`mutant=false` for all three), then dropped it ("dropped cr_k2_rv").
- I deleted the mutant copies.

## Relevant paths (worktree `/home/user/igaming-platform/.claude/worktrees/agent-a6572bf833858955c`)
- `migrations/0113_governed_manual_adjustments.up.sql:1101-1109` (R-1); `:1349-1407` (counting); `:1415` (the fence); `:782` (the idempotency key)
- `internal/adjustment/execute.go:105-112` (R-2); `:81-286` (A8 flow)
- `internal/httpserver/manual_adjustment_routes.go:399` (R-2's 200); `:172-203` (mapping)
- `internal/adjustment/adjustment.go:207-236`, specifically `:230` (R-3)
- `internal/reconciliation/unlinked_manual_adjustment_helpers_integration_test.go:39` (R-4)
- `internal/adjustment/layered_counting_integration_test.go` (the MK1/MK2 killer)
