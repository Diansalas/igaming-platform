-- Reverses migration 0098 ONLY on a database where no casino_statement
-- evidence exists (the migration 0091/0097 down technique). The check uses
-- constraint validation, which FORCE ROW LEVEL SECURITY cannot filter (a
-- count(*) here would see zero tenant rows and wrongly proceed). After the
-- first cas_mock_statement_mismatch row exists, roll forward instead.
ALTER TABLE reconciliation_mismatches DROP CONSTRAINT reconciliation_mismatches_mismatch_kind_check;
DO $$
BEGIN
    ALTER TABLE reconciliation_mismatches ADD CONSTRAINT reconciliation_mismatches_mismatch_kind_check
        CHECK (mismatch_kind IN ('missing_projection', 'balance_mismatch',
            'sb_locked_mismatch', 'sb_bet_net_mismatch', 'sb_orphan_ledger',
            'sb_orphan_history', 'sb_status_mismatch', 'sb_mock_statement_mismatch',
            'cas_round_binding_mismatch', 'cas_posting_shape_mismatch', 'cas_orphan_win',
            'cas_rollback_linkage_mismatch', 'cas_tombstone_conflict',
            'cas_unposted_provider_event', 'cas_tombstone_late_original'));
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0098 (down): casino_statement reconciliation evidence exists (a cas_mock_statement_mismatch has been recorded). This migration is irreversible once any casino_statement evidence exists; roll forward';
END $$;
