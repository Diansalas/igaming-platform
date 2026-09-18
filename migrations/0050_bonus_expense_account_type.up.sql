-- Adds ADR 0032 §2's bonus_expense account type: house-level, per
-- (tenant_id, asset_code), wallet_id IS NULL, debit-normal - the
-- operator's RECOGNIZED promotional cost, debited at the instant bonus
-- value leaves the BONUS_SET for any reason other than forfeiture (ADR
-- 0032 §3's recognition position). Approved architecture since Stage
-- 4H-A; unmigrated until now because an account type with no posting
-- path would have been speculative
-- (ledger-accounting-model.md §6.5.2/§7.2).
--
-- This is precondition (i) of HR-9's removal condition
-- (ledger-accounting-model.md §6.5.7). Precondition (ii) - the Rule B2
-- (extended) mirror generator - is Go code in internal/ledger
-- (bonus_mirror.go), landed in the SAME Stage 4H-B1 Wave 2 dispatch as
-- this migration, so HR-9's guard is removed together with this
-- migration rather than left up awaiting a later one. See that file's
-- package doc comment for the removal record.
--
-- Postgres has no ALTER CHECK, so the constraint is dropped and
-- recreated - the mechanic migrations 0035 and 0048 both used. Unlike
-- 0048 this is PURELY ADDITIVE: twelve accepted values become thirteen,
-- none is removed, so the re-added constraint is a strict superset of
-- the one it replaces and cannot be violated by any row that satisfied
-- the previous one. That is a proof, not an expectation.
--
-- Deliberately NOT in this migration, each absence being a decision:
--   * no transaction_type change - the bonus_* types are migration 0051
--     (ledger-accounting-model.md §7.3), sequenced immediately after this
--     one so the account type a bonus_conversion's mirror leg debits
--     exists first. Postgres does not structurally enforce that ordering
--     between two independent CHECK constraints; it is a practical safety
--     ordering (doc 27 §1.1 item 2);
--   * no index. bonus_expense is house-level, so it is covered unchanged
--     by idx_ledger_accounts_wallet_type_asset's house-level counterpart,
--     the partial unique index on (tenant_id, account_type, asset_code)
--     WHERE wallet_id IS NULL (migration 0020) - the same index
--     promo_liability, house_gaming and psp_clearing already use;
--   * no RLS change, no trigger change. ledger_accounts' RLS keys on
--     tenant_id/player_account_id and the
--     ledger_accounts_populate_from_wallet trigger is account_type-blind
--     (migration 0020);
--   * no seed row. Accounts are minted on first use by
--     GetOrCreateAccount's race-free INSERT ... ON CONFLICT DO NOTHING
--     (ledger-accounting-model.md §1.1); seeding one per tenant per asset
--     would create rows that may never be posted to and would have to be
--     kept in step with the asset registry forever.
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
    -- Defense in depth, and honestly labelled as such: this branch is
    -- believed UNREACHABLE, because the widened list is a strict
    -- superset of migration 0048's. It is present for two reasons, not
    -- as a ritual copy of 0048's guard. (1) If it ever does fire, the
    -- database holds an account_type outside all thirteen values - i.e.
    -- migration 0048 did not in fact apply, or a row was written while
    -- the constraint was absent - and a bare SQLSTATE 23514 would not say
    -- so. (2) It is RLS-immune by construction and the obvious
    -- alternative is not: ledger_accounts carries FORCE ROW LEVEL
    -- SECURITY (migration 0020), which applies to the table owner too,
    -- so a `SELECT ... WHERE account_type NOT IN (...)` pre-flight check
    -- run by a migration connection with no app.tenant_id set sees ZERO
    -- rows regardless of what the table holds and is silently inert.
    -- That is exactly the defect found in migration 0048's first draft
    -- (ledger-accounting-model.md §6.5's implementation-status note).
    -- Constraint validation evaluates every row and cannot be filtered by
    -- RLS. No RLS setting is toggled here, and none ever should be: the
    -- restore would be transaction-local and an operator running this
    -- file standalone under `psql -v ON_ERROR_STOP=1 -f` would leave
    -- FORCE permanently off (security finding S-1, Stage 4H-B0-R7).
    RAISE EXCEPTION 'migration 0050: ledger_accounts holds an account_type outside the thirteen admitted values (detected at constraint validation, which row-level security cannot filter). This widening is additive, so this should be unreachable: verify migration 0048 applied and that no row was written while the constraint was absent (ledger-accounting-model.md §7.2)';
END $$;
