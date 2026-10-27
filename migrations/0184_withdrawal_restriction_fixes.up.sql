-- 0184 (lic17): fixes to the withdrawal restriction policy of migration 0182 from the independent security and ledger-finance
-- reviews. MONEY-PATH GOVERNANCE: FOR `security`, `ledger-finance`, `code-reviewer` REVIEW BEFORE MERGE. Record:
-- docs/plans/lic17-record.md.
--
--   F-2  withdrawal_restriction_is_widening: a change whose resulting action is `allow` is WIDENING (two distinct approvers)
--        whenever the effective source at ANY stage is still `default_unconfigured`. Before, a first configured `allow` over the
--        interim default `allow` was "equal" and single actor, so ONE admin could manufacture an approved-looking `allow` that
--        hides the UNCONFIGURED notice. Every other rule is unchanged (less strict at any stage = widening).
--   M-3  withdrawal_restriction_effective and withdrawal_restriction_set_version are bound to the SESSION scope (the aml_hold_active
--        AM026 pattern): they refuse (WR003) a player or acting session, a session carrying BOTH a tenant and a platform principal
--        (the RLS policies show such a session no row, so a configured block would silently resolve to the default), a tenant
--        session asking for another tenant, and a session with no scope. A platform session (no tenant) may read any tenant.
--
-- No table, row, grant or other function changes. The down restores the 0182 bodies byte for byte.
SET LOCAL lock_timeout = '5s';

CREATE FUNCTION withdrawal_restriction_assert_scope(p_tenant uuid) RETURNS void
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_tenant   text := NULLIF(pg_catalog.current_setting('app.tenant_id', true), '');
    v_platform text := NULLIF(pg_catalog.current_setting('app.platform_admin_principal_id', true), '');
BEGIN
    IF p_tenant IS NULL THEN
        RAISE EXCEPTION 'withdrawal restriction: tenant is required' USING ERRCODE = 'WR003';
    END IF;
    IF NULLIF(pg_catalog.current_setting('app.player_account_id', true), '') IS NOT NULL OR public.financial_acting_gucs_present() THEN
        RAISE EXCEPTION 'withdrawal restriction: a player or acting session may not resolve the policy' USING ERRCODE = 'WR003';
    END IF;
    IF v_tenant IS NOT NULL AND v_platform IS NOT NULL THEN
        RAISE EXCEPTION 'withdrawal restriction: a session with both a tenant and a platform principal sees no policy row' USING ERRCODE = 'WR003';
    END IF;
    IF v_tenant IS NOT NULL THEN
        IF v_tenant::uuid IS DISTINCT FROM p_tenant THEN
            RAISE EXCEPTION 'withdrawal restriction: the session tenant is not the requested tenant' USING ERRCODE = 'WR003';
        END IF;
        RETURN;
    END IF;
    IF v_platform IS NULL THEN
        RAISE EXCEPTION 'withdrawal restriction: the session has no tenant or platform scope' USING ERRCODE = 'WR003';
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION withdrawal_restriction_effective(p_tenant uuid, p_cause text, p_stage text,
    OUT action text, OUT source text, OUT policy_id uuid, OUT set_version bigint)
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_c public.withdrawal_restriction_causes%ROWTYPE;
BEGIN
    PERFORM public.withdrawal_restriction_assert_scope(p_tenant); -- 0184 M-3
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
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    PERFORM public.withdrawal_restriction_assert_scope(p_tenant); -- 0184 M-3
    RETURN (SELECT COALESCE(max(p.set_version), 0)::bigint FROM public.withdrawal_restriction_policies p WHERE p.tenant_id = p_tenant);
END
$$;

CREATE OR REPLACE FUNCTION withdrawal_restriction_is_widening(p_tenant uuid, p_cause text, p_action text) RETURNS boolean
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_stage text;
    v_cur   text;
    v_src   text;
BEGIN
    IF public.withdrawal_restriction_action_rank(p_action) IS NULL THEN
        RAISE EXCEPTION 'withdrawal restriction: unknown action %', p_action USING ERRCODE = 'WR010';
    END IF;
    FOREACH v_stage IN ARRAY ARRAY['request', 'dispatch', 'redispatch'] LOOP
        SELECT e.action, e.source INTO v_cur, v_src FROM public.withdrawal_restriction_effective(p_tenant, p_cause, v_stage) e;
        IF public.withdrawal_restriction_action_rank(p_action) < public.withdrawal_restriction_action_rank(v_cur) THEN
            RETURN true;
        END IF;
        -- 0184 F-2: the first configured `allow` over the interim default is a decision to allow: four-eyes.
        IF p_action = 'allow' AND v_src = 'default_unconfigured' THEN
            RETURN true;
        END IF;
    END LOOP;
    RETURN false;
END
$$;
