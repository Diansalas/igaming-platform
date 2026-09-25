> Stage 10.1 planning gate — specialist working paper (verbatim, recorded 2026-09-25 against 56f5135). Where it differs from the Orchestrator rulings in `docs/plans/stage-10.1-planning-gate-proposal.md` §O, the rulings govern.

# ledger-finance planning-gate review: PAY-REV-1 (0092) + SB-T1-XMIN (0093)

Repo 56f5135, read-only review, no repo files touched. Overall: **APPROVED WITH REQUIRED CHANGES.** No P0. Two P1s, both in the plan text; the code direction is sound.

## Verdicts per item

**(1) DB invariant: APPROVED, type-scoped. A global index is rejected for now.**
These writers set `reverses_transaction_id` today (grep, non-test):
- `deposit_reversal` (payments :1015)
- `casino_rollback` (orchestrator.go:1428, bonus_settlement.go:708)
- `sportsbook_rollback` (settlement.go:911, one per settlement generation, backstopped by the 0091 history index)
- **`withdrawal_rejected` / `withdrawal_failed`** (withdrawal.go:802/1193/1278, reversing the hold under the L1 `withdrawal_requests` state machine). **The plan's §1.3 misses these.**

Tombstones never set the link, because they claim `(provider_id, provider_tx_id)` instead. So each current writer already produces at most one reversal per original, and a global "one reversal per original" is semantically true of today's design. It is still not safe now:
- a) The pre-flight would refuse on any dev or CI database holding pre-fix casino duplicates (casino's own race was empirically reproduced before its L2 fix), which would block this P1 fix on unrelated data.
- b) The `IdempotentInsert` misclassification (P2-1) would hit every writer.
- c) Partial refunds and refund-plus-chargeback are open business shapes that need an amount-aware invariant, not uniqueness.

Ruling:
- 0092 = `CREATE UNIQUE INDEX ux_ledger_transactions_one_deposit_reversal ON ledger_transactions (reverses_transaction_id) WHERE transaction_type = 'deposit_reversal' AND reverses_transaction_id IS NOT NULL`.
- No `tenant_id` column is needed, because the id is a global UUID PK. Drop the "un-tombstoned" and "for a given tenant" wording from INV-PAY-REV-1; it is noise.
- Casino, withdrawal and sportsbook stay lock- or history-enforced. Register a follow-up (e.g. `LEDGER-REV-UNIQ`) for a cross-type index covering all five types. It needs P2-1 fixed first and a read-only duplicate census.
- The plan's §2.2 reasons 1 and 2 are self-refuting or speculative. Replace them with a) to c) above.

**(2) L2 FOR UPDATE: APPROVED. It is a plain ADR 0082 L2 instance and needs no exception.**
- The webhook path holds no L0/L1 before `receiveDepositReversalCallback` (the handler only opens `WithTenant`).
- A single row, so the ascending-id rule is trivially met. It sits before `GetOrCreateAccounts`; both accounts already exist from the original deposit. It sits before `Post`'s L3.
- Order: tombstone branch → **L2 `SELECT transaction_type FROM ledger_transactions WHERE id=$1 FOR UPDATE`** → assert the type is `'deposit'`, else integrity error → amount/asset check → already-reversed `EXISTS` (a new statement, so under READ COMMITTED it takes a fresh snapshot and sees the winner's commit) → accounts → `Post` → audit.
- Keep the `EXISTS` type-agnostic, as casino's is.
- **Required:** amend ADR 0082 (an A5 note in the style of A4). The §1.6 inventory row "deposit reversal … no" and §4.5's "no code change required" become stale once L2 is added.

**(3) Idempotency / F-7 interplay: APPROVED WITH CHANGES.** Each case after the fix:
- **Legitimate retry (same ref, same payload):** L2, then the own-ref exclusion, then `Post` replay returns `AlreadyPosted`. Unchanged and correct.
- **Same ref, different amount/asset:** `ErrCallbackProviderMismatch` before `Post`.
- **Same ref naming a different original:** L2 on the other original, then `Post` returns a `reversal_link` diff, `ErrIdempotencyPayloadMismatch`, then `ErrCallbackPayloadMismatch` (409).
- **Different ref, original already reversed:** `ErrDepositAlreadyReversed`, with no `Post`.
- **Concurrent different refs:** serialize on L2; the loser's post-lock re-check gives `ErrDepositAlreadyReversed`.
- **Concurrent same ref:** serialize on L2; the second goes to `Post` replay, `AlreadyPosted`, with the same transaction id.
- **Concurrent reversals of an unposted original:** unchanged tombstone collapse (F-7 site #21).
- **Backstop behaviour is NOT what the plan (§2.3 / §6.4) says** (see P2-1). It fails closed, but with an opaque error.
- The plan's "optional" denial audit and alert become mandatory (P2-3).

**(4) Migration pre-flight: CONFIRMED, with corrections.**
- The index build reads the heap directly and is not subject to RLS. Under FORCE RLS a `SELECT count(*)` pre-check sees zero rows (the 0078/0091-down precedent), and the owner cannot `SET row_security=off` without an error.
- **Therefore: no SELECT-based pre-check at all.** Do:
  `DO $$ BEGIN CREATE UNIQUE INDEX …; EXCEPTION WHEN unique_violation THEN GET STACKED DIAGNOSTICS d = PG_EXCEPTION_DETAIL; RAISE EXCEPTION 'migration 0092: duplicate deposit_reversal rows exist (PAY-REV-1): %. Do not delete rows. See remediation (item 5).', d; END $$;`
- Non-`CONCURRENTLY` is required: `MigrateUp` wraps every file in one tx (migrate.go:223). The SHARE lock is acceptable at dev volumes.
- Down: `DROP INDEX IF EXISTS`. That is always safe, and the code-level L2 lock remains.
- The refusal text in the plan's §5.2 ("resolve via a compensating entry before re-running") is **wrong** (P1-1).

**(5) Financial impact and remediation: RULED.**
- **Staging at 9190d5d:**
  - It is at migration **0090** and is pre-F-7 (`36616f1` is not an ancestor). It needs 0091 before 0092.
  - It carries the distinct-ref race, but reversals can only arrive via a signed mock-PSP webhook, and no reversal load test is recorded against it. Expected duplicates: **none, but unverified**. Do not touch AWS.
  - The census, when authorized, is RLS-proof and read-only in effect: `BEGIN; CREATE UNIQUE INDEX … ; ROLLBACK;` as the owner. The 23505 DETAIL names one duplicate.
- **Local and dev databases** that ran the deleted W1 probe or the F-7 implementer's 6-way run most likely **do** hold duplicates. 0092 will refuse there, which is correct.
- **Remediation:**
  - a) For synthetic, disposable databases (all of them per CLAUDE.md), recreate the database. That discards the whole synthetic database; no historical ledger row is edited.
  - b) Any database whose history must be kept: STOP and escalate to ledger-finance plus the human. A compensating entry (a four-eyes `manual_adjustment`, Dr `psp_clearing` / Cr `player_cash` per surplus reversal) corrects balances but **does not remove the duplicate row, so the index still cannot be built**. That case needs a separately designed exemption, such as an immutable literal-id predicate. It is not built now.
  - Never delete.
- Negative `player_cash` from a single legitimate reversal is OB-1-adjacent and out of scope. Agreed.

**(6) SB-T1-XMIN: APPROVED WITH CHANGES.**
- `pg_xact_status(...)='in progress'` on a *visible* row means it belongs to the current transaction tree, including released or nested subtransactions. A concurrent uncommitted row is invisible, so it hits the `cause.id IS NULL` branch.
- **The epoch rule in the plan is wrong for the case it exists for (P2-2).**
- No financial effect: validation only, no postings or amounts, and T-2 plus the Go decision table are unchanged.
- The down migration restores 0091's body verbatim. That is safe with any history present: it is BEFORE INSERT only, so existing rows are never re-validated. It only re-narrows acceptance, which fails closed.

**(7) Numbering: APPROVED.**
- **Two migrations: 0092 PAY-REV-1, then 0093 SB-T1-XMIN.** The owners differ, the failure modes differ (0092 can refuse on data, 0093 cannot), and the downs are independent. A 0092 refusal must not block 0093 from being reviewed as a separate unit, and they commute.
- Update all tip-pin tests to the new tip:
  - `migration_0075` / `0077` (jurisdiction)
  - `wave3_phase2_migrations` (bonus)
  - `migration_0076` and `qa_migration_rls_survives_failed_rollback` (operatingmarket)
- These tests MigrateDown through the new downs, which are clean, and re-run 0092 up on the shared test DB (see P2-4).

## Findings

- **P1-1 (plan §5.2 / §7.5, remediation):** "compensate then re-run" cannot succeed, because the compensating entry leaves the duplicate row in place. Fix the refusal message and the plan text as in item 5: synthetic databases are recreated, anything else is STOP-and-escalate.
- **P1-2 (plan §1.3 / §2.2, incomplete inventory):** the withdrawal release postings set `reverses_transaction_id` and are absent from the analysis. Correct the inventory, and add them to the ADR 0082 amendment text and the `LEDGER-REV-UNIQ` follow-up. No code change for withdrawal now; it is L1-protected.
- **P2-1 (`ledger.Post` backstop path):**
  - Behaviour: `db.IdempotentInsert` treats **any** 23505 as an idempotency conflict (idempotency.go:39). A new-index violation then runs `lookupByIdempotencyKey`, which finds no row for the loser's own key.
  - Result: `ledger: look up existing transaction…: no rows`, then HTTP 500. It fails closed (nothing written, no false success; the dual-violation case resolves to a correct replay), but the plan's §2.3 / §6.4 description is wrong.
  - Required: in the 0092 change, make `Post` route by `PgError.ConstraintName`. The idempotency-key index goes to the replay path, the provider-tx index keeps the existing pinned untyped error, and the new index maps to a new `ledger.ErrReversalAlreadyExists`. Payments maps that to `ErrDepositAlreadyReversed` (409). Add a direct-SQL/Post test that bypasses L2.
  - This is a ledger change; ledger-finance sign-off is pre-granted for exactly this scope.
- **P2-2 (xid8 construction):**
  - The problem: subxids are always numerically **greater** than the top-level xid. The plan's rule ("xmin > low32(pg_current_xact_id()) ⇒ epoch−1") therefore mislabels every own-tree subxid, so the savepoint case still fails.
  - Required: reconstruct relative to the next xid.
    `nxt := pg_snapshot_xmax(pg_current_snapshot())::text::bigint; raw := xmin::text::bigint;`
    `full := (nxt & ~4294967295) | raw; IF raw > (nxt & 4294967295) THEN full := full - 4294967296;`
  - Why next xid: it never yields a future xid, so `pg_xact_status` never raises.
  - Accept only if `full >= pg_current_xact_id()::text::bigint AND pg_xact_status(full::text::xid8) IS NOT DISTINCT FROM 'in progress'`. NULL (xid too old) means reject.
  - Tests: plain insert, released savepoint, nested savepoint, `ROLLBACK TO`, and an earlier committed transaction. The `_SavepointRollbackIsAccepted` test is the one that catches this bug.
- **P2-3 (audit and alert):** make the `deposit.reversal_rejected_already_reversed` audit (OutcomeDenied, recorded in the handler's tx before returning the error, or via a separate audit write if the tx rolls back; confirm which) and the named alert mandatory.
  - Why: a second distinct PSP reversal can mean real money moved twice at the PSP (refund plus chargeback). The ledger rightly refuses it, but ops and daily PSP reconciliation must see it.
  - Also add the 409 mapping in `deposit_handlers.go`.
- **P2-4 (tests):** the "0092 refuses on duplicates" test must not leave duplicates in the shared test DB, or every later tip-pin MigrateUp will fail. Run it in one tx: drop the index, seed two rows, exec the 0092 up text, assert the message, then ROLLBACK. Or use a scratch DB. Also required: the regression test shown failing on the pre-fix code, with blocker-PID-scoped polling on the **L2** row.
- **P2-5 (ADR 0082):** add the A5 inventory amendment (item 2). It is not an exception.
- **P3-1:** optional `CHECK (transaction_type <> 'deposit_reversal' OR reverses_transaction_id IS NOT NULL)`. It closes the "omit the link to bypass the index" hole. Recommended for 0092 if the reviewer agrees; otherwise add it to the follow-up.
- **P3-2:** the re-check relies on READ COMMITTED (the `WithTenant` default, tenant_rls.go:35). State this in a code comment. The backstop covers any future RR/SERIALIZABLE caller.
- **P3-3:** xid alias after at least 2^32 xids. An old frozen rollback row of the *same bet* could alias to an own-tree xid. Optional belt-and-braces: add `cause.created_at = now()`, since `now()` is transaction-start time and is equal for subtransactions, provided `created_at` is never caller-supplied. Otherwise record it as accepted.
- **P3-4 (pre-existing, out of scope):** a reversal ref equal to a deposit ref gives `ErrIdempotencyKeyReused` → 500. Note it only.

## Required changes to the plan (summary)

1. Index scope: deposit_reversal only. Fix the invariant wording. Add the withdrawal writers to the inventory. Register `LEDGER-REV-UNIQ`.
2. Refusal: a DO/EXCEPTION block around `CREATE UNIQUE INDEX` with the DETAIL. No SELECT pre-check. Corrected remediation text.
3. `ledger.Post` constraint-name routing plus the `ErrReversalAlreadyExists` sentinel and its payments mapping.
4. L2 order as in item 2, with the type assertion. Add the ADR 0082 A5 note.
5. Mandatory denial audit, alert and 409.
6. XMIN: the next-xid-relative xid8 plus the `>= top` conjunct plus NULL-means-reject. Down = 0091 body verbatim (test via `pg_get_functiondef` diff).
7. Two migrations, 0092 then 0093. Update the tip-pin tests. The refusal test must be rollback-isolated.
8. A human decision is needed only for stage authorization, and for any future duplicate found in a database whose history must be kept.
