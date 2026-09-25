> Stage 10.1 planning gate — specialist working paper (verbatim, recorded 2026-09-25 against 56f5135). Where it differs from the Orchestrator rulings in `docs/plans/stage-10.1-planning-gate-proposal.md` §O, the rulings govern.

# Stage 10.1 binding test plan — PAY-REV-1 & SB-T1-XMIN (QA)

Gate: none of the below merges without green build-test-lint on the
implementation head plus this file's evidence-assertion step (§5). All
concurrency tests follow Stage 10 W0 rules: deterministic interleaving via
`loStartRacer`/`loRunABBA`/blocker-PID-scoped `loWaitBlocked`, no sleeps,
must be shown failing pre-fix.

## PAY-REV-1 — package `internal/payments`

New file `payrev1_concurrency_integration_test.go` (build tag `integration`),
extends `lockorder_harness_test.go` with `loHoldLedgerTransactionRow`
(blocker analogous to `loHoldProjectionRow`, `SELECT ... FROM
ledger_transactions WHERE id=$1 FOR UPDATE`).

1. `TestPayRev1_ConcurrentDistinctReferenceReversals_ExactlyOnePosts`
   (integration). A (ref R1) races B (ref R2) for the same original via
   `loRunABBA`, blocked on the new L2 lock. Asserts: exactly one
   `deposit_reversal` row for the original; loser returns
   `ErrDepositAlreadyReversed`; `player_cash` reflects one reversal only
   (assert exact projection value, reject any negative); `loAssertBalanced`
   and `loAssertProjectionMatchesRebuild` hold. Must be run and shown
   failing against pre-fix `orchestrator.go` (stash fix, confirm double
   posting / negative `player_cash`, restore fix, confirm pass) — record
   both runs in the PR. Proves: race is closed (core acceptance criterion).
2. `TestPayRev1_ConcurrentIdenticalReferenceReversals_OnePostsOneReplays`
   (integration). Both racers use ref R1. Asserts both succeed with the
   identical `LedgerTransactionID`, exactly one row, `AlreadyPosted=true`
   on the loser. Proves: L2 lock does not break F-7 same-reference replay.
3. `TestPayRev1_SequentialDistinctReferenceReversal_Rejected` (integration,
   no concurrency). Reversal A completes; B (different ref, same original)
   follows. Asserts `ErrDepositAlreadyReversed`, zero new ledger rows, and
   (if audit denial added) an `audit.OutcomeDenied` record for
   `deposit.reversal_rejected_already_reversed` with tenant/actor/entity
   populated. Proves: sequential rejection contract + audit trail.
4. `TestPayRev1_SequentialSameReferenceRedelivery_StillIdempotent`
   (integration). Regression guard for F-7; same reversal ref redelivered
   after success. Asserts `AlreadyPosted=true`, same tx id, no new row.
5. `TestPayRev1_LockOrder_NoDeadlockAgainstConcurrentNewDeposit` — re-run
   existing `TestLockOrder_ConcurrentDepositAndDepositReversal_NoDeadlock`
   (`lockorder_integration_test.go:106`) unmodified post-fix; must still
   pass with `loAssertNoDeadlock`. Proves: no new deadlock (ADR 0082 §3.4).
6. `TestPayRev1_UniqueIndex_BackstopsBypassOfLock` (integration, direct
   SQL). Bypasses the Go lock: two direct `ledger.Post` calls (or raw
   inserts) constructing a second `deposit_reversal` row with the same
   `reverses_transaction_id`, distinct idempotency keys, inside the same
   tenant. Asserts the second insert fails with a unique-violation
   (SQLSTATE 23505) on the new partial index. Proves: DB backstop is real,
   independent of the Go-level lock (§2.3 defense-in-depth requirement).
7. `TestMigration0092_RefusesWithExistingDuplicates` (integration, scratch
   DB, migrate to 0091, seed duplicate `deposit_reversal` rows for one
   original **across two tenants** with `FORCE RLS` active — verifies the
   pre-flight check is not silently RLS-scoped per the analysis's §5.2
   risk — then run `0092` up and assert it aborts with the named refusal
   message, no index created, no rows deleted/merged).
8. `TestMigration0092_SucceedsOnCleanDatabase` (integration, scratch DB, no
   duplicates) — asserts `0092` applies cleanly, both partial unique
   indexes exist (`\d`-equivalent catalog query), and RLS/FORCE RLS on
   `ledger_transactions` is unchanged.
9. `TestMigration0092_DownRestoresPriorState` (integration) — `0092` up
   then down; asserts both new indexes are dropped, no data changed, `0091`
   schema fully intact (existing tip-pin/schema-shape assertions for prior
   migrations still pass — see §4 tip-pin note).
10. `TestPaymentWebhookHandler_DepositReversalAlreadyReversed_Maps409`
    (unit or integration, `internal/httpserver`) — `errors.Is(err,
    payments.ErrDepositAlreadyReversed)` maps to `apierror.CodeConflict`
    (409), not 500. Regression-proves the §3.3/§6.2 handler fix.
11. `TestPayRev1_TenantIsolation_CannotLockOrObserveAnotherTenantsOriginal`
    (integration) — tenant B's reversal callback naming tenant A's
    provider reference must resolve zero rows (existing
    `loadDepositIntentByProviderRef` RLS behavior) and never reach/lock
    tenant A's `ledger_transactions` row. Proves: no cross-tenant leakage
    introduced by the new L2 lock query.
12. Regression: full existing F-7 replay suite (payments package,
    `idempotency`/`replay`-named tests) re-run unmodified and must stay
    green — no new failures from the check reordering (amount/asset check
    now runs post-lock).

Acceptance mapping: race closed→#1; idempotency preserved→#2,#4,#12;
sequential rejection+audit→#3; DB backstop→#6; migration refusal/success/
down→#7,#8,#9; HTTP mapping→#10; tenant isolation→#11; no deadlock→#5.

## SB-T1-XMIN — package `internal/sportsbook`

In `settlement_db_constraints_integration_test.go` (rename/replace) and new
service-level file `settlement_composed_void_savepoint_integration_test.go`.

13. Rename `TestDBConstraints_T1_ComposedVoidCausation_SavepointRollbackIsRejected`
    → `TestDBConstraints_T1_ComposedVoidCausation_SavepointRollbackIsAccepted`.
    Flip assertion to `err == nil`; update doc comment removing the
    "deliberately pinned as REJECTED" framing. Proves: savepoint-inserted
    rollback is now accepted (core SB-T1-XMIN fix).
14. `TestDBConstraints_T1_ComposedVoidCausation_NestedSavepointIsAccepted`
    (new) — `SAVEPOINT sp_outer; SAVEPOINT sp_inner; INSERT rollback;
    RELEASE sp_inner; RELEASE sp_outer;` then void insert on outer tx.
    Asserts acceptance. Proves: "however nested" claim.
15. `TestDBConstraints_T1_ComposedVoidCausation_RollbackToSavepointFailsFK`
    (new) — savepoint, insert rollback row, `ROLLBACK TO SAVEPOINT`
    (not RELEASE), then void insert referencing the now-gone row id.
    Asserts failure — confirm empirically at implementation time whether
    it is the FK violation or T-1's `cause.id IS NULL` branch, and assert
    that exact shape (not the xmin-specific message).
16. Keep unmodified, must still pass:
    `TestDBConstraints_T1_ComposedVoidCausation_RejectsEarlierTransactionRollback`,
    `TestDBConstraints_T1_ComposedVoidCausation_AcceptsSameTransaction`.
17. `TestSettlementScenario_ComposedVoid_InsideOuterSavepoint_ServiceLevel`
    (new, integration, service-level not raw-SQL) — wraps a full
    `SimulateSettlementEvent` composed-void call inside a test-driven
    `tx.Begin`-issued savepoint (per analysis §5's helper shape), commits
    savepoint then outer tx. Must be shown failing pre-fix (reproduces the
    same-transaction rejection end-to-end) and passing post-fix. Proves:
    real driver shape unblocked, not just raw-SQL probe.
18. `TestMigration0093_DownRestoresExactPriorFunctionBody` (integration) —
    down migration's `CREATE OR REPLACE FUNCTION` body diffed byte-for-byte
    against 0091's original `sportsbook_bet_settlements_validate` text
    (up.sql:145-262). Fails on any deviation. Proves: down restores old
    body exactly (explicit task requirement).
19. `TestMigration0093_UpChangesOnlyFunctionBody` (static/integration) —
    asserts no table/column/index/RLS-policy diff versus pre-0093 catalog
    state (schema snapshot compare), only `pg_proc` definition changes.

Acceptance mapping: savepoint accepted→#13; nested savepoints→#14; rollback-
to-savepoint case→#15; earlier-committed still rejected→#16; service-level
composed void in outer savepoint→#17; down restores old body→#18.

## Governance doc updates (binding, part of this stage's DoD)

- `docs/governance/stage-10-w1-mutation-and-sql-branch-coverage.md:267`:
  split the "composed void, same-transaction xmin rule" row into: (a)
  earlier-transaction reject → `_RejectsEarlierTransactionRollback`
  (unchanged), (b) same-transaction accept (plain + savepoint + nested) →
  `_AcceptsSameTransaction`, `_SavepointRollbackIsAccepted` (renamed),
  `_NestedSavepointIsAccepted` (new), (c) rolled-back-savepoint FK case →
  `_RollbackToSavepointFailsFK` (new). Add mutation-relevant note: mutating
  `pg_xact_status(...) = 'in progress'` to `<> 'in progress'` or to a
  hardcoded `true` must be caught by #13/#16 together (one requires
  accept, the other requires reject) — record this pair as the
  mutation-kill pair for this branch.
- Tip-pin migration tests: `internal/bonus/wave3_phase2_migrations_integration_test.go`,
  `internal/jurisdiction/migration_0075_integration_test.go`,
  `internal/jurisdiction/migration_0077_integration_test.go` (per
  orchestrator correction, 0075/0077 combined per note),
  `internal/operatingmarket/migration_0076_integration_test.go`,
  `internal/operatingmarket/qa_migration_rls_survives_failed_rollback_test.go`
  — each must be re-run and, where they assert a `MigrateDown` count or an
  explicit version list, updated to include `0092`/`0093`. This is a
  required task, not optional cleanup: a stale count silently passing
  while masking a broken new-migration count is exactly the "fake
  completion" class CLAUDE.md forbids. QA verifies post-implementation
  that each of these files' counts/lists were actually touched (grep diff
  in the PR), not merely that they still pass.
- Task registry: SB-T1-XMIN row moves Deferred→Done only after #13-#19 are
  green; PAY-REV-1 stage item closed only after #1-#12 are green plus the
  audit item (#3) either implemented and tested or explicitly deferred as
  a recorded decision (not silently dropped).

## CI evidence requirement

- Full `build`, `test` (unit+integration tags), and `lint` green on the
  exact commit proposed as the implementation head — link the CI run.
- Evidence-assertion step: PR description must include (a) the pre-fix
  failing run output/log excerpt for #1 and #17 (the two "must fail before
  fix" tests), and (b) post-fix green run for the full `internal/payments`
  and `internal/sportsbook` integration suites, not just the new tests in
  isolation (guards against a new test silently loosening an old one).
- Mutation/branch coverage, proportionate: no full mutation-testing tool
  required for this scope, but every new IF/RAISE branch touched (T-1's
  causation branch) must have both a True and False case per the existing
  `stage-10-w1-mutation-and-sql-branch-coverage.md` table convention
  (already satisfied by #13/#14/#15/#16), and the new unique indexes (#6,
  payments) must have an explicit backstop test per the existing
  `..._one_per_generation`-style pattern (satisfied by #6). QA signs off
  only once the governance doc's table rows are updated to name these
  tests, matching every other row's format.

## Verdict gate

Neither PAY-REV-1 nor SB-T1-XMIN may be marked `IMPLEMENTED` until: all
tests #1-#19 exist, pass, and #1/#17 have documented pre-fix-failing runs;
governance doc and tip-pin tests are updated (not just passing); CI is
green on the implementation head. Any gap is reported as
`PARTIALLY IMPLEMENTED` with the specific missing item named — QA does not
sign off on "server starts / one manual click worked."
