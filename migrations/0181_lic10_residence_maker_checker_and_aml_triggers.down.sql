-- Reverses 0181 (lic10; DEV/STAGING ONLY, per the migrate-down policy).
--
-- REFUSES (RD099) while any residence policy version, approval, determination check, AML trigger policy or AML screening
-- request row exists, or while any alert of kind aml.screening_attention exists: those rows are compliance history and are
-- never deleted to make a down pass. FORCE RLS is lifted on the tables only to count their rows (a raise rolls the whole
-- transaction back; on success the tables are dropped).

DO $$
BEGIN
    ALTER TABLE residence_approval_policies NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE residence_approval_policy_approvals NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE residence_determination_checks NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE aml_screening_trigger_policies NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE aml_screening_requests NO FORCE ROW LEVEL SECURITY;
    IF EXISTS (SELECT 1 FROM residence_approval_policies) OR EXISTS (SELECT 1 FROM residence_approval_policy_approvals)
       OR EXISTS (SELECT 1 FROM residence_determination_checks) OR EXISTS (SELECT 1 FROM aml_screening_trigger_policies)
       OR EXISTS (SELECT 1 FROM aml_screening_requests) THEN
        RAISE EXCEPTION 'migration 0181 down refused: residence maker-checker or AML screening trigger history exists (compliance history)' USING ERRCODE = 'RD099';
    END IF;
END
$$;

-- B: AML triggers
DROP TRIGGER kyc_verifications_aml_enqueue_ins ON kyc_verifications;
DROP TRIGGER kyc_verifications_aml_enqueue_upd ON kyc_verifications;
DROP TRIGGER player_accounts_aml_enqueue ON player_accounts;

-- aml_hold_active returns to migration 0141's exact body (same signature, grants and comment).
CREATE OR REPLACE FUNCTION aml_hold_active(p_tenant uuid, p_person uuid, p_account uuid, p_operation text) RETURNS boolean
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF p_tenant IS NULL OR p_person IS NULL OR p_operation IS NULL OR NOT public.aml_valid_operation(p_operation) THEN
        RAISE EXCEPTION 'aml: invalid hold evaluation arguments' USING ERRCODE = 'AM004';
    END IF;
    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AM026';
    END IF;
    RETURN EXISTS (
        SELECT 1 FROM public.aml_holds h
         WHERE h.tenant_id = p_tenant AND h.person_id = p_person
           AND (h.player_account_id IS NULL OR h.player_account_id = p_account)
           AND (h.hold_scope = 'all_value_movement' OR p_operation IN ('withdrawal', 'payout', 'manual_adjustment'))
           AND NOT EXISTS (SELECT 1 FROM public.aml_hold_lifts l WHERE l.hold_id = h.id)
    );
END
$$;
COMMENT ON FUNCTION aml_hold_active(uuid, uuid, uuid, text) IS 'ADR 0116 s5.1: the ONLY hold read a gate performs. SECURITY DEFINER, STABLE, search_path pinned, session-tenant bound (AM026).';

DROP FUNCTION aml_screening_gate_denies(uuid, uuid, text);
DROP FUNCTION aml_screening_request_finish(uuid, uuid, uuid, text, text[], text, text, text, text, timestamptz, integer);
DROP FUNCTION aml_screening_request_claim(uuid, integer, integer);
DROP FUNCTION aml_screening_enqueue_due(uuid, integer);
DROP FUNCTION aml_screening_request_list(uuid, uuid);
DROP FUNCTION aml_screening_trigger_policy_effective(uuid, uuid);
DROP FUNCTION aml_screening_trigger_policy_set(uuid, uuid, text[], boolean, timestamptz, text, text);
DROP FUNCTION kyc_verifications_aml_enqueue();
DROP FUNCTION player_accounts_aml_enqueue();
DROP FUNCTION aml_screening_enqueue(uuid, uuid, text, text);
DROP TABLE aml_screening_requests;
DROP TABLE aml_screening_trigger_policies;
DROP FUNCTION aml_screening_requests_guard();
DROP FUNCTION aml_screening_trigger_policies_guard();

-- The alert Kind (the 0170 pattern). REFUSES while an alert of the Kind exists.
DO $$
BEGIN
    ALTER TABLE alert_kinds DISABLE TRIGGER alert_kinds_deny_update_delete;
    CREATE POLICY alert_kinds_unseed_0181 ON alert_kinds
        FOR DELETE TO CURRENT_USER USING (kind = 'aml.screening_attention');
    DELETE FROM alert_kinds WHERE kind = 'aml.screening_attention';
    DROP POLICY alert_kinds_unseed_0181 ON alert_kinds;
    ALTER TABLE alert_kinds ENABLE TRIGGER alert_kinds_deny_update_delete;
EXCEPTION WHEN foreign_key_violation THEN
    RAISE EXCEPTION 'migration 0181 down refused: alerts of kind aml.screening_attention exist' USING ERRCODE = 'RD099';
END
$$;

-- A: residence maker-checker
DROP TRIGGER kyc_verifications_residence_check_ins ON kyc_verifications;
DROP TRIGGER kyc_verifications_residence_check_upd ON kyc_verifications;
DROP FUNCTION kyc_verifications_residence_check();
DROP FUNCTION residence_determination_approve(uuid, uuid, timestamptz, text);
DROP FUNCTION residence_policy_list(uuid);
DROP FUNCTION residence_policy_approve(uuid, uuid, text);
DROP FUNCTION residence_policy_set(uuid, text, text);
DROP FUNCTION residence_authorize_policy_actor(uuid, text, text, text);
DROP TABLE residence_determination_checks;
DROP TABLE residence_approval_policy_approvals;
DROP TABLE residence_approval_policies;
DROP FUNCTION residence_policy_mode(uuid);
DROP FUNCTION residence_determination_checks_guard();
DROP FUNCTION residence_approval_policy_approvals_guard();
DROP FUNCTION residence_approval_policies_guard();
DROP FUNCTION residence_mc_deny_mutation();
DROP FUNCTION residence_mode_rank(text);
