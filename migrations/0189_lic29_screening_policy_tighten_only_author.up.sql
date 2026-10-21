-- lic29: the governed HTTP route that authors the AML SCREENING POLICY (rescreen interval, backfill grace, lists) may only TIGHTEN.
-- aml_screening_policy_set (0154) has no four-eyes step and can loosen screening (SR7-3: any route that exposes it needs step-up
-- critical AND a four-eyes path for loosening first). That four-eyes path is NOT built, so the route calls THIS function instead:
-- identical authorisation (platform_acting officer, quorum, signed actor proof of op aml_screening_policy:set bound to the same
-- arguments, jurisdiction configured for the tenant) and identical row, plus one rule: a version may not be LESS STRICT than the latest
-- version of the same tenant and jurisdiction (a list removed, a longer rescreen interval, a longer backfill grace, or an earlier
-- effective time). The first version of a jurisdiction has nothing to loosen. Loosening stays possible only through the unrouted service
-- method, i.e. not from the HTTP API. 0154 is released and is not edited.
CREATE FUNCTION aml_screening_policy_author(
    p_tenant uuid, p_jurisdiction uuid, p_lists text[], p_rescreen_seconds bigint, p_grace_seconds bigint,
    p_effective_from timestamptz, p_legal_ref text, p_reason_code text
) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a        record;
    v_id     uuid;
    v_lists  text;
    v_tenant text;
    v_cfg    boolean;
    v_prev   record;
BEGIN
    IF p_lists IS NULL OR cardinality(p_lists) NOT BETWEEN 1 AND 2 THEN
        RAISE EXCEPTION 'aml: a screening policy names one or two lists' USING ERRCODE = 'AM004';
    END IF;
    SELECT string_agg(x, ',' ORDER BY x) INTO v_lists FROM unnest(p_lists) AS x;
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_screening_policy:set', 'new',
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_jurisdiction::text, v_lists, p_rescreen_seconds::text, p_grace_seconds::text,
            to_char(p_effective_from AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), p_legal_ref, p_reason_code)));
    IF a.o_scope <> 'platform_acting' THEN
        RAISE EXCEPTION 'aml: screening configuration is authored by a platform officer' USING ERRCODE = 'AM002', HINT = 'platform_officer_required';
    END IF;
    PERFORM public.aml_assert_quorum(p_tenant, public.aml_owner_scope_of_tenant(p_tenant));
    v_tenant := pg_catalog.current_setting('app.tenant_id', true);
    PERFORM pg_catalog.set_config('app.tenant_id', p_tenant::text, true);
    SELECT EXISTS (SELECT 1 FROM public.tenant_jurisdiction_configs c
                    WHERE c.tenant_id = p_tenant AND c.jurisdiction_id = p_jurisdiction
                      AND (c.effective_to IS NULL OR c.effective_to > pg_catalog.clock_timestamp())) INTO v_cfg;
    PERFORM pg_catalog.set_config('app.tenant_id', COALESCE(v_tenant, ''), true);
    IF NOT v_cfg THEN
        RAISE EXCEPTION 'aml: the tenant is not configured to operate in this jurisdiction' USING ERRCODE = 'AM004', HINT = 'tenant_not_in_jurisdiction';
    END IF;
    -- The latest version of this tenant and jurisdiction (in force or pending).
    SELECT p.lists, p.rescreen_interval_seconds, p.backfill_grace_seconds, p.effective_from INTO v_prev
      FROM public.aml_screening_policies p
     WHERE p.tenant_id = p_tenant AND p.licensing_jurisdiction_id = p_jurisdiction
     ORDER BY p.created_at DESC, p.id DESC LIMIT 1;
    IF FOUND AND (NOT (p_lists @> v_prev.lists)
                  OR p_rescreen_seconds > v_prev.rescreen_interval_seconds
                  OR p_grace_seconds > v_prev.backfill_grace_seconds
                  OR p_effective_from < v_prev.effective_from) THEN
        RAISE EXCEPTION 'aml: the new screening policy version is less strict than the latest one; loosening has no four-eyes path'
            USING ERRCODE = 'AT003', HINT = 'policy_loosening_refused';
    END IF;
    INSERT INTO public.aml_screening_policies (tenant_id, licensing_jurisdiction_id, lists, rescreen_interval_seconds, backfill_grace_seconds,
                                               effective_from, legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope)
    VALUES (p_tenant, p_jurisdiction, p_lists, p_rescreen_seconds, p_grace_seconds, p_effective_from, p_legal_ref, p_reason_code,
            a.o_actor, a.o_person_id, a.o_scope)
    RETURNING id INTO v_id;
    RETURN v_id;
END
$$;

REVOKE ALL ON FUNCTION aml_screening_policy_author(uuid, uuid, text[], bigint, bigint, timestamptz, text, text) FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        GRANT EXECUTE ON FUNCTION aml_screening_policy_author(uuid, uuid, text[], bigint, bigint, timestamptz, text, text) TO igaming_runtime;
    END IF;
END
$$;
COMMENT ON FUNCTION aml_screening_policy_author(uuid, uuid, text[], bigint, bigint, timestamptz, text, text) IS '0189 (lic29): the HTTP route''s screening policy author: aml_screening_policy_set plus a tighten-only rule (SR7-3: loosening needs a four-eyes path that is not built).';
