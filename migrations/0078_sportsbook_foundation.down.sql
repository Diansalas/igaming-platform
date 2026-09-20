DROP TABLE IF EXISTS sportsbook_bets;
DROP TABLE IF EXISTS sb_selections;
DROP TABLE IF EXISTS sb_markets;
DROP TABLE IF EXISTS sb_events;
DROP TABLE IF EXISTS sb_competitions;
DROP TABLE IF EXISTS sb_sports;

-- Restores migration 0051's exact sixteen-value transaction_type list.
-- Reversible ONLY on a database where no sportsbook_bet transaction has
-- ever been posted - ledger_entries/ledger_transactions are append-only
-- (migration 0021/0022's ledger_deny_mutation) and the offending rows
-- cannot be deleted first, identical to the position every prior
-- transaction-type-widening migration's own down script records.
ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_transaction_type_check;
DO $$
BEGIN
    ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_transaction_type_check CHECK (transaction_type IN (
        'deposit', 'deposit_reversal',
        'withdrawal_requested', 'withdrawal_completed', 'withdrawal_rejected',
        'withdrawal_failed', 'withdrawal_reversed',
        'manual_adjustment', 'tombstone',
        'casino_bet', 'casino_win', 'casino_rollback',
        'bonus_grant', 'bonus_conversion', 'bonus_forfeiture', 'bonus_reversal'
    ));
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0078 (down): at least one sportsbook_bet transaction exists (detected at constraint validation, which row-level security cannot filter). This migration is irreversible once a sportsbook bet has been posted: ledger_transactions is append-only. Roll forward with a compensating change instead';
END $$;
