-- Restores migration 0021's original reason_code equality (manual_
-- adjustment only) and migration 0050's exact thirteen-value transaction_
-- type list documented state (i.e. removes the four bonus_* values).
-- Reversible ONLY on a database where no bonus_grant/bonus_conversion/
-- bonus_forfeiture/bonus_reversal transaction has ever been posted, and
-- where no bonus_forfeiture row's reason_code needs to survive the
-- narrower reason_code check. Once either exists, ledger_entries/
-- ledger_transactions are append-only (migration 0021/0022's
-- ledger_deny_mutation) and the offending rows cannot be deleted first -
-- correct, deliberate behavior for an append-only financial ledger
-- (CLAUDE.md), identical to the position migrations 0035/0048/0050 all
-- record in their own down scripts, not a defect in this script.
--
-- Order matters: the reason_code constraint is narrowed FIRST, while
-- bonus_forfeiture is still an accepted transaction_type value, so a
-- violating bonus_forfeiture row is detected by ITS OWN check (a
-- reason-code violation) rather than surfacing later as a transaction_type
-- violation that would misdescribe the actual problem.
ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_check1;
DO $$
BEGIN
    ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_check1 CHECK (
        (transaction_type = 'manual_adjustment') = (reason_code IS NOT NULL)
    );
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0051 (down): at least one bonus_forfeiture row carries a reason_code (detected at constraint validation, which row-level security cannot filter). This migration is irreversible once a bonus_forfeiture has been recognized with its required reason code: ledger_transactions is append-only. Roll forward with a compensating change instead (ledger-accounting-model.md §7.3)';
END $$;

ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_transaction_type_check;
DO $$
BEGIN
    ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_transaction_type_check CHECK (transaction_type IN (
        'deposit', 'deposit_reversal',
        'withdrawal_requested', 'withdrawal_completed', 'withdrawal_rejected',
        'withdrawal_failed', 'withdrawal_reversed',
        'manual_adjustment', 'tombstone',
        'casino_bet', 'casino_win', 'casino_rollback'
    ));
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0051 (down): at least one bonus_grant/bonus_conversion/bonus_forfeiture/bonus_reversal transaction exists (detected at constraint validation, which row-level security cannot filter). This migration is irreversible once bonus value has been posted: ledger_transactions is append-only. Roll forward with a compensating change instead (ledger-accounting-model.md §7.3)';
END $$;
