-- Stage 10.3 W3a, CAS-RECON-STMT-1 (ADR 0092; docs/plans/stage-10.3-
-- planning/02-casino-financial-analysis.md §2.5, §2.8).
--
-- Widens reconciliation_mismatches.mismatch_kind with the casino_statement
-- stream's single kind, cas_mock_statement_mismatch: a divergence between
-- the ledger and the casino statement source, which today is only the
-- in-house MOCK source (real provider statement matching is PROVIDER
-- DEPENDENT; the first real source adds its own kind).
--
-- Purely additive: a strict superset of migration 0097's list. Constraint
-- validation runs anyway and cannot be blinded by FORCE ROW LEVEL
-- SECURITY. No pre-flight is needed (existing rows hold only older kinds).
-- No other table is touched: reconciliation_runs.stream is free TEXT, and
-- the stream only ever inserts run/mismatch rows.
ALTER TABLE reconciliation_mismatches DROP CONSTRAINT reconciliation_mismatches_mismatch_kind_check;
ALTER TABLE reconciliation_mismatches ADD CONSTRAINT reconciliation_mismatches_mismatch_kind_check
    CHECK (mismatch_kind IN ('missing_projection', 'balance_mismatch',
        'sb_locked_mismatch', 'sb_bet_net_mismatch', 'sb_orphan_ledger',
        'sb_orphan_history', 'sb_status_mismatch', 'sb_mock_statement_mismatch',
        'cas_round_binding_mismatch', 'cas_posting_shape_mismatch', 'cas_orphan_win',
        'cas_rollback_linkage_mismatch', 'cas_tombstone_conflict',
        'cas_unposted_provider_event', 'cas_tombstone_late_original',
        'cas_mock_statement_mismatch'));
