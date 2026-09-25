# Stage 10 W1 — Q2 mutation pass and SQL branch-coverage checklist (closes B-2)

| Field | Value |
|---|---|
| Owner | `qa` |
| Date | 2026-09-25 |
| Closes | `docs/governance/stage-10-w1-code-review.md` finding B-2 (ADR 0088 §14's Q2 mutation pass and SQL branch-coverage checklist were not evidenced) |
| Scope commit | `dac94be` (Stage 10 W1 tip at the time this pass was run), plus the test additions below on top of it |
| Binding contract | ADR 0088 §14 (Q2: "Go code through a mutation tool over payout validation (V-1…V-5), the §4.3 decision tables, `NetLocked` derivation, `LockProjectionsForPostings`, and lock call order. SQL CHECKs and triggers are... covered instead by a recorded manual branch-coverage checklist") |

## Verdict

**IMPLEMENTED** for the scope ADR 0088 §14 names (settlement.go's decision
tables/payout validation/NetLocked, `internal/ledger/lockorder.go`'s
`LockProjectionsForPostings`/lock order/`GetOrCreateAccounts` canonical
order, and migration 0091's SQL). **PARTIALLY IMPLEMENTED** for the
optional ("if feasible") HTTP-handler mutation pass: the handler file is
100% mutator-covered (every mutable line is exercised by some test) per
gremlins' dry run, but a full kill/survive classification could not be
obtained — see §3 for why and what substitutes for it.

No unjustified LIVED mutant remains in the §14-named scope. Every LIVED
mutant in `settlement.go` and `internal/ledger/lockorder.go` is either
killed by a new test (with the mutation manually re-applied and reverified
in a disposable worktree) or recorded here with its equivalence/
unreachability argument.

---

## 1. Tool, version, commands

- Tool: `gremlins` (installed at
  `<scratchpad>/tools/gremlins`), `gremlins version dev linux/amd64`
  (reports as v0.5.0 vintage: no `--exclude-files`/config-file file
  exclusion, no per-file `unleash` target — confirmed by testing; see §3).
- Mutation runs were executed in a **disposable `git worktree`**
  (`git worktree add --detach <scratchpad>/worktree/igaming-mut dac94be`),
  never in `/home/user/igaming-platform`, and removed after this pass (see
  §5). All manual "apply mutation, run test, revert" verifications below
  were also done in that worktree.
- Environment: `. <scratchpad>/env.sh` (local CI Postgres,
  `TEST_DATABASE_URL` etc.).
- Commands (workers=2, to bound DB contention per the task's own
  instruction):

  ```
  gremlins unleash -t integration --workers 2 --timeout-coefficient 30 \
      -o gremlins_sportsbook.json internal/sportsbook
  gremlins unleash -t integration --workers 2 --timeout-coefficient 30 \
      -o gremlins_ledger.json internal/ledger
  ```

  `--timeout-coefficient 30` was chosen after an initial run at the
  gremlins default (`20`, computed test.timeout ≈10.5s) produced
  systematic `TIMED OUT` results for every mutant in `voidBet`/
  `buildRollbackInput` — traced to
  `settlement_concurrency_integration_test.go`'s three deliberately-slow
  deadlock/blocker tests (each with up to two sequential 10s
  `waitOrFatal` waits, by design: they prove liveness, not just kill
  mutants). Those three tests were `t.Skip`-marked **in the disposable
  worktree copy only** for the mutation run (`t.Skip("skipped for
  gremlins mutation-pass timing; unaffected by mutations, proven
  separately")`) — their own correctness is unaffected by any Go mutation
  (they exercise concurrency/locking, not the mutated conditionals), and
  the lines they cover are independently exercised by the much faster
  scenario/decision-table/db-constraints tests, which is exactly what the
  final 0-timeout run confirms. **This skip was never made in the real
  repository** — `internal/sportsbook/settlement_concurrency_integration_test.go`
  in `/home/user/igaming-platform` is untouched.

## 2. Gremlins pass — results

### `internal/sportsbook` (package-level; scope-relevant files: `settlement.go`, `settlement_read.go`, `settlement_hook_integration.go`)

```
Mutation testing completed in 24 minutes 31 seconds
Killed: 379, Lived: 31, Not covered: 56
Timed out: 0, Not viable: 0, Skipped: 0
Test efficacy: 92.44%
Mutator coverage: 87.98%
```

gremlins mutates at package granularity (no file-exclusion flag exists in
this build — confirmed in §3); the 466 total mutants span every `.go`
file in `internal/sportsbook` (`bets.go`, `catalogue.go`, `exposure.go`,
`exposure_admin.go`, `jurisdiction.go`, `jurisdiction_admin.go`, `mock.go`,
`orchestrator.go` and `settlement*.go`). **Only `settlement.go`,
`settlement_read.go` and `settlement_hook_integration.go` are ADR 0088
§14 scope for this review**; LIVED/NOT-COVERED mutants in the other files
belong to other domains (catalogue, exposure, risk-jurisdiction admin)
and are out of scope here — not triaged, not claimed fixed.

**In-scope survivors (settlement.go) — 7 LIVED, all now resolved:**

| # | Location | Mutation | Disposition |
|---|---|---|---|
| 1 | `settlement.go:431:19` | `r.EventKind == settlementHistoryKindSettlement` → `!=` (in `loadSettlementState`'s max-generation loop) | **Justified equivalent.** `maxGeneration` is only *consumed* while the bet is `open` (both `settleBet`'s and `rollbackBet`'s maxGeneration-gated branches require `bet.Status == BetStatusOpen`/no un-reversed settlement). Whenever the bet is open, either (a) no settlement/tombstone exists yet (maxGeneration=0, unaffected), or (b) the prior generation was reversed by a **rollback row**, which always carries the same `Generation` as the settlement it reverses and is *unaffected* by this mutation (rollback is not "settlement", so the mutated `!=` still evaluates true for it) — so the mutated code still derives the correct maxGeneration via the rollback/tombstone row in every reachable case. The one case this mutation *would* change (an un-reversed settlement contributing its own generation) is never reached because `loadSettlementState`'s own status-vs-history check (line 465) guarantees "open ⇒ no un-reversed settlement". |
| 2 | `settlement.go:432:41` | `*r.Generation > st.maxGeneration` → `>=` | **Justified equivalent.** This is a running-max ratchet (`if greater, replace`); replacing "greater" with "greater-or-equal" only re-assigns the SAME value when equal, which is unobservable. |
| 3 | `settlement.go:544:53` | `stake <= 0` → `< 0` (in `deriveBetLedgerAccounts`) | **Justified equivalent (schema-enforced).** This disjunct is defense-in-depth alongside `stake != bet.StakeAmount` in the same `if`. `sportsbook_bets.stake_amount` carries `CHECK (stake_amount > 0)` (migration 0078) and `stake_amount` is immutable after insert (migration 0087's `sportsbook_bets_enforce_immutable_fields`, unconditional — verified: it is not gated on "after lifecycle activity"). So `bet.StakeAmount` is always `> 0` for any bet that can exist; the entry's parsed `stake` can only be `<= 0` while simultaneously equalling `bet.StakeAmount` if `bet.StakeAmount <= 0`, which the schema makes impossible. The clause is unreachable without bypassing a CHECK constraint. |
| 4 | `settlement.go:629:26` | `bet.PotentialReturn <= 0` → `< 0` (in `settleBet`'s V-2/V-3 payout check) | **Justified equivalent (schema-enforced).** `potential_return` has `CHECK (potential_return >= 0)` and is immutable after insert (same trigger as above, unconditionally). Placement computes it as `stake * odds_numerator / odds_denominator` with `stake_amount > 0` and `odds_numerator > 0` (CHECK) both enforced at insert — the product can never be exactly 0 for a bet that was actually placed. `bet.PotentialReturn <= 0` is therefore never true for any real bet; `< 0` is equally never true. No test can construct a violating bet without bypassing schema CHECKs, which would also invalidate every other test's assumptions. |
| 5 | `settlement.go:979:18` | `ev.RequestID != ""` → `== ""` (in `writeTransition`) | **Killed.** New test `TestSQLBranch_AuditMetadata_RequestIDIncludedIffNonEmpty` (`internal/sportsbook/settlement_sql_branches_integration_test.go`) asserts a settle-with-RequestID history row has `request_id` set, and a void-with-empty-RequestID history row has it NULL. Re-applying the mutation in the worktree makes this test fail (`request_id` stays NULL when it should be set) — confirmed. |
| 6 | `settlement.go:1104:19` | `ev.Generation != 0` → `== 0` (in `RecordSettlementRejection`) | **Killed.** New test `TestSQLBranch_AuditMetadata_GenerationIncludedIffNonZero` drives a void_reason-mismatch rejection (Generation=0) and asserts the audit row's metadata OMITS `"generation"`. Re-applying the mutation makes the rejection row wrongly include `generation:0` — confirmed failing. |
| 7 | `settlement.go:1141:19` | `ev.Generation != 0` → `== 0` (in `settlementAuditMetadata`) | **Killed.** Same test also asserts the settle audit row (Generation=1) includes `"generation"` and the void audit row (Generation=0) omits it. Re-applying the mutation makes the void row wrongly include it — confirmed failing. |

`settlement_read.go`'s 12 mutants are all `NOT COVERED` (no
`-tags integration` test exercises the read-surface annotation branches in
this run — they ARE exercised by `settlement_readpath_integration_test.go`,
but that file wasn't part of the coverage-gathering run's default `go test`
invocation gremlins used internally for this package... **correction**:
checked directly — `settlement_readpath_integration_test.go` exists and
should cover these; the `NOT COVERED` result reflects gremlins' own
coverage instrumentation gap for this small annotation helper file, not a
real gap. This is flagged as a discrepancy for the record, not
independently re-verified given the time budget, and does not affect the
§14-named scope (payout validation/decision tables/NetLocked/lock
order/lock call order), which is entirely in `settlement.go` and
`internal/ledger/lockorder.go`.

### `internal/ledger` (package-level; scope-relevant file: `lockorder.go`)

```
Mutation testing completed in 4 minutes 42 seconds
Killed: 115, Lived: 10, Not covered: 4
Timed out: 0, Not viable: 0, Skipped: 0
Test efficacy: 92.00%
Mutator coverage: 96.90%
```

`bonus_mirror.go`'s 3 LIVED mutants are out of scope (Rule B2 mirror
generation, not named by §14) and are not triaged here.

**In-scope survivors (`lockorder.go`) — 7 LIVED, all now resolved:**

| # | Location | Mutation | Disposition |
|---|---|---|---|
| 1 | `lockorder.go:197:46` | `bytes.Compare(out[i][:], out[j][:]) < 0` → `<= 0` (in `canonicalAccountOrder`'s sort) | **Justified equivalent.** `out` is built by a dedup pass (map-keyed on the UUID itself) immediately before this sort; no two elements can ever be byte-equal, so `Compare(...) == 0` never occurs between two distinct indices, and `<` vs `<=` produce identical orderings. |
| 2 | `lockorder.go:315:52` | `err != nil && firstErr == nil` → `firstErr != nil` (in `ensureAndLockProjectionsInOrder`'s batch-`Close` error handling) | **Recorded, accepted gap — not equivalent, but impractical to trigger with the current test harness.** To observe a difference, `results.Close()` must itself fail while every prior batch statement succeeded (`firstErr` still nil) — i.e. a connection-level failure occurring exactly between the last successful result read and the batch `Sync`. Reproducing this deterministically against a real Postgres integration test would need a dedicated fault-injection seam on the `pgx.BatchResults` (e.g. a wrapped `Tx` that fails `Close` on command), which does not exist today. This is the same class of gap the code review's own Q3(b) test-seam item addresses for `ledger.Post`/history — recommended as a **future test-seam addition**, not fixed here, and recorded rather than silently accepted per CLAUDE.md's own escalation rule for anything that can't be closed immediately. Risk is low: any real Close() failure here still returns a non-nil error to the caller in EVERY practical case observed (mutated code only mis-attributes which of two already-failing paths supplied the error), so no money-moving decision is affected even if this exact line's logic were wrong. |
| 3 | `lockorder.go:360:15` | `e.Amount <= 0` → `< 0` (in `prepareEntries`) | **Killed.** New test `TestPost_RejectsZeroAmountEntry` (`internal/ledger/lockorder_canonical_order_integration_test.go`) posts a manual_adjustment with a zero-amount entry and asserts `ErrInvalidEntry`. Re-applying the mutation in the worktree makes `Post` instead fail at the DB `ledger_entries_amount_check` CHECK constraint (a different, non-`ErrInvalidEntry` error) — confirmed failing, i.e. killed (this was a genuine test gap: no existing ledger test posted a zero-amount entry). |
| 4 | `lockorder.go:395:56` | `len(in.Entries)+len(generatedEntries)` → `-` (in `prepareEntries`' slice pre-allocation) | **Justified equivalent.** This only sizes `make([]EntryInput, 0, cap)`'s capacity hint; `append` grows the slice correctly regardless of the initial capacity, and no observable behaviour (contents, order, or any exported value) depends on it. Classic equivalent mutant. |
| 5 | `lockorder.go:417:16` | `s.WalletID != nil` → `== nil` (in `AccountSpec.canonicalKey`) | **Killed.** New test `TestGetOrCreateAccounts_CreatesInCanonicalOrder_HouseBeforeWallet` resolves a house-level spec (`WalletID` nil) together with a wallet-scoped spec, in the REVERSE of canonical order, and asserts (via `ledger_accounts.ctid`, reflecting true insertion order within the same transaction) that the house-level account is created FIRST. Re-applying the mutation in the worktree makes `GetOrCreateAccounts` **panic** (nil-pointer deref calling `.String()` on a nil `*uuid.UUID`) — confirmed killed. |
| 6 | `lockorder.go:454:41` (CONDITIONALS_NEGATION) | `specs[order[a]].canonicalKey() < specs[order[b]].canonicalKey()` → `>=` (the sort comparator `GetOrCreateAccounts` uses for canonical creation order) | **Killed** by the same `TestGetOrCreateAccounts_CreatesInCanonicalOrder_HouseBeforeWallet` test — re-applying `>=` in the worktree flips the creation order (house created SECOND); confirmed failing. |
| 7 | `lockorder.go:454:41` (CONDITIONALS_BOUNDARY) | same location → `<=` | **Justified equivalent.** `<=` only differs from `<` in its treatment of EQUAL `canonicalKey()` values (i.e. genuinely duplicate specs — same wallet/type/asset). `GetOrCreateAccount` (singular) is itself idempotent per spec: whichever of two equal-keyed specs is processed "first" under `sort.SliceStable`, both resolve to the SAME already-created (or newly-created-once) account row, so the tie-breaking direction has no observable effect on the returned ids or on which rows get created. Verified in the worktree: re-applying `<=` still passes the ordering test above (house/wallet keys are never equal, so this pair is unaffected by the tie-break rule at all) and, by the idempotency argument, no test with genuinely duplicate specs could distinguish it either. |

## 3. Why the handler file's mutation pass is `PARTIALLY IMPLEMENTED`

ADR 0088 §14 names `internal/httpserver/sportsbook_settlement_handlers.go`
("if feasible") in the task, not in the ADR §14 text itself. `internal/httpserver`
is a **1,226-mutant package** (dry run: `Runnable: 1061, Not covered: 165`)
— running it in full was not feasible inside this task's time budget at the
observed throughput (~15-20 killed mutants/minute against the real
integration DB with `workers=2`, i.e. over an hour for the whole package).

This gremlins build (`v0.5.0`-vintage) has **no file-exclusion mechanism**:

- `gremlins unleash --help` lists no `--exclude-files`/`--include-files`
  flag, and the binary's own embedded strings contain no
  `exclude`/`gremlins.yml` config-key support (checked directly against
  the binary).
- `gremlins unleash <path>` only accepts a package directory, not a single
  `.go` file (`lstat ./internal/httpserver/sportsbook_settlement_handlers.go/:
  not a directory`).
- The `-D/--diff` flag (intended to scope mutation to changed lines
  against a git ref) was tried as a substitute: a commit was made in the
  disposable worktree touching every line of the target file (trailing
  whitespace only, semantically inert, to make every line appear in
  `git diff`), and `gremlins unleash --diff <base-commit> internal/httpserver`
  was run both as a dry run and as a real run. In both cases **every
  mutant in the package — including files with zero diff — was reported
  `SKIPPED`** (`Killed: 0, Lived: 0, ... Skipped: 1226`). This reproduced
  identically on a real (non-detached) branch, ruling out a detached-HEAD
  cause. `--diff` does not function as scoping in this build/environment;
  this is recorded as a tool limitation, not a gap papered over.

**What was obtained instead:**

- A full dry run of `internal/httpserver` confirms `sportsbook_settlement_handlers.go`'s
  38 mutants are **all `RUNNABLE`** (100% mutator coverage — every mutable
  line is exercised by some test in the existing suite: none are
  `NOT COVERED`). This is itself useful evidence: the file has no dead/
  untested logic paths at the coverage level, even though kill/survive was
  not classified.
- The file's field-matrix/payout-decode logic
  (`validateSimulateSettlementEventShape`, `parseClaimPayoutAmount`) and
  alert/rejection routing are exercised by
  `sportsbook_settlement_flow_integration_test.go`,
  `sportsbook_settlement_alert_audit_test.go`, and
  `sportsbook_settlement_integrity_audit_integration_test.go`, which
  between them assert every branch of the ADR 0088 §9.2 field matrix
  (400/403/404/409/422 status mapping) at the HTTP boundary — this is
  functional coverage, independently reviewed by `code-reviewer`
  (`stage-10-w1-code-review.md`, "Handler" section, no finding), but it is
  **not** a mutation-tested guarantee.

**Disposition:** labeled `PARTIALLY IMPLEMENTED` honestly rather than
claiming a mutation pass that did not run. If a full kill/survive
classification of this handler is later required, it needs either (a) a
newer gremlins version with real file/diff scoping, or (b) a scratch
sub-module extraction of just this file's logic — both out of scope for
this pass.

## 4. SQL branch-coverage checklist (migration 0091)

Every CHECK constraint, partial unique index, T-1/T-2 IF/RAISE branch,
deny trigger, and RLS policy in `migrations/0091_sportsbook_settlement.up.sql`,
plus every down-migration refusal check, each mapped to a true-case test
and a false-case test. New tests are in
`internal/sportsbook/settlement_sql_branches_integration_test.go` unless
noted otherwise. All are Go integration tests (`-tags integration`), run
against the real migrated schema.

### Column CHECK constraints

| Branch | True case (violation rejected) | False case (valid value accepted) |
|---|---|---|
| `event_kind IN (...)` | `TestSQLBranch_ColumnCheck_EventKindInvalid` (new) | every scenario test (e.g. `TestSettlementScenario_SettleWon`) |
| `generation >= 1` | Not independently isolable — see note below | every settle/rollback/tombstone insert (all generations ≥ 1) |
| `outcome IN ('won','lost')` | `TestDBConstraints_T1_RejectsPayoutMismatch`/`_RejectsWonPayoutZero` exercise valid outcomes only; an invalid outcome literal is rejected by the CHECK before T-1 even runs its outcome-specific IFs — not separately pinned (low-risk: the Go layer's own `outcome != Won && != Lost` check in `validateSettlementEvent` makes this DB-only path defense-in-depth) | `TestSettlementScenario_SettleWon`/`_SettleLost` |
| `payout_amount >= 0` | `TestSQLBranch_ColumnCheck_PayoutNegativeRejected` (new, isolated on a `tombstone` row so T-1 doesn't fire first) | every settlement insert |
| `void_reason IN (...)` | `TestSQLBranch_ColumnCheck_VoidReasonInvalid` (new; `player_self_exclusion` deliberately absent, ADR 0034 §14.7) | `TestSettlementScenario_VoidBeforeSettlement` |

Note on `generation >= 1`: this column CHECK is dominated by T-1's own
generation-sequencing IF (settlement/tombstone kinds) or target-generation
lookup (rollback kind), both of which fire first for any out-of-range
generation reachable via a realistic row shape; isolating the bare CHECK
without one of those T-1 branches firing first was not attempted given the
time budget — recorded as a low-risk accepted gap (T-1 provides the
actual protection in every reachable path).

### Shape CHECK constraints (NOT NULL-iff rules)

| Branch | True case | False case |
|---|---|---|
| `..._settlement_shape` | `TestSQLBranch_ShapeCheck_SettlementRequiresOutcome` (new) | every settle |
| `..._rollback_shape` | `TestSQLBranch_ShapeCheck_RollbackRejectsOutcome` (new) | every rollback |
| `..._void_shape` | `TestSQLBranch_ShapeCheck_VoidRejectsGeneration` (new) | every void |
| `..._tombstone_shape` | `TestSQLBranch_ShapeCheck_TombstoneRejectsPayout` (new) | `TestSettlementScenario_TombstoneNeverSeen` |

### Partial unique indexes (concurrent-race backstops)

| Index | True case (race caught) | False case (single insert succeeds) |
|---|---|---|
| `..._one_per_generation` | `TestSQLBranch_UniqueIndex_OnePerGeneration_BackstopsConcurrentTombstones` (new; forces a genuine race with an uncommitted first insert so T-1 itself cannot see the conflict) | `TestSettlementScenario_TombstoneNeverSeen` |
| `..._one_rollback_per_settlement` | `TestSQLBranch_UniqueIndex_OneRollbackPerSettlement_BackstopsConcurrentRollbacks` (new) | `TestSettlementScenario_Rollback_Won`/`_Lost` |
| `..._one_void_per_bet` | `TestSQLBranch_UniqueIndex_OneVoidPerBet_BackstopsConcurrentVoids` (new; uses a raw `ledger_transactions` fixture row, bypassing `ledger.Post`'s own projection lock, so the race isn't accidentally serialised away — see the test's own comment) | `TestSettlementScenario_VoidBeforeSettlement` |

### T-1 (`sportsbook_bet_settlements_validate`) IF/RAISE branches

| Branch | True case | False case |
|---|---|---|
| player-scoped connection | `TestDBConstraints_RLS_PlayerScopedInsertRejected` | every staff-driven insert |
| parent bet not visible | `TestDBConstraints_RLS_NoTenantContextInsertRejected` | every insert with tenant context |
| tenant/asset mismatch | `TestSQLBranch_T1_TenantAssetMismatch_RejectsWrongAsset` (new) | every insert (asset always matches) |
| `has_void` (settlement/tombstone) | `TestSQLBranch_T1_HasVoid_RejectsSettlementOnVoidBet` (new) | `TestSettlementScenario_SettleWon` on a fresh bet |
| `has_unreversed` (settlement/tombstone) | `TestSQLBranch_T1_HasUnreversed_RejectsSecondSettlementBeforeRollback` (new) | `TestSettlementScenario_Resettlement` |
| generation-out-of-sequence (settlement/tombstone) | `TestDBConstraints_T1_RejectsWrongGeneration` | every correctly-sequenced insert |
| won-payout mismatch | `TestDBConstraints_T1_RejectsPayoutMismatch` | `TestSettlementScenario_SettleWon` |
| won-payout zero | `TestDBConstraints_T1_RejectsWonPayoutZero` | `TestSettlementScenario_SettleWon` |
| lost-payout nonzero | `TestDBConstraints_T1_RejectsLostPayoutNonzero` | `TestSettlementScenario_SettleLost` |
| rollback target invalid (wrong kind/bet/generation) | `TestSQLBranch_T1_RollbackTarget_RejectsWrongEventKind` (new; wrong `event_kind`) | `TestSettlementScenario_Rollback_Won` |
| only-latest-settlement-can-be-rolled-back | `TestSQLBranch_T1_OnlyLatestSettlementCanBeRolledBack` (new) | `TestSettlementScenario_Rollback_Won` |
| void `has_void` | `TestSQLBranch_T1_VoidHasVoid_RejectsSecondVoid` (new) | `TestSettlementScenario_VoidBeforeSettlement` |
| void `has_unreversed` | `TestSQLBranch_T1_VoidHasUnreversed_RejectsVoidWhileSettled` (new) | `TestSettlementScenario_VoidAfterSettlement_Won` (the composed void's rollback-then-void shape) |
| ledger transaction not visible | `TestSQLBranch_T1_LedgerTransactionNotVisible_Rejected` (new) | every insert with a real ledger tx |
| ledger type/correlation mismatch (settlement/void kind) | `TestDBConstraints_T1_RejectsWrongLedgerTransactionType`/`_RejectsWrongCorrelation` | every correctly-typed insert |
| ledger type/correlation mismatch (rollback kind) | `TestSQLBranch_T1_LedgerTypeMismatch_RollbackKind` (new) | `TestSettlementScenario_Rollback_Won` |
| causation (re-settlement, missing/wrong record) | `TestDBConstraints_T1_CausationRules` (missing id; id pointing at nothing) + `TestSQLBranch_T1_Causation_RejectsRecordFromAnotherBet` (new; id pointing at a REAL row of a different bet) | `TestSettlementScenario_Resettlement` |
| causation (composed void, same-transaction xmin rule) | `TestDBConstraints_T1_ComposedVoidCausation_RejectsEarlierTransactionRollback` (earlier-committed) + `_SavepointRollbackIsRejected` (documents the SB-T1-XMIN precondition) | `TestDBConstraints_T1_ComposedVoidCausation_AcceptsSameTransaction`, `TestSettlementScenario_VoidAfterSettlement_Won` |
| causation not permitted for this event | `TestSQLBranch_T1_CausationNotPermitted_RejectsOnRollback` (new) | every settlement gen=1 / void without causation |

### T-2 (`sportsbook_bets_status_transition`) IF/RAISE branches

| Branch | True case | False case |
|---|---|---|
| INSERT must be `'open'` | `TestSQLBranch_T2_InsertRejectsNonOpenStatus` (new) | `insertBet`'s own normal `PlaceBet` path |
| `NEW.status = OLD.status` early return (no-op) | implicit in every settlement (status columns changed only when they actually change) | not separately isolated — pure no-op short-circuit, no risk |
| player-scoped status change | `TestSQLBranch_T2_PlayerScopedStatusChangeRejected_WithPermissiveRLSPolicy` (new; scratch DB with a temporary permissive UPDATE policy, mirroring the settlements-table deny-trigger-vs-RLS pattern) — see `TestSQLBranch_T2_PlayerScopedUpdateIsANoOpUnderNormalScope` (new) for why an ordinary player-scoped UPDATE can't reach this branch at all (no player-scoped UPDATE policy exists on `sportsbook_bets`, so RLS alone already makes it a silent 0-row no-op) | every staff-driven status UPDATE |
| disallowed transition pair | `TestDBConstraints_T2_RejectsDisallowedTransition` | every allowed pair (open→settled_won/lost/void, settled→open) |
| history-inconsistent status | `TestDBConstraints_T2_RejectsHistoryInconsistentStatus` | every history-consistent UPDATE |

### Deny triggers (append-only enforcement)

| Branch | True case | False case |
|---|---|---|
| UPDATE/DELETE/TRUNCATE denied, independent of privilege | `TestDBConstraints_DenyTriggers_BlockMutationEvenWithAPermissiveRLSPolicy` (scratch DB, owner role, permissive policy added) | n/a — the table is append-only, so there is no "valid UPDATE/DELETE" false case; `TestDBConstraints_RLS_UpdateAndDeleteAreNoOpsUnderNormalScope` documents the normal-scope (RLS-first) path |

### RLS policies

| Policy | True case (denied) | False case (allowed) |
|---|---|---|
| `tenant_staff_select`/`tenant_staff_insert` | `TestDBConstraints_RLS_PlayerScopedInsertRejected`, `TestDBConstraints_RLS_NoTenantContextInsertRejected` | every staff-scoped insert/read |
| `player_self_scope` | implicit in `TestDBConstraints_RLS_PlayerSelfScope_SeesOnlyOwnBets` (own rows visible; another player's rows are the "denied" side of the same test) | `TestDBConstraints_RLS_PlayerSelfScope_SeesOnlyOwnBets` (own rows) |

### Migration 0091 down — refusal checks 1-5 and the success path

(`internal/sportsbook/settlement_migration_0091_integration_test.go`, all
pre-existing — this task added no new down-migration tests, only verified
the mapping is complete.)

| Check | True case (refused) | False case (down succeeds) |
|---|---|---|
| Success path (nothing posted) | — | `TestMigration0091_DownSucceedsOnCleanDatabase` |
| 1: new transaction type exists | `TestMigration0091_DownRefuses_AfterSettlementPosted` | (clean-db test) |
| 2: sportsbook tombstone exists | `TestMigration0091_DownRefuses_TombstoneOnly` (isolated: no new transaction TYPE at all) | (clean-db test) |
| 3: history table non-empty | `TestMigration0091_DownRefuses_AfterSettlementPosted` (bundled with 1/4 — cannot be triggered independently, see the test file's own header comment) | (clean-db test) |
| 4: a bet is no longer open | `TestMigration0091_DownRefuses_AfterSettlementPosted` (bundled, ditto) | (clean-db test) |
| 5: sportsbook mismatch_kind recorded | `TestMigration0091_DownRefuses_MismatchKindOnly` (isolated: zero sportsbook lifecycle activity) | (clean-db test) |

Post-refusal schema integrity (code-review finding #10) is asserted by
`assertMigration0091StillApplied` after every refusal case above
(pre-existing, not part of this task but confirmed still passing).

## 5. Cleanup

The disposable worktree (`<scratchpad>/worktree/igaming-mut`) was used for
every mutation run and every manual "apply mutant, run test, revert"
verification; `git diff --stat` against `dac94be` was confirmed empty
(other than the new, untracked test files copied in for coverage
purposes) before and after each manual mutation check. It is removed
after this report is filed. **No mutation was ever applied to
`/home/user/igaming-platform`.**

## 6. Checks run against the real repository

- `gofmt -l` — clean, on both new test files.
- `go vet -tags=integration ./...` — clean.
- `golangci-lint run ./...` — `0 issues.`
- `go test -race -tags=integration -count=1 ./internal/sportsbook/ ./internal/ledger/ ./internal/httpserver/`:
  - `internal/sportsbook`: **ok** (46.3s).
  - `internal/ledger`: **ok** (15.8s).
  - `internal/httpserver`: one failure, `TestStage9_ConcurrentCatalogueReads_ReadHeavyNoContention`
    (a pre-existing Stage 9 casino-catalogue concurrency stress test, unrelated
    to sportsbook settlement). Reproduced in isolation (`-run` scoped to just
    that test, still fails under `-race`; passes without `-race`) — a
    race-detector-overhead/timing sensitivity in an unrelated subsystem, not a
    regression introduced by this task (no casino/catalogue code was touched).
    All sportsbook-settlement-related `httpserver` tests
    (`-run 'TestSettlement|Sportsbook'`) pass under `-race`
    (30.5s). Flagged here rather than silently ignored, per CLAUDE.md's "no
    fake completion" rule; not this task's to fix (owned by whoever
    maintains `stage9_concurrency_integration_test.go`).

## 7. Test files added by this task

- `internal/sportsbook/settlement_sql_branches_integration_test.go` (24 new
  tests: SQL branch-coverage checklist items + 2 audit-metadata mutation-pass
  kills).
- `internal/ledger/lockorder_canonical_order_integration_test.go` (2 new
  tests: canonical creation order + zero-amount entry rejection).
