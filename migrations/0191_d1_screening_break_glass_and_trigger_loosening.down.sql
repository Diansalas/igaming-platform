-- Down of 0191 (DEV/STAGING ONLY). Refused (LC095) while any platform-authored trigger policy version or any licensing change request of the
-- trigger loosening kind exists (the decision records would be lost). Otherwise it restores exactly the 0190 classifier wrapper, approval trigger
-- function and reconciliation, the 0181 trigger policy guard, drops the third kind, its functions, the column and the constraints, and re-grants
-- the two break-glass setters to the runtime role (their pre-0191 state; NOT recommended anywhere else).
SET LOCAL lock_timeout = '5s';

DO $$
DECLARE
    v_any boolean;
BEGIN
    ALTER TABLE aml_screening_trigger_policies NO FORCE ROW LEVEL SECURITY;
    SELECT EXISTS (SELECT 1 FROM aml_screening_trigger_policies WHERE created_by_scope = 'platform' OR change_request_id IS NOT NULL) INTO v_any;
    ALTER TABLE aml_screening_trigger_policies FORCE ROW LEVEL SECURITY;
    IF v_any THEN
        RAISE EXCEPTION 'migration 0191 down refused: platform-authored trigger policy versions exist' USING ERRCODE = 'LC095';
    END IF;
    ALTER TABLE licensing_change_requests NO FORCE ROW LEVEL SECURITY;
    SELECT EXISTS (SELECT 1 FROM licensing_change_requests WHERE kind = 'aml_screening_trigger_policy_loosen') INTO v_any;
    ALTER TABLE licensing_change_requests FORCE ROW LEVEL SECURITY;
    IF v_any THEN
        RAISE EXCEPTION 'migration 0191 down refused: trigger policy loosening licensing change requests exist' USING ERRCODE = 'LC095';
    END IF;
END
$$;

-- Part 2 (D4)
CREATE OR REPLACE FUNCTION licensing_change_classify(p_kind text, p jsonb,
    OUT widening boolean, OUT subject_tenant uuid, OUT subject_licence uuid)
    LANGUAGE plpgsql VOLATILE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_tenant uuid;
    v_en     boolean;
    v_cool   bigint;
    st       record;
    a        record;
BEGIN
    IF p_kind = 'aml_screening_policy_loosen' THEN
        PERFORM public.screening_policy_loosen_assert(p, NULL);
        widening := true;
        RETURN;
    END IF;
    IF p_kind = 'aml_screening_loosening_configure' THEN
        IF NOT public.licensing_change_keys_are(p, ARRAY['tenant_id', 'enabled', 'cooling_seconds', 'evidence_reference'])
           OR NOT public.licensing_change_is_uuid(p -> 'tenant_id')
           OR pg_catalog.jsonb_typeof(p -> 'enabled') <> 'boolean'
           OR NOT public.licensing_change_is_text(p -> 'evidence_reference', 200) OR (p ->> 'evidence_reference') !~ '^[A-Za-z0-9][A-Za-z0-9._:/#-]{0,199}$'
           OR ((p -> 'enabled') = 'true'::jsonb AND (pg_catalog.jsonb_typeof(p -> 'cooling_seconds') <> 'number' OR (p ->> 'cooling_seconds') !~ '^[0-9]{1,6}$'
                                                      OR (p ->> 'cooling_seconds')::bigint > 255600))
           OR ((p -> 'enabled') = 'false'::jsonb AND pg_catalog.jsonb_typeof(p -> 'cooling_seconds') <> 'null') THEN
            RAISE EXCEPTION 'licensing change: invalid aml_screening_loosening_configure payload' USING ERRCODE = 'LC010';
        END IF;
        v_tenant := (p ->> 'tenant_id')::uuid;
        IF NOT EXISTS (SELECT 1 FROM public.tenants t WHERE t.id = v_tenant) THEN
            RAISE EXCEPTION 'licensing change: unknown tenant' USING ERRCODE = 'LC010';
        END IF;
        -- Serialise changes of one tenant's capability, so "widening" is decided against the version a concurrent change leaves.
        PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('screening_loosening:' || v_tenant::text, 190));
        SELECT * INTO st FROM public.screening_loosening_state(v_tenant);
        v_en := (p -> 'enabled') = 'true'::jsonb;
        v_cool := CASE WHEN v_en THEN (p ->> 'cooling_seconds')::bigint END;
        IF v_en = st.o_enabled AND v_cool IS NOT DISTINCT FROM st.o_cooling THEN
            RAISE EXCEPTION 'licensing change: the loosening capability would be unchanged' USING ERRCODE = 'LC010';
        END IF;
        -- Enabling, or a SHORTER cooling period, makes loosening easier (widening). Disabling or a longer period is narrowing.
        widening := v_en AND (NOT st.o_enabled OR v_cool < st.o_cooling);
        -- c4 fix (security HIGH-1): enabling loosening, or shortening its cooling period, is an AML configuration change: the acting platform admin
        -- needs the in-force aml_officer grant and the owner-scope quorum must hold. Disabling or lengthening (narrowing) stays open to any
        -- platform_admin (the fail-closed direction).
        IF widening THEN
            SELECT * INTO a FROM public.licensing_change_actor();
            PERFORM public.screening_loosening_officers_assert(v_tenant, a.actor, a.person_id);
        END IF;
        RETURN;
    END IF;
    SELECT b.widening, b.subject_tenant, b.subject_licence INTO widening, subject_tenant, subject_licence
      FROM public.licensing_change_classify_base(p_kind, p) b;
END
$$;

CREATE OR REPLACE FUNCTION licensing_change_approvals_screening_officer() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    r public.licensing_change_requests%ROWTYPE;
BEGIN
    SELECT * INTO r FROM public.licensing_change_requests q WHERE q.id = NEW.request_id;
    IF FOUND AND r.kind IN ('aml_screening_policy_loosen', 'aml_screening_loosening_configure') AND r.required_approvals > 0 THEN
        PERFORM public.screening_loosening_officers_assert((r.payload ->> 'tenant_id')::uuid, NEW.approver_id, NEW.approver_person_id);
    END IF;
    RETURN NEW;
END
$$;

CREATE OR REPLACE FUNCTION screening_loosening_reconcile(OUT o_problem text, OUT o_kind text, OUT o_request_id uuid, OUT o_tenant_id uuid)
    RETURNS SETOF record
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a        record;
    v_prev   text;
    v_reqs   jsonb;
    v_ten    uuid[];
    t        uuid;
    q        record;
    n        integer;
BEGIN
    SELECT * INTO a FROM public.licensing_change_actor();
    IF a.scope <> 'platform' OR a.role <> 'platform_admin' THEN
        RAISE EXCEPTION 'licensing change: reconciliation is for platform_admin sessions' USING ERRCODE = 'LC030';
    END IF;
    v_prev := NULLIF(pg_catalog.current_setting('app.tenant_id', true), '');
    SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('id', r.id, 'kind', r.kind, 'tenant', r.payload ->> 'tenant_id')), '[]'::jsonb) INTO v_reqs
      FROM public.licensing_change_requests r WHERE r.status = 'executed' AND r.kind IN ('aml_screening_policy_loosen', 'aml_screening_loosening_configure');
    SELECT COALESCE(pg_catalog.array_agg(x.id), ARRAY[]::uuid[]) INTO v_ten FROM public.tenants x;
    FOREACH t IN ARRAY v_ten LOOP
        PERFORM pg_catalog.set_config('app.tenant_id', t::text, true);
        FOR q IN SELECT e ->> 'id' AS id, e ->> 'kind' AS kind FROM pg_catalog.jsonb_array_elements(v_reqs) e WHERE e ->> 'tenant' = t::text LOOP
            IF q.kind = 'aml_screening_policy_loosen' THEN
                SELECT count(*) INTO n FROM public.aml_screening_policies p WHERE p.change_request_id = q.id::uuid;
            ELSE
                SELECT count(*) INTO n FROM public.aml_screening_loosening_settings s WHERE s.change_request_id = q.id::uuid;
            END IF;
            IF n <> 1 THEN
                o_problem := 'executed_request_without_exactly_one_row'; o_kind := q.kind; o_request_id := q.id::uuid; o_tenant_id := t;
                RETURN NEXT;
            END IF;
        END LOOP;
        FOR q IN SELECT p.change_request_id AS id FROM public.aml_screening_policies p
                  WHERE p.created_by_scope = 'platform'
                    AND NOT EXISTS (SELECT 1 FROM pg_catalog.jsonb_array_elements(v_reqs) e WHERE e ->> 'id' = p.change_request_id::text AND e ->> 'kind' = 'aml_screening_policy_loosen')
        LOOP
            o_problem := 'policy_row_without_executed_request'; o_kind := 'aml_screening_policy_loosen'; o_request_id := q.id; o_tenant_id := t;
            RETURN NEXT;
        END LOOP;
        FOR q IN SELECT s.change_request_id AS id FROM public.aml_screening_loosening_settings s
                  WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.jsonb_array_elements(v_reqs) e WHERE e ->> 'id' = s.change_request_id::text AND e ->> 'kind' = 'aml_screening_loosening_configure')
        LOOP
            o_problem := 'settings_row_without_executed_request'; o_kind := 'aml_screening_loosening_configure'; o_request_id := q.id; o_tenant_id := t;
            RETURN NEXT;
        END LOOP;
    END LOOP;
    PERFORM pg_catalog.set_config('app.tenant_id', COALESCE(v_prev, ''), true);
    RETURN;
END
$$;
COMMENT ON FUNCTION licensing_change_classify(text, jsonb) IS '0190 (c4): wraps licensing_change_classify_base (the 0180 body) and adds the two screening loosening kinds.';

DROP FUNCTION screening_trigger_loosen_apply(uuid);
DROP FUNCTION screening_trigger_loosen_assert(jsonb, timestamptz);
DROP FUNCTION aml_screening_trigger_policy_author(uuid, uuid, text[], boolean, bigint, timestamptz, text, text);

ALTER TABLE licensing_change_kinds NO FORCE ROW LEVEL SECURITY;
DELETE FROM licensing_change_kinds WHERE kind = 'aml_screening_trigger_policy_loosen';
ALTER TABLE licensing_change_kinds FORCE ROW LEVEL SECURITY;

DROP INDEX aml_screening_trigger_policies_change_request;
ALTER TABLE aml_screening_trigger_policies DROP CONSTRAINT aml_screening_trigger_policies_scope_request;
ALTER TABLE aml_screening_trigger_policies DROP CONSTRAINT aml_screening_trigger_policies_created_by_scope_check;
ALTER TABLE aml_screening_trigger_policies ADD CONSTRAINT aml_screening_trigger_policies_created_by_scope_check CHECK (created_by_scope = 'platform_acting');
ALTER TABLE aml_screening_trigger_policies DROP COLUMN change_request_id;

-- The 0181 guard body, byte for byte.
CREATE OR REPLACE FUNCTION aml_screening_trigger_policies_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor record;
BEGIN
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.created_by IS DISTINCT FROM v_actor.actor OR NEW.created_by_scope IS DISTINCT FROM v_actor.scope
       OR NEW.created_by_person_id IS DISTINCT FROM v_actor.person_id THEN
        RAISE EXCEPTION 'aml: the trigger policy author columns are not the proven session actor' USING ERRCODE = 'AT001';
    END IF;
    RETURN NEW;
END
$$;

DROP FUNCTION screening_trigger_cmp(text[], boolean, bigint, text[], boolean, bigint);

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'GRANT EXECUTE ON FUNCTION aml_screening_trigger_policy_set(uuid, uuid, text[], boolean, bigint, timestamptz, text, text) TO igaming_runtime';
    END IF;
END
$$;
COMMENT ON FUNCTION aml_screening_trigger_policy_set(uuid, uuid, text[], boolean, bigint, timestamptz, text, text) IS NULL;

-- Part 1 (D7)
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'GRANT EXECUTE ON FUNCTION aml_screening_policy_set(uuid, uuid, text[], bigint, bigint, timestamptz, text, text) TO igaming_runtime';
    END IF;
END
$$;
COMMENT ON FUNCTION aml_screening_policy_set(uuid, uuid, text[], bigint, bigint, timestamptz, text, text) IS NULL;
