-- Stage 10.1 PAY-REV-1 (ADR 0090; docs/plans/stage-10.1-planning-gate-
-- proposal.md §D-§F, rulings §O R-3/R-4). Root cause: internal/payments'
-- deposit-reversal callback path used to check "already reversed?" BEFORE
-- taking any lock, so two distinct-reference reversal callbacks for the
-- SAME original deposit could both observe "not yet reversed" and both
-- post, over-debiting player_cash. The primary fix is an ADR 0082
-- class-L2 `FOR UPDATE` lock plus a post-lock re-check in
-- internal/payments/orchestrator.go (no schema change). This migration
-- adds the BACKSTOP: a database-enforced invariant (INV-PAY-REV-1) so a
-- future writer that skips that lock still cannot post two
-- deposit_reversal transactions against one original deposit.
--
-- Scope, deliberately narrow (§F):
--   - Per transaction_type ('deposit_reversal' only), tenant-leading.
--     NOT a global "one reversal per anything" rule: withdrawal_rejected
--     and withdrawal_failed both legitimately reference the same
--     withdrawal hold transaction, and a global index would also block
--     this migration on unrelated legacy data (e.g. pre-lock-fix casino
--     rollbacks in development databases). A cross-type rule is deferred
--     as LEDGER-REV-UNIQ (§U).
--   - tenant_id leads the index (security review S-1): reverses_
--     transaction_id's foreign key is a single, non-tenant-scoped column
--     that does not respect RLS, so without tenant_id a buggy writer
--     naming another tenant's transaction id would collide with it, and
--     the resulting unique-violation error would itself reveal that the
--     other tenant's row exists (an existence oracle). The single-column
--     FK is recorded as deferred, not fixed here.
--
-- Refusal, not a pre-check (ruling R-4): the index build ITSELF is the
-- check, inside a DO/EXCEPTION block, exactly like the 0091 down-
-- migration's own technique. Deliberately NOT a `SELECT count(*) ...`
-- pre-check and NOT `SET row_security = off`: ledger_transactions carries
-- FORCE ROW LEVEL SECURITY (migration 0021), so a SELECT run as a role
-- without the RLS-bypass attribute would silently see zero rows and let
-- a database with real duplicates through, while `SET row_security = off`
-- raises its own error for such a role instead of failing the way this
-- migration means to. Building the unique index itself scans every row
-- unconditionally, regardless of RLS, so it cannot be fooled this way.
--
-- Also deliberately NOT `CREATE UNIQUE INDEX CONCURRENTLY`: this
-- codebase's migration runner (internal/db.Pool.MigrateUp) applies every
-- migration file inside one transaction, and CONCURRENTLY cannot run
-- inside a transaction block. A production rollout plan for this index
-- at scale (it takes a SHARE lock on ledger_transactions for the
-- duration of the build) is a launch-stage concern, not this migration's.
--
-- If this refuses: STOP. Per CLAUDE.md, ledger rows are never deleted and
-- a duplicate deposit_reversal cannot be cleared by a compensating entry
-- (the duplicate row itself would remain either way) - escalate to the
-- human (Stage 10.1 plan §L, ruling R-5). Synthetic local/dev databases
-- that ran earlier PAY-REV-1 probes are the expected place this refuses;
-- recreate those databases rather than trying to resolve the refusal in
-- place.
DO $$
BEGIN
    CREATE UNIQUE INDEX ledger_transactions_one_deposit_reversal
        ON ledger_transactions (tenant_id, reverses_transaction_id)
        WHERE transaction_type = 'deposit_reversal';
EXCEPTION WHEN unique_violation THEN
    RAISE EXCEPTION 'migration 0092: duplicate deposit reversals exist for at least one (tenant_id, reverses_transaction_id) pair; this migration cannot run until they are escalated and resolved - see Stage 10.1 plan (docs/plans/stage-10.1-planning-gate-proposal.md) §L; never delete ledger rows';
END $$;
