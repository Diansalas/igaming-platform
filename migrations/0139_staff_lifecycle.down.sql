-- Reverses 0139 (ADR 0119 S5 + S6; DEV/STAGING ONLY, per the migrate-down policy).
--
-- Refuses (SF094), BEFORE changing anything, while any of these exists (a staff lifecycle record is never dropped by a
-- migration, and the old schema cannot represent the new states):
--   - a staff_users row whose status is 'invited' or 'offboarded' (the old CHECK admits only active / suspended, and
--     an offboarded row has no password, which the old NOT NULL refuses);
--   - a staff_lifecycle_events, staff_lifecycle_requests or staff_lifecycle_approvals row;
--   - an alert of Kind staff.platform_break_glass_used.
-- FORCE ROW LEVEL SECURITY is lifted inside this transaction for the counts only (the 0137 / 0138 down precedent) and
-- restored; a refusal rolls the lift back.
--
-- Everything else returns to 0138 + 0140-0142 + 0151 exactly: actor_proof_require gets the 0135 body back, the
-- staff_users UPDATE grant becomes table-level again, the staff_users columns, triggers and policies are dropped.
SET LOCAL lock_timeout = '5s';

ALTER TABLE staff_users NO FORCE ROW LEVEL SECURITY;
ALTER TABLE staff_lifecycle_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE staff_lifecycle_requests NO FORCE ROW LEVEL SECURITY;
ALTER TABLE staff_lifecycle_approvals NO FORCE ROW LEVEL SECURITY;
ALTER TABLE alerts NO FORCE ROW LEVEL SECURITY;
DO $$
DECLARE
    v_staff  bigint;
    v_events bigint;
    v_reqs   bigint;
    v_apps   bigint;
    v_alerts bigint;
BEGIN
    -- (Review fix F2: a duplicate live / offboarded email pair needs an offboarded row, so this count also refuses
    -- every state the restored UNIQUE NULLS NOT DISTINCT (tenant_id, email) could not hold.)
    SELECT pg_catalog.count(*) INTO v_staff FROM public.staff_users WHERE status IN ('invited', 'offboarded');
    SELECT pg_catalog.count(*) INTO v_events FROM public.staff_lifecycle_events;
    SELECT pg_catalog.count(*) INTO v_reqs FROM public.staff_lifecycle_requests;
    SELECT pg_catalog.count(*) INTO v_apps FROM public.staff_lifecycle_approvals;
    SELECT pg_catalog.count(*) INTO v_alerts FROM public.alerts WHERE kind = 'staff.platform_break_glass_used';
    IF v_staff > 0 OR v_events > 0 OR v_reqs > 0 OR v_apps > 0 OR v_alerts > 0 THEN
        RAISE EXCEPTION 'migration 0139 down refused: staff lifecycle data exists (invited_or_offboarded_staff=%, events=%, requests=%, approvals=%, break_glass_alerts=%). Nothing was changed (ADR 0119 S5 + S6); on a local / dev database rebuild from migrations.',
            v_staff, v_events, v_reqs, v_apps, v_alerts USING ERRCODE = 'SF094';
    END IF;
END
$$;
ALTER TABLE alerts FORCE ROW LEVEL SECURITY;
ALTER TABLE staff_users FORCE ROW LEVEL SECURITY;

-- 9. Grants: back to the table-level UPDATE on staff_users.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE UPDATE (status, role, password_hash, status_changed_at, offboarded_at, person_id, display_name, updated_at) ON staff_users FROM igaming_runtime';
        EXECUTE 'GRANT UPDATE ON staff_users TO igaming_runtime';
        EXECUTE 'GRANT DELETE ON staff_users TO igaming_runtime';
    END IF;
END
$$;

-- 8. The alert Kind (and the widened CHECK)
DO $$
BEGIN
    ALTER TABLE alert_kinds DISABLE TRIGGER alert_kinds_deny_update_delete;
    CREATE POLICY alert_kinds_unseed_0139 ON alert_kinds
        FOR DELETE TO CURRENT_USER USING (kind = 'staff.platform_break_glass_used');
    DELETE FROM alert_kinds WHERE kind = 'staff.platform_break_glass_used';
    DROP POLICY alert_kinds_unseed_0139 ON alert_kinds;
    ALTER TABLE alert_kinds ENABLE TRIGGER alert_kinds_deny_update_delete;
END
$$;
ALTER TABLE alert_kinds DROP CONSTRAINT alert_kinds_check1;
ALTER TABLE alert_kinds ADD CONSTRAINT alert_kinds_check1 CHECK (
    requires_subject = true
    OR kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed',
                'staff.platform_mfa_integrity', 'staff.platform_session_integrity', 'staff.platform_mfa_hard_locked',
                'staff.platform_mfa_recovery_used', 'staff.platform_mfa_failure_pattern'));

-- 7. actor_proof_require: the 0135 body.
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
    -- The NULL-tenant encoding (an EMPTY tenant field) exists only for scope
    -- 'platform' and only for the K1 capability-grant, financial-policy-change and
    -- (ADR 0112, migration 0130) launch_authorisation operations; every other scope
    -- and operation must carry a tenant.
    IF p_scope = 'platform' THEN
        IF p_tenant IS NOT NULL OR NOT (p_operation LIKE 'capability\_grant:%' OR p_operation LIKE 'financial\_policy\_change:%'
                                         OR p_operation LIKE 'launch\_authorisation:%'
                                         OR p_operation LIKE 'rg\_limit\_config:%') THEN
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

-- 6. / 5. The lifecycle tables (events first: they reference the requests).
DROP TABLE staff_lifecycle_events;
DROP TABLE staff_lifecycle_approvals;
DROP TABLE staff_lifecycle_requests;
DROP FUNCTION sf_lifecycle_event_actor();
DROP FUNCTION actor_proof_staff_lifecycle_approvals_guard();
DROP FUNCTION actor_proof_staff_lifecycle_requests_guard();
DROP FUNCTION sf_deny_update();
DROP FUNCTION sf_lifecycle_no_executing_at_commit();
DROP FUNCTION sf_lifecycle_approval_guard();
DROP FUNCTION sf_lifecycle_request_guard();
DROP FUNCTION sf_lifecycle_actor_session(text);
DROP FUNCTION staff_lifecycle_request_digest(text, uuid, text, uuid, text, text);

-- 4. The s9.5(b) pair.
DROP POLICY staff_actor_self_lock ON staff_users;
DROP POLICY staff_actor_self_select ON staff_users;

-- 3. The staff_users triggers.
DROP TRIGGER sf_20_last_admin ON staff_users;
DROP TRIGGER sf_14_staff_person_required ON staff_users;
DROP TRIGGER sf_13_staff_self_target ON staff_users;
DROP TRIGGER sf_12_staff_version ON staff_users;
DROP TRIGGER sf_11_staff_immutable ON staff_users;
DROP TRIGGER sf_10_staff_transition ON staff_users;
DROP FUNCTION sf_last_admin();
DROP FUNCTION sf_staff_person_required();
DROP FUNCTION sf_staff_self_target();
DROP FUNCTION sf_staff_version();
DROP FUNCTION sf_staff_immutable();
DROP FUNCTION sf_staff_transition();

-- 2. Columns and CHECKs.
DROP INDEX staff_users_platform_live_person;
DROP INDEX staff_users_live_email;
ALTER TABLE staff_users ADD CONSTRAINT staff_users_tenant_id_email_key UNIQUE NULLS NOT DISTINCT (tenant_id, email);
ALTER TABLE staff_users DROP CONSTRAINT staff_users_offboarded_at_state;
ALTER TABLE staff_users DROP COLUMN invited_by;
ALTER TABLE staff_users DROP COLUMN offboarded_at;
ALTER TABLE staff_users DROP COLUMN status_changed_at;
ALTER TABLE staff_users DROP COLUMN staff_version;
ALTER TABLE staff_users DROP CONSTRAINT staff_users_offboarded_no_password;
ALTER TABLE staff_users DROP CONSTRAINT staff_users_password_state;
ALTER TABLE staff_users ALTER COLUMN password_hash SET NOT NULL;
ALTER TABLE staff_users DROP CONSTRAINT staff_users_status_check;
ALTER TABLE staff_users ADD CONSTRAINT staff_users_status_check CHECK (status IN ('active', 'suspended'));
