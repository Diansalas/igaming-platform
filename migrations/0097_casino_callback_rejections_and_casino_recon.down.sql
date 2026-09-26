-- Reverses migration 0097 ONLY on a database where no casino
-- reconciliation evidence exists (the migration 0091 down technique).
-- Every check uses constraint validation, which FORCE ROW LEVEL SECURITY
-- cannot filter (a count(*) here would see zero tenant rows and wrongly
-- proceed). All checks run before anything is dropped. After the first
-- casino_callback_rejections row or casino_consistency mismatch exists,
-- roll forward instead.

-- Check 1: no casino_consistency mismatch has been recorded (restores
-- migration 0091's list).
ALTER TABLE reconciliation_mismatches DROP CONSTRAINT reconciliation_mismatches_mismatch_kind_check;
DO $$
BEGIN
    ALTER TABLE reconciliation_mismatches ADD CONSTRAINT reconciliation_mismatches_mismatch_kind_check
        CHECK (mismatch_kind IN ('missing_projection', 'balance_mismatch',
            'sb_locked_mismatch', 'sb_bet_net_mismatch', 'sb_orphan_ledger',
            'sb_orphan_history', 'sb_status_mismatch', 'sb_mock_statement_mismatch'));
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0097 (down): casino reconciliation evidence exists (a casino_consistency mismatch has been recorded). This migration is irreversible once any casino reconciliation evidence exists; roll forward';
END $$;

-- Check 2: the rejection record is empty.
DO $$
BEGIN
    ALTER TABLE casino_callback_rejections ADD CONSTRAINT tmp_0097_empty CHECK (false);
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0097 (down): casino reconciliation evidence exists (casino_callback_rejections is not empty). This migration is irreversible once any casino reconciliation evidence exists; roll forward';
END $$;

-- Only now drop.
DROP TABLE casino_callback_rejections;
