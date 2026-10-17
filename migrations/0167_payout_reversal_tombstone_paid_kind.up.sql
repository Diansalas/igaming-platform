-- F3 (platform-freeze work list) / A5 R9 / LF-A5-1 (b): the standing payment_statement finding for a payout whose
-- withdrawal the A5 reversal tombstone branch failed ("the PSP returned it", hold released) while a statement line
-- says the PSP PAID it (status succeeded). FOR `ledger-finance`, `security` REVIEW.
--
-- NUMBERING: 0167 (allocated to this work item by the orchestrator; 0159 and 0161 are reserved, 0166 is F1's).
--
-- What it does, and only this: reconciliation_mismatches_mismatch_kind_check becomes 0134's list plus
--   - pay_payout_reversal_tombstone_but_paid: a payout attempt whose withdrawal request carries the A5 tombstone
--     (a ledger tombstone correlated to the request on the provider's original reference) and for which ANY
--     persisted statement import holds a succeeded payout line. Reported by the payment_statement stream on every
--     run (unwindowed) until it clears: on PSP return evidence (an eligible `reversed` payout line on the original
--     reference, same amount and asset, no older than the newest succeeded line) or on a K2 net recovery of the
--     withdrawn amount (compensating_entry debits with causation = the request's withdrawal_failed transaction,
--     the M2 (d) rule). P1, internal/reconciliation.
-- The constraint NAME is kept (the 0097/0098/0134 tests match it). No data is touched. The down fails loud (WD099)
-- while a row of the new kind exists: findings are never deleted to make a down pass.

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
        'pay_payout_reversal_tombstone_but_paid'
    ));
