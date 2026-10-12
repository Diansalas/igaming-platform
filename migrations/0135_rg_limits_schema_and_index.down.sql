-- Down of 0135 (ADR 0115 S1). Refuses (RL099), BEFORE dropping anything, while any RG limit
-- configuration, floor change request, player limit change or time_out restriction exists: RG history
-- is never dropped by a migration. The FORCE RLS of the three RG tables is lifted first, inside this
-- same transaction, so the owner's counts are not RLS-blind (the tables are dropped right after).
-- Restores 0130's actor_proof_require, 0037's player_restrictions CHECK and player_self_insert policy
-- byte for byte (whole-schema snapshot test: internal/rg/migration_0135_integration_test.go).
ALTER TABLE rg_limit_configs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE rg_limit_config_change_requests NO FORCE ROW LEVEL SECURITY;
ALTER TABLE player_rg_limit_changes NO FORCE ROW LEVEL SECURITY;
ALTER TABLE player_restrictions NO FORCE ROW LEVEL SECURITY;
DO $$
DECLARE
    v_cfg bigint; v_req bigint; v_chg bigint; v_to bigint;
BEGIN
    SELECT count(*) INTO v_cfg FROM rg_limit_configs;
    SELECT count(*) INTO v_req FROM rg_limit_config_change_requests;
    SELECT count(*) INTO v_chg FROM player_rg_limit_changes;
    SELECT count(*) INTO v_to FROM player_restrictions WHERE restriction_type = 'time_out';
    IF v_cfg + v_req + v_chg + v_to > 0 THEN
        RAISE EXCEPTION 'migration 0135 down refused: RG limit data exists (configs=%, change_requests=%, player_changes=%, time_outs=%)',
            v_cfg, v_req, v_chg, v_to USING ERRCODE = 'RL099';
    END IF;
END
$$;
ALTER TABLE player_restrictions FORCE ROW LEVEL SECURITY;

DROP INDEX IF EXISTS idx_deposit_intents_tenant_player_asset_created;
DROP INDEX IF EXISTS idx_ledger_entries_tenant_player_asset_created;

ALTER TABLE casino_launch_sessions DROP COLUMN auth_session_root_started_at;

DROP POLICY player_self_insert ON player_restrictions;
CREATE POLICY player_self_insert ON player_restrictions
    FOR INSERT
    WITH CHECK (
        tenant_id IS NULL
        AND brand_id IS NULL
        AND restriction_type = 'self_exclusion'
        AND source = 'player_self_service'
        AND created_by_actor_type = 'player'
        AND created_by_actor_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
        AND player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
        AND person_id = (
            SELECT person_id FROM player_accounts
            WHERE id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
        )
    );
ALTER TABLE player_restrictions DROP CONSTRAINT player_restrictions_time_out_bounded;
ALTER TABLE player_restrictions DROP CONSTRAINT player_restrictions_restriction_type_check;
ALTER TABLE player_restrictions ADD CONSTRAINT player_restrictions_restriction_type_check
    CHECK (restriction_type IN ('self_exclusion'));

DROP TABLE player_rg_limit_changes;
DROP FUNCTION player_rg_limit_changes_classify();
DROP FUNCTION rg_limit_change_classify(uuid, uuid, uuid, uuid, text, text, text, text, text, numeric, uuid, text, text, timestamptz);
DROP FUNCTION rg_limit_realized(uuid, uuid, uuid, text, text, text, text, timestamptz);
DROP FUNCTION rg_limit_config_compose(text, uuid, uuid, text, text, timestamptz);
DROP FUNCTION rg_limit_config_loosening(rg_limit_configs, rg_limit_configs);
DROP FUNCTION rg_limit_config_payload_hash(rg_limit_configs);
DROP TABLE rg_limit_configs;
DROP FUNCTION rg_limit_configs_10_actor();
DROP FUNCTION rg_limit_configs_20_no_backdate();
DROP FUNCTION rg_limit_configs_30_synthetic();
DROP FUNCTION rg_limit_configs_40_floor_four_eyes();
DROP FUNCTION rg_limit_configs_50_tighten_only();
DROP FUNCTION rg_limit_config_request_digest(rg_limit_config_change_requests);
DROP TABLE rg_limit_config_change_requests;
DROP FUNCTION rg_limit_config_change_requests_guard();
DROP FUNCTION rg_deny_mutation();
DROP FUNCTION rg_scope_rank(text);
DROP FUNCTION rg_valid_interval_set(interval[]);
DROP FUNCTION rg_valid_text_set(text[], text[]);
DROP FUNCTION rg_fixed_interval(interval);
DROP FUNCTION rg_actor_session();

-- 0130's actor_proof_require body, verbatim (CREATE OR REPLACE).
CREATE OR REPLACE FUNCTION actor_proof_require(
    p_actor        uuid,
    p_scope        text,
    p_tenant       uuid,
    p_operation    text,
    p_target       text,
    p_payload_hash text
) RETURNS void AS $$
DECLARE
    v_token   text := NULLIF(pg_catalog.current_setting('app.actor_proof', true), '');
    v_parts   text[];
    v_secret  bytea;
    v_signed  text;
    v_mac     text;
    v_iat     bigint;
    v_exp     bigint;
    v_now     bigint := pg_catalog.floor(pg_catalog.date_part('epoch', pg_catalog.clock_timestamp()))::bigint;
    v_actor   uuid;
    v_tenant  uuid;
    v_n       int;
BEGIN
    IF v_token IS NULL OR p_actor IS NULL OR p_scope IS NULL OR p_operation IS NULL OR p_target IS NULL OR p_payload_hash IS NULL THEN
        RAISE EXCEPTION 'actor_proof: no proof presented' USING ERRCODE = 'AP001';
    END IF;
    IF p_scope NOT IN ('tenant', 'platform_acting', 'platform') THEN
        RAISE EXCEPTION 'actor_proof: scope is not provable' USING ERRCODE = 'AP004';
    END IF;
    -- ADR 0110 operation table (ADR 0111 M-10): withdrawal_hold_resolution:* is
    -- provable ONLY for scope platform_acting with a tenant.
    IF p_operation LIKE 'withdrawal\_hold\_resolution:%' AND (p_scope <> 'platform_acting' OR p_tenant IS NULL) THEN
        RAISE EXCEPTION 'actor_proof: this operation is provable only for a platform_acting scope with a tenant' USING ERRCODE = 'AP004';
    END IF;
    -- ADR 0112 6.4 (migration 0130): launch_authorisation:* is provable only for scope
    -- 'tenant' or 'platform' (never platform_acting), and a decision (approve / reject)
    -- only for scope 'platform'. The operation set is closed.
    IF p_operation LIKE 'launch\_authorisation:%' THEN
        IF p_operation NOT IN ('launch_authorisation:request', 'launch_authorisation:suspend', 'launch_authorisation:cancel',
                               'launch_authorisation:approve', 'launch_authorisation:reject')
           OR p_scope NOT IN ('platform', 'tenant')
           OR (p_operation IN ('launch_authorisation:approve', 'launch_authorisation:reject') AND p_scope <> 'platform') THEN
            RAISE EXCEPTION 'actor_proof: this launch_authorisation operation is not provable for this scope' USING ERRCODE = 'AP004';
        END IF;
    END IF;
    -- The NULL-tenant encoding (an EMPTY tenant field) exists only for scope
    -- 'platform' and only for the K1 capability-grant, financial-policy-change and
    -- (ADR 0112, migration 0130) launch_authorisation operations; every other scope
    -- and operation must carry a tenant.
    IF p_scope = 'platform' THEN
        IF p_tenant IS NOT NULL OR NOT (p_operation LIKE 'capability\_grant:%' OR p_operation LIKE 'financial\_policy\_change:%'
                                         OR p_operation LIKE 'launch\_authorisation:%') THEN
            RAISE EXCEPTION 'actor_proof: platform scope is not provable for this operation' USING ERRCODE = 'AP004';
        END IF;
    ELSIF p_tenant IS NULL THEN
        RAISE EXCEPTION 'actor_proof: no proof presented' USING ERRCODE = 'AP001';
    END IF;

    v_parts := pg_catalog.string_to_array(v_token, '|');
    IF pg_catalog.array_length(v_parts, 1) IS DISTINCT FROM 12 OR v_parts[1] <> 'v1' THEN
        RAISE EXCEPTION 'actor_proof: malformed proof' USING ERRCODE = 'AP001';
    END IF;
    IF v_parts[9] !~ '^[0-9]{1,12}$' OR v_parts[10] !~ '^[0-9]{1,12}$'
       OR v_parts[11] !~ '^[A-Za-z0-9_-]{16,64}$' OR v_parts[12] !~ '^[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'actor_proof: malformed proof' USING ERRCODE = 'AP001';
    END IF;
    BEGIN
        v_actor  := v_parts[3]::uuid;
        v_tenant := NULLIF(v_parts[5], '')::uuid;
    EXCEPTION WHEN OTHERS THEN
        RAISE EXCEPTION 'actor_proof: malformed proof' USING ERRCODE = 'AP001';
    END;
    v_iat := v_parts[9]::bigint;
    v_exp := v_parts[10]::bigint;

    -- Key lookup (active keys only) and MAC.
    SELECT k.secret INTO v_secret FROM public.actor_proof_keys k WHERE k.kid = v_parts[2] AND k.status = 'active';
    IF NOT FOUND THEN
        RAISE EXCEPTION 'actor_proof: proof rejected' USING ERRCODE = 'AP002';
    END IF;
    v_signed := pg_catalog.array_to_string(v_parts[1:11], '|');
    v_mac := pg_catalog.encode(public.hmac(pg_catalog.convert_to(v_signed, 'UTF8'), v_secret, 'sha256'), 'hex');
    -- Compare HMACs of both values under the same key, so the comparison is
    -- not a byte-wise early-exit on attacker-controlled input.
    IF public.hmac(pg_catalog.convert_to(v_mac, 'UTF8'), v_secret, 'sha256')
       IS DISTINCT FROM public.hmac(pg_catalog.convert_to(v_parts[12], 'UTF8'), v_secret, 'sha256') THEN
        RAISE EXCEPTION 'actor_proof: proof rejected' USING ERRCODE = 'AP002';
    END IF;

    -- Time window: not expired, not issued in the future (5 s skew), and the
    -- lifetime is at most 60 s.
    IF v_exp <= v_now OR v_iat > v_now + 5 OR v_exp <= v_iat OR v_exp - v_iat > 60 THEN
        RAISE EXCEPTION 'actor_proof: proof expired or not yet valid' USING ERRCODE = 'AP003';
    END IF;

    -- Binding: the proof must be for exactly this actor/scope/tenant and this
    -- operation/target/payload.
    IF v_actor IS DISTINCT FROM p_actor OR v_parts[4] <> p_scope OR v_tenant IS DISTINCT FROM p_tenant
       OR v_parts[6] <> p_operation OR v_parts[7] <> p_target OR v_parts[8] <> p_payload_hash THEN
        RAISE EXCEPTION 'actor_proof: proof does not match the actor/operation/target' USING ERRCODE = 'AP004';
    END IF;

    -- Replay protection: one proof, one consumption. The row commits or rolls
    -- back with the governed write, so a failed attempt does not burn it and a
    -- committed one can never be replayed (the UNIQUE nonce key serialises two
    -- concurrent consumers).
    INSERT INTO public.actor_proof_nonces
        (nonce, kid, actor, scope, tenant, operation, target, payload_hash, expires_at, consumed_txid)
    VALUES (v_parts[11], v_parts[2], v_actor, p_scope, v_tenant, p_operation, p_target, p_payload_hash,
            pg_catalog.to_timestamp(v_exp), pg_catalog.txid_current())
    ON CONFLICT (nonce) DO NOTHING;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    IF v_n = 0 THEN
        RAISE EXCEPTION 'actor_proof: proof already used' USING ERRCODE = 'AP005';
    END IF;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp;

DROP FUNCTION rg_synthetic_config_allowed();
DROP FUNCTION rg_database_environment();
DROP TABLE database_environment;
DROP FUNCTION database_environment_guard();
