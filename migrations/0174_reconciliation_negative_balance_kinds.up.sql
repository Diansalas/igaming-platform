-- C3 (closure ledger-finance review) / F4-R2-NEGBAL-SQL, detection half: the standing ledger_vs_projection findings for
-- a negative player wallet and for a payout dispatched while its wallet was negative. FOR `ledger-finance` REVIEW.
--
-- NUMBERING: 0174 (allocated to this work item by the orchestrator; 0173 belongs to the bonus work item).
--
-- What it does, and only this: reconciliation_mismatches_mismatch_kind_check becomes 0167's list (a strict superset,
-- byte for byte) plus
--   - player_wallet_negative_balance: a player_cash / player_locked_cash account whose balance is below zero
--     (credit_total < debit_total, the exact predicate of the WD050 gate, owner decision 13), rebuilt from
--     ledger_entries or read from wallet_balance_projection. Keyed by the ledger account id.
--   - payout_dispatched_while_wallet_negative: a submitted / completed withdrawal on a wallet that is negative now and
--     already was at the request's dispatch instant. Keyed by the withdrawal request id.
-- Both are written only by internal/reconciliation (negative_balance.go), read-only detection, P1 via the existing
-- reconciliation.ledger_projection_drift Kind (no alert_kinds change). No ledger, projection or business table is
-- touched; no data is touched. The constraint NAME is kept (the 0097/0098/0134/0167 tests match it). The down fails
-- loud (WD099) while a row of a new kind exists: findings are never deleted to make a down pass.

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
        'pay_declared_paid_unconfirmed', 'pay_declared_not_paid_but_paid', 'pay_declared_paid_compensated_but_paid',
        'withdrawal_hold_open_mismatch', 'withdrawal_request_ledger_link_invalid',
        'pay_payout_reversal_tombstone_but_paid',
        'player_wallet_negative_balance', 'payout_dispatched_while_wallet_negative'
    ));
