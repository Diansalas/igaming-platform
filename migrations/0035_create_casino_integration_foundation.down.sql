DROP TABLE IF EXISTS casino_launch_sessions;
DROP TABLE IF EXISTS casino_provider_capabilities;
DROP TABLE IF EXISTS casino_game_availability;
DROP TABLE IF EXISTS casino_games;

-- NOTE (architect specialist review, Stage 4A): this down migration is
-- fully reversible on a database that has never posted a casino_bet/
-- casino_win/casino_rollback row. Once any such row exists, the ADD
-- CONSTRAINT below re-validates every existing ledger_transactions row
-- and fails with SQLSTATE 23514 - and the rows cannot be deleted first
-- either, since ledger_transactions is append-only
-- (ledger_deny_mutation(), migration 0021). This is the correct,
-- deliberate behavior for an append-only financial ledger (CLAUDE.md:
-- "Corrections are compensating entries, never edits or deletions of
-- historical entries"), not a bug in this script - migration 0035 is
-- therefore effectively irreversible in practice on any environment
-- where the casino integration has actually processed a transaction.
-- MigrateDown wraps this in one transaction, so a failed attempt here
-- leaves the schema fully intact (verified by specialist review).
ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_transaction_type_check;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_transaction_type_check CHECK (transaction_type IN (
    'deposit', 'deposit_reversal',
    'withdrawal_requested', 'withdrawal_completed', 'withdrawal_rejected',
    'withdrawal_failed', 'withdrawal_reversed',
    'manual_adjustment', 'tombstone'
));
