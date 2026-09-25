-- Reverses migration 0091 ONLY on a database where no sportsbook
-- settlement evidence exists (ADR 0088 §12). Every check uses constraint
-- validation, which FORCE ROW LEVEL SECURITY cannot filter (a count(*)
-- here would see zero tenant rows and wrongly proceed — the migration
-- 0078 down-script technique). All checks run before anything is dropped.
-- Each validating ADD CONSTRAINT takes ACCESS EXCLUSIVE and scans the
-- whole table; acceptable because this path is only ever used in
-- dev/scratch databases before any posting. After the first settlement,
-- void, rollback or sportsbook tombstone, roll forward instead.

-- Check 1: no sportsbook_settlement/_void/_rollback transaction exists.
ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_transaction_type_check;
DO $$
BEGIN
    ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_transaction_type_check CHECK (transaction_type IN (
        'deposit', 'deposit_reversal',
        'withdrawal_requested', 'withdrawal_completed', 'withdrawal_rejected',
        'withdrawal_failed', 'withdrawal_reversed',
        'manual_adjustment', 'tombstone',
        'casino_bet', 'casino_win', 'casino_rollback',
        'bonus_grant', 'bonus_conversion', 'bonus_forfeiture', 'bonus_reversal',
        'sportsbook_bet'
    ));
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0091 (down): sportsbook settlement evidence exists (a settlement, void or rollback transaction was posted). This migration is irreversible once any settlement, void, rollback or sportsbook tombstone has been posted; roll forward';
END $$;

-- Check 2: no sportsbook tombstone exists.
DO $$
BEGIN
    ALTER TABLE ledger_transactions ADD CONSTRAINT tmp_0091_no_sportsbook_tombstone
        CHECK (NOT (transaction_type = 'tombstone' AND idempotency_key LIKE 'sportsbook\_settlement:%'));
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0091 (down): sportsbook settlement evidence exists (a sportsbook tombstone was posted). This migration is irreversible once any settlement, void, rollback or sportsbook tombstone has been posted; roll forward';
END $$;
ALTER TABLE ledger_transactions DROP CONSTRAINT tmp_0091_no_sportsbook_tombstone;

-- Check 3: the history table is empty.
DO $$
BEGIN
    ALTER TABLE sportsbook_bet_settlements ADD CONSTRAINT tmp_0091_empty CHECK (false);
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0091 (down): sportsbook settlement evidence exists (sportsbook_bet_settlements is not empty). This migration is irreversible once any settlement, void, rollback or sportsbook tombstone has been posted; roll forward';
END $$;
ALTER TABLE sportsbook_bet_settlements DROP CONSTRAINT tmp_0091_empty;

-- Check 4: every bet is still open.
DO $$
BEGIN
    ALTER TABLE sportsbook_bets ADD CONSTRAINT tmp_0091_all_open CHECK (status = 'open');
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0091 (down): sportsbook settlement evidence exists (a bet is no longer open). This migration is irreversible once any settlement, void, rollback or sportsbook tombstone has been posted; roll forward';
END $$;
ALTER TABLE sportsbook_bets DROP CONSTRAINT tmp_0091_all_open;

-- Check 5 (restores migration 0031's list; refuses if a sportsbook
-- mismatch row exists).
ALTER TABLE reconciliation_mismatches DROP CONSTRAINT reconciliation_mismatches_mismatch_kind_check;
DO $$
BEGIN
    ALTER TABLE reconciliation_mismatches ADD CONSTRAINT reconciliation_mismatches_mismatch_kind_check
        CHECK (mismatch_kind IN ('missing_projection', 'balance_mismatch'));
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0091 (down): a sportsbook reconciliation mismatch has been recorded; roll forward';
END $$;

-- Only now drop.
DROP TRIGGER sportsbook_bets_status_transition ON sportsbook_bets;
DROP FUNCTION sportsbook_bets_status_transition();
DROP TABLE sportsbook_bet_settlements;
DROP FUNCTION sportsbook_bet_settlements_validate();
ALTER TABLE sportsbook_bets DROP CONSTRAINT sportsbook_bets_id_tenant_key;
