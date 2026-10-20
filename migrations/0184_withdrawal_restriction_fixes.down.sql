-- Down of 0184 (DEV/STAGING ONLY): restores the 0182 bodies byte for byte and drops the scope helper.
SET LOCAL lock_timeout = '5s';

CREATE OR REPLACE FUNCTION withdrawal_restriction_effective(p_tenant uuid, p_cause text, p_stage text,
    OUT action text, OUT source text, OUT policy_id uuid, OUT set_version bigint)
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_c public.withdrawal_restriction_causes%ROWTYPE;
BEGIN
    IF p_stage IS NULL OR p_stage NOT IN ('request', 'dispatch', 'redispatch') THEN
        RAISE EXCEPTION 'withdrawal restriction: unknown stage %', p_stage USING ERRCODE = 'WR010';
    END IF;
    SELECT * INTO v_c FROM public.withdrawal_restriction_causes c WHERE c.cause = p_cause;
    IF NOT FOUND OR NOT v_c.configurable THEN
        RAISE EXCEPTION 'withdrawal restriction: cause % is unknown or not configurable', p_cause USING ERRCODE = 'WR002';
    END IF;
    SELECT p.action, p.id, p.set_version INTO action, policy_id, set_version
      FROM public.withdrawal_restriction_policies p
     WHERE p.tenant_id = p_tenant AND p.cause = p_cause
     ORDER BY p.set_version DESC
     LIMIT 1;
    IF FOUND THEN
        source := 'configured';
        RETURN;
    END IF;
    action := CASE p_stage WHEN 'request' THEN v_c.default_request WHEN 'dispatch' THEN v_c.default_dispatch ELSE v_c.default_redispatch END;
    source := 'default_unconfigured';
    policy_id := NULL;
    set_version := NULL;
END
$$;

CREATE OR REPLACE FUNCTION withdrawal_restriction_set_version(p_tenant uuid) RETURNS bigint
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT COALESCE(max(p.set_version), 0)::bigint FROM public.withdrawal_restriction_policies p WHERE p.tenant_id = p_tenant
$$;

CREATE OR REPLACE FUNCTION withdrawal_restriction_is_widening(p_tenant uuid, p_cause text, p_action text) RETURNS boolean
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_stage text;
    v_cur   text;
BEGIN
    IF public.withdrawal_restriction_action_rank(p_action) IS NULL THEN
        RAISE EXCEPTION 'withdrawal restriction: unknown action %', p_action USING ERRCODE = 'WR010';
    END IF;
    FOREACH v_stage IN ARRAY ARRAY['request', 'dispatch', 'redispatch'] LOOP
        SELECT e.action INTO v_cur FROM public.withdrawal_restriction_effective(p_tenant, p_cause, v_stage) e;
        IF public.withdrawal_restriction_action_rank(p_action) < public.withdrawal_restriction_action_rank(v_cur) THEN
            RETURN true;
        END IF;
    END LOOP;
    RETURN false;
END
$$;

DROP FUNCTION withdrawal_restriction_assert_scope(uuid);
