-- Reverses 0166 (F1, actor-proof key isolation; DEV/STAGING ONLY, per the migrate-down policy). Moving the key table back into public
-- makes the signing secret readable by igaming_backup again (residual R1 returns).
--
-- Refuses (APM02; nothing is changed) while the previous state cannot be restored faithfully:
--   * the key table is not where 0166 put it, or public.actor_proof_keys exists again;
--   * the schema actorproof_private holds anything besides the key table (its index, constraints and row type): DROP SCHEMA would destroy it;
--   * the key table or the schema carries a grant to anyone other than the owner, or a column-level grant, or a view/rule/trigger/policy
--     depends on the table (0166 left none; something was added since, and the down cannot know what 0165's state of it would be).
-- Otherwise the schema returns to 0165 byte for byte (whole-schema snapshot test): the table (rows included, never copied) moves back, the two
-- function bodies below are 0139's and 0120's verbatim, actor_proof_key_inventory is dropped, and igaming_backup's table-level SELECT on the
-- key table is restored exactly when it holds SELECT on actor_proof_nonces (0166 refused to run unless both were alike).

LOCK TABLE actorproof_private.actor_proof_keys IN ACCESS EXCLUSIVE MODE;

DO $$
DECLARE
    v_rel oid := 'actorproof_private.actor_proof_keys'::regclass;
    v_nsp oid := pg_catalog.to_regnamespace('actorproof_private');
    v_bad text;
    v_n   bigint;
BEGIN
    IF pg_catalog.to_regclass('public.actor_proof_keys') IS NOT NULL THEN
        RAISE EXCEPTION 'migration 0166 down refused: public.actor_proof_keys exists; nothing was changed' USING ERRCODE = 'APM02';
    END IF;
    SELECT count(*) INTO v_n FROM pg_catalog.pg_class c
     WHERE c.relnamespace = v_nsp AND c.oid <> v_rel
       AND NOT (c.relkind = 'i' AND EXISTS (SELECT 1 FROM pg_catalog.pg_index i WHERE i.indexrelid = c.oid AND i.indrelid = v_rel));
    v_n := v_n + (SELECT count(*) FROM pg_catalog.pg_proc p WHERE p.pronamespace = v_nsp)
               + (SELECT count(*) FROM pg_catalog.pg_type t WHERE t.typnamespace = v_nsp AND t.typrelid <> v_rel
                    AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_type e WHERE e.oid = t.typelem AND e.typrelid = v_rel))
               + (SELECT count(*) FROM pg_catalog.pg_collation x WHERE x.collnamespace = v_nsp)
               + (SELECT count(*) FROM pg_catalog.pg_operator o WHERE o.oprnamespace = v_nsp)
               + (SELECT count(*) FROM pg_catalog.pg_ts_config f WHERE f.cfgnamespace = v_nsp)
               + (SELECT count(*) FROM pg_catalog.pg_conversion v WHERE v.connamespace = v_nsp)
               + (SELECT count(*) FROM pg_catalog.pg_extension e WHERE e.extnamespace = v_nsp);
    IF v_n > 0 THEN
        RAISE EXCEPTION 'migration 0166 down refused: schema actorproof_private holds % object(s) that 0166 did not create; nothing was changed', v_n
            USING ERRCODE = 'APM02';
    END IF;
    SELECT pg_catalog.string_agg(DISTINCT CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(a.grantee) END || ':' || a.privilege_type, ', ')
      INTO v_bad
      FROM (SELECT a.* FROM pg_catalog.pg_class c, LATERAL pg_catalog.aclexplode(COALESCE(c.relacl, pg_catalog.acldefault('r', c.relowner))) a
             WHERE c.oid = v_rel AND a.grantee <> c.relowner
            UNION ALL
            SELECT a.* FROM pg_catalog.pg_namespace n, LATERAL pg_catalog.aclexplode(COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))) a
             WHERE n.oid = v_nsp AND a.grantee <> n.nspowner) a;
    IF v_bad IS NOT NULL THEN
        RAISE EXCEPTION 'migration 0166 down refused: actorproof_private (or its key table) carries grants added after 0166 (%); revoke them first, nothing was changed', v_bad
            USING ERRCODE = 'APM02';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_attribute WHERE attrelid = v_rel AND attacl IS NOT NULL) THEN
        RAISE EXCEPTION 'migration 0166 down refused: the key table carries column-level grants; nothing was changed' USING ERRCODE = 'APM02';
    END IF;
    SELECT count(*) INTO v_n FROM pg_catalog.pg_depend d
     WHERE d.refclassid = 'pg_catalog.pg_class'::regclass AND d.refobjid = v_rel
       AND d.classid IN ('pg_catalog.pg_rewrite'::regclass, 'pg_catalog.pg_trigger'::regclass, 'pg_catalog.pg_policy'::regclass);
    IF v_n > 0 THEN
        RAISE EXCEPTION 'migration 0166 down refused: % view/rule/trigger/policy object(s) depend on the key table; nothing was changed', v_n
            USING ERRCODE = 'APM02';
    END IF;
END;
$$;

DROP FUNCTION actor_proof_key_inventory();

ALTER TABLE actorproof_private.actor_proof_keys SET SCHEMA public;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'igaming_backup')
       AND EXISTS (SELECT 1 FROM pg_catalog.pg_class c, LATERAL pg_catalog.aclexplode(c.relacl) a
                    WHERE c.oid = 'public.actor_proof_nonces'::regclass AND a.privilege_type = 'SELECT'
                      AND a.grantee = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = 'igaming_backup')) THEN
        GRANT SELECT ON public.actor_proof_keys TO igaming_backup;
    END IF;
END;
$$;

DROP SCHEMA actorproof_private;

-- 0120's probe, verbatim.
CREATE OR REPLACE FUNCTION actor_proof_key_active(p_kid text) RETURNS boolean AS $$
    SELECT EXISTS (SELECT 1 FROM public.actor_proof_keys k WHERE k.kid = p_kid AND k.status = 'active');
$$ LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp;

-- 0139's verifier, verbatim.
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
    -- ADR 0115 S1 (migration 0135): rg_limit_config:* (the jurisdiction-floor four-eyes flow,
    -- section 2.1) is provable only for scope 'platform' (NULL tenant). The operation set is closed.
    IF p_operation LIKE 'rg\_limit\_config:%' THEN
        IF p_operation NOT IN ('rg_limit_config:request', 'rg_limit_config:approve', 'rg_limit_config:reject',
                               'rg_limit_config:cancel', 'rg_limit_config:apply')
           OR p_scope <> 'platform' THEN
            RAISE EXCEPTION 'actor_proof: this rg_limit_config operation is not provable for this scope' USING ERRCODE = 'AP004';
        END IF;
    END IF;
    -- ADR 0119 S5 + S6 (migration 0139): staff_lifecycle:* (the platform-staff four-eyes flow, section 10.4) is
    -- provable only for scope 'platform' (NULL tenant). The operation set is closed.
    IF p_operation LIKE 'staff\_lifecycle:%' THEN
        IF p_operation NOT IN ('staff_lifecycle:request', 'staff_lifecycle:approve', 'staff_lifecycle:reject',
                               'staff_lifecycle:cancel')
           OR p_scope <> 'platform' THEN
            RAISE EXCEPTION 'actor_proof: this staff_lifecycle operation is not provable for this scope' USING ERRCODE = 'AP004';
        END IF;
    END IF;
    -- The NULL-tenant encoding (an EMPTY tenant field) exists only for scope
    -- 'platform' and only for the K1 capability-grant, financial-policy-change,
    -- (ADR 0112, migration 0130) launch_authorisation, (ADR 0115) rg_limit_config and
    -- (ADR 0119, migration 0139) staff_lifecycle operations; every other scope and
    -- operation must carry a tenant.
    IF p_scope = 'platform' THEN
        IF p_tenant IS NOT NULL OR NOT (p_operation LIKE 'capability\_grant:%' OR p_operation LIKE 'financial\_policy\_change:%'
                                         OR p_operation LIKE 'launch\_authorisation:%'
                                         OR p_operation LIKE 'rg\_limit\_config:%'
                                         OR p_operation LIKE 'staff\_lifecycle:%') THEN
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
