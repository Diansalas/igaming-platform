-- Restores migration 0048's exact twelve-value list. Reversible ONLY on
-- a database where no bonus_expense account has ever been created. Once
-- one exists the ADD CONSTRAINT below re-validates every row and fails
-- with SQLSTATE 23514, and the offending account cannot be deleted first
-- once it holds entries: ledger_entries is append-only (migration 0022's
-- ledger_deny_mutation) and carries an FK to ledger_accounts. This is
-- correct, deliberate behavior for an append-only financial ledger
-- (CLAUDE.md), identical to the position migrations 0035 and 0048 both
-- record in their own down scripts, not a defect in this script.
-- NOT VALID is deliberately NOT used: it would let the narrower
-- constraint be re-added while violating rows remain, which is silent
-- constraint/data divergence rather than a loud failure.
--
-- THIS is the direction where the wrapper below actually earns its
-- place - narrowing can genuinely fail, and it is the direction an
-- operator reaches for under time pressure.
ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_account_type_check;
DO $$
BEGIN
    ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_account_type_check CHECK (account_type IN (
        'player_cash', 'player_bonus',
        'player_locked_cash', 'player_locked_bonus',
        'player_withdrawal_hold',
        'house_gaming', 'provider_payable', 'psp_clearing', 'psp_reserve',
        'jackpot_contribution', 'promo_liability', 'manual_adjustment'
    ));
EXCEPTION WHEN check_violation THEN
    -- The only value this down migration removes is 'bonus_expense', so
    -- a violating row is necessarily one. Rolling back is not possible
    -- without deleting financial history.
    RAISE EXCEPTION 'migration 0050 (down): at least one bonus_expense ledger account exists (detected at constraint validation, which row-level security cannot filter). This migration is irreversible once bonus value has been recognized: ledger_entries is append-only and the account cannot be removed. Roll forward with a compensating change instead (ledger-accounting-model.md §7.2)';
END $$;
