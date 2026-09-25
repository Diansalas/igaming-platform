> Stage 10.1 planning gate — specialist working paper (verbatim, recorded 2026-09-25 against 56f5135). Where it differs from the Orchestrator rulings in `docs/plans/stage-10.1-planning-gate-proposal.md` §O, the rulings govern.

# Stage 10.1 planning gate — architect ruling (PAY-REV-1, SB-T1-XMIN)
Repo 56f5135. Review only; no repo file changed. Inputs: CLAUDE.md, ADR 0020 (+F-7), 0082 (+A1–A4), 0087, 0088 §3.3,
07-payments-architecture.md, financial-transaction-flows.md Flow 2, p101-payrev.md, p101-xmin.md, code at HEAD.

## (1) PAY-REV-1 lock: VERDICT — a plain L2 instance. No new exception (E-5) is needed. ADR 0082 Amendment A5 is needed for the inventory only.
- Verified at HEAD: the reversal path takes no L0 (no advisory) and no L1 lock. `loadDepositIntentByProviderRef` is an unlocked read,
  and `deposit_intents` is not mutated by a reversal. The only lock is L3/L4 inside `Post`. Adding one L2 row lock before
  `GetOrCreateAccounts` → L3 → L4 matches the §2.1 order, R8 and the `casino.postRollback` precedent.
- Locking `deposit_intents` as L1 is REJECTED as the serialization point. The object being reversed is the ledger transaction.
  `ledger_transaction_id` is write-once, and an L1 lock would add a domain lock that no other payments path takes.
- Amendment A5 is still REQUIRED, because ADR 0082 is wrong after the fix: the §1.6 "deposit reversal" row says "implicit
  projections only", and §4.5 says "No code change required … neither path takes any lock outside Post".
  A5 = (1) update the §1.6 row; (2) add a pointer note in §4.5; (3) state "no rule/class change; no E-5". R1–R8 are unchanged.
- NORMATIVE SEQUENCE (to go into ADR 0090 §contract verbatim):
  S0 `WithTenant(tenant from URL slug)`; `HandleCallback` signature check (no locks).
  S1 `loadDepositIntentByProviderRef` (unlocked). If not found or `ledger_transaction_id IS NULL` → tombstone branch, unchanged, no L2.
  S2 **L2**: `SELECT transaction_type FROM ledger_transactions WHERE id=$orig FOR UPDATE`. Use FOR UPDATE, the ADR 0082 L2
     wording and the casino precedent. Do not use NO KEY UPDATE. The lock is on one row, so the ascending-id rule is trivial.
     Zero rows, or type ≠ 'deposit' → fail closed with an integrity error. No Post, no 500-as-success.
  S3 Amount/asset validation (now under the lock).
  S4 The already-reversed `EXISTS` check as a NEW statement after S2. READ COMMITTED gives it a fresh snapshot, so the winner's commit
     is visible. Keep the F-7 own-reference exclusion. If true → `ErrDepositAlreadyReversed`, before any account resolution or Post.
  S5 `GetOrCreateAccounts` (before L3, per §2.1 note). S6 `Post` (L3 → L4). S7 `deposit.reversed` audit.
- Loser outcomes: payrev §3.3/§4 are CONFIRMED. Distinct ref → `ErrDepositAlreadyReversed` → HTTP 409 with alert log
  `payment_webhook_integrity_alert_already_reversed` (REQUIRED; today the handler gives 500). Same ref → idempotent replay (F-7).
- REQUIRED CORRECTION to payrev §6.1 (denial audit). `WithTenant` rolls back on any returned error, so an `audit.Record` followed by
  `return Err…` is silently discarded. Ruling: a persisted denial audit is NOT required in 10.1. Nothing was mutated, and this
  matches the F-7 `ErrCallbackPayloadMismatch` treatment (alert log only). If `payments` wants one anyway, it must use the
  commit-with-result pattern (a non-error result flag that the handler maps to 409), never audit-then-error. Otherwise defer it.
- Test note for qa: before the fix, a blocker holding the original row FOR UPDATE ALREADY blocks the reversal, at the L4 FK check
  (reverses_transaction_id FK takes KEY SHARE). "Blocked on blocker PID" alone therefore passes pre-fix. The L2 test must also
  assert that the waiting statement is the S2 `SELECT … FOR UPDATE` (`pg_stat_activity.query`) and that no L3 lock is held while waiting.

## (2) Schema invariant: VERDICT — yes, it belongs in the schema, with a type-scoped partial unique index. It is recorded by ADR, not only in docs.
- Index (migration 0092): `CREATE UNIQUE INDEX ux_ledger_transactions_deposit_reversal_once ON ledger_transactions
  (tenant_id, reverses_transaction_id) WHERE transaction_type='deposit_reversal' AND reverses_transaction_id IS NOT NULL;`
  Lead with `tenant_id`. The `reverses_transaction_id` FK is to `id` only, not `(id, tenant_id)`, so a tenant-less key would make
  cross-tenant collision an existence oracle (the 0021 header rationale). Per-type scoping is CONFIRMED as correct: at HEAD,
  withdrawal_rejected and withdrawal_failed both reference the hold tx, and sportsbook rollback generations exist, so a global
  index is wrong.
- OPTIONAL (ledger-finance decides): `CHECK (transaction_type <> 'deposit_reversal' OR reverses_transaction_id IS NOT NULL)`.
  Without it, a NULL-reverses deposit_reversal escapes the index. Put it in the same migration only if its pre-check passes.
- REQUIRED CORRECTION to payrev §2.3/§5.2:
  (a) `db.IdempotentInsert` treats ANY 23505 as an idempotent conflict. A lock-bypassing writer that hits the new index is
      therefore NOT a bare pgconn error. `Post` takes the replay path and `lookupByIdempotencyKey` returns ErrNoRows. This fails
      closed but gives a misleading error. Plan: `Post` discriminates by `PgError.ConstraintName`, mapping the new index to a typed
      sentinel (e.g. `ledger.ErrTransactionAlreadyReversed`). This is owned by ledger-finance. Test #6 must also run through
      `Post`, not only raw SQL.
  (b) Do NOT use `SET row_security=off`. For a NOBYPASSRLS owner it raises an error rather than bypassing RLS. Follow the repo
      precedent (0091 down / 0078 technique): constraint validation is not filtered by RLS. Wrap `CREATE UNIQUE INDEX` in
      `DO $$ … EXCEPTION WHEN unique_violation THEN RAISE EXCEPTION 'migration 0092: duplicate deposit_reversal … PAY-REV-1 …
      resolve by reviewed compensating entry; never delete' $$`. Run it non-CONCURRENTLY inside the migration transaction.
      The down migration is `DROP INDEX` (plus the CHECK if one was added).
- Record set (minimal):
  - NEW **ADR 0090 "Stage 10.1 definition: PAY-REV-1 remediation + SB-T1-XMIN"**, following the 0087 precedent. Status PROPOSED
    until the human authorizes it. For a stage this small, the stage definition and a short implementation contract share one ADR;
    no separate contract ADR is needed. The contract contains: INV-PAY-REV-1 (a deposit is reversed at most once per tenant,
    enforced by L2 lock + DB index), the S0–S7 sequence, 409 mapping, migration 0092/0093 specs, tests, out-of-scope list,
    gates, and reviewers (ledger-finance, architect, security for the webhook status change, qa, code-reviewer).
  - **ADR 0082 Amendment A5**: inventory and pointer only (see (1)).
  - **ADR 0020 amendment (Stage 10.1)**: REQUIRED. The "Callback races … resolved by the same idempotency key … not a separate
    mechanism" bullet is proven false for SEMANTIC duplicates (distinct provider refs for one economic fact).
    New doctrine: key idempotency dedupes deliveries. Any "at most one X per original" rule needs an L2 lock on the original
    before the decision, plus a type-scoped DB unique index. Check-then-insert without both is forbidden.
  - **ADR 0088 follow-up note** (text in (3)).
  - Docs: financial-transaction-flows.md Flow 2 "Failure behavior" gets the one-reversal-per-original rule, the 409 and a
    pointer to ADR 0090. Its OPEN DECISION on negative player_cash is untouched and stays OPEN. 07-payments-architecture.md gets
    a one-line pointer. task-registry, active-stage and progress are updated at the gate.

## (3) SB-T1-XMIN: VERDICT — `pg_xact_status` is sound. Adopt it as a function-body-only swap in migration 0093, with three required guards.
- Soundness: a VISIBLE row whose xmin reports 'in progress' can only belong to the checker's own xact tree. Rows from concurrent
  in-progress transactions are invisible to T-1's SELECT, so they hit `cause.id IS NULL` and are rejected. Earlier-committed rows
  report 'committed' and are rejected. T-1 is a BEFORE trigger, so it runs before the FK (AFTER) check. The ROLLBACK-TO-SAVEPOINT
  test must therefore assert T-1's not-found message, not an FK violation.
- G1 NULL-safety (a fail-open hazard). `pg_xact_status` returns NULL for xids older than clog truncation (old frozen rows keep their
  raw xmin). `IF … OR status <> 'in progress'` evaluates NULL as not-true, which would ACCEPT the row. The check MUST be
  `IS DISTINCT FROM 'in progress'`. Add a unit/SQL test that forces NULL, e.g. by calling a helper with a fabricated old xid8.
- G2 epoch reference. Derive the epoch from `pg_snapshot_xmax(pg_current_snapshot())`, which exceeds every xid assigned in the tree
  including subxids, not from `pg_current_xact_id()`. Use `epoch-1` when `xmin_low >= ref_low`. The top-level xid can precede an
  epoch rollover that a later subxid crosses, and the current wording in 0088 would then build a wrong xid8.
  A negative or "future" xid8 must fail closed: catch the error and raise the T-1 message.
- G3 immutability assumption: xmin is stable only because history rows are never UPDATEd (0091 T-2 / immutability). State this
  dependency in the function comment.
- Alternatives. (b) enumerating subxids: no SQL API, rejected. (c) GUC marker: caller-assertable, rejected. (d) ledger causation
  link: necessary but not sufficient, rejected as primary. (e) Stamped column `origin_xact xid8`, set by a BEFORE INSERT trigger
  from `pg_current_xact_id()` (which returns the top-level xid even inside a subxact): structurally sound, with no epoch arithmetic
  and no NULL path. But it alters 0091's table and adds a trigger, more than a body swap. It is the recorded FALLBACK if
  ledger-finance or qa cannot satisfy G1/G2 cleanly.
- Down 0093 restores 0091's exact body. Safe with evidence present (a pure function swap; the old rule accepts every existing row).
- ADR 0088 amendment text (append after the 2026-09-25 note; do not edit it):
  > **Follow-up note (Stage 10.1, ADR 0090) — `SB-T1-XMIN` RESOLVED by migration 0093.** T-1's composed-void causation check now
  > requires `pg_xact_status(<cause xmin as xid8, epoch from pg_snapshot_xmax(pg_current_snapshot())>) IS NOT DISTINCT FROM
  > 'in progress'` in place of xmin = top-level xid. Any xid in the current transaction's tree (top-level or released savepoint,
  > however nested) is accepted. A committed, aborted, unknown (NULL) or unconstructible xid is rejected (fail closed). The
  > precondition note above no longer constrains callers. Migration 0091 is unchanged. Bet match and kind=rollback are unchanged.
  > Tests: SavepointRollbackIsAccepted, nested-savepoint accepted, rolled-back-savepoint rejected (not-found branch),
  > earlier-transaction rejected, NULL-status rejected.

## (4) Cross-domain extension: VERDICT — do not extend in 10.1. Record as deferred, with a binding rule for future posters.
- casino_rollback: already L2-locked (the precedent). Adding a ledger index would change casino `Post` error semantics (see 2a),
  pull in the casino specialist, and run a data pre-flight on casino rows. That is scope creep. Defer as **REV-UNIQ-CASINO** (P3).
- sportsbook_rollback: L1 + L2, plus the 0091 history partial-unique indexes. No action.
- withdrawal_reversed / bonus_reversal: no posters exist (NOT IMPLEMENTED). No action now. The ADR 0020 amendment binds them:
  the migration and stage that introduce the first poster MUST add the L2 lock and a type-scoped unique index.
- withdrawal_rejected/failed → hold tx: guarded by the `withdrawal_requests` L1 state machine. Noted in ADR 0090 only, no change.
- Pre-existing, out of scope: the tombstone-vs-late-success collision also goes through the any-23505 misclassification. Log it
  as a deferred item; if 2a lands, it may resolve this for free, but do not expand the stage to make it do so.

## (5) Numbering: CONFIRMED
- ADRs: 0089 is taken (AI-agent boundary, in flight), so the next is **ADR 0090**. Add a numbering note to ADR 0090, as 0087 did:
  migration `0090_sb_jurisdiction_restrictions_require_platform_principal` is an unrelated file.
- Migrations: **0092_deposit_reversal_uniqueness** (P1 first), then **0093_sportsbook_t1_xact_status**. The two are independent and commute.
- Amendment labels: ADR 0082 "Amendment A5"; ADR 0020 "Amendment 2026-xx-xx — Stage 10.1 semantic-duplicate reversals";
  ADR 0088 "Follow-up note (Stage 10.1)".

## Required plan changes (summary)
1. Add the S2 type/existence assertion. Move amount/asset validation and the EXISTS check after S2.
2. Drop audit-then-error; add the 409 handler branch and the alert log.
3. Add the `tenant_id` leading column to the index. Pre-flight uses the unique_violation DO-block, NOT `row_security=off`.
4. Add `Post` constraint-name discrimination with a typed sentinel (ledger-finance), and test it through `Post`.
5. Strengthen the L2 test beyond blocker-PID (assert the waiting statement is S2).
6. SB-T1-XMIN guards G1–G3, with NULL and epoch tests.
7. Records: ADR 0090 (PROPOSED), 0082 A5, 0020 amendment, 0088 follow-up, Flow 2 doc. The casino index is deferred.
Human decision: only the Stage 10.1 authorization (approval of ADR 0090). Negative-cash OPEN DECISION and OB-1 remain OPEN and untouched.
