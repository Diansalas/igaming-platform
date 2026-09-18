-- Stage 4H-B1 Wave 2 Phase 4 (risk): lands ADR 0031 §16/§16a/§40's
-- long-documented `bonus_conversion` Risk `Operation` value - step 1 of
-- the six-step extension-process ADR 0031 §16 mandates land together in
-- one authorized change (§16a/§40's own verified checklist; this
-- migration, the Go constant in internal/risk/types.go, the HTTP
-- allowlist in internal/httpserver/risk_handlers.go, all three OpenAPI
-- `operation` enum occurrences in docs/api/openapi/platform-api.yaml,
-- the operationCumulativeSpecs entry in internal/risk/cumulative.go, and
-- the already-written call site in internal/bonus/conversion.go land
-- together in this same commit).
--
-- Naming is fixed by ADR 0031 §15a-ii and is not re-openable here:
-- `bonus_conversion`, never doc 10 §4's superseded placeholder
-- `bonus_convert`, never `bonus_activate` (rejected, activation reuses
-- `bonus_grant`).
--
-- Postgres has no ALTER CHECK, so the constraint is dropped and
-- recreated - migrations 0035/0048/0050/0051's identical mechanic.
-- Purely additive: six accepted operation values become seven, none
-- removed. The exact live constraint name (risk_rules_operation_check)
-- was confirmed against the running schema for this dispatch, not
-- assumed from migration 0041's inline CHECK syntax.
ALTER TABLE risk_rules DROP CONSTRAINT risk_rules_operation_check;
DO $$
BEGIN
    ALTER TABLE risk_rules ADD CONSTRAINT risk_rules_operation_check CHECK (operation IN (
        'casino_launch', 'casino_bet', 'deposit', 'withdrawal', 'sportsbook_bet', 'bonus_grant',
        'bonus_conversion'
    ));
EXCEPTION WHEN check_violation THEN
    -- Defense in depth, believed UNREACHABLE for the identical reason
    -- migration 0051's own guard gives: the widened list is a strict
    -- superset of the six values every existing risk_rules row was
    -- written under, and risk_rules carries FORCE ROW LEVEL SECURITY
    -- (migration 0041's own trigger/RLS discipline) - constraint
    -- validation, unlike a SELECT, cannot be blinded by it. No RLS
    -- setting is toggled here, and none ever should be.
    RAISE EXCEPTION 'migration 0065: risk_rules holds an operation value outside the seven admitted values (detected at constraint validation, which row-level security cannot filter). This widening is additive, so this should be unreachable: verify migrations 0041-0064 applied cleanly and that no row was written while the constraint was absent';
END $$;
