-- Reverses 0155 (ADR 0116 A7 round 3, SR7-10; DEV/STAGING ONLY, per the migrate-down policy).
--
-- REFUSES (AM099) while any hold or screening result of the class sanctions_potential exists: compliance history is never
-- rewritten or deleted to make a down pass. FORCE RLS is lifted on the two tables only to count their rows and is restored in
-- the same transaction (a raise rolls everything back). Restores the 0154 aml_screening_record, the 0141 aml_hold_lift and the
-- 0142 aml_hold_lifts_guard bodies verbatim and the 0141 / 0154 constraint definitions (same names).
-- Returns the schema to 0154 exactly (whole-schema snapshot test: internal/aml/migration_0155_integration_test.go).

DO $$
BEGIN
    ALTER TABLE aml_holds NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE aml_screening_results NO FORCE ROW LEVEL SECURITY;
    IF EXISTS (SELECT 1 FROM aml_holds WHERE reason_class = 'sanctions_potential')
       OR EXISTS (SELECT 1 FROM aml_screening_results WHERE hold_reason_class = 'sanctions_potential') THEN
        RAISE EXCEPTION 'migration 0155 down refused: sanctions_potential holds or results exist (append-only compliance history)' USING ERRCODE = 'AM099';
    END IF;
    ALTER TABLE aml_holds FORCE ROW LEVEL SECURITY;
    ALTER TABLE aml_screening_results FORCE ROW LEVEL SECURITY;
END
$$;

CREATE OR REPLACE FUNCTION aml_hold_lifts_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_hold  record;
    v_actor record;
    v_ev    record;
BEGIN
    SELECT h.tenant_id, h.owner_scope, h.reason_class, h.person_id INTO v_hold FROM public.aml_holds h WHERE h.id = NEW.hold_id;
    IF NOT FOUND OR v_hold.tenant_id <> NEW.tenant_id THEN
        RAISE EXCEPTION 'aml: not found' USING ERRCODE = 'AM040';
    END IF;
    -- The monotonic hold-change mark (security F8) is FORCED here, under the per-person lock, so the invariant does not depend on
    -- which function (or owner-level statement) inserts the row.
    PERFORM public.aml_person_lock(NEW.tenant_id, v_hold.person_id);
    NEW.change_seq := pg_catalog.nextval('public.aml_hold_change_seq');
    IF NEW.owner_scope <> v_hold.owner_scope THEN
        RAISE EXCEPTION 'aml: only the hold''s owner scope may lift it' USING ERRCODE = 'AM006';
    END IF;
    IF v_hold.reason_class = 'confirmed_sanctions' AND NEW.approval_event_id IS NULL THEN
        RAISE EXCEPTION 'aml: lifting a confirmed sanctions hold needs a proof-bound four-eyes approval' USING ERRCODE = 'AM006';
    END IF;
    IF v_hold.reason_class <> 'confirmed_sanctions' AND NEW.approval_event_id IS NOT NULL THEN
        RAISE EXCEPTION 'aml: an approval event is only valid for a confirmed sanctions lift' USING ERRCODE = 'AM006';
    END IF;
    IF NEW.approval_event_id IS NOT NULL THEN
        SELECT e.event_type, e.proposal_kind, e.subject_hold_id, e.tenant_id, e.actor_id INTO v_ev
          FROM public.aml_case_events e WHERE e.id = NEW.approval_event_id;
        IF NOT FOUND OR v_ev.event_type <> 'approval' OR v_ev.proposal_kind <> 'lift_hold'
           OR v_ev.subject_hold_id IS DISTINCT FROM NEW.hold_id OR v_ev.tenant_id <> NEW.tenant_id THEN
            RAISE EXCEPTION 'aml: the approval event does not authorise this hold lift' USING ERRCODE = 'AM006';
        END IF;
    END IF;
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.lifted_by IS DISTINCT FROM v_actor.actor OR NEW.lifted_by_scope IS DISTINCT FROM v_actor.scope
       OR NEW.lifted_by_person_id IS DISTINCT FROM v_actor.person_id THEN
        RAISE EXCEPTION 'aml: the lifting actor columns are not the proven session actor' USING ERRCODE = 'AM001';
    END IF;
    RETURN NEW;
END
$$;

CREATE OR REPLACE FUNCTION aml_hold_lift(p_tenant uuid, p_hold uuid, p_reason_code text) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a       record;
    v_hold  record;
    v_id    uuid;
BEGIN
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_hold:lift', p_hold::text,
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_hold::text, p_reason_code)));
    SELECT h.owner_scope, h.reason_class, h.person_id INTO v_hold FROM public.aml_holds h WHERE h.id = p_hold AND h.tenant_id = p_tenant;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'aml: not found' USING ERRCODE = 'AM040';
    END IF;
    PERFORM public.aml_assert_write_scope(a.o_scope, v_hold.owner_scope);
    PERFORM public.aml_assert_not_subject(a.o_person_id, v_hold.person_id);
    PERFORM public.aml_person_lock(p_tenant, v_hold.person_id);
    IF v_hold.reason_class = 'confirmed_sanctions' THEN
        RAISE EXCEPTION 'aml: a confirmed sanctions hold is lifted only by a proof-bound four-eyes approval' USING ERRCODE = 'AM006';
    END IF;
    INSERT INTO public.aml_hold_lifts (hold_id, tenant_id, owner_scope, lifted_by, lifted_by_person_id, lifted_by_scope, reason_code)
    VALUES (p_hold, p_tenant, v_hold.owner_scope, a.o_actor, a.o_person_id, a.o_scope, p_reason_code)
    RETURNING id INTO v_id;
    RETURN v_id;
END
$$;

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
        -- SR7-2: the scope and the class are DERIVED from the outcome and the list, never taken from the caller. All-value-movement
        -- belongs to a confirmed outcome or a sanctions-list match; the four-eyes class (confirmed_sanctions) to a CONFIRMED outcome
        -- that names the sanctions list only. An error, a PEP-only match and a potential match are manual-class.
        IF p_decision = 'hold_all_value_movement' AND NOT (p_outcome = 'confirmed' OR COALESCE('sanctions' = ANY (p_match_lists), false)) THEN
            RAISE EXCEPTION 'aml: all-value-movement is the decision of a confirmed outcome or a sanctions-list match only' USING ERRCODE = 'AM004';
        END IF;
        v_class := CASE WHEN p_outcome = 'confirmed' AND COALESCE('sanctions' = ANY (p_match_lists), false) THEN 'confirmed_sanctions' ELSE 'manual' END;
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

ALTER TABLE aml_screening_results DROP CONSTRAINT aml_screening_results_check10;
ALTER TABLE aml_screening_results ADD CONSTRAINT aml_screening_results_check10
    CHECK (hold_reason_class IS NULL
           OR ((hold_reason_class = 'confirmed_sanctions') = (outcome = 'confirmed' AND COALESCE('sanctions' = ANY (match_lists), false))));
ALTER TABLE aml_screening_results DROP CONSTRAINT aml_screening_results_hold_reason_class_check;
ALTER TABLE aml_screening_results ADD CONSTRAINT aml_screening_results_hold_reason_class_check
    CHECK (hold_reason_class IN ('manual', 'confirmed_sanctions'));
ALTER TABLE aml_holds DROP CONSTRAINT aml_holds_check;
ALTER TABLE aml_holds ADD CONSTRAINT aml_holds_check CHECK (reason_class <> 'confirmed_sanctions' OR hold_scope = 'all_value_movement');
ALTER TABLE aml_holds DROP CONSTRAINT aml_holds_reason_class_check;
ALTER TABLE aml_holds ADD CONSTRAINT aml_holds_reason_class_check CHECK (reason_class IN ('manual', 'confirmed_sanctions'));
