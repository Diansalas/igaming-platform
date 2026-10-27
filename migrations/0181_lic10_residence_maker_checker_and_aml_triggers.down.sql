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

-- aml_screening_record returns to migration 0155's exact body.
CREATE OR REPLACE FUNCTION aml_screening_record(
    p_tenant uuid, p_person uuid, p_idempotency_key text, p_trigger text, p_provider_id text, p_provider_reference text,
    p_subject_ref text, p_lists text[], p_outcome text, p_match_lists text[], p_error_code text, p_screened_at timestamptz,
    p_decision text, p_hold_reason_code text
) RETURNS TABLE (o_result_id uuid, o_hold_id uuid, o_replayed boolean)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a        record;
    v_owner  text;
    v_lists  text;
    v_match  text;
    v_prior  record;
    v_hold   uuid;
    v_result uuid;
    v_hash   text;
    v_class  text;
BEGIN
    SELECT string_agg(x, ',' ORDER BY x) INTO v_lists FROM unnest(p_lists) AS x;
    SELECT string_agg(x, ',' ORDER BY x) INTO v_match FROM unnest(p_match_lists) AS x;
    v_hash := public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_person::text, p_idempotency_key, p_trigger, p_provider_id, p_provider_reference,
            p_subject_ref, v_lists, p_outcome, v_match, p_error_code,
            to_char(p_screened_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), p_decision, p_hold_reason_code));
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_screening:record', 'new', v_hash);
    v_owner := public.aml_owner_scope_of_tenant(p_tenant);
    IF public.aml_actor_owner_scope(a.o_scope) IS DISTINCT FROM v_owner THEN
        RAISE EXCEPTION 'aml: the actor does not own this tenant''s AML scope' USING ERRCODE = 'AM002';
    END IF;
    PERFORM public.aml_assert_not_subject(a.o_person_id, p_person);
    PERFORM public.aml_person_lock(p_tenant, p_person);
    -- Idempotent replay: the same key with the same content returns the original result and places nothing; the same key with
    -- different content is refused (a key never silently re-binds).
    -- SR7-7: the comparison is over the digest of EVERY input field, not a subset.
    SELECT r.id, r.hold_id, r.payload_hash INTO v_prior
      FROM public.aml_screening_results r WHERE r.tenant_id = p_tenant AND r.idempotency_key = p_idempotency_key;
    IF FOUND THEN
        IF v_prior.payload_hash <> v_hash THEN
            RAISE EXCEPTION 'aml: the idempotency key was used with different content' USING ERRCODE = 'AM004';
        END IF;
        RETURN QUERY SELECT v_prior.id, v_prior.hold_id, true;
        RETURN;
    END IF;
    IF p_decision <> 'none' THEN
        IF p_hold_reason_code IS NULL OR p_hold_reason_code !~ '^[a-z0-9_]{1,64}$' THEN
            RAISE EXCEPTION 'aml: a hold decision needs a reason code' USING ERRCODE = 'AM004';
        END IF;
        -- SR7-2 / SR7-10: the scope and the class are DERIVED from the outcome and the list, never taken from the caller. All-value-movement
        -- belongs to a confirmed outcome or a sanctions-list match; a sanctions-list match is always a four-eyes-lift class
        -- (confirmed_sanctions / sanctions_potential); an error and a PEP-only match are manual-class.
        IF p_decision = 'hold_all_value_movement' AND NOT (p_outcome = 'confirmed' OR COALESCE('sanctions' = ANY (p_match_lists), false)) THEN
            RAISE EXCEPTION 'aml: all-value-movement is the decision of a confirmed outcome or a sanctions-list match only' USING ERRCODE = 'AM004';
        END IF;
        -- SR7-10: ANY sanctions-list match is a four-eyes-lift hold (ADR 0116 D6 / s4.3): a CONFIRMED outcome is confirmed_sanctions, a
        -- POTENTIAL one is sanctions_potential (same lift rule, distinct label). A PEP-only match and an error stay manual.
        v_class := CASE WHEN COALESCE('sanctions' = ANY (p_match_lists), false)
                        THEN CASE WHEN p_outcome = 'confirmed' THEN 'confirmed_sanctions' ELSE 'sanctions_potential' END
                        ELSE 'manual' END;
        -- SR7-2: a hold recorded through an officer needs the officer quorum of the owner scope (no single officer can place a
        -- hold, in particular none of the four-eyes class, by recording a result). The ADR 0116 s6 exemption for holds placed by
        -- the screening ORCHESTRATOR returns with the F1 system-actor path (a separate, non-officer principal); until then every
        -- record is an officer-proved record, and the orchestrator's record is one of them.
        PERFORM public.aml_assert_quorum(p_tenant, v_owner);
        INSERT INTO public.aml_holds (tenant_id, owner_scope, person_id, player_account_id, hold_scope, reason_class, reason_code,
                                      placed_by, placed_by_person_id, placed_by_scope)
        VALUES (p_tenant, v_owner, p_person, NULL,
                CASE p_decision WHEN 'hold_all_value_movement' THEN 'all_value_movement' ELSE 'withdrawal_and_payout' END,
                v_class,
                p_hold_reason_code, a.o_actor, a.o_person_id, a.o_scope)
        RETURNING id INTO v_hold;
    END IF;
    INSERT INTO public.aml_screening_results (tenant_id, owner_scope, person_id, idempotency_key, trigger, provider_id, provider_reference,
                                              subject_ref, lists, outcome, match_lists, error_code, screened_at, decision, hold_id,
                                              hold_reason_class, payload_hash, recorded_by, recorded_by_person_id, recorded_by_scope)
    VALUES (p_tenant, v_owner, p_person, p_idempotency_key, p_trigger, p_provider_id, p_provider_reference, p_subject_ref, p_lists,
            p_outcome, p_match_lists, p_error_code, p_screened_at, p_decision, v_hold, v_class, v_hash, a.o_actor, a.o_person_id, a.o_scope)
    RETURNING id INTO v_result;
    RETURN QUERY SELECT v_result, v_hold, false;
END
$$;

DROP FUNCTION aml_screening_gate_overdue(uuid, integer);
DROP FUNCTION aml_screening_gate_denies(uuid, uuid, text);
DROP FUNCTION aml_screening_parked_summary(uuid, timestamptz);
DROP FUNCTION aml_screening_parked_overdue(uuid, timestamptz, uuid, integer);
DROP FUNCTION aml_screening_request_replace(uuid, uuid, uuid, text);
DROP FUNCTION aml_screening_request_finish(uuid, uuid, uuid, text, text[], text, text, text, text, timestamptz, integer, text);
DROP FUNCTION aml_screening_review_cleared_at(uuid);
DROP FUNCTION aml_screening_request_resolved(uuid);
DROP FUNCTION aml_screening_gate_environment_ok();
DROP FUNCTION aml_screening_request_claim(uuid, integer, integer);
DROP FUNCTION aml_screening_enqueue_due(uuid, integer);
DROP FUNCTION aml_screening_request_list(uuid, uuid);
DROP FUNCTION aml_screening_trigger_policy_effective(uuid, uuid);
DROP FUNCTION aml_screening_trigger_policy_set(uuid, uuid, text[], boolean, bigint, timestamptz, text, text);
DROP FUNCTION kyc_verifications_aml_enqueue();
DROP FUNCTION player_accounts_aml_enqueue();
DROP FUNCTION aml_screening_enqueue(uuid, uuid, text, text);
DROP FUNCTION aml_screening_jurisdictions(uuid);
DROP FUNCTION aml_screening_assert_worker_provider(text);
DROP FUNCTION screening_worker_heartbeat(text, boolean);
DROP FUNCTION screening_worker_available();
DROP TABLE screening_worker_heartbeats;
DROP TABLE aml_screening_requests;
DROP TABLE aml_screening_trigger_policies;
DROP FUNCTION aml_screening_requests_guard();
DROP FUNCTION aml_screening_trigger_policies_guard();

-- The alert Kind (the 0170 pattern). REFUSES while an alert of the Kind exists.
DO $$
BEGIN
    ALTER TABLE alert_kinds DISABLE TRIGGER alert_kinds_deny_update_delete;
    CREATE POLICY alert_kinds_unseed_0181 ON alert_kinds
        FOR DELETE TO CURRENT_USER USING (kind IN ('aml.screening_attention', 'aml.screening_gate_trapped'));
    DELETE FROM alert_kinds WHERE kind IN ('aml.screening_attention', 'aml.screening_gate_trapped');
    DROP POLICY alert_kinds_unseed_0181 ON alert_kinds;
    ALTER TABLE alert_kinds ENABLE TRIGGER alert_kinds_deny_update_delete;
EXCEPTION WHEN foreign_key_violation THEN
    RAISE EXCEPTION 'migration 0181 down refused: alerts of kind aml.screening_attention or aml.screening_gate_trapped exist' USING ERRCODE = 'RD099';
END
$$;

-- A: residence maker-checker
DROP TRIGGER kyc_verifications_residence_check_ins ON kyc_verifications;
DROP TRIGGER kyc_verifications_residence_guard ON kyc_verifications;
DROP FUNCTION kyc_verifications_residence_guard();
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
