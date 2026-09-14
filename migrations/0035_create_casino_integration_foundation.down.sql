DROP TABLE IF EXISTS casino_launch_sessions;
DROP TABLE IF EXISTS casino_provider_capabilities;
DROP TABLE IF EXISTS casino_game_availability;
DROP TABLE IF EXISTS casino_games;

ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_transaction_type_check;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_transaction_type_check CHECK (transaction_type IN (
    'deposit', 'deposit_reversal',
    'withdrawal_requested', 'withdrawal_completed', 'withdrawal_rejected',
    'withdrawal_failed', 'withdrawal_reversed',
    'manual_adjustment', 'tombstone'
));
