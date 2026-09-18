-- Adds player_bonus_held (ledger-accounting-model.md §7.7.2.4): a single,
-- dedicated, Grant-attributed bonus-origin HOLDING account, credited by
-- casino's hold-capture posting (§7.7.2.2) for a WIN settlement whose
-- disposition is undecided pending the human-supplied G-2 answer (ADR
-- 0039 Decision 2). Player-owned, one row per (wallet_id, account_type,
-- asset_code) - identical shape to every other player_* type - credit-
-- normal (a liability pending disposition, §7.7.2.4's schema table).
--
-- This is the THIRD and (for this dispatch) final BONUS_SET member:
-- BONUS_SET = {player_bonus, player_locked_bonus, player_bonus_held}
-- (ledger-accounting-model.md §6.1's extension note, §7.7.2.3). Like the
-- other two, no posting against it is legal until HR-9's original
-- precondition pair exists - which, as of this same Stage 4H-B1 Wave 2
-- dispatch, it does (migrations 0050/0051 plus
-- internal/ledger/bonus_mirror.go's Rule B2 (extended) generator, which
-- already treats player_bonus_held as a BONUS_SET member from the start -
-- see that file). HR-23 (ledger-accounting-model.md §7.14) is therefore
-- satisfied by the same generator that satisfies HR-9, not separately.
--
-- RENUMBERED FROM THE DESIGN DOCUMENT'S SPECULATIVE CLAIM. §7.7.2.4
-- claimed migration number 0055 for this widening, on the assumption
-- that 0050-0054 would already be consumed by other specialists' Wave-1.5
-- work by the time it landed. That assumption does not hold: this Stage
-- 4H-B1 Wave 2 dispatch builds 0050, 0051 AND this migration together as
-- the ledger-finance financial substrate, in one contiguous block, with
-- nothing else having claimed 0052-0054 in the meantime (verified against
-- the live migrations/ directory before this file was written, per this
-- dispatch's own instruction to trust the filesystem over a speculative
-- number in a design document). ledger-accounting-model.md §7.7.2.4 and
-- §7.16 are updated in the same change to record 0052 as the real,
-- applied number; 0053-0054 are deliberately left unclaimed by this
-- migration for bonus-engine's own domain-table range (see below).
--
-- Deliberately NOT in this migration, each absence being a decision,
-- mirroring 0050's list:
--   * bonus_held_dispositions (the HeldDispositionRecord, §7.7.2.5) is
--     NOT created here. That table is bonus-engine-OWNED, built in its
--     own migration in the 0053+ range during a LATER phase of this same
--     Stage 4H-B1 Wave 2 dispatch (Phase 3, per the dispatch's own phase
--     sequencing) - ledger-finance (this specialist, this migration)
--     supplies only the ledger-side account type player_bonus_held holds
--     value in; the bonus-domain record that tracks WHICH disposition
--     each hold belongs to, and its resolution workflow, is explicitly
--     out of scope for this migration and is not anticipated by it. This
--     comment exists so there is no ambiguity about ownership: if
--     bonus_held_dispositions does not exist yet, that is expected, not a
--     missing dependency of this migration;
--   * no index. player_bonus_held is player-owned like every other
--     player_* type, so it is covered unchanged by
--     idx_ledger_accounts_wallet_type_asset (migration 0020), the partial
--     unique index on (wallet_id, account_type, asset_code) WHERE
--     wallet_id IS NOT NULL;
--   * no RLS change, no trigger change, no new column - identical
--     reasoning to migration 0050's "Deliberately NOT in this migration"
--     list, restated there rather than here;
--   * player_bonus_held is NEVER added to §6.6.6's nullifiable-predicate
--     account list or to `08 §16.4`'s locked-origin-resolution query's
--     account_type IN (...) list - it is a HELD, not a LOCKED, account,
--     and the two families are deliberately kept disjoint at every read
--     site that currently enumerates one or the other (§7.7.2.4's
--     explicit, permanent exclusion). This migration does not touch any
--     such read site, and this note exists so a future reader does not
--     "helpfully" add it to one.
--
-- Postgres has no ALTER CHECK, so the constraint is dropped and
-- recreated - migrations 0035/0048/0050/0051's mechanic. Purely
-- additive: thirteen accepted account_type values become fourteen, none
-- removed.
ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_account_type_check;
DO $$
BEGIN
    ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_account_type_check CHECK (account_type IN (
        'player_cash', 'player_bonus',
        'player_locked_cash', 'player_locked_bonus',
        'player_withdrawal_hold',
        'house_gaming', 'provider_payable', 'psp_clearing', 'psp_reserve',
        'jackpot_contribution', 'promo_liability', 'manual_adjustment',
        'bonus_expense', 'player_bonus_held'
    ));
EXCEPTION WHEN check_violation THEN
    -- Defense in depth, believed UNREACHABLE for the identical reason
    -- migration 0050's own guard gives: the widened list is a strict
    -- superset, ledger_accounts carries FORCE ROW LEVEL SECURITY
    -- (migration 0020), and constraint validation - unlike a SELECT -
    -- cannot be blinded by it. No RLS setting is toggled here, and none
    -- ever should be (security finding S-1, Stage 4H-B0-R7).
    RAISE EXCEPTION 'migration 0052: ledger_accounts holds an account_type outside the fourteen admitted values (detected at constraint validation, which row-level security cannot filter). This widening is additive, so this should be unreachable: verify migrations 0048/0050 applied and that no row was written while the constraint was absent (ledger-accounting-model.md §7.7.2.4)';
END $$;
