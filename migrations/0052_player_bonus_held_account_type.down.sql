-- Restores migration 0050's exact thirteen-value list. Reversible ONLY on
-- a database where no player_bonus_held account has ever been created.
-- Once one exists the ADD CONSTRAINT below re-validates every row and
-- fails - re-raised below as a legible, remedy-naming message (SQLSTATE
-- P0001, plpgsql RAISE EXCEPTION's default with no explicit SQLSTATE -
-- the underlying check_violation is 23514, but this migration follows
-- migration 0050's own down.sql precedent of catching it and re-raising
-- with a clearer message, since narrowing is the direction that can
-- genuinely fail and the one an operator reaches for under pressure).
-- The offending account cannot be deleted first once it holds entries
-- either way: ledger_entries is append-only (migration 0022's
-- ledger_deny_mutation) and carries an FK to ledger_accounts. This is
-- correct, deliberate behavior for an append-only financial ledger
-- (CLAUDE.md), identical to the position migrations 0035/0048/0050 all
-- record in their own down scripts, not a defect in this script.
-- NOT VALID is deliberately NOT used, for the same reason those three
-- migrations give: it would let the narrower constraint be re-added while
-- violating rows remain, which is silent constraint/data divergence
-- rather than a loud failure.
ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_account_type_check;
DO $$
BEGIN
    ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_account_type_check CHECK (account_type IN (
        'player_cash', 'player_bonus',
        'player_locked_cash', 'player_locked_bonus',
        'player_withdrawal_hold',
        'house_gaming', 'provider_payable', 'psp_clearing', 'psp_reserve',
        'jackpot_contribution', 'promo_liability', 'manual_adjustment',
        'bonus_expense'
    ));
EXCEPTION WHEN check_violation THEN
    -- The only value this down migration removes is 'player_bonus_held',
    -- so a violating row is necessarily one.
    RAISE EXCEPTION 'migration 0052 (down): at least one player_bonus_held ledger account exists (detected at constraint validation, which row-level security cannot filter). This migration is irreversible once bonus-origin settlement value has been held: ledger_entries is append-only and the account cannot be removed. Roll forward with a compensating change instead (ledger-accounting-model.md §7.7.2.4)';
END $$;
