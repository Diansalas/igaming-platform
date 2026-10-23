-- Down of 0190 (DEV/STAGING ONLY). Refused (LC095) while any loosening capability row, any platform-authored policy version or any
-- licensing change request of a 0190 kind exists (the decision records would be lost). Otherwise restores exactly the 0180 classifier
-- (renamed back), the 0154 policy guard and constraint, drops the kind rows, the column and the table.
SET LOCAL lock_timeout = '5s';

DO $$
DECLARE
    v_any boolean;
BEGIN
    ALTER TABLE aml_screening_loosening_settings NO FORCE ROW LEVEL SECURITY;
    SELECT EXISTS (SELECT 1 FROM aml_screening_loosening_settings) INTO v_any;
    ALTER TABLE aml_screening_loosening_settings FORCE ROW LEVEL SECURITY;
    IF v_any THEN
        RAISE EXCEPTION 'migration 0190 down refused: aml_screening_loosening_settings rows exist' USING ERRCODE = 'LC095';
    END IF;
    ALTER TABLE aml_screening_policies NO FORCE ROW LEVEL SECURITY;
    SELECT EXISTS (SELECT 1 FROM aml_screening_policies WHERE created_by_scope = 'platform' OR change_request_id IS NOT NULL) INTO v_any;
    ALTER TABLE aml_screening_policies FORCE ROW LEVEL SECURITY;
    IF v_any THEN
        RAISE EXCEPTION 'migration 0190 down refused: platform-authored screening policy versions exist' USING ERRCODE = 'LC095';
    END IF;
    ALTER TABLE licensing_change_requests NO FORCE ROW LEVEL SECURITY;
    SELECT EXISTS (SELECT 1 FROM licensing_change_requests WHERE kind IN ('aml_screening_policy_loosen', 'aml_screening_loosening_configure')) INTO v_any;
    ALTER TABLE licensing_change_requests FORCE ROW LEVEL SECURITY;
    IF v_any THEN
        RAISE EXCEPTION 'migration 0190 down refused: screening loosening licensing change requests exist' USING ERRCODE = 'LC095';
    END IF;
END
$$;

DROP TRIGGER licensing_change_approvals_screening_officer ON licensing_change_approvals;
DROP FUNCTION licensing_change_approvals_screening_officer();
DROP FUNCTION screening_policy_loosen_apply(uuid);
DROP FUNCTION screening_loosening_settings_apply(uuid);
DROP FUNCTION licensing_change_classify(text, jsonb);
ALTER FUNCTION licensing_change_classify_base(text, jsonb) RENAME TO licensing_change_classify;
DROP FUNCTION screening_policy_loosen_assert(jsonb, timestamptz);
DROP FUNCTION screening_loosening_state(uuid);
DROP FUNCTION screening_loosening_approvers_assert(uuid, uuid, uuid, uuid);
DROP FUNCTION screening_loosening_officers_assert(uuid, uuid, uuid);
DROP FUNCTION screening_loosening_officer_ok(uuid, uuid, uuid);

ALTER TABLE licensing_change_kinds NO FORCE ROW LEVEL SECURITY;
DELETE FROM licensing_change_kinds WHERE kind IN ('aml_screening_policy_loosen', 'aml_screening_loosening_configure');
ALTER TABLE licensing_change_kinds FORCE ROW LEVEL SECURITY;

DROP INDEX aml_screening_policies_change_request;
ALTER TABLE aml_screening_policies DROP CONSTRAINT aml_screening_policies_scope_request;
ALTER TABLE aml_screening_policies DROP CONSTRAINT aml_screening_policies_created_by_scope_check;
ALTER TABLE aml_screening_policies ADD CONSTRAINT aml_screening_policies_created_by_scope_check CHECK (created_by_scope = 'platform_acting');
ALTER TABLE aml_screening_policies DROP COLUMN change_request_id;

-- The 0154 guard body, byte for byte.
CREATE OR REPLACE FUNCTION aml_screening_policies_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor record;
BEGIN
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.created_by IS DISTINCT FROM v_actor.actor OR NEW.created_by_scope IS DISTINCT FROM v_actor.scope
       OR NEW.created_by_person_id IS DISTINCT FROM v_actor.person_id THEN
        RAISE EXCEPTION 'aml: the policy author columns are not the proven session actor' USING ERRCODE = 'AM001';
    END IF;
    RETURN NEW;
END
$$;

DROP TABLE aml_screening_loosening_settings;
DROP FUNCTION aml_screening_loosening_settings_guard();
