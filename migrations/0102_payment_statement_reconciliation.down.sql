-- Reverses migration 0102 ONLY on a database where no payment_statement
-- evidence exists: no stored statement import and no pay_* mismatch (the
-- migration 0091/0097/0098 down technique). Both checks use constraint
-- validation, which FORCE ROW LEVEL SECURITY cannot filter (a count(*) here
-- would see zero tenant rows and wrongly proceed). Once any statement has
-- been stored or any pay_* mismatch recorded, roll forward instead.
DO $$
BEGIN
    ALTER TABLE payment_statement_imports ADD CONSTRAINT payment_statement_imports_down_guard CHECK (false);
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0102 (down): payment statement imports exist. This migration is irreversible once any payment statement has been stored; roll forward';
END $$;

ALTER TABLE reconciliation_mismatches DROP CONSTRAINT reconciliation_mismatches_mismatch_kind_check;
DO $$
BEGIN
    ALTER TABLE reconciliation_mismatches ADD CONSTRAINT reconciliation_mismatches_mismatch_kind_check
        CHECK (mismatch_kind IN ('missing_projection', 'balance_mismatch',
            'sb_locked_mismatch', 'sb_bet_net_mismatch', 'sb_orphan_ledger',
            'sb_orphan_history', 'sb_status_mismatch', 'sb_mock_statement_mismatch',
            'cas_round_binding_mismatch', 'cas_posting_shape_mismatch', 'cas_orphan_win',
            'cas_rollback_linkage_mismatch', 'cas_tombstone_conflict',
            'cas_unposted_provider_event', 'cas_tombstone_late_original',
            'cas_mock_statement_mismatch'));
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0102 (down): payment_statement reconciliation evidence exists (a pay_* mismatch has been recorded). This migration is irreversible once any payment_statement evidence exists; roll forward';
END $$;

DROP TABLE payment_statement_lines;
DROP TABLE payment_statement_imports;
DROP FUNCTION payment_statement_lines_count_cap();
DROP FUNCTION payment_statement_imports_count_exact();
