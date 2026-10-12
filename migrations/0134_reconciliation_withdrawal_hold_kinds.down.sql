-- Reverses 0134 (ADR 0113 slice 3; DEV/STAGING ONLY, per the migrate-down policy).
--
-- Fails loud (WD099) while a mismatch row of either new kind exists: findings are never deleted to
-- make a down pass. reconciliation_mismatches is FORCE RLS and this session carries no tenant, so the
-- check lifts FORCE for the owner inside this migration transaction and restores it (the 0115 down
-- pattern). Otherwise the 0115 CHECK is restored byte for byte (whole-schema snapshot test).
--
-- Hand-applied with psql: run it as ONE transaction (`psql -1 -v ON_ERROR_STOP=1 -f ...`). Statement by
-- statement in autocommit, a refusal after the NO FORCE step would leave reconciliation_mismatches
-- without FORCE ROW LEVEL SECURITY. The migrate runner already applies each file in one transaction.

ALTER TABLE reconciliation_mismatches NO FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reconciliation_mismatches
                WHERE mismatch_kind IN ('withdrawal_hold_open_mismatch', 'withdrawal_request_ledger_link_invalid')) THEN
        RAISE EXCEPTION '0134 down refused: withdrawal hold reconciliation mismatches exist' USING ERRCODE = 'WD099';
    END IF;
END $$;
ALTER TABLE reconciliation_mismatches FORCE ROW LEVEL SECURITY;

ALTER TABLE reconciliation_mismatches DROP CONSTRAINT reconciliation_mismatches_mismatch_kind_check;
ALTER TABLE reconciliation_mismatches ADD CONSTRAINT reconciliation_mismatches_mismatch_kind_check
    CHECK (mismatch_kind IN (
        'missing_projection', 'balance_mismatch',
        'sb_locked_mismatch', 'sb_bet_net_mismatch', 'sb_orphan_ledger', 'sb_orphan_history',
        'sb_status_mismatch', 'sb_mock_statement_mismatch',
        'cas_round_binding_mismatch', 'cas_posting_shape_mismatch', 'cas_orphan_win',
        'cas_rollback_linkage_mismatch', 'cas_tombstone_conflict', 'cas_unposted_provider_event',
        'cas_tombstone_late_original', 'cas_mock_statement_mismatch',
        'pay_missing_platform_record', 'pay_missing_provider_record', 'pay_amount_mismatch',
        'pay_asset_mismatch', 'pay_reference_mismatch', 'pay_status_mismatch', 'pay_duplicate',
        'pay_unresolved', 'pay_captured_unposted',
        'ledger_unlinked_manual_adjustment',
        'pay_declared_paid_unconfirmed', 'pay_declared_not_paid_but_paid', 'pay_declared_paid_compensated_but_paid'
    ));
