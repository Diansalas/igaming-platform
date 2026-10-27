-- Down of 0177 (DEV/STAGING ONLY, per the migrate-down policy). Refused (LC095) while any licensing change request exists
-- (the decision records would be lost). Otherwise it drops exactly what the up created, restores the 0005
-- tenant_jurisdiction_configs policy and the 0175 verifier body (whole-schema snapshot test).
SET LOCAL lock_timeout = '5s';

DO $$
DECLARE
    v_any boolean;
BEGIN
    -- FORCE RLS would hide every row from a session without the platform GUCs: lift it for the owner's count only.
    ALTER TABLE licensing_change_requests NO FORCE ROW LEVEL SECURITY;
    SELECT EXISTS (SELECT 1 FROM licensing_change_requests) INTO v_any;
    ALTER TABLE licensing_change_requests FORCE ROW LEVEL SECURITY;
    IF v_any THEN
        RAISE EXCEPTION 'migration 0177 down refused: licensing change requests exist' USING ERRCODE = 'LC095';
    END IF;
END
$$;

DROP TRIGGER zz_licensing_change_governed ON licence_country_ceilings;
DROP TRIGGER zz_licensing_change_governed ON operating_country_policies;
DROP TRIGGER zz_licensing_change_governed ON tenant_jurisdiction_configs;
DROP TRIGGER zz_licensing_change_governed ON licences;
DROP TRIGGER zz_licensing_change_governed ON tenants;

DROP POLICY tjc_tenant_read ON tenant_jurisdiction_configs;
DROP POLICY tjc_platform_read ON tenant_jurisdiction_configs;
DROP POLICY tjc_platform_insert ON tenant_jurisdiction_configs;
DROP POLICY tjc_platform_close ON tenant_jurisdiction_configs;
DROP POLICY tjc_owner_tenant_insert ON tenant_jurisdiction_configs;
DROP POLICY tjc_owner_tenant_close ON tenant_jurisdiction_configs;
CREATE POLICY tenant_isolation ON tenant_jurisdiction_configs
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
DROP INDEX uq_tenant_jurisdiction_configs_open;
DROP TRIGGER tenant_jurisdiction_configs_append_only ON tenant_jurisdiction_configs;
DROP TRIGGER tenant_jurisdiction_configs_deny_truncate ON tenant_jurisdiction_configs;
DROP FUNCTION tenant_jurisdiction_configs_append_only();

DROP TABLE licensing_change_approvals;
DROP TABLE licensing_change_requests;
DROP TABLE licensing_change_kinds;

DROP FUNCTION licensing_change_governed_write_guard();
DROP FUNCTION licensing_change_approvals_guard();
DROP FUNCTION licensing_change_requests_guard();
DROP FUNCTION licensing_change_deny_mutation();
DROP FUNCTION licensing_change_fresh_approvals(uuid);
DROP FUNCTION licensing_change_row_matches(text, jsonb, text, uuid, text, text, jsonb, jsonb);
DROP FUNCTION licensing_change_classify(text, jsonb);
DROP FUNCTION licensing_change_payload_hash(text, jsonb, text);
DROP FUNCTION licensing_change_is_text_array(jsonb, int);
DROP FUNCTION licensing_change_is_opt_text(jsonb, int);
DROP FUNCTION licensing_change_is_text(jsonb, int);
DROP FUNCTION licensing_change_is_uuid(jsonb);
DROP FUNCTION licensing_change_keys_are(jsonb, text[]);
DROP FUNCTION licensing_change_actor();

-- The 0175 verifier body, unchanged.
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
    IF p_scope NOT IN ('tenant', 'platform_acting', 'platform', 'system') THEN
        RAISE EXCEPTION 'actor_proof: scope is not provable' USING ERRCODE = 'AP004';
    END IF;
    -- F4-R1 (migration 0171): the WD-RG-1 dispatch gate ALLOW is a SYSTEM attestation, not a staff action. The scope
    -- 'system' is provable ONLY for the single operation withdrawal_dispatch_gate:allow, with a tenant (checked below
    -- like every non-platform scope) and ONLY for the fixed system actor of the dispatch gate; that operation is
    -- provable for no other scope. The operation set is closed.
    -- 0175 (closure security review F-1): the scope 'system' is ALSO provable for exactly one more operation,
    -- deposit_reversal_tombstone:write, ONLY for its own fixed system actor (...00de0175, distinct from the dispatch
    -- gate's); that operation is provable for no other scope. Each operation is bound to its own actor.
    IF p_scope = 'system' OR p_operation LIKE 'withdrawal\_dispatch\_gate:%' OR p_operation LIKE 'deposit\_reversal\_tombstone:%' THEN
        IF NOT ((p_operation = 'withdrawal_dispatch_gate:allow' AND p_scope = 'system'
                 AND p_actor = '00000000-0000-0000-0000-00000f4d1500'::uuid)
             OR (p_operation = 'deposit_reversal_tombstone:write' AND p_scope = 'system'
                 AND p_actor = '00000000-0000-0000-0000-0000000de175'::uuid)) THEN
            RAISE EXCEPTION 'actor_proof: this operation is not provable for this scope or actor' USING ERRCODE = 'AP004';
        END IF;
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
    SELECT k.secret INTO v_secret FROM actorproof_private.actor_proof_keys k WHERE k.kid = v_parts[2] AND k.status = 'active';
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
