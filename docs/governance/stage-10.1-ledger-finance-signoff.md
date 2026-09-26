# Stage 10.1 — ledger-finance financial review and sign-off

- **Reviewer:** `ledger-finance` specialist
- **Date:** 2026-09-26
- **Diff reviewed:** `git diff 8561ac2..250828b` (commits `9fb1aa2`, `cff2eef`, `b8d1927`, `50d94af`, `e9e0ad8`, `250828b`), ADR 0090 scope: PAY-REV-1 (migration 0092), SB-T1-XMIN (migration 0093), PAY-WH-TENANT-1.
- **Inputs read:** CLAUDE.md; `docs/plans/stage-10.1-planning-gate-proposal.md` §D–§G, §I, §L, §M, §O; my planning review `docs/plans/stage-10.1-planning/03-review-ledger-finance.md`; ADR 0020 / 0082 amendments (`e9e0ad8`); ADR 0019 matrix change (`250828b`); ADR 0088 §3.3 follow-up; the code, migrations and tests in the diff.
- **Mode:** review only. No code changed and nothing committed.

## Overall verdict

**CONDITIONALLY APPROVED.** There is no P0 and no P1.

The core financial invariant is proven. A deposit cannot be reversed twice, concurrently or sequentially, and a rejected reversal leaves no financial effect.

Two P2 findings must be closed before PAY-REV-1 is labelled `IMPLEMENTED`. Both are small, both are in ledger-owned or ledger-adjacent code, and neither involves money moving incorrectly today:

- **P2-A:** legitimate-retry classification in `ledger.Post` depends on the physical order of Postgres indexes.
- **P2-B:** the denial audit record is too thin for PSP reconciliation.

SB-T1-XMIN is **APPROVED**. The implementer's deviation from my P2-2 construction is correct, and my original construction was wrong (see §3).

## 1. Evidence run (this review)

The Postgres 16.13 local CI database was left running. The suites were run against a clean `git archive 250828b` export, because the working tree contains an untracked file, `internal/payments/webhook_replay_duplicate_integration_test.go`. That file is not part of the diff under review, and it breaks the build of the `payments` integration package (`undefined: uuid`); see P3-9.

| Command (integration tag) | Result |
|---|---|
| `go build ./...`, `go vet` on db/ledger/payments/httpserver/sportsbook | OK |
| `go test -count=1 -p 1` over db, ledger, payments, withdrawal, bonus, sportsbook, reconciliation, jurisdiction, operatingmarket | all `ok` |
| `go test -count=1 ./internal/httpserver/...` | `ok` (37.9s) |
| `go test -race -count=10 -run 'TestPayRev1_\|TestF7Payments_ConcurrentIdenticalReversalRedelivery\|TestF7Payments_ConcurrentTombstone' ./internal/payments/` | `ok`, stable over 10 iterations |
| SB-T1-XMIN: `TestDBConstraints_T1_ComposedVoid*` (6), `TestMigration0093_*` (2), `TestSBT1XMIN_*`, `TestSettlementScenario_ComposedVoid_InsideOuterSavepoint_ServiceLevel` | all PASS |

I also ran three ad-hoc probes, each in a rolled-back transaction or a dropped throwaway schema. They left no residue.

1. **Dual unique violation (P2-A).**
   - As deployed, a same-key retry of a `deposit_reversal` reports `ledger_transactions_tenant_idempotency_key_key`, which is the correct replay route.
   - After rebuilding the idempotency and provider-tx indexes (so their OIDs are newer than 0092's), the same retry reports `ledger_transactions_one_deposit_reversal`.
2. **`REINDEX INDEX CONCURRENTLY` flips the reported constraint** on a toy table with the same two-index shape: `t_k` before, `t_r` after. Postgres checks unique indexes in index-OID order, and a concurrent reindex creates a new, higher-OID index.
3. **xid8 anchoring.** Rows were inserted plain, in a released savepoint, in a nested released savepoint, and in an open savepoint.
   - The 0093 construction (anchored on `pg_current_xact_id()`) accepts all four.
   - My P2-2 construction (anchored on `pg_snapshot_xmax`) rejects all three savepoint rows. Observed: top-level xid = snapshot xmax = 2413265; subxids 2413266–2413269.

A global reconciliation probe (Σdebits = Σcredits, projection drift, duplicate reversals) could not be run. No available role has BYPASSRLS, so FORCE RLS returned 0 rows. Package-level assertions and the `internal/reconciliation` suite stand in for it.

## 2. Required items A–G

| # | Claim | Verdict | Evidence |
|---|---|---|---|
| A | The same deposit cannot be reversed twice | **APPROVED** | See A below |
| B | Concurrent reversal attempts cannot both post | **APPROVED** | See B below |
| C | Legitimate retry behaves correctly | **APPROVED, conditional on P2-A** | See C below |
| D | A different payload under an existing idempotency key is rejected (`ErrIdempotencyPayloadMismatch`) | **APPROVED** | See D below |
| E | A cross-tenant payment webhook cannot trigger a financial effect, including tombstones | **APPROVED (payments, MOCK resolver)** | See E below |
| F | A rejected reversal leaves no partial financial effect | **APPROVED** | See F below |
| G | The rejection audit survives the rejected transaction | **APPROVED for survival; content: P2-B** | See G below |

**A. Evidence.**
- Primary control: S2 `FOR UPDATE` on the original's `ledger_transactions` row, then a fresh-statement S4 `EXISTS` re-check (`orchestrator.go`, `receiveDepositReversalCallback`).
- Backstop: 0092 partial unique index `(tenant_id, reverses_transaction_id) WHERE transaction_type='deposit_reversal'`.
- `TestReceiveCallback_SecondReversalOfSameDepositRejected` (cash stays 0).
- `TestF7Payments_SequentialReversalRedeliveryIsIdempotent` (a distinct second ref gives `ErrDepositAlreadyReversed`).
- `TestPaymentWebhookHandler_DepositReversalAlreadyReversed_Maps409` (exactly 1 `deposit_reversal`).
- `TestPayRev1_UniqueIndex_BackstopsBypassOfLock`: through `Post` gives `ErrReversalAlreadyExists`; raw SQL gives 23505 naming the 0092 index; count = 1.

**B. Evidence.**
- `TestPayRev1_ConcurrentDistinctReferenceReversals_ExactlyOnePosts` asserts that the waiter's statement is S2 and that it holds no `wallet_balance_projection` lock, so it is not blocked "for the wrong reason".
- `TestPayRev1_DefectRepro_DistinctReferenceRaceNeverDoublePosts` asserts count = 1, cash = 0 and loser `ErrDepositAlreadyReversed`.
- Pre-fix failures are recorded in `evidence/pay-rev-1-*-prefix*.txt`: 2 reversals, cash −1,000, and the waiter blocked inside `Post`'s INSERT.
- Stable under `-race -count=10`.
- Serialization holds because S4 is a new statement under READ COMMITTED (commented in code, per my P3-2). The 0092 index covers any future RR/SERIALIZABLE caller.

**C. Evidence.**
- As deployed:
  - `TestF7Payments_SequentialReversalRedeliveryIsIdempotent` returns the same tx id with no new rows.
  - `TestF7Payments_ConcurrentIdenticalReversalRedelivery` (6-way) gives one posting and one id for all callers.
  - `TestF7Payments_ConcurrentTombstoneRedeliveryIsIdempotent` passes.
  - `TestReplay_DifferentReversalLinkRejected` → `expectAlreadyPosted` covers a `Post`-level same-key `deposit_reversal` replay that violates both indexes.
- Probe 1 confirms that correctness depends on OID order. See P2-A.

**D. Evidence.**
- `TestF7Payments_ReversalRefReusedForDifferentDepositRejected`: same ref naming another deposit gives `ErrCallbackPayloadMismatch` wrapping `ErrIdempotencyPayloadMismatch`, with no posting and cash unchanged.
- The ledger-level `TestReplay_*` family (amount, account, link, provider ref / causation, reason code, bonus mirror, concurrent mixed payloads) all PASS.
- A same-ref reversal with a different amount or asset is rejected earlier with `ErrCallbackProviderMismatch`. Nothing is posted.

**E. Evidence.**
- Verification happens strictly before any ledger, intent, lock or audit work. The only reads before that are `ProviderAcceptsWebhook` (read-only EXISTS) and the handler's tenant-slug lookup. The mock signature covers `prefix|tenant|provider|key_id|body`.
- `TestWebhook_CrossTenant_ReversalOfUnseenRef_NoTombstone`: an A-signed reversal of an unseen ref delivered to B produces no tombstone, ledger row or audit row in B. Before the fix, a tombstone was written (`evidence/pay-wh-tenant-1-cross-tenant-prefix.txt`).
- `TestWebhook_CrossTenant_SameRefCollision_Rejected`: B's colliding intent stays `pending`.
- `TestWebhook_SharedSecretAcrossTenants_TenantStillBound`.
- `TestPayRev1_TenantIsolation_CannotLockOrObserveAnotherTenantsOriginal`.
- The S2 lock carries an explicit `tenant_id` predicate in addition to RLS.
- Residual, labelled correctly by the author: the real credential resolver is `NOT IMPLEMENTED`, so this is `MOCK`. Casino callbacks remain non-conforming (CAS-WH-TENANT-1), so a cross-tenant **casino** callback financial effect is still open. That is outside this diff and launch-blocking.

**F. Evidence.**
- The whole callback runs in one `WithTenant` transaction, and any returned error rolls it back.
- On the S4 path, nothing is written before the rejection.
- On the backstop path, `IdempotentInsert` rolls back its savepoint and then the transaction rolls back. L3 projection materialisation, if any, rolls back with it.
- The success audit is written only after `Post`.
- Tests: cash = 0 after rejection (A/B tests); reversal count = 1 (handler test); `assertNoFinancialEffect` on the cross-tenant tests; `TestMigration0092_RefusesWithExistingDuplicates` shows no partial index and no deleted rows.

**G. Evidence.**
- The handler opens a **separate** `WithTenant` for `RecordDepositReversalRejection` after the failed transaction rolled back (ADR 0088 §4.7 pattern).
- The handler test reads the `deposit.reversal_rejected` row from a fresh transaction: count 1, outcome `denied`.
- The named alert `payment_webhook_integrity_alert_deposit_already_reversed` has allow-listed fields.
- An audit-write failure is logged and never blocks the 409. That is acceptable, because it is a denial and not a mutation.

## 3. Additional verifications

| Item | Verdict | Evidence / notes |
|---|---|---|
| Append-only | **APPROVED** | See notes below |
| Double-entry | **APPROVED** | Posting shape unchanged: Dr `player_cash` / Cr `psp_clearing`, same amount (the original's). `ledger_entries_balanced` is still forced IMMEDIATE in `Post`. |
| No balance UPDATE | **APPROVED** | No projection write was added. Projections move only through the 0023 trigger on entry insert. |
| No floats | **APPROVED** | No float, real or double precision in added Go or SQL. Amounts stay `int64` minor units, and SQL xid arithmetic is `bigint`. |
| Deterministic lock order | **APPROVED** | See notes below |
| `db.IdempotentInsert` signature change | **APPROVED, behaviour-preserving at all 5 call sites** | See notes below |
| Migration 0092 refusal semantics | **APPROVED** | See notes below |
| SB-T1-XMIN deviation (epoch anchored to `pg_current_xact_id()`) | **APPROVED; I withdraw my P2-2 construction** | See notes below |
| ADR 0019 matrix change (`250828b`) | **CONCURRENCE GIVEN** | See notes below |
| ADR 0020 amendment (`e9e0ad8`) | **ACCEPTED with one required wording correction (P2-A)** | See notes below |
| ADR 0082 Amendment A5 (`e9e0ad8`) | **ACCEPTED** | The inventory row is struck through, not deleted, and §4.5 has a pointer. No new class or exception. The withdrawal writers named in my P1-2 are covered by the ADR 0020 amendment and plan §F/§U. P3-6 is a date inconsistency. |
| ADR 0088 §3.3 follow-up | **ACCEPTED; this document is the requested ledger-finance re-review** | P3-3 is a wording correction on the wraparound claim. |

**Append-only notes.**
- No `UPDATE`, `DELETE` or `TRUNCATE` was added in non-test code. The only match is the S2 `SELECT … FOR UPDATE`, which is a row lock and not a mutation; the deny triggers do not fire.
- 0092 adds an index only, and the refusal path never deletes rows (tested).
- 0093 is body-only; the history deny triggers are unchanged (`TestMigration0093_UpChangesOnlyFunctionBody`).
- The `reconciliation` immutability suite passes.

**Lock order notes.**
- Sequence: S2 **L2** (a single row, so ascending order is trivial), then `GetOrCreateAccounts`, then `Post` (L3 pre-lock, then L4 insert).
- No L0 or L1 lock is taken on this path, and nothing is locked before verification. Before S2 the path does only `ProviderAcceptsWebhook` (plain EXISTS), the resolver call, HMAC verification and an unlocked `deposit_intents` SELECT.
- Test #1 proves the waiter sits at S2 with no L3 held.
- `TestLockOrder_ConcurrentDepositAndDepositReversal_NoDeadlock` passes.
- The tombstone branch is unchanged, and no lock is taken there before `Post`.
- The runtime role has the UPDATE privilege that `FOR UPDATE` needs (checked). That dependency is recorded in plan §E.

**`db.IdempotentInsert` notes.**
- `UniqueViolationConstraintName(err)` returns `ok` exactly when `IsUniqueViolation(err)` would: same `errors.As` and the same SQLSTATE 23505. `conflict` and `err` are therefore identical to before on every path.
- Four sites discard the name: bonus `IssueGrant`, sportsbook `insertBet`, withdrawal `RequestWithdrawal`, payments `InitiateDeposit`.
- `ledger.Post` adds one branch that is taken only when the name equals the 0092 index. Every other conflict follows the unchanged replay path.
- The owning suites (bonus, sportsbook, withdrawal, payments, ledger, db) all pass.

**Migration 0092 notes.**
- The refusal comes from the index build itself inside `DO … EXCEPTION WHEN unique_violation`. It has no SELECT pre-check and no `row_security=off` (ruling R-4). It is not `CONCURRENTLY`, because each migration runs in one transaction.
- `TestMigration0092_RefusesWithExistingDuplicates` covers duplicates in 2 tenants under FORCE RLS, run as a NOBYPASSRLS owner. It confirms the refusal, that nothing is recorded in `schema_migrations`, that no partial index is left, and that no rows are deleted. It runs on a scratch database, which satisfies my P2-4.
- The clean-database and down/up round-trip tests pass.
- The message follows R-5: escalate, never delete.
- Tenant-leading index: I accept ruling R-3 (security S-1) over my planning text.

**SB-T1-XMIN notes.**
- My P2-2 claim that `pg_snapshot_xmax` "never yields a future xid, so it is a safe anchor" was wrong for this purpose. Snapshot xmax is the latest *completed* xid plus 1, so own-tree subxids routinely exceed it. Probe 3 reproduces this on PG 16.13, and the implementer's diagnosis is correct.
- **Correctness by case:**
  - **Plain insert:** accepted (`AcceptsSameTransaction`; probe).
  - **Released savepoint:** accepted (`SavepointRollbackIsAccepted`, service-level savepoint test; the pre-fix failure is recorded).
  - **Nested savepoint:** accepted (`NestedSavepointIsAccepted`; probe).
  - **`ROLLBACK TO`:** the row is gone and the transaction is rejected (`RollbackToSavepointFailsFK`).
- **Older committed transaction, same epoch:** `raw < low32(top)` reconstructs below `pg_current_xact_id()`, so it is rejected by the `>=` conjunct (`RejectsEarlierTransactionRollback`).
- **Wraparound, cited row from an earlier epoch with low bits ≥ the current top's low bits.** The id reconstructs into the current epoch.
  - If it lands beyond the next xid, `pg_xact_status` raises "in the future". The error is caught and the row is rejected (fail closed; probe test covers it).
  - If it lands on a completed xid, the status is `committed` or `aborted`, so the row is rejected.
  - If it lands on a *currently in-progress* xid (own tree or a concurrent transaction), it is **accepted**. This is the theoretical alias I recorded as P3-3 at planning. It needs ≥ 2^32 xids of age and a coincident low-32 value, and the cited row must still be this bet's `rollback` with every other T-1 precondition passing. T-1 is a provenance guard, and amounts are decided by the Go table plus the ledger, so there is **no financial effect**. Accepted residual; see P3-3.
- **NULL status** (clog-truncated): rejected via `IS DISTINCT FROM 'in progress'`.
- **Any error:** rejected, because `EXCEPTION WHEN OTHERS` re-raises T-1's message.
- **A transaction whose own subxids cross an epoch boundary:** the reconstruction goes one epoch low and the row is falsely *rejected*. That is fail closed and acceptable. The comment's "cannot span" claim is inaccurate (P3-3).
- 0093 down restores 0091's `prosrc` byte-for-byte (`TestMigration0093_DownRestoresExactPriorFunctionBody`).

**ADR 0019 notes.**
- The new "verified provider callback" wording (route resolves the tenant, the single per-(tenant, provider) credential verifies it, and the tenant is bound into the MAC) is accurate for payments as implemented. The status note (payments `MOCK`; casino non-conforming, CAS-WH-TENANT-1; sportsbook unimplemented) is accurate and does not over-claim.
- The orchestrator may replace "`ledger-finance` concurrence pending final review" with a reference to this document.
- P3-7 is a pre-existing omission.

**ADR 0020 notes.**
- The doctrine is correct and matches my planning ruling: delivery duplicates are resolved by idempotency keys, while semantic duplicates need an L2 lock plus a type-scoped, tenant-leading index. So are the future-poster binding for `withdrawal_reversed` / `bonus_reversal` and the REV-UNIQ-CASINO deferral.
- The bullet "No existing idempotency semantics changed" is true only while the idempotency index's OID is older than 0092's. It must be amended when P2-A lands.

## 4. Findings

### P0 / P1
None.

### P2 (must be closed before PAY-REV-1 is labelled `IMPLEMENTED`)

**P2-A — `ledger.Post` classifies a legitimate reversal retry by index OID order (ledger-owned).**

When a same-key `deposit_reversal` retry is inserted, it violates three unique indexes at once: `(tenant_id, idempotency_key)`, `(tenant_id, provider_id, provider_tx_id)`, and 0092's index. Postgres reports whichever it checks first, and it checks in index-OID order.

Today the idempotency index is older, so `Post` takes the replay path and returns `AlreadyPosted`. Probes 1 and 2 show that any future rebuild of that index flips this. A rebuild includes a routine production `REINDEX INDEX CONCURRENTLY` against bloat, or a migration that re-creates the constraint. After a flip, the retry gets `ErrReversalAlreadyExists`, which becomes `ErrDepositAlreadyReversed`. The result is an HTTP 409 to the PSP for a reversal that *did* post, a false integrity alert, and a false `deposit.reversal_rejected` audit row. The same applies to a same-key/different-payload replay, which would be mislabelled rather than reported as `ErrIdempotencyPayloadMismatch`.

The failure is fail-closed: no double posting and no lost posting. It still breaks the idempotency contract for legitimate retries, silently, under an ordinary DBA action.

Required fix (inside my pre-granted P2-1 scope): in `Post`, when `conflictConstraint == reversalOneDepositReversalConstraint`, first call `lookupByIdempotencyKey`.
- If a row exists, fall through to the existing replay comparison, which gives `AlreadyPosted`, `ErrIdempotencyPayloadMismatch` or `ErrIdempotencyKeyReused`.
- Only on `pgx.ErrNoRows`, return `ErrReversalAlreadyExists`.

Required tests:
- A ledger test that forces the flipped order. For example, within a rolled-back transaction or a scratch database, drop and re-add `ledger_transactions_tenant_idempotency_key_key` and `idx_ledger_transactions_tenant_provider_tx`. Then assert that a same-key replay still returns `AlreadyPosted`, and that a same-key/different-amount replay still returns `ErrIdempotencyPayloadMismatch`.
- Amend ADR 0020's "No existing idempotency semantics changed" bullet accordingly.

**P2-B — the `deposit.reversal_rejected` audit record cannot be reconciled.**

`RecordDepositReversalRejection` records only `provider_id` (metadata and `TargetID`) and `request_id`. It records no deposit intent id, no original ledger transaction id, no rejected reversal provider reference, and no id for the existing reversal.

My planning P2-3, adopted as ruling R-1, made this audit mandatory for one reason: a second distinct PSP reversal can mean money moved twice at the PSP (for example a refund plus a chargeback), so operations and daily PSP reconciliation must be able to act on it. With only `provider_id`, they cannot tell which deposit is affected. The request body is deliberately never logged, and `request_id` does not recover it.

The security restriction in plan §D.4 applies to the **alert log line**, which remains correctly restricted. It does not apply to the append-only audit store, and the success-path `deposit.reversed` audit already stores the same references.

Required fix:
- Return a typed error from `receiveDepositReversalCallback` that wraps `ErrDepositAlreadyReversed` and carries `DepositIntentID`, `OriginalLedgerTransactionID`, the rejected `ReversalProviderReference` and, where known, the existing reversal's transaction id. Apply this on both the S4 path and the backstop path.
- Record those fields in the denial audit, with `TargetType` `deposit_intent` and `TargetID` the intent id. Keep the HTTP body and the alert fields unchanged.
- Extend `TestPaymentWebhookHandler_DepositReversalAlreadyReversed_Maps409` to assert them.

### P3 (record; fix opportunistically or carry to the named follow-up)

- **P3-1: the 0092 refusal message omits the Postgres DETAIL.** My planning text included it. Operators must run the §L census (`BEGIN; CREATE UNIQUE INDEX …; ROLLBACK;`) to find the pair. Acceptable, because the census procedure is documented.
- **P3-2: optional CHECK not added.** The CHECK `transaction_type <> 'deposit_reversal' OR reverses_transaction_id IS NOT NULL` (my planning P3-1) is not in 0092. A writer that omits the link bypasses the index. The only writer always sets the link (from `original.LedgerTransactionID`, never nil on that path), and tombstones use `TxTombstone`. Carry it to LEDGER-REV-UNIQ.
- **P3-3: 0093 comment and ADR 0088 note overstate the wraparound behaviour.** They say the wraparound case "fails CLOSED". That holds only when the reconstruction lands beyond the next xid or on a completed xid. An alias onto a currently in-progress xid is accepted (analysis in §3), with no financial effect. The "a single transaction cannot span an epoch" statement is also false, though that case fails closed. Correct the wording in the next touch of those records. The belt-and-braces `AND cause.created_at = now()` conjunct (transaction-start time, equal across subtransactions) would close the alias. It should be recorded as a deferred option, because it depends on `created_at` never being caller-supplied.
- **P3-4: the G1/G2 guard test exercises a replica, not the real trigger.** `TestSBT1XMIN_ReconstructionGuard_NullAndErrorCasesRejectClosed` tests a hand-copied probe function, so drift between the probe and the real trigger body would go unnoticed. Consider asserting that the probe's expression text appears verbatim in `pg_get_functiondef(sportsbook_bet_settlements_validate)`.
- **P3-5: `ErrDepositReversalIntegrity` has no named alert.** This is a corrupted deposit link, an integrity event. It currently surfaces only as the generic `payment_webhook_failed` log line plus an HTTP 500. Recommend a named alert such as `payment_webhook_integrity_alert_reversal_link`.
- **P3-6: A5 dates are inconsistent.** ADR 0082 dates A5 as 2026-09-25 in the §1.6/§4.5 notes and 2026-09-26 in the amendment heading.
- **P3-7: pre-existing omission in ADR 0019.** The "verified provider callback" row does not explicitly name `deposit_reversal` or the payments `tombstone` among the types it may originate. Add them at the next edit.
- **P3-8: the handler-level 409 test asserts only the reversal count.** It does not check the `player_cash` balance or the entry count. Balance is covered in the `payments` tests, so this is optional.
- **P3-9: the untracked working-tree file breaks the integration build.** `internal/payments/webhook_replay_duplicate_integration_test.go` is not in the reviewed diff and fails to compile (`undefined: uuid`), which breaks the `payments` integration build. Whoever owns it must fix it before committing, or CI will go red. I did not touch it.

## 5. Sign-off statement

- **PAY-REV-1 financial correctness (A, B, D, E, F, G-survival; append-only; double-entry; no floats; no balance UPDATE; lock order; `IdempotentInsert` change; 0092 semantics):** **APPROVED.**
- **PAY-REV-1 as a whole:** ledger-finance sign-off is **conditional on P2-A and P2-B** being fixed with the named tests. That is a small ledger change plus an audit-content change. Once both land and the named tests pass, this review needs no re-run beyond my confirming those two diffs.
- **SB-T1-XMIN (0093, including the `pg_current_xact_id()` anchoring deviation):** **APPROVED.** No financial effect.
- **PAY-WH-TENANT-1 financial aspects (item E):** **APPROVED** for payments under the `MOCK` resolver. The real resolver remains `NOT IMPLEMENTED` and `PROVIDER DEPENDENT`. CAS-WH-TENANT-1 remains open and launch-blocking.
- **ADR 0019 matrix change:** ledger-finance **concurrence given**.
- **No human decision is required by this review.** The contingent §R.3 decision (duplicates found in a database whose history must be kept) is unchanged.
