-- 0187 (lic25, F1 of lic14 / SN-3 of lic23): the RG limit classification (cooling-off of an increase / removal) follows EVERY jurisdiction
-- the tenant relies on, not only the jurisdiction of tenants.licence_id. SECURITY-SENSITIVE (RG enforcement): FOR `security`, `architect`,
-- `code-reviewer`, `qa` REVIEW BEFORE MERGE. Proposed ADR text: docs/plans/lic25-record.md.
--
-- WHAT THIS CHANGES (no table, no grant, no seeded value, no role change)
--   1. rg_basis_jurisdictions(tenant, at): the jurisdiction codes of the tenant's approved, active, in-force operating bases (the same
--      predicate as operatingmarket.TenantHasApprovedBasis), whether any exists, and whether every one resolved a code. Refuses (RL026)
--      another tenant's id and a player-scoped session (never an empty "no basis" answer).
--   2. rg_limit_change_classify_set(... p_jurisdictions text[], p_incomplete ...): rg_limit_change_classify with a SET of jurisdictions. It
--      composes each jurisdiction with rg_limit_config_compose and folds: the longest cooling-off, reconfirmation if any requires it,
--      the union of the configuration version ids. An increase or removal is refused (RL023) when the set is empty, p_incomplete is
--      set, or any member has no floor in force; initial values and decreases are never refused for that reason.
--   3. rg_limit_change_classify (the 0135 signature) is now a one-element wrapper: with one jurisdiction (or none) it is the pre-lic25
--      function exactly.
--   4. The BEFORE INSERT trigger passes the set (licence jurisdiction plus approved-basis jurisdictions) instead of the licence
--      jurisdiction alone. A tenant with no approved basis passes exactly what it passed before.
-- Down (DEV/STAGING ONLY) restores the 0135 bodies byte for byte.

SET LOCAL lock_timeout = '5s';

-- 1. relied-on jurisdictions of the approved bases
CREATE FUNCTION rg_basis_jurisdictions(
    p_tenant uuid, p_at timestamptz,
    OUT has_basis boolean, OUT complete boolean, OUT codes text[]
) LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    PERFORM public.rg_assert_session_tenant(p_tenant);
    -- operating_bases is tenant-scoped under RLS: a player-scoped session would see no rows and "no basis" would read as permission.
    IF NULLIF(pg_catalog.current_setting('app.player_account_id', true), '') IS NOT NULL THEN
        RAISE EXCEPTION 'rg limits: the relied-on jurisdictions are read in a tenant-scoped, non-player session' USING ERRCODE = 'RL026';
    END IF;
    SELECT COALESCE(pg_catalog.bool_or(true), false),
           COALESCE(pg_catalog.bool_and(j.code IS NOT NULL), true),
           COALESCE(pg_catalog.array_agg(DISTINCT j.code ORDER BY j.code) FILTER (WHERE j.code IS NOT NULL), '{}'::text[])
      INTO has_basis, complete, codes
      FROM public.operating_bases b
      LEFT JOIN public.licences l ON l.id = b.licence_id
      LEFT JOIN public.jurisdictions j ON j.id = COALESCE(b.jurisdiction_id, l.jurisdiction_id)
     WHERE b.tenant_id = p_tenant AND b.status = 'active' AND b.change_request_id IS NOT NULL
       AND b.effective_from <= p_at AND (b.effective_to IS NULL OR b.effective_to > p_at);
END
$$;

-- 2. the set-aware classification (the 0135 body with the composition block replaced)
-- Runs with the CALLER's row visibility.
CREATE FUNCTION rg_limit_change_classify_set(
    p_tenant uuid, p_account uuid, p_person uuid, p_brand uuid, p_jurisdictions text[], p_incomplete boolean,
    p_scope text, p_kind text, p_asset text, p_period text, p_value numeric,
    p_supersedes uuid, p_intent text, p_source text, p_at timestamptz,
    OUT change_kind text, OUT effective_at timestamptz, OUT requires_confirmation boolean,
    OUT value numeric, OUT config_version_ids uuid[]
) LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_cur    RECORD;
    v_latest RECORD;
    v_cfg    RECORD;
    v_j      text;
    v_ids    uuid[] := '{}';
    v_missing boolean := false;
    v_cool   interval;
    v_reconf boolean := false;
BEGIN
    PERFORM public.rg_assert_session_tenant(p_tenant);
    IF p_supersedes IS NOT NULL THEN
        IF p_intent IS NULL OR p_intent NOT IN ('cancel_pending', 'confirm') THEN
            RAISE EXCEPTION 'rg limit change: a superseding request is cancel_pending or confirm' USING ERRCODE = 'RL022';
        END IF;
        SELECT c.* INTO v_latest FROM public.player_rg_limit_changes c
         WHERE c.tenant_id = p_tenant AND c.scope = p_scope AND c.control_kind = p_kind
           AND c.asset_code IS NOT DISTINCT FROM p_asset AND c.period_kind IS NOT DISTINCT FROM p_period
           AND CASE WHEN p_scope = 'account' THEN c.player_account_id = p_account ELSE c.person_id = p_person END
         ORDER BY c.requested_at DESC, c.seq DESC LIMIT 1;
        IF NOT FOUND OR v_latest.id <> p_supersedes THEN
            RAISE EXCEPTION 'rg limit change: % is not the latest request of this limit', p_supersedes USING ERRCODE = 'RL022';
        END IF;
        IF v_latest.change_kind NOT IN ('increase', 'removal') THEN
            RAISE EXCEPTION 'rg limit change: only a pending increase or removal can be cancelled or confirmed' USING ERRCODE = 'RL022';
        END IF;
        IF p_intent = 'cancel_pending' THEN
            IF NOT (v_latest.requires_confirmation OR v_latest.effective_at > p_at) THEN
                RAISE EXCEPTION 'rg limit change: the change is no longer pending' USING ERRCODE = 'RL022';
            END IF;
            SELECT * INTO v_cur FROM public.rg_limit_realized(p_tenant, p_account, p_person, p_scope, p_kind, p_asset, p_period, p_at);
            change_kind := 'cancel_pending'; effective_at := p_at; requires_confirmation := false;
            value := v_cur.value; config_version_ids := '{}';
            RETURN;
        END IF;
        IF NOT v_latest.requires_confirmation THEN
            RAISE EXCEPTION 'rg limit change: the change does not await a confirmation' USING ERRCODE = 'RL022';
        END IF;
        IF p_at < v_latest.effective_at THEN
            RAISE EXCEPTION 'rg limit change: the cooling-off period runs until %', v_latest.effective_at USING ERRCODE = 'RL022';
        END IF;
        IF p_source <> 'player_self_service' THEN
            RAISE EXCEPTION 'rg limit change: only the player may confirm' USING ERRCODE = 'RL022';
        END IF;
        change_kind := 'confirm'; effective_at := p_at; requires_confirmation := false;
        value := v_latest.value; config_version_ids := v_latest.config_version_ids;
        RETURN;
    END IF;
    IF p_intent IN ('cancel_pending', 'confirm') THEN
        RAISE EXCEPTION 'rg limit change: % names the change it supersedes', p_intent USING ERRCODE = 'RL022';
    END IF;

    SELECT * INTO v_cur FROM public.rg_limit_realized(p_tenant, p_account, p_person, p_scope, p_kind, p_asset, p_period, p_at);
    IF p_value IS NULL THEN
        IF NOT v_cur.has_row OR v_cur.value IS NULL THEN
            RAISE EXCEPTION 'rg limit change: there is no limit to remove' USING ERRCODE = 'RL021';
        END IF;
        change_kind := 'removal';
    ELSIF NOT v_cur.has_row OR v_cur.value IS NULL THEN
        change_kind := 'initial';
    ELSIF p_value < v_cur.value THEN
        change_kind := 'decrease';
    ELSIF p_value > v_cur.value THEN
        change_kind := 'increase';
    ELSE
        RAISE EXCEPTION 'rg limit change: the value equals the limit in force' USING ERRCODE = 'RL021';
    END IF;
    value := p_value;
    -- lic25: the STRICTEST FOLD over every relied-on jurisdiction (the licence jurisdiction and the jurisdictions of the tenant's
    -- approved operating bases), with the fold rg_limit_config_compose applies to its own rows (longest cooling-off, reconfirmation
    -- if any requires it). A relied jurisdiction with no floor in force (or an unresolvable one, p_incomplete) means the increase /
    -- removal cannot be timed and is refused (RL023); initial values and decreases are never refused for it. One element and
    -- p_incomplete = false is the pre-lic25 behaviour exactly.
    FOREACH v_j IN ARRAY COALESCE(p_jurisdictions, '{}'::text[]) LOOP
        SELECT * INTO v_cfg FROM public.rg_limit_config_compose(v_j, p_tenant, p_brand, p_kind, p_asset, p_at);
        IF v_cfg.found THEN
            v_ids := v_ids || v_cfg.version_ids;
            IF v_cfg.increase_cooldown IS NULL THEN
                v_missing := true;
            ELSE
                v_cool := GREATEST(COALESCE(v_cool, v_cfg.increase_cooldown), v_cfg.increase_cooldown);
            END IF;
            v_reconf := v_reconf OR COALESCE(v_cfg.increase_requires_reconfirmation, false);
        ELSE
            v_missing := true;
        END IF;
    END LOOP;
    v_ids := ARRAY(SELECT DISTINCT x FROM unnest(v_ids) AS x ORDER BY x);
    IF change_kind IN ('initial', 'decrease') THEN
        effective_at := p_at; requires_confirmation := false;
        config_version_ids := v_ids;
        RETURN;
    END IF;
    IF cardinality(COALESCE(p_jurisdictions, '{}'::text[])) = 0 OR p_incomplete OR v_missing OR v_cool IS NULL THEN
        RAISE EXCEPTION 'rg limit change: no RG configuration is in force for this limit; an increase or removal is refused' USING ERRCODE = 'RL023';
    END IF;
    effective_at := p_at + v_cool;
    requires_confirmation := v_reconf OR p_source = 'staff_on_player_request';
    config_version_ids := v_ids;
END
$$;

-- 3. the 0135 signature, as a wrapper
CREATE OR REPLACE FUNCTION rg_limit_change_classify(
    p_tenant uuid, p_account uuid, p_person uuid, p_brand uuid, p_jurisdiction text,
    p_scope text, p_kind text, p_asset text, p_period text, p_value numeric,
    p_supersedes uuid, p_intent text, p_source text, p_at timestamptz,
    OUT change_kind text, OUT effective_at timestamptz, OUT requires_confirmation boolean,
    OUT value numeric, OUT config_version_ids uuid[]
) LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    SELECT s.change_kind, s.effective_at, s.requires_confirmation, s.value, s.config_version_ids
      INTO change_kind, effective_at, requires_confirmation, value, config_version_ids
      FROM public.rg_limit_change_classify_set(p_tenant, p_account, p_person, p_brand,
               CASE WHEN p_jurisdiction IS NULL THEN '{}'::text[] ELSE ARRAY[p_jurisdiction] END, false,
               p_scope, p_kind, p_asset, p_period, p_value, p_supersedes, p_intent, p_source, p_at) s;
END
$$;

-- 4. the BEFORE INSERT trigger
CREATE OR REPLACE FUNCTION player_rg_limit_changes_classify() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_tenant    text := NULLIF(current_setting('app.tenant_id', true), '');
    v_player    text := NULLIF(current_setting('app.player_account_id', true), '');
    v_principal text := NULLIF(current_setting('app.principal_id', true), '');
    v_platform  text := NULLIF(current_setting('app.platform_admin_principal_id', true), '');
    v_service   text := NULLIF(current_setting('app.platform_service_id', true), '');
    v_acting    text := COALESCE(NULLIF(current_setting('app.acting_tenant_id', true), ''), NULLIF(current_setting('app.acting_platform_principal_id', true), ''));
    v_acct      RECORD;
    v_staff     RECORD;
    v_staff_person uuid;
    v_jur       text;
    v_bas       RECORD;
    v_set       text[];
    v_incomplete boolean;
    v_res       RECORD;
    v_intent    text := NEW.change_kind;
BEGIN
    -- Security C1: the ordering tiebreaker and the key are never caller values (a caller-chosen seq
    -- or id would turn the UNIQUE / PK constraints into a cross-tenant existence oracle).
    NEW.seq := pg_catalog.nextval('public.player_rg_limit_changes_seq_seq'::regclass);
    NEW.id := pg_catalog.gen_random_uuid();
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'rg limit change: refused under % isolation (READ COMMITTED only)', current_setting('transaction_isolation')
            USING ERRCODE = 'RL024';
    END IF;
    IF v_tenant IS NULL OR NEW.tenant_id IS DISTINCT FROM v_tenant::uuid OR v_platform IS NOT NULL OR v_service IS NOT NULL OR v_acting IS NOT NULL THEN
        RAISE EXCEPTION 'rg limit change: the row''s tenant is not the session''s tenant' USING ERRCODE = 'RL025';
    END IF;
    IF NEW.requested_at IS NOT NULL OR NEW.effective_at IS NOT NULL THEN
        RAISE EXCEPTION 'rg limit change: requested_at / effective_at are set by the database' USING ERRCODE = 'RL020';
    END IF;
    IF v_player IS NOT NULL AND v_principal IS NULL THEN
        IF NEW.player_account_id IS DISTINCT FROM v_player::uuid OR NEW.source <> 'player_self_service' THEN
            RAISE EXCEPTION 'rg limit change: a player writes only their own self-service changes' USING ERRCODE = 'RL025';
        END IF;
        NEW.created_by_actor_type := 'player';
        NEW.created_by_actor_id := v_player::uuid;
    ELSIF v_principal IS NOT NULL AND v_player IS NULL THEN
        SELECT su.id, su.person_id, su.status INTO v_staff
          FROM public.staff_users su WHERE su.id = v_principal::uuid AND su.tenant_id = v_tenant::uuid;
        IF NOT FOUND OR v_staff.status <> 'active' THEN
            RAISE EXCEPTION 'rg limit change: the principal is not an active staff member of the tenant' USING ERRCODE = 'RL025';
        END IF;
        IF NEW.source <> 'staff_on_player_request' THEN
            RAISE EXCEPTION 'rg limit change: staff write only on the player''s request' USING ERRCODE = 'RL025';
        END IF;
        NEW.created_by_actor_type := 'staff';
        NEW.created_by_actor_id := v_staff.id;
        v_staff_person := v_staff.person_id;
    ELSE
        RAISE EXCEPTION 'rg limit change: the session is neither a player nor a tenant staff session' USING ERRCODE = 'RL025';
    END IF;

    SELECT pa.person_id, pa.brand_id INTO v_acct FROM public.player_accounts pa
     WHERE pa.id = NEW.player_account_id AND pa.tenant_id = NEW.tenant_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'rg limit change: the player account is not visible in this tenant' USING ERRCODE = 'RL025';
    END IF;
    IF v_staff_person IS NOT NULL AND v_staff_person = v_acct.person_id THEN
        RAISE EXCEPTION 'rg limit change: staff may not change their own Person''s limits' USING ERRCODE = 'RL025';
    END IF;
    NEW.person_id := v_acct.person_id;
    NEW.brand_id := v_acct.brand_id;

    -- L0.4 (ADR 0082; the key of internal/rg lockPerson): serialises every change of this Person
    -- before the classification read and before requested_at is taken.
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtext('player_restrictions'), pg_catalog.hashtext(NEW.person_id::text));
    NEW.requested_at := pg_catalog.clock_timestamp();

    -- The classification reads with the tenant's staff visibility (the 0121 / 0130 technique: bind
    -- app.tenant_id, clear app.player_account_id, restore), so a player session sees every change of
    -- the key (its tenant_person rows written from a linked account included) and the tenant's licence.
    PERFORM set_config('app.player_account_id', '', true);
    SELECT j.code INTO v_jur
      FROM public.tenants t JOIN public.licences l ON l.id = t.licence_id JOIN public.jurisdictions j ON j.id = l.jurisdiction_id
     WHERE t.id = NEW.tenant_id;
    -- lic25: the relied-on jurisdictions are the licence jurisdiction AND the jurisdictions of the tenant's approved operating bases
    -- (the player scope is cleared here, as rg_basis_jurisdictions requires). The strictest fold decides, as in the Go preview.
    SELECT b.has_basis, b.complete, b.codes INTO v_bas FROM public.rg_basis_jurisdictions(NEW.tenant_id, NEW.requested_at) b;
    v_set := ARRAY(SELECT DISTINCT x FROM unnest(COALESCE(v_bas.codes, '{}'::text[])
                                                 || CASE WHEN v_jur IS NULL THEN '{}'::text[] ELSE ARRAY[v_jur] END) AS x ORDER BY x);
    v_incomplete := COALESCE(v_bas.has_basis, false) AND (NOT COALESCE(v_bas.complete, true) OR cardinality(v_set) = 0);
    SELECT * INTO v_res FROM public.rg_limit_change_classify_set(NEW.tenant_id, NEW.player_account_id, NEW.person_id, NEW.brand_id, v_set, v_incomplete,
        NEW.scope, NEW.control_kind, NEW.asset_code, NEW.period_kind, NEW.value,
        NEW.supersedes_change_id, CASE WHEN NEW.supersedes_change_id IS NOT NULL THEN v_intent END, NEW.source, NEW.requested_at);
    PERFORM set_config('app.player_account_id', COALESCE(v_player, ''), true);

    IF v_intent IS NOT NULL AND v_intent <> v_res.change_kind THEN
        RAISE EXCEPTION 'rg limit change: the asserted change_kind % is not the server classification %', v_intent, v_res.change_kind
            USING ERRCODE = 'RL020';
    END IF;
    IF v_res.change_kind IN ('cancel_pending', 'confirm') AND NEW.value IS NOT NULL AND NEW.value IS DISTINCT FROM v_res.value THEN
        RAISE EXCEPTION 'rg limit change: the value of a % row is set by the database', v_res.change_kind USING ERRCODE = 'RL020';
    END IF;
    IF v_res.change_kind = 'confirm' AND NEW.player_account_id IS DISTINCT FROM v_player::uuid THEN
        RAISE EXCEPTION 'rg limit change: only the player''s own session confirms' USING ERRCODE = 'RL022';
    END IF;
    NEW.change_kind := v_res.change_kind;
    NEW.effective_at := v_res.effective_at;
    NEW.requires_confirmation := v_res.requires_confirmation;
    NEW.value := v_res.value;
    NEW.config_version_ids := v_res.config_version_ids;
    RETURN NEW;
END
$$;
