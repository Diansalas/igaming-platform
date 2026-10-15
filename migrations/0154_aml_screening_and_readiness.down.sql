-- Reverses 0154 (ADR 0116 S2 foundation + S7 readiness reader; DEV/STAGING ONLY, per the migrate-down policy).
--
-- REFUSES (AM099) while any screening policy or screening result row exists: screening history and policy versions are
-- compliance history and are never deleted to make a down pass. FORCE RLS is lifted on the two tables only to count their rows
-- (a raise rolls the whole transaction back; on success the tables are dropped). Returns the schema to 0151 exactly
-- (whole-schema snapshot test: internal/aml/migration_0154_integration_test.go).

DO $$
BEGIN
    ALTER TABLE aml_screening_policies NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE aml_screening_results NO FORCE ROW LEVEL SECURITY;
    IF EXISTS (SELECT 1 FROM aml_screening_policies) OR EXISTS (SELECT 1 FROM aml_screening_results) THEN
        RAISE EXCEPTION 'migration 0154 down refused: AML screening history exists (append-only compliance history)' USING ERRCODE = 'AM099';
    END IF;
END
$$;

DROP FUNCTION aml_readiness(uuid, uuid[]);
DROP FUNCTION aml_screening_list(uuid, uuid);
DROP FUNCTION aml_screening_record(uuid, uuid, text, text, text, text, text, text[], text, text[], text, timestamptz, text, text);
DROP FUNCTION aml_screening_policy_effective(uuid, uuid);
DROP FUNCTION aml_screening_policy_set(uuid, uuid, text[], bigint, bigint, timestamptz, text, text);

DROP TABLE aml_screening_results;
DROP TABLE aml_screening_policies;

DROP FUNCTION aml_screening_results_guard();
DROP FUNCTION aml_screening_policies_guard();
