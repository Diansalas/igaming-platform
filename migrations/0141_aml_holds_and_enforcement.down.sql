-- Reverses 0141 (ADR 0116 S1; DEV/STAGING ONLY, per the migrate-down policy).
--
-- REFUSES (AM099) while any aml_holds / aml_hold_lifts / aml_enforcement_decisions row exists, or while any alert of
-- kind aml.hold_eval_failed exists, or while any AML table of 0142 still exists: a hold is compliance history and is never
-- deleted to make a down pass. kyc_enforcement_decisions rows with outcome `review` also refuse (the CHECK would otherwise
-- be violated). The tables are FORCE RLS with an owner policy bound to the session tenant, so the row counts below lift
-- FORCE for this transaction (a raise rolls the whole transaction back; on success the tables are dropped).
-- Returns the schema to 0140 exactly (whole-schema snapshot test: internal/aml/migration_0141_integration_test.go).

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_class WHERE relnamespace = 'public'::regnamespace AND relname LIKE 'aml\_%'
                  AND relname NOT IN ('aml_holds', 'aml_hold_lifts', 'aml_enforcement_decisions') AND relkind = 'r') THEN
        RAISE EXCEPTION 'migration 0141 down refused: AML tables of a later migration still exist; roll those back first' USING ERRCODE = 'AM099';
    END IF;
    ALTER TABLE aml_holds NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE aml_hold_lifts NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE aml_enforcement_decisions NO FORCE ROW LEVEL SECURITY;
    IF EXISTS (SELECT 1 FROM aml_holds) OR EXISTS (SELECT 1 FROM aml_hold_lifts) OR EXISTS (SELECT 1 FROM aml_enforcement_decisions) THEN
        RAISE EXCEPTION 'migration 0141 down refused: AML hold history exists (append-only compliance history)' USING ERRCODE = 'AM099';
    END IF;
    ALTER TABLE kyc_enforcement_decisions NO FORCE ROW LEVEL SECURITY;
    IF EXISTS (SELECT 1 FROM kyc_enforcement_decisions WHERE outcome = 'review') THEN
        RAISE EXCEPTION 'migration 0141 down refused: kyc_enforcement_decisions rows with the public outcome review exist' USING ERRCODE = 'AM099';
    END IF;
    ALTER TABLE kyc_enforcement_decisions FORCE ROW LEVEL SECURITY;
END
$$;

DROP FUNCTION aml_hold_list(uuid, uuid);
DROP FUNCTION aml_hold_lift(uuid, uuid, text);
DROP FUNCTION aml_hold_place(uuid, uuid, uuid, text, text, text);
DROP FUNCTION aml_record_enforcement_decision(uuid, uuid, uuid, text, text, text, text, boolean, uuid, uuid);
DROP FUNCTION aml_hold_active(uuid, uuid, uuid, text);
DROP FUNCTION aml_hold_match(uuid, uuid, uuid, text);
DROP TABLE aml_enforcement_decisions;
DROP TABLE aml_hold_lifts;
DROP TABLE aml_holds;
DROP FUNCTION aml_hold_lifts_guard();
DROP FUNCTION aml_holds_guard();
DROP FUNCTION aml_valid_operation(text);
DROP FUNCTION aml_deny_mutation();
DROP FUNCTION aml_assert_write_scope(text, text);
DROP FUNCTION aml_actor_owner_scope(text);
DROP FUNCTION aml_session_tenant();

ALTER TABLE kyc_enforcement_decisions DROP CONSTRAINT kyc_enforcement_decisions_outcome_check;
ALTER TABLE kyc_enforcement_decisions ADD CONSTRAINT kyc_enforcement_decisions_outcome_check
    CHECK (outcome IN ('not_required', 'passed', 'pending', 'failed', 'unavailable'));

DO $$
BEGIN
    ALTER TABLE alert_kinds DISABLE TRIGGER alert_kinds_deny_update_delete;
    CREATE POLICY alert_kinds_unseed_0141 ON alert_kinds
        FOR DELETE TO CURRENT_USER USING (kind = 'aml.hold_eval_failed');
    DELETE FROM alert_kinds WHERE kind = 'aml.hold_eval_failed';
    DROP POLICY alert_kinds_unseed_0141 ON alert_kinds;
    ALTER TABLE alert_kinds ENABLE TRIGGER alert_kinds_deny_update_delete;
EXCEPTION WHEN foreign_key_violation THEN
    RAISE EXCEPTION 'migration 0141 down refused: alerts of kind aml.hold_eval_failed exist' USING ERRCODE = 'AM099';
END
$$;
