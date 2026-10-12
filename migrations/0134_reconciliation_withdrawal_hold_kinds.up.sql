-- ADR 0113 SLICE 3 (M-C): the two reconciliation mismatch kinds of check D (ADR 0113 s4.4), the
-- standing detective control of invariant #12. FOR `ledger-finance`, `qa`, `code-reviewer` REVIEW.
--
-- NUMBERING: 0134 (gap-free after 0133; no file, branch or doc reference used 0134 at authoring time).
-- Migration numbers are allocated by the orchestrator; confirm before merge.
--
-- What it does, and only this: reconciliation_mismatches_mismatch_kind_check becomes 0115's list plus
--   - withdrawal_hold_open_mismatch: a wallet's player_withdrawal_hold balance REBUILT FROM
--     ledger_entries differs from the sum of its non-terminal withdrawal requests (invariant #12);
--   - withdrawal_request_ledger_link_invalid: a request fails check A's link rules (ADR 0113 s4.2:
--     WD030-WD034 shapes), read-only.
-- Both are P1 (internal/reconciliation, the ledger_vs_projection run; raised under their own
-- discriminator of the drift alert Kind). The constraint NAME is kept (the 0097/0098 tests match it).
-- No data is touched. The down fails loud (WD099) if rows of the new kinds exist: they are never
-- deleted to make a down pass.

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
        'withdrawal_hold_open_mismatch', 'withdrawal_request_ledger_link_invalid'
    ));
