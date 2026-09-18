-- Adds ADR 0032's four bonus transaction types
-- (ledger-accounting-model.md §7.3): bonus_grant (§3, §3.1), bonus_conversion
-- (§4), bonus_forfeiture (§5, §3.1 - covers expiry AND cancellation-after-
-- activation too, distinguished from each other and from a genuine
-- forfeiture only by reason_code, never by a separate type or account -
-- ADR 0032 §3.1 is binding on this point) and bonus_reversal (§7).
--
-- Precondition (i) of HR-9's removal condition, alongside migration 0050
-- (ledger-accounting-model.md §6.5.7): together with the Rule B2
-- (extended) mirror generator (internal/ledger/bonus_mirror.go, landed in
-- this same Stage 4H-B1 Wave 2 dispatch) these are what makes a
-- bonus_grant/bonus_conversion/bonus_forfeiture/bonus_reversal posting
-- legal against invariant B1.
--
-- Sequenced immediately after migration 0050 so the bonus_expense account
-- a bonus_conversion posting's mirror-generated recognition leg debits
-- already exists (doc 27 §1.1 item 2's practical safety ordering; not
-- structurally enforced by Postgres between two independent CHECK
-- constraints, which is why the ordering is a deliberate migration-number
-- choice, not a foreign key).
--
-- Postgres has no ALTER CHECK, so the constraint is dropped and
-- recreated - migrations 0035/0048/0050's mechanic. Purely additive:
-- twelve accepted transaction_type values become sixteen, none removed.
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
    -- Defense in depth, believed UNREACHABLE for the identical reason
    -- migration 0050's own guard gives: the widened list is a strict
    -- superset, ledger_transactions carries FORCE ROW LEVEL SECURITY
    -- (migration 0021), and constraint validation - unlike a SELECT -
    -- cannot be blinded by it. No RLS setting is toggled here, and none
    -- ever should be (security finding S-1, Stage 4H-B0-R7).
    RAISE EXCEPTION 'migration 0051: ledger_transactions holds a transaction_type outside the sixteen admitted values (detected at constraint validation, which row-level security cannot filter). This widening is additive, so this should be unreachable: verify migrations 0035/0048/0050 applied and that no row was written while the constraint was absent (ledger-accounting-model.md §7.3)';
END $$;

-- reason_code becomes required on bonus_forfeiture too, not only
-- manual_adjustment (ledger-accounting-model.md §7.3). ADR 0032 §3.1
-- distinguishes expiry from staff cancellation BY reason code alone -
-- without this widening every bonus_forfeiture posting that supplies one
-- (as ADR 0032 §3.1 requires it to) would fail this exact constraint,
-- and the tempting workaround (omit the reason code) would silently
-- destroy the only signal that distinguishes "the bonus expired" from
-- "staff cancelled it", a distinction a disputing player is entitled to.
--
-- The migration 0021 constraint being widened here is an EQUALITY
-- (CHECK ((transaction_type = 'manual_adjustment') = (reason_code IS NOT
-- NULL))), not an implication - so this is a genuine widening of what is
-- accepted (bonus_forfeiture rows may now ALSO carry a reason code),
-- never a loosening of what is required (every OTHER transaction_type
-- still cannot carry one; the corresponding Go-side check in
-- internal/ledger.Post is extended identically, so the requirement is
-- enforced twice - at the boundary with a legible error, and at the
-- database as the actual guarantee).
--
-- Postgres auto-named this constraint ledger_transactions_check1 when
-- migration 0021 declared it inline (the table's second unnamed CHECK,
-- after ledger_transactions_check on provider_id/provider_tx_id); no
-- later migration has touched it, so that name is exactly what is live
-- at HEAD (verified against the running schema for this dispatch).
ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_check1;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_check1 CHECK (
    (transaction_type IN ('manual_adjustment', 'bonus_forfeiture')) = (reason_code IS NOT NULL)
);
