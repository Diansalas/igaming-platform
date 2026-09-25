# Stage 10.1 Planning Gate: PAY-REV-1 and SB-T1-XMIN Remediation (plus a Future AI-Agent Architecture Record)

- **Date:** 2026-09-25
- **Status:** **PLANNING ONLY — awaiting human approval.** This gate changes no application code and adds no implementation migration. Nothing was deployed.
- **Authority:** human directive "STAGE 10.1 PLANNING GATE + FUTURE AI ARCHITECTURE" (2026-09-25). Stage 10 was accepted at `20c72e4` (last code commit `312db16`; CI #259 green).
- **Stage definition:** ADR 0090 (**PROPOSED**; becomes ACCEPTED on human approval).
- **Specialist record:** `docs/plans/stage-10.1-planning/` holds ten verbatim working papers: two analyses, five cross-reviews and three ADR 0089 reviews. Where a paper differs from the rulings in §O below, the rulings govern.
- **Owner:** Master Orchestrator.

---

## A. Current state

| Item | State |
|---|---|
| Branch | `claude/focused-wright-jw88w9` |
| Planning base | `20c72e4` (Stage 10 records), plus `56f5135` and `6e25d24` (ADR 0089) |
| Stage 10 | **COMPLETE** (`docs/governance/stage-10-completion-report.md`) |
| Migration tip | `0091_sportsbook_settlement` |
| Staging (AWS) | Running, deployed commit `9190d5d`, at migration 0090. **Untouched.** Staging does not yet carry 0091 or F-7; a later staging refresh needs its own authorization. |

Defects carried from Stage 10:
- **PAY-REV-1:** P1, pre-existing since Stage 3B.
- **SB-T1-XMIN:** deferred P2/technical. Fail-closed today; no current code path is affected.

Human decisions that remain OPEN and are not touched here:
- ADR 0009
- HDR-J-6, J-7, J-8, J-9
- HDR-M-1, M-2
- HDR-SB-1
- OB-1
- vendor, legal and retail decisions

## B. Stage 10 evidence

- **W0 gate:** CI runs #239–#243 green, five consecutive. Each run: 32 packages ok, 2,202 tests passed, 5 expected HDR-J-7 skips, 0 failed.
- **W1:** CI runs #247–#259 green.
- **Local check:** a fresh-database CI replay passed 3 out of 3 times.
- **Reviews:** security, code-review and ledger-finance sign-offs are in `docs/governance/stage-10-*`.
- **F-7:** fixed at `36616f1`. `ErrIdempotencyPayloadMismatch` is added, and the ADR 0020 amendment dated 2026-09-25 records the change.

This gate repeats none of that work.

## C. PAY-REV-1: root cause

The defect is in `internal/payments/orchestrator.go` `receiveDepositReversalCallback`, lines 931–1046. Four gaps combine:
1. The original deposit is loaded without a lock (`loadDepositIntentByProviderRef`, lines 383–396).
2. The "already reversed?" `EXISTS` check (lines 986–992) runs **before** any lock, so it is a check-then-insert. CLAUDE.md forbids that pattern.
3. The ledger idempotency key comes from the **reversal's own** provider reference (line 1013). Two different references therefore produce two different keys, and both pass `ledger.Post`.
4. `idx_ledger_transactions_reverses` (migration 0021, line 55) is **not unique**. Nothing in the database limits a deposit to one reversal.

The result: two signed reversals of one deposit that race each other both post, over-debiting `player_cash`. This was reproduced three times by `ledger-finance`, with one 1,000 deposit reaching −1,000. Reconciliation cannot see it because every posting balances.

**Reachability:**
- It is only reachable through the signed PSP webhook `POST /v1/webhooks/payments/{tenantSlug}/{providerID}`.
- The player simulation route cannot emit a reversal, no admin path posts one, and reconciliation is read-only.

**Precedent:** `casino.postRollback` (`internal/casino/orchestrator.go`, lines 1276–1288) already has the correct `FOR UPDATE` pattern.

**Other reversal writers** (found by ledger-finance P1-2):
- `withdrawal_rejected` and `withdrawal_failed` also set `reverses_transaction_id`. The withdrawal state machine already allows only one per hold.
- `withdrawal_reversed` and `bonus_reversal` have no production poster yet.
- `sportsbook_rollback` is protected by its lock plus the unique indexes on the history table.

## D. PAY-REV-1: proposed fix

The fix is `payments`-owned. `ledger-finance` owns the `ledger.Post` change, and `architect` reviews the lock placement.
1. **L2 lock with a re-check.** Take an L2 lock on the original deposit's `ledger_transactions` row, then re-run the already-reversed check after the lock is held. The normative sequence is in §E.
2. **Database invariant (migration 0092).** A partial unique index `ledger_transactions_one_deposit_reversal` on `(tenant_id, reverses_transaction_id) WHERE transaction_type = 'deposit_reversal'`. §F covers the design.
3. **Typed index violation.** `ledger.Post` routes on the constraint name: a violation of the new index returns the new `ledger.ErrReversalAlreadyExists` instead of today's misleading "idempotency key lookup found no row". Payments maps it to `ErrDepositAlreadyReversed`. `ledger-finance` has signed off on exactly this change in advance (review P2-1).
4. **Mandatory denial handling.** A distinct-reference reversal against an already-reversed deposit must:
   - post nothing;
   - return HTTP **409** with the generic body "callback rejected" (today it returns 500 because `ErrDepositAlreadyReversed` has no mapping in `deposit_handlers.go`, lines 340–353);
   - emit the alert `payment_webhook_integrity_alert_deposit_already_reversed`, whose fields are limited to `provider_id`, `tenant_id` and `request_id`;
   - write a `deposit.reversal_rejected` **audit record in a separate tenant-scoped transaction that commits**. `WithTenant` rolls back on any returned error, so the ADR 0088 §4.7 pattern is required.

   A second PSP reversal can mean money really moved twice at the PSP (for example a refund plus a chargeback), so operations and PSP reconciliation must see it.
5. **Records:**
   - ADR 0090, the stage definition and implementation contract;
   - ADR 0082 Amendment A5, an inventory amendment covering the §1.6 row and the §4.5 "no lock outside Post" note;
   - an ADR 0020 amendment: the "callback races are resolved by the same idempotency key" line is false for semantic duplicates, and whichever stage first builds a `withdrawal_reversed` or `bonus_reversal` poster must add both a lock and a type-scoped index;
   - Flow 2 in `docs/architecture/financial-transaction-flows.md`;
   - a pointer from `docs/architecture/07-payments-architecture.md`.

## E. PAY-REV-1: concurrency model

Everything runs in one tenant-scoped transaction (`WithTenant`, READ COMMITTED). ADR 0082 classes apply.

| Step | Action | Class |
|---|---|---|
| S0 | Verify the callback signature. Open the transaction for the tenant resolved by the existing handler; no lock is taken before verification. | — |
| S1 | Load the deposit intent without a lock. If no original deposit transaction exists, take the existing tombstone branch unchanged. | — |
| S2 | `SELECT transaction_type FROM ledger_transactions WHERE id = $orig AND tenant_id = $tenant FOR UPDATE`. If there is no row or the type is not `deposit`, fail closed with an integrity error; this is **never** routed to the tombstone branch. | **L2** |
| S3 | Validate amount and asset against the original deposit. | — |
| S4 | Re-run, as a new statement, the already-reversed `EXISTS` check, keeping the F-7 exclusion of the callback's own reference. Under READ COMMITTED it sees the winner's committed reversal. | — |
| S5 | `GetOrCreateAccounts`, then `LockProjectionsForPosting` | L3 |
| S6 | `ledger.Post` | L4 |
| S7 | `deposit.reversed` audit, in the same transaction | — |

**Safety of the ordering:**
- The lock is a **plain L2 instance**. No new ADR 0082 exception is needed, and the classes and rules R1–R8 are unchanged (architect ruling).
- No L0 or L1 lock exists on this path.
- `deposit_intents` is never locked by a reversal and is not modified by one.
- **Deadlock analysis:** a concurrent deposit success or withdrawal on the same wallet takes no lock on the original deposit's `ledger_transactions` row, so no cycle is possible (payments §3, ledger-finance item 2).

**Stated dependencies:**
- The S4 re-check relies on READ COMMITTED; the code must carry a comment saying so.
- `FOR UPDATE` needs the UPDATE privilege on `ledger_transactions`. A future REVOKE would make this lock, and the existing casino lock, fail closed.

## F. PAY-REV-1: database invariant

> **INV-PAY-REV-1:** at most one `deposit_reversal` ledger transaction per original deposit per tenant, enforced by the partial unique index `(tenant_id, reverses_transaction_id) WHERE transaction_type = 'deposit_reversal'`.

The L2 lock (§E) is the **primary** control: it gives the loser a typed rejection, an audit record and a 409. The index is the **backstop** against a future writer that skips the lock.

**Why the index is scoped to one type:**
- An index over all types is wrong today, because `withdrawal_rejected` and `withdrawal_failed` both reference the same hold transaction.
- It would also block migration on unrelated legacy data, such as casino rollbacks from before casino's lock fix in development databases.
- Partial refunds and refund-plus-chargeback need a rule that knows about amounts, which a uniqueness index cannot express.

A broader, cross-type invariant is deferred as **LEDGER-REV-UNIQ** (§U).

**Why the index includes `tenant_id`** (security S-1): the foreign key on `reverses_transaction_id` is single-column and does not respect RLS. Without `tenant_id`, a buggy writer naming another tenant's transaction would collide on it, and the unique-violation error would reveal that the other tenant's row exists. The single-column foreign key is recorded as deferred.

**Optional (P3):** a CHECK that every `deposit_reversal` carries a `reverses_transaction_id`. It can be added only if the implementation confirms no existing row violates it, and never by weakening data.

## G. PAY-REV-1: idempotency model (F-7 re-audit)

All seven cases were walked by `ledger-finance` and confirmed by `payments`.

| Case | Outcome |
|---|---|
| Legitimate retry: same reversal reference, same payload | Idempotent success (`AlreadyPosted`, HTTP 200). Stage 10's same-reference redelivery fix is preserved. |
| Same reference, different payload | `ErrIdempotencyPayloadMismatch`, HTTP 409 (unchanged F-7 behaviour) |
| Same reference naming a different original deposit | Rejected: mismatch or integrity error, no posting |
| Different reference, deposit already reversed | `ErrDepositAlreadyReversed`, HTTP 409, alert, committed denial audit, no posting |
| Concurrent different references | Exactly one posts; the other blocks at S2, then is rejected at S4 as in the previous row |
| Concurrent same reference | One posts; the other replays idempotently |
| Reversal for a deposit never posted | Tombstone branch (unchanged) |

No conflicting idempotency semantics are introduced. The ledger key remains derived from the reversal reference, so legitimate redelivery replays exactly. "One reversal per original" is a separate, state-level rule.

## H. SB-T1-XMIN: root cause

Trigger T-1 (`migrations/0091_sportsbook_settlement.up.sql`, lines 244–255) accepts a composed void's causation row only if that rollback row's `xmin` equals the low 32 bits of `pg_current_xact_id()`, the **top-level** transaction id. A row inserted under a `SAVEPOINT` keeps the **subtransaction's** transaction id as its `xmin`, even after `RELEASE`. The check therefore rejects a legitimate composed void whose rollback row was written inside a savepoint.

This was verified empirically on PostgreSQL 16.13 in a rolled-back scratch transaction (`02-…analysis.md`).

**Affected paths today:** none.
- The only writer is `insertSettlementRecord` (`internal/sportsbook/settlement.go`), a plain INSERT on the `WithTenant` top-level transaction.
- `ledger.Post`'s savepoint (`db.IdempotentInsert`) wraps only the `ledger_transactions` insert.

The risk is a future batch or provider driver that uses savepoints per bet. That driver would get 409 `SETTLEMENT_INTEGRITY` on every void-after-settlement: fail closed, no money moved.

## I. SB-T1-XMIN: proposed fix

Migration **0093** replaces **only the body** of `sportsbook_bet_settlements_validate()` (`CREATE OR REPLACE FUNCTION`, still SECURITY INVOKER). The causation branch becomes:
- rebuild the row's 64-bit transaction id from its 32-bit `xmin`, relative to the next transaction id `pg_snapshot_xmax(pg_current_snapshot())`: take the largest candidate that is not greater than it;
- **accept only if** that id is `>= pg_current_xact_id()` **and** `pg_xact_status(...)` is not distinct from `'in progress'`;
- anything that cannot be built, raises an error (the "future transaction id" error is caught), or returns a NULL status (a very old transaction) is **rejected** with the existing T-1 message.

This is fail closed: G1 and G2 from the architect, X-2 from security, P2-2 from ledger-finance.

**Why it keeps the guarantee:**
- `xmin` is set by the database, and nothing can rewrite it because history rows are never updated (the deny triggers apply to every role, including the owner; this is guard G3).
- Rows written by *other* uncommitted transactions are invisible to the trigger, and committed ones report `committed`.
- So "in progress" on a visible row can only mean the current transaction or one of its savepoints (security X-1).

**Rejected alternatives:**
- A caller-settable GUC marker: its provenance is the client, not the database.
- A causation-only structural rule: it shows the rows are causally linked, not that they were written in the same transaction.
- **Fallback, only if the guards cannot be met cleanly during implementation:** a trigger-stamped `origin_xact` column. Adopting it requires a separate ruling by the Orchestrator.

**Other properties:**
- Every other branch of T-1 stays byte-identical to 0091.
- 0091 itself is never edited (checksum-immutable).
- The down migration restores 0091's function body verbatim. It stays reversible even with settlement history present, because the trigger only checks new inserts.
- No data changes.
- Records: an ADR 0088 §3.3 follow-up note (text drafted in `05-review-architect.md`); the SB-T1-XMIN row in the task registry is closed on implementation.

## J. Required tests (binding: `qa` plan, `06-review-qa-test-plan.md`)

**PAY-REV-1** (`internal/payments`, deterministic, with waits scoped to the blocker's process ID; no sleeps):

| # | Test | What it proves |
|---|---|---|
| 1 | `TestPayRev1_ConcurrentDistinctReferenceReversals_ExactlyOnePosts` | The probe shape: A holds the lock uncommitted, B waits, A commits, B re-checks and is rejected. Must be **shown failing before the fix**. Must assert B waits on the **S2 `SELECT … FOR UPDATE`** statement with no projection locks held (architect plan change 5: before the fix, B would block later, at the foreign-key check). |
| 2 | `TestPayRev1_ConcurrentIdenticalReferenceReversals_OnePostsOneReplays` | Concurrent same reference |
| 3 | `TestPayRev1_SequentialDistinctReferenceReversal_Rejected` | 409, no posting, denial audit **visible from a fresh transaction** |
| 4 | `TestPayRev1_SequentialSameReferenceRedelivery_StillIdempotent` | F-7 regression |
| 5 | `TestPayRev1_LockOrder_NoDeadlockAgainstConcurrentNewDeposit` | No deadlock; reruns `TestLockOrder_ConcurrentDepositAndDepositReversal_NoDeadlock` |
| 6 | `TestPayRev1_UniqueIndex_BackstopsBypassOfLock` | Through `ledger.Post` (typed `ErrReversalAlreadyExists`) and through raw SQL |
| 7 | `TestMigration0092_RefusesWithExistingDuplicates` | Scratch database, FORCE RLS active, duplicates seeded in **two tenants**, run as the real migration role. The shared test database is never polluted (ledger-finance P2-4). |
| 8 | `TestMigration0092_SucceedsOnCleanDatabase` | — |
| 9 | `TestMigration0092_DownRestoresPriorState` | — |
| 10 | `TestPaymentWebhookHandler_DepositReversalAlreadyReversed_Maps409` | Generic body, alert fields restricted to the allowed list |
| 11 | `TestPayRev1_TenantIsolation_CannotLockOrObserveAnotherTenantsOriginal` | Tenant isolation |
| 12 | Existing F-7 replay suites | Must stay green unchanged |

**SB-T1-XMIN** (`internal/sportsbook`):

| # | Test | What it proves |
|---|---|---|
| 13 | `…SavepointRollbackIsRejected` renamed to `…_SavepointRollbackIsAccepted` | Savepoint case now passes |
| 14 | `…_NestedSavepointIsAccepted` | Nested savepoints |
| 15 | `…_RollbackToSavepointFailsFK` | A rolled-back savepoint's row is gone, so the insert is rejected |
| 16 | `…_RejectsEarlierTransactionRollback`, `…_AcceptsSameTransaction` | Unchanged regression guards |
| 17 | `TestSettlementScenario_ComposedVoid_InsideOuterSavepoint_ServiceLevel` | Shown failing before the fix |
| 18 | `TestMigration0093_DownRestoresExactPriorFunctionBody` | — |
| 19 | `TestMigration0093_UpChangesOnlyFunctionBody` | — |
| + | New tests for the NULL-status and failed-reconstruction cases | Both must reject (security X-2) |

**Also required:**
- **Tip-pin updates**, verified by diff, in `internal/bonus/wave3_phase2_migrations_integration_test.go`, `internal/jurisdiction/migration_0075/0077_integration_test.go`, `internal/operatingmarket/migration_0076_integration_test.go` and `qa_migration_rls_survives_failed_rollback_test.go`. They pin the chain tip, which moves 0091 → 0093.
- **SQL branch-coverage checklist:** the T-1 causation rows in `docs/governance/stage-10-w1-mutation-and-sql-branch-coverage.md` are split and updated, and the index backstop row is added.
- **Logs:** CI logs from the failing runs before the fix are recorded for tests #1 and #17.

## K. Security and RLS impact

**Security verdict:** approve with required changes S-1 to S-5, X-2 and X-3, all adopted above:
- tenant-leading index;
- RLS-proof migration refusal;
- denial audit in a separate committed transaction;
- tenant predicate on the lock query, and a missing row is an integrity error, never a tombstone;
- generic 409 body and restricted alert fields;
- fail-closed `pg_xact_status` handling and SECURITY INVOKER hygiene.

**RLS:**
- The lock runs on the tenant-scoped connection.
- The migration check relies on the index build itself, which scans every row regardless of RLS, never on an RLS-filtered `SELECT`.
- No policy changes.

**Pre-existing finding, outside Stage 10.1's approved scope (S-6, High), registered as PAY-WH-TENANT-1:**
- The payments webhook resolves the tenant from the **URL slug**.
- The mock uses **one global HMAC secret**, and the signed fields (`internal/payments/mock.go`, lines 207–213) do not include the tenant.
- So a callback correctly signed for tenant A also verifies when posted to tenant B's URL. A reversal posted that way writes a tombstone in tenant B.
- This is ADR 0022 §3's open question.
- It **blocks launch with any real PSP**: per-tenant verification keys and a tenant bound into the signed payload are needed.
- It is **not** fixed in Stage 10.1 unless the human widens the scope (§R).
- Correction: no record may state "tenant from the verified credential" for this webhook.

## L. Financial and ledger impact

- **No change to** posting shapes, account types, the double-entry rule or balances. No data is modified.
- **After the fix:** a second reversal of the same deposit can no longer post, which removes an over-debit of `player_cash`.
- **Out of scope:** negative `player_cash` from a **single** legitimate reversal of a spent deposit. It is OB-1-adjacent and remains OPEN.

**Pre-existing duplicates:**
- **Synthetic local and dev databases** that ran the earlier probes probably contain duplicates. 0092 will correctly refuse there; recreate those databases.
- **Staging** (at 0090) is expected to have none, but that is unverified; AWS was not touched. When a staging refresh is authorized, an RLS-proof check with no lasting effect is `BEGIN; CREATE UNIQUE INDEX …; ROLLBACK;` run as the table owner, before applying 0092.
- **Any database whose history must be kept:** if duplicates are found, stop and escalate to the human. The refusal cannot be cleared by a compensating entry (the duplicate row remains), and ledger rows are never deleted (ledger-finance P1-1).

**SB-T1-XMIN:** no financial effect.

## M. Migration strategy

**Numbering and ordering:**
- Two migrations with independent down migrations: **0092** (PAY-REV-1, the P1), then **0093** (SB-T1-XMIN).
- The next ADR number is **0090**. Migration `0090_sb_jurisdiction…` is an unrelated file, the same situation as the ADR 0087 note.

**0092:**
- `DO $$ BEGIN CREATE UNIQUE INDEX ledger_transactions_one_deposit_reversal ON ledger_transactions (tenant_id, reverses_transaction_id) WHERE transaction_type = 'deposit_reversal'; EXCEPTION WHEN unique_violation THEN RAISE EXCEPTION 'migration 0092: duplicate deposit reversals exist (…detail…); this migration cannot run until they are escalated and resolved - see Stage 10.1 plan §L'; END $$;`
- Not `CONCURRENTLY`: `MigrateUp` runs every file in one transaction.
- No `SELECT`-based check and no `SET row_security` (it raises an error for a role without RLS bypass).
- Down: `DROP INDEX IF EXISTS ledger_transactions_one_deposit_reversal`.
- Lock cost: a SHARE lock on `ledger_transactions` for the duration of the build. Acceptable at current volumes; a production rollout plan is for the launch stage.

**0093:**
- `CREATE OR REPLACE FUNCTION` with the body only.
- Down: restore 0091's body verbatim.

**Both:**
- Migration checksum verification and the CI reversibility step (fresh database, 4 down / 4 up) cover them.
- Tip-pin tests are updated in the same commit.

## N. Rollback strategy

- **Code revert with the migrations kept:**
  - With 0092 kept, the index still blocks a duplicate. After a code revert that duplicate surfaces as a 500 again, but it still fails closed.
  - With 0093 kept, T-1 is only more accepting of legitimate savepoint cases. No Go code depends on the old rejection.
- **Down migrations:**
  - 0093 down is always safe.
  - 0092 down only drops the index, so it is safe at any time. It removes the backstop, not data.
- **Forward fix preferred.** Ledger and history rows are never deleted.
- **Staging and production:** no rollout in Stage 10.1.

## O. Specialist reviews, disagreements and Orchestrator rulings

| Specialist | Scope | Verdict |
|---|---|---|
| `payments` | PAY-REV-1 analysis | Root cause, fix, tests (`01`) |
| `sportsbook` | SB-T1-XMIN analysis | `pg_xact_status` recommended (`02`) |
| `ledger-finance` | Both | APPROVED WITH REQUIRED CHANGES; P1-1, P1-2 (plan text), P2-1…P2-5 (`03`) |
| `security` | Both | APPROVE WITH REQUIRED CHANGES S-1…S-5, X-2, X-3; S-6 pre-existing High (`04`) |
| `architect` | Both | SOUND; 7 plan changes; ADR 0090 + A5 + records (`05`) |
| `qa` | Both | Binding 19-test plan (`06`) |
| `backend` | API | 500 → 409 confirmed; webhook missing from OpenAPI; ADR 0089 API claims accurate (`07`) |
| `security`, `bonus-engine`, `identity-compliance` | ADR 0089 | ACCEPT WITH CHANGES (applied, `6e25d24`); PASS; no blocking (`08`–`10`) |

**Disagreements and rulings:**

| # | Disagreement | Ruling |
|---|---|---|
| R-1 | **Denial audit.** Architect: "not required in 10.1". Ledger-finance: mandatory. Security: it must survive the rollback. | **Mandatory**, written in a separate committed tenant-scoped transaction (the ADR 0088 §4.7 pattern), plus the 409 and the named alert. |
| R-2 | **Epoch construction for `pg_xact_status`.** The payments/sportsbook draft used "epoch − 1". | **Ledger-finance and architect rule adopted:** rebuild relative to `pg_snapshot_xmax(pg_current_snapshot())`, accept only if the id is ≥ `pg_current_xact_id()` and the status is `'in progress'`, and reject on NULL or error. |
| R-3 | **Index scope.** Global vs per-type; tenant-leading or not. | **Per-type (`deposit_reversal`) and tenant-leading.** The cross-type index is deferred (LEDGER-REV-UNIQ, REV-UNIQ-CASINO). |
| R-4 | **Migration pre-check.** The draft used a `SELECT` or per-tenant loop, or `row_security = off`. | **Refuse through the index build itself** inside a `DO … EXCEPTION WHEN unique_violation`, following the 0091 down-migration precedent. |
| R-5 | **Remediation text.** The draft said "compensating entry, then re-run". | **Replaced:** stop and escalate to the human; recreate synthetic databases. |
| R-6 | **"No tests pin 0091 as tip"** (payments §5). | **Incorrect.** Five tip-pin test files exist and must be updated. |
| R-7 | **"Tenant from verified credential"** wording. | **Incorrect for the payments webhook** (S-6). Recorded as PAY-WH-TENANT-1, outside scope. |

## P. Risks

| Risk | Mitigation |
|---|---|
| 0092 refuses on a database holding legacy duplicates | Synthetic databases: recreate. Kept history: escalate (§L). CI uses fresh databases. |
| The lock test passes for the wrong reason (it blocks at the foreign key before the fix) | Test #1 asserts the waiting statement is S2, with no projection locks held |
| A future writer hits the index without the lock and gets a misleading error | `ErrReversalAlreadyExists` routed by constraint name (§D.3) |
| `pg_xact_status` edge cases (NULL, future transaction id, wraparound alias after 2^32 transactions) | Fail-closed guards with tests. The theoretical alias can additionally be closed with `cause.created_at = now()` (ledger-finance P3). |
| Scope creep into other reversal types or webhook tenant binding | Deferred and registered (§U); S-6 needs a human ruling (§R) |
| Staging drift (at 0090, no F-7, no 0091) | No deploy in Stage 10.1. A staging refresh is a separately authorized step. |

## Q. Existing human decisions

All remain OPEN and are not reopened:
- ADR 0009
- HDR-J-6/7/8/9
- HDR-M-1/2
- HDR-SB-1
- OB-1, including negative cash after a legitimate single reversal
- vendor, legal and retail decisions

## R. New human decisions (only those genuinely required)

1. **Approve Stage 10.1 as scoped in §T**, which accepts ADR 0090. Required.
2. **PAY-WH-TENANT-1 (S-6):** keep it outside Stage 10.1 as a separately planned P1/launch-blocking item (the Orchestrator's recommendation, because it touches the PSP credential and adapter model and ADR 0022 §3), or add it to Stage 10.1. Required only to confirm the scope boundary. Default if not answered: kept outside.
3. **Contingent, not now:** if a database whose history must be kept is ever found to contain duplicate deposit reversals, the resolution is a human decision.

Engineering choices are **not** human decisions: index shape, numbering, error names, test design.

## S. Future AI-agent architecture requirement

This is recorded as **ADR 0089** (`docs/decisions/0089-future-ai-agent-architecture-boundary.md`): "ACCEPTED as a binding future requirement — NOT IMPLEMENTED". It was reviewed by `security` (S-1…S-9 applied), `bonus-engine` (PASS) and `identity-compliance` (all applied); the backend API claims were verified.

The principle: **AI may analyse and propose; deterministic core services validate and execute.** Agents are never authoritative for:
- ledger entries, wallet balances or financial calculations;
- jurisdiction, KYC, AML, responsible gaming or self-exclusion;
- risk decisions;
- payment authorization or withdrawal approval;
- bonus financial liability;
- tenant isolation or RBAC.

Future controls:
- a dedicated `ai_agent` principal, with an allow-listed grant set kept disjoint from staff roles;
- tenant and brand scoping, fail-closed;
- a database-enforced rule that the proposer is never the checker;
- actor/subject audit;
- dry-run and simulation;
- execution limits enforced inside the database transaction;
- recording of model and provider version;
- credential lifecycle and kill switches;
- defences against inbound and outbound prompt injection.

Agents propose through the existing `BonusSuggestion` channel and execute only through the canonical Bonus, Gamification and Reward APIs.

Registered as **AI-ARCH-FUTURE** in the task registry. **No AI code, dependency, model provider or infrastructure is added in Stage 10.1.**

Future human decisions recorded in ADR 0089 §8, not current:
- model and provider selection;
- automated targeting and profiling restrictions per jurisdiction;
- a vulnerable-player status and inducement rule.

## T. Exact Stage 10.1 scope (on approval)

1. **PAY-REV-1**, per §D–§G and §M:
   - the L2 lock with the re-check;
   - migration 0092 (tenant-leading, type-scoped unique index with an RLS-proof refusal);
   - `ledger.Post` constraint-name routing to `ErrReversalAlreadyExists`;
   - `ErrDepositAlreadyReversed` mapped to 409, with a generic body, a restricted alert and a denial audit committed separately;
   - an optional P3 fix: stop logging amounts in the `ErrCallbackProviderMismatch` 500 (security S-5).
2. **SB-T1-XMIN**, per §H–§I: migration 0093, body-only and fail-closed.
3. **Tests** per §J, tip-pin updates, and the SQL branch checklist update.
4. **Records:**
   - ADR 0090 (ACCEPTED on approval);
   - ADR 0082 Amendment A5;
   - the ADR 0020 amendment;
   - the ADR 0088 §3.3 follow-up;
   - Flow 2;
   - a pointer from `07-payments-architecture.md`;
   - the task registry (close PAY-REV-1 and SB-T1-XMIN);
   - progress, active-stage, testing strategy;
   - a completion report.
5. **Reviews** of the real diff: `ledger-finance`, `security` (mandatory before IMPLEMENTED), `code-reviewer`, `qa`.

## U. Explicitly out of scope (recorded, not built)

| Item | Where it is recorded |
|---|---|
| **PAY-WH-TENANT-1** (S-6: bind the webhook tenant to the credential; launch-blocking for a real PSP) | Pending the human ruling in §R.2 |
| **LEDGER-REV-UNIQ:** a cross-type "one reversal per original" rule, amount-aware for partial refunds | Deferred |
| **REV-UNIQ-CASINO:** a unique index for `casino_rollback` | Deferred (P3; casino review plus a data check) |
| Future posters of `withdrawal_reversed` and `bonus_reversal` | Bound by the ADR 0020 amendment (lock plus type-scoped index) |
| Single-column `reverses_transaction_id` foreign key, not tenant-scoped | Deferred (security S-1) |
| **API-DOC-PAYWH:** the payments webhook contract is absent from OpenAPI | Documentation follow-up (backend) |
| AI-agent implementation of any kind | ADR 0089, AI-ARCH-FUTURE |
| Any AWS, staging, deploy, Terraform, IAM or network change | — |
| Real provider integrations, cashout, bonus-funded sportsbook | — |
| OB-1 and negative-cash policy | — |

## V. Acceptance criteria

1. Test #1 fails before the fix and passes after it. Exactly one reversal posts under a distinct-reference race, and `player_cash` never goes below its value after one reversal.
2. Tests #2–#12 pass, including every existing F-7 replay suite unchanged.
3. Migration 0092 refuses on duplicates in two tenants with FORCE RLS active, succeeds on a clean database, and its down migration works. 0093's up changes only the function body and its down restores 0091's exact body.
4. Tests #13–#19, plus the NULL and error cases, pass; the service-level savepoint test fails before the fix and passes after.
5. Tip-pin tests and the SQL branch checklist are updated and verified by diff.
6. `build-test-lint` is green on the implementation head: the evidence assertion, reversibility, lint at 0 issues, and all 32 or more packages.
7. `ledger-finance`, `security`, `code-reviewer` and `qa` have signed off on the real diff.
8. All §T.4 records are updated.
9. The tree is clean and the head is pushed. AWS is untouched.

Anything short of this is labelled PARTIALLY IMPLEMENTED, with the gap named.

## W. Implementation gates

| Gate | Condition |
|---|---|
| G0 | **Human approval of this plan (ADR 0090)** — **current stop point** |
| G1 | PAY-REV-1 implemented; tests #1–#12 green locally; pre-fix failure of #1 recorded |
| G2 | SB-T1-XMIN implemented; tests #13–#19 plus the NULL/error cases green; pre-fix failure of #17 recorded |
| G3 | CI green on the implementation head (every step, including the evidence assertion) |
| G4 | Reviews of the real diff complete; findings fixed or recorded |
| G5 | Records complete (§T.4) and completion report written |
| G6 | Clean tree, push verified, stop at the next human gate |
