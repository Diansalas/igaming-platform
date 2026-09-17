-- Splits player_locked into player_locked_cash / player_locked_bonus.
-- Shape A, ledger-accounting-model.md §6.3.1/§6.3.2; implementation
-- contract §6.4; this migration's design §6.5. Postgres has no
-- ALTER CHECK, so the constraint is dropped and recreated - the same
-- mechanic migration 0035 used for ledger_transactions.transaction_type.
-- Unlike 0035 this is NOT purely additive: bare 'player_locked' is
-- REMOVED from the accepted set (§6.5.3, invariant L1).
--
-- Invariant L1 (locked-origin determinacy, ledger-accounting-model.md
-- §6.5.4): every ledger entry against a locked-funds account is
-- attributable to the origin of the value it holds from the account's own
-- account_type alone. This migration is layer 1 (database CHECK) and
-- layer 3 (the pre-flight guard below) of L1's five enforcement layers.
--
-- Deliberately NOT in this migration, each absence being a decision
-- (§6.5.2): no new index (the partial unique index
-- idx_ledger_accounts_wallet_type_asset covers both new values unchanged,
-- both being player-owned like every other player_* type); no new column,
-- no RLS change, no trigger change (ledger_accounts' RLS keys on
-- tenant_id/player_account_id and the ledger_accounts_populate_from_wallet
-- trigger is account_type-blind); no ledger_transactions.transaction_type
-- change (the sportsbook_* types are ADR 0038's own migration step and
-- are independent - so this migration deliberately ships a schema in
-- which the new locked accounts exist but no transaction type that would
-- post to them does, §6.5.6); no 'bonus_expense' (ADR 0032 §2, approved
-- but belonging to the stage that builds the Rule B2 (extended) mirror
-- generator - which is why HR-9's fail-closed posting guard in
-- internal/ledger is required, §6.5.7); no data migration/backfill (zero
-- rows to move, proven by the guard below rather than assumed).

-- Pre-flight guard. Zero bare player_locked accounts exist in any
-- environment (verified §6.5.1) and HR-8 forbids ever minting one. If one
-- exists, stop with a legible message: the ADD CONSTRAINT below fails
-- anyway with a bare SQLSTATE 23514, but the remedy is an authorized
-- backfill decision, not a retry, and a bare 23514 does not say so.
--
-- CORRECTION to §6.5.2's literal SQL, made at implementation time by its
-- own author (`ledger-finance`) and caught by the guard's own test:
-- §6.5.2 designed the guard as a `SELECT count(*) FROM ledger_accounts`.
-- That guard would have been silently INERT. ledger_accounts carries
-- FORCE ROW LEVEL SECURITY (migration 0020:98), which applies to the
-- table OWNER too, and a migration connection sets no app.tenant_id - so
-- the count sees ZERO rows no matter what the table holds. It would
-- always have "passed", and the migration would then have failed with
-- exactly the bare SQLSTATE 23514 the guard exists to replace.
--
-- The guard is therefore implemented with ONE mechanism, which needs no
-- count and no visibility past RLS: the ADD CONSTRAINT below is wrapped
-- so that a check violation is re-raised with a legible, remedy-naming
-- message. Constraint validation evaluates EVERY row in the table, so
-- RLS is irrelevant to it - the mechanism cannot be blinded the way a
-- SELECT is, and it works regardless of which role runs the migration.
--
-- An earlier revision additionally lifted RLS enforcement
-- (`ALTER TABLE ... NO FORCE ROW LEVEL SECURITY`) around a row count, to
-- put the number of offending rows into the message, restoring FORCE
-- afterwards. That was REMOVED at security review (Stage 4H-B0-R7,
-- finding S-1): the restore is transaction-local, so an operator running
-- this file standalone under `psql -v ON_ERROR_STOP=1 -f` - precisely
-- what they would do to read this guard's message during an incident -
-- stops at the RAISE and leaves ledger_accounts with FORCE permanently
-- OFF, a silent loss of tenant isolation on a financial table. A row
-- count in an error message is not worth weakening a core isolation
-- control, and no forward migration has ever toggled FORCE RLS. The
-- message below consequently names the condition but not a count; the
-- count is not available to this mechanism, and an operator who needs it
-- can run a scoped query themselves.
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
    -- The only value migration 0020 admitted and this migration removes
    -- is bare 'player_locked', so a violating row is necessarily one -
    -- and validation, unlike a SELECT, cannot be filtered by RLS. This is
    -- the pre-flight guard in full (see the note above).
    RAISE EXCEPTION 'migration 0048: bare player_locked account(s) exist (detected at constraint validation, which row-level security cannot filter); a player_locked -> player_locked_cash backfill must be designed and authorized first (ledger-accounting-model.md §6.5.3)';
END $$;
