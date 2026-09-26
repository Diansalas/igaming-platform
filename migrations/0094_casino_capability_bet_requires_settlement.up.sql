-- Stage 10.3 CAS-CAP-ROLLBACK-1 (M-CAS-1; docs/plans/stage-10.3-planning/
-- 02-casino-financial-analysis.md §1.4 step 6, §1.5). Root defect: the
-- casino capability's Go-level gate used to double as a settlement kill
-- switch (disabling it 503'd a verified win/rollback and skipped writing
-- a rollback-of-unseen-original's tombstone). The fix (orchestrator.go,
-- capability.go) moves capability/status to gate NEW BETS ONLY; win and
-- rollback settle unconditionally. Under that new contract,
-- supports_win/supports_rollback stop being runtime gates and become
-- CONFIGURATION ASSERTIONS: "this provider relationship can settle what
-- it opens". A row asserting supports_bet=true while asserting
-- supports_win=false or supports_rollback=false would let a tenant open
-- exposure it has separately declared it cannot settle - the exact
-- economic hazard this stage exists to close, just moved from "a runtime
-- kill switch strands exposure" to "a static misconfiguration opens
-- exposure with no path to close it". This CHECK makes that
-- misconfiguration impossible to write in the first place, at the
-- database layer - a backstop below WriteCapability's own identical
-- application-level check (capability.go), never the only control.
--
-- Pre-flight: the validating ALTER TABLE ... ADD CONSTRAINT below IS the
-- pre-flight. It scans every existing row unconditionally, regardless of
-- RLS. casino_provider_capabilities carries FORCE ROW LEVEL SECURITY
-- (migration 0035) - a `SELECT count(*) ...` pre-check run as the
-- NOBYPASSRLS runtime role would see ZERO rows (no player_account_id
-- setting is ever pushed for this table, and FORCE RLS still applies to
-- the table owner's own non-bypass roles) and let a violating database
-- through silently. This is migration 0048/0091/0092's own established
-- lesson, applied here again: never `SELECT count(*)` as a pre-check on a
-- FORCE RLS table; let the constraint's own validation scan do the work.
--
-- No automatic data fix: flipping supports_bet (or supports_win/
-- supports_rollback) inside a migration would be an unaudited
-- configuration change with no actor and no reason code - CLAUDE.md
-- requires every mutating administrative/financial action to be audited
-- with an actor; a migration has neither. Refusal is correct. Expected
-- violators: none in a fresh database (the seed data and every test
-- fixture already assert bet+win+rollback together, or bet=false).
-- Development/staging databases that happen to carry a bet-without-
-- settlement row from an earlier stage's test data must remediate through
-- the audited capability admin API (narrow supports_bet, or enable
-- win+rollback where the adapter already declares them), then re-run this
-- migration - never by hand-editing this table.
--
-- Lock: ACCESS EXCLUSIVE, briefly - casino_provider_capabilities is a
-- tiny, low-write configuration table.
DO $$
BEGIN
    ALTER TABLE casino_provider_capabilities
        ADD CONSTRAINT casino_provider_capabilities_bet_requires_settlement
        CHECK (NOT supports_bet OR (supports_win AND supports_rollback));
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0094: a casino_provider_capabilities row has supports_bet = true without supports_win AND supports_rollback; remediate through the audited capability admin API (narrow supports_bet, or enable win+rollback where the adapter declares them), then re-run this migration - never edit this table directly';
END $$;
