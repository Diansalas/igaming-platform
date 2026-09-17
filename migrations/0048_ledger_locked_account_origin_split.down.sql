-- Restores migration 0020's exact eleven-value list. Reversible ONLY on a
-- database where no player_locked_cash/player_locked_bonus account has
-- ever been created. Once one exists the ADD CONSTRAINT below
-- re-validates every row and fails with SQLSTATE 23514. Such an account
-- cannot be deleted first either, once it holds entries: ledger_entries
-- is append-only (migration 0022's ledger_deny_mutation) and carries an
-- FK to ledger_accounts. This is correct, deliberate behavior for an
-- append-only financial ledger (CLAUDE.md), identical to the position
-- migration 0035's own down.sql records, not a defect in this script.
-- MigrateDown wraps this in one transaction (internal/db/migrate.go), so
-- a failed attempt here leaves the schema fully intact.
-- NOT VALID is deliberately NOT used: it would let the narrower
-- constraint be re-added while violating rows remain, which is silent
-- data/constraint divergence rather than a loud failure.
--
-- Restoring 'player_locked' here is a schema rollback, not a
-- re-authorization of that value: HR-8 (ledger-accounting-model.md
-- §6.4.7) forbids minting a bare player_locked account, and no Go const
-- for it exists after this change (§6.5.3).
ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_account_type_check;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_account_type_check CHECK (account_type IN (
    'player_cash', 'player_bonus', 'player_locked', 'player_withdrawal_hold',
    'house_gaming', 'provider_payable', 'psp_clearing', 'psp_reserve',
    'jackpot_contribution', 'promo_liability', 'manual_adjustment'
));
