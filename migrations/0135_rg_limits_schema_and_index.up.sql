-- ADR 0115 SLICE S1 (Responsible Gaming limits: schema + index + database environment anchor).
-- RG / MONEY-ADJACENT SCHEMA: FOR `architect`, `security`, `ledger-finance`, `qa` REVIEW BEFORE MERGE.
--
-- NUMBERING: 0135, allocated by the orchestrator (0133 is the ADR 0113 slice-2 migration, 0134 is
-- reserved). Until those merge, `migrate verify` reports the 0133/0134 gap; nothing else depends on it.
--
-- NO ENFORCEMENT. Nothing in this file refuses a deposit, a wager, a launch or a login: the
-- enforcement boundary rg.EvaluateAction and its hook points are slice S4. NO LEGAL VALUE: this file
-- seeds no configuration row (compliance supplies the content, ADR 0115 OQ-2). The only floor rows
-- that exist anywhere are the SYNTHETIC-NOT-LEGAL fixture rows of internal/testsupport/rgfix, which
-- the guard below refuses on any database not anchored as development / test.
--
-- What it adds:
--   0. Pre-flight: refuses (RL098) if any object this file creates already exists, if the
--      player_restrictions type CHECK / player_self_insert policy are not 0037's, or if a pre-built
--      index of section 9 is INVALID or differently defined (DEVOPS-0107-INDEX-WINDOW-1 practice).
--   1. database_environment: the NON-FORGEABLE environment anchor (one row, written only by the table
--      owner; missing = production). rg_synthetic_config_allowed() requires the anchor AND the new
--      per-connection GUC app.environment to both say development / test. The GUC alone is forgeable
--      by any session (set_config), so it can only NARROW; the anchor is the binding control.
--   2. actor_proof_require: 0130's body plus the closed rg_limit_config:* family (scope platform only).
--   3. rg_limit_config_change_requests: the floor four-eyes request (platform scope, signed proofs).
--   4. rg_limit_configs: versioned, append-only jurisdiction floors (tenant_id NULL) and operator bounds;
--      shape CHECKs (every field of the kind explicit, every other field absent), no-backdating (5 s),
--      synthetic guard, floor four-eyes (approved request, distinct Persons, payload hash recomputed from
--      the inserted row, signed apply proof), operator tighten-only / widen-only, read-time composition
--      (strictest / UNION / OR) in rg_limit_config_compose().
--   5. player_rg_limit_changes: append-only player limit history; a BEFORE INSERT trigger takes L0.4,
--      sets requested_at / effective_at from clock_timestamp(), forces actor / person / brand and calls
--      the ONE classification function rg_limit_change_classify() (which itself calls the ONE
--      realization function rg_limit_realized()); a differing asserted change_kind or any client time
--      is refused.
--   6. player_restrictions: restriction_type gains 'time_out' (bounded: ends_at required) and the
--      player_self_insert policy is extended for it (platform_person, or tenant_person pinned to
--      app.tenant_id). Locks, RLS for everything else and immutability are unchanged.
--   7. casino_launch_sessions.auth_session_root_started_at (nullable; written by S4).
--   8. Grants to igaming_runtime (mirrored by deploy/init-app-role.sql).
--   9. ledger_entries (tenant_id, player_account_id, asset_code, created_at) and deposit_intents
--      (tenant_id, player_account_id, asset_code, created_at) indexes. This runner applies every file
--      in ONE transaction, so CREATE INDEX CONCURRENTLY cannot run here. Production practice
--      (docs/runbooks/expand-contract-migrations.md section 5): the migration operator pre-builds both
--      indexes CONCURRENTLY out of band; the pre-flight then accepts them only if VALID and identically
--      defined and the CREATE INDEX IF NOT EXISTS below is a no-op. Without a pre-build the index is
--      built here, non-concurrently (a SHARE lock on the table for the build, as 0092 / 0107 / 0121).
--
-- SQLSTATE class RL (new): RL001 actor session, RL003 backdated, RL004 operator row loosens / narrows,
-- RL005 operator row with no floor in force, RL006 floor without a usable approved request, RL007
-- four-eyes person / payload binding, RL008 synthetic row refused, RL009 append-only, RL010 request
-- state machine, RL011 calendar_tz, RL020 client-asserted time / change_kind, RL021 no change, RL022
-- cancel / confirm invalid, RL023 increase / removal with no configuration in force, RL024 isolation
-- level, RL025 tenant / actor / self-target, RL098 pre-flight, RL099 down refused. AP0xx from the verifier.
--
-- No function is SECURITY DEFINER except the existing verifier; every function pins search_path.

-- =========================================================================
-- 0. Pre-flight
-- =========================================================================
DO $$
DECLARE
    v_def   text;
    v_n     int;
    r       record;
BEGIN
    IF to_regclass('public.database_environment') IS NOT NULL
       OR to_regclass('public.rg_limit_config_change_requests') IS NOT NULL
       OR to_regclass('public.rg_limit_configs') IS NOT NULL
       OR to_regclass('public.player_rg_limit_changes') IS NOT NULL THEN
        RAISE EXCEPTION 'migration 0135 pre-flight: an RG limit table already exists' USING ERRCODE = 'RL098';
    END IF;
    SELECT count(*) INTO v_n FROM pg_catalog.pg_proc
     WHERE pronamespace = 'public'::regnamespace AND (proname LIKE 'rg\_%' OR proname LIKE 'player\_rg\_%');
    IF v_n > 0 THEN
        RAISE EXCEPTION 'migration 0135 pre-flight: % rg_* function(s) already exist', v_n USING ERRCODE = 'RL098';
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public'
                AND table_name = 'casino_launch_sessions' AND column_name = 'auth_session_root_started_at') THEN
        RAISE EXCEPTION 'migration 0135 pre-flight: casino_launch_sessions.auth_session_root_started_at already exists' USING ERRCODE = 'RL098';
    END IF;
    SELECT pg_catalog.pg_get_constraintdef(oid) INTO v_def FROM pg_catalog.pg_constraint
     WHERE conrelid = 'public.player_restrictions'::regclass AND conname = 'player_restrictions_restriction_type_check';
    IF v_def IS DISTINCT FROM 'CHECK ((restriction_type = ''self_exclusion''::text))' THEN
        RAISE EXCEPTION 'migration 0135 pre-flight: player_restrictions_restriction_type_check is not the 0037 definition (%)', v_def USING ERRCODE = 'RL098';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_constraint WHERE conrelid = 'public.player_restrictions'::regclass
                AND conname = 'player_restrictions_time_out_bounded') THEN
        RAISE EXCEPTION 'migration 0135 pre-flight: player_restrictions_time_out_bounded already exists' USING ERRCODE = 'RL098';
    END IF;
    SELECT count(*) INTO v_n FROM pg_catalog.pg_policies
     WHERE schemaname = 'public' AND tablename = 'player_restrictions' AND policyname = 'player_self_insert'
       AND cmd = 'INSERT' AND with_check LIKE '((tenant_id IS NULL) AND (brand_id IS NULL) AND (restriction_type = ''self_exclusion''::text)%';
    IF v_n <> 1 THEN
        RAISE EXCEPTION 'migration 0135 pre-flight: player_self_insert on player_restrictions is not the 0037 policy' USING ERRCODE = 'RL098';
    END IF;
    -- A pre-built (CONCURRENTLY) index is accepted only if VALID, READY and defined exactly as below.
    FOR r IN SELECT * FROM (VALUES
        ('idx_ledger_entries_tenant_player_asset_created',
         'CREATE INDEX idx_ledger_entries_tenant_player_asset_created ON public.ledger_entries USING btree (tenant_id, player_account_id, asset_code, created_at)'),
        ('idx_deposit_intents_tenant_player_asset_created',
         'CREATE INDEX idx_deposit_intents_tenant_player_asset_created ON public.deposit_intents USING btree (tenant_id, player_account_id, asset_code, created_at)')
    ) AS v(name, def)
    LOOP
        IF to_regclass('public.' || r.name) IS NOT NULL THEN
            SELECT count(*) INTO v_n FROM pg_catalog.pg_index i
             WHERE i.indexrelid = ('public.' || r.name)::regclass AND i.indisvalid AND i.indisready
               AND pg_catalog.pg_get_indexdef(i.indexrelid) = r.def;
            IF v_n <> 1 THEN
                RAISE EXCEPTION 'migration 0135 pre-flight: index % exists but is INVALID or not defined as expected; DROP INDEX CONCURRENTLY it and rebuild (runbook expand-contract-migrations section 5)', r.name
                    USING ERRCODE = 'RL098';
            END IF;
        END IF;
    END LOOP;
END
$$;

-- =========================================================================
-- 1. The database environment anchor
-- =========================================================================
-- One row at most. Written ONLY by the table owner (the migration / provisioning role): the runtime
-- role has SELECT only, RLS is enabled with no write policy, and the insert guard below refuses any
-- role that is not a member of the owner role. The row can never change or disappear (deny trigger,
-- binding on the owner too while enabled). NO ROW = PRODUCTION. A production database never carries a
-- development / test row; the provisioning runbook may insert 'production' to make that permanent.
CREATE TABLE database_environment (
    singleton    BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    environment  TEXT NOT NULL CHECK (environment IN ('production', 'staging', 'development', 'test')),
    recorded_at  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    recorded_by  TEXT NOT NULL DEFAULT current_user
);
ALTER TABLE database_environment ENABLE ROW LEVEL SECURITY;
CREATE POLICY database_environment_read ON database_environment FOR SELECT USING (true);

CREATE FUNCTION database_environment_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'database_environment is write-once: % is not permitted', TG_OP USING ERRCODE = 'RL009';
    END IF;
    IF NOT pg_catalog.pg_has_role(current_user,
            (SELECT c.relowner FROM pg_catalog.pg_class c WHERE c.oid = 'public.database_environment'::regclass), 'USAGE') THEN
        RAISE EXCEPTION 'database_environment: only the table owner may record the environment' USING ERRCODE = 'RL009';
    END IF;
    NEW.recorded_at := pg_catalog.clock_timestamp();
    NEW.recorded_by := current_user;
    RETURN NEW;
END
$$;
CREATE TRIGGER database_environment_guard
    BEFORE INSERT OR UPDATE OR DELETE ON database_environment
    FOR EACH ROW EXECUTE FUNCTION database_environment_guard();
CREATE TRIGGER database_environment_no_truncate
    BEFORE TRUNCATE ON database_environment
    FOR EACH STATEMENT EXECUTE FUNCTION database_environment_guard();

-- The anchored environment ('production' when no row exists).
CREATE FUNCTION rg_database_environment() RETURNS text
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT COALESCE((SELECT e.environment FROM public.database_environment e WHERE e.singleton), 'production')
$$;

-- Synthetic (SYNTHETIC-NOT-LEGAL) configuration is admissible only when BOTH the anchor and the
-- connection's app.environment (set per connection from cfg.GuardEnvironment(); absent = production)
-- say development or test. AND composition: the forgeable GUC can only narrow, never widen.
CREATE FUNCTION rg_synthetic_config_allowed() RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT public.rg_database_environment() IN ('development', 'test')
       AND COALESCE(NULLIF(pg_catalog.current_setting('app.environment', true), '') IN ('development', 'test'), false)
$$;

-- =========================================================================
-- 2. The signed actor proof verifier: 0130's body plus rg_limit_config:*
-- =========================================================================
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

-- =========================================================================
-- 3. Helpers
-- =========================================================================
-- The session actor for RG configuration writes: exactly a tenant principal or exactly a platform
-- principal, an ACTIVE staff row; never an acting / player / service / mixed session (RL001).
CREATE FUNCTION rg_actor_session(
    OUT actor uuid, OUT scope text, OUT tenant uuid, OUT person_id uuid
) LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_tenant           text := NULLIF(current_setting('app.tenant_id', true), '');
    v_principal        text := NULLIF(current_setting('app.principal_id', true), '');
    v_platform         text := NULLIF(current_setting('app.platform_admin_principal_id', true), '');
    v_player           text := NULLIF(current_setting('app.player_account_id', true), '');
    v_service          text := NULLIF(current_setting('app.platform_service_id', true), '');
    v_acting_tenant    text := NULLIF(current_setting('app.acting_tenant_id', true), '');
    v_acting_principal text := NULLIF(current_setting('app.acting_platform_principal_id', true), '');
    rec RECORD;
BEGIN
    IF v_acting_tenant IS NOT NULL OR v_acting_principal IS NOT NULL OR v_player IS NOT NULL OR v_service IS NOT NULL THEN
        RAISE EXCEPTION 'rg limits: this session may not write RG configuration' USING ERRCODE = 'RL001';
    END IF;
    IF v_tenant IS NOT NULL AND v_principal IS NOT NULL AND v_platform IS NULL THEN
        SELECT su.id, su.person_id, su.status INTO rec
          FROM public.staff_users su WHERE su.id = v_principal::uuid AND su.tenant_id = v_tenant::uuid;
        IF NOT FOUND OR rec.status <> 'active' OR rec.person_id IS NULL THEN
            RAISE EXCEPTION 'rg limits: the tenant principal is not an active staff member of the tenant' USING ERRCODE = 'RL001';
        END IF;
        actor := rec.id; scope := 'tenant'; tenant := v_tenant::uuid; person_id := rec.person_id;
        RETURN;
    END IF;
    IF v_platform IS NOT NULL AND v_tenant IS NULL AND v_principal IS NULL THEN
        SELECT su.id, su.person_id, su.status INTO rec
          FROM public.staff_users su WHERE su.id = v_platform::uuid AND su.tenant_id IS NULL;
        IF NOT FOUND OR rec.status <> 'active' OR rec.person_id IS NULL THEN
            RAISE EXCEPTION 'rg limits: the platform principal is not an active platform staff member' USING ERRCODE = 'RL001';
        END IF;
        actor := rec.id; scope := 'platform'; tenant := NULL; person_id := rec.person_id;
        RETURN;
    END IF;
    RAISE EXCEPTION 'rg limits: the session is not a valid tenant or platform principal session' USING ERRCODE = 'RL001';
END
$$;

-- A fixed (DST-insensitive) positive interval: hours / minutes / seconds only (ADR 0115 1.2: never
-- '1 day'). Also makes every interval's canonical encoding (epoch seconds) exact.
CREATE FUNCTION rg_fixed_interval(i interval) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT i > interval '0' AND date_part('year', i) = 0 AND date_part('month', i) = 0 AND date_part('day', i) = 0
$$;

-- A non-empty, NULL-free, duplicate-free subset of p_allowed.
CREATE FUNCTION rg_valid_text_set(p text[], p_allowed text[]) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT p IS NOT NULL AND cardinality(p) >= 1 AND array_position(p, NULL) IS NULL AND p <@ p_allowed
       AND cardinality(p) = (SELECT count(DISTINCT x) FROM unnest(p) AS x)
$$;

-- A non-empty, NULL-free, duplicate-free set of fixed positive intervals.
CREATE FUNCTION rg_valid_interval_set(p interval[]) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT p IS NOT NULL AND cardinality(p) >= 1 AND array_position(p, NULL) IS NULL
       AND (SELECT bool_and(public.rg_fixed_interval(x)) FROM unnest(p) AS x)
       AND cardinality(p) = (SELECT count(DISTINCT x) FROM unnest(p) AS x)
$$;

-- Scope strictness (broader reach = more protective).
CREATE FUNCTION rg_scope_rank(p text) RETURNS int
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT CASE p WHEN 'account' THEN 0 WHEN 'tenant_person' THEN 1 WHEN 'platform_person' THEN 2 END
$$;

CREATE FUNCTION rg_deny_mutation() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted', TG_TABLE_NAME, TG_OP USING ERRCODE = 'RL009';
END
$$;

-- =========================================================================
-- 4. rg_limit_config_change_requests (floor four-eyes, ADR 0115 2.1 / security condition 9)
-- =========================================================================
CREATE TABLE rg_limit_config_change_requests (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    jurisdiction_code       TEXT NOT NULL REFERENCES jurisdictions (code) ON DELETE RESTRICT,
    control_kind            TEXT NOT NULL CHECK (control_kind IN ('deposit', 'loss', 'wager', 'session_duration', 'reality_check_interval', 'time_out')),
    asset_code              TEXT NULL REFERENCES assets (code) ON DELETE RESTRICT,
    -- The proposed floor row (its content columns, as JSON) the approver reviews; payload_hash is
    -- derived from it BY THE DATABASE (rg_limit_config_payload_hash), never trusted from the caller.
    proposed_config         JSONB NOT NULL CHECK (jsonb_typeof(proposed_config) = 'object' AND octet_length(proposed_config::text) <= 16384),
    payload_hash            TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    reason_code             TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64),
    requested_by            UUID NOT NULL,
    requested_by_person_id  UUID NOT NULL,
    status                  TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'cancelled')),
    decided_by              UUID NULL,
    decided_by_person_id    UUID NULL,
    decided_at              TIMESTAMPTZ NULL,
    created_at              TIMESTAMPTZ NOT NULL,
    expires_at              TIMESTAMPTZ NOT NULL,
    CONSTRAINT rg_limit_config_change_requests_decided CHECK ((status = 'pending') = (decided_at IS NULL)),
    CONSTRAINT rg_limit_config_change_requests_decider CHECK (status = 'pending' OR (decided_by IS NOT NULL AND decided_by_person_id IS NOT NULL)),
    CONSTRAINT rg_limit_config_change_requests_four_eyes CHECK (status <> 'approved' OR decided_by_person_id <> requested_by_person_id)
);
CREATE INDEX idx_rg_limit_config_change_requests_scope
    ON rg_limit_config_change_requests (jurisdiction_code, control_kind, status);

-- What the requester's proof binds (the payload_hash is the hash of the proposed floor row).
CREATE FUNCTION rg_limit_config_request_digest(r rg_limit_config_change_requests) RETURNS text
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT public.k2_sha256_hex(public.k2_canonical(r.jurisdiction_code, r.control_kind, r.asset_code, r.payload_hash, r.reason_code))
$$;


ALTER TABLE rg_limit_config_change_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE rg_limit_config_change_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY platform_only ON rg_limit_config_change_requests
    FOR ALL
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- =========================================================================
-- 5. rg_limit_configs (ADR 0115 2.1)
-- =========================================================================
-- Field encoding: a field that may be "an attested none" is a value column plus a <field>_none
-- boolean (exactly one meaningful); allowed_durations' none is allowed_durations_any. A field that
-- does not apply to the row's control_kind is NULL; a field that applies is NOT NULL (CHECKs below),
-- so a partial configuration cannot exist.
CREATE TABLE rg_limit_configs (
    id                                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    jurisdiction_code                 TEXT NOT NULL REFERENCES jurisdictions (code) ON DELETE RESTRICT,
    tenant_id                         UUID NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    brand_id                          UUID NULL,
    control_kind                      TEXT NOT NULL CHECK (control_kind IN ('deposit', 'loss', 'wager', 'session_duration', 'reality_check_interval', 'time_out')),
    asset_code                        TEXT NULL REFERENCES assets (code) ON DELETE RESTRICT,
    effective_from                    TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    enabled                           BOOLEAN NOT NULL,
    mandatory                         BOOLEAN NOT NULL,
    ceiling                           NUMERIC(38, 0) NULL CHECK (ceiling > 0),
    ceiling_none                      BOOLEAN NULL,
    allowed_periods                   TEXT[] NULL,
    calendar_tz                       TEXT NULL CHECK (octet_length(calendar_tz) BETWEEN 1 AND 64),
    week_start                        SMALLINT NULL CHECK (week_start BETWEEN 1 AND 7),
    increase_cooldown                 INTERVAL NULL,
    increase_requires_reconfirmation  BOOLEAN NULL,
    scope                             TEXT NOT NULL CHECK (scope IN ('account', 'tenant_person', 'platform_person')),
    count_bonus_funded                BOOLEAN NULL,
    loss_computation                  TEXT NULL CHECK (loss_computation = 'net_of_wins'),
    loss_may_go_negative              BOOLEAN NULL,
    loss_may_go_negative_none         BOOLEAN NULL,
    post_session_break                INTERVAL NULL,
    post_session_break_none           BOOLEAN NULL,
    concurrent_chain_aggregation      BOOLEAN NULL,
    concurrent_chain_aggregation_none BOOLEAN NULL,
    pending_intent_ttl                INTERVAL NULL,
    pending_intent_ttl_none           BOOLEAN NULL,
    allowed_durations                 INTERVAL[] NULL,
    allowed_durations_any             BOOLEAN NULL,
    allowed_intervals                 INTERVAL[] NULL,
    reason_code                       TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64),
    attestation_ref                   TEXT NOT NULL CHECK (octet_length(attestation_ref) BETWEEN 1 AND 200),
    change_request_id                 UUID NULL UNIQUE REFERENCES rg_limit_config_change_requests (id) ON DELETE RESTRICT,
    created_by_actor_type             TEXT NOT NULL CHECK (created_by_actor_type = 'staff'),
    created_by_actor_id               UUID NOT NULL,
    created_by_person_id              UUID NOT NULL,
    created_at                        TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id) ON DELETE RESTRICT,
    CONSTRAINT rg_limit_configs_scope_key UNIQUE NULLS NOT DISTINCT (jurisdiction_code, tenant_id, brand_id, control_kind, asset_code, effective_from),
    CONSTRAINT rg_limit_configs_brand_needs_tenant CHECK (brand_id IS NULL OR tenant_id IS NOT NULL),
    CONSTRAINT rg_limit_configs_floor_has_request CHECK ((tenant_id IS NULL) = (change_request_id IS NOT NULL)),
    -- Limit kinds: ceiling (value | none), cooldown (fixed, > 0, never none), reconfirmation, scope.
    CONSTRAINT rg_limit_configs_limit_fields CHECK (CASE WHEN control_kind IN ('deposit', 'loss', 'wager', 'session_duration')
        THEN ceiling_none IS NOT NULL AND (ceiling IS NULL) = ceiling_none
             AND increase_cooldown IS NOT NULL AND rg_fixed_interval(increase_cooldown)
             AND increase_requires_reconfirmation IS NOT NULL AND scope IN ('account', 'tenant_person')
        ELSE ceiling IS NULL AND ceiling_none IS NULL AND increase_cooldown IS NULL AND increase_requires_reconfirmation IS NULL END),
    -- Amount kinds: periods, calendar timezone, week start; per-asset or all-assets (an all-assets
    -- row has no numeric ceiling: minor units are per asset, there is no FX).
    CONSTRAINT rg_limit_configs_amount_fields CHECK (CASE WHEN control_kind IN ('deposit', 'loss', 'wager')
        THEN rg_valid_text_set(allowed_periods, ARRAY['rolling_24h', 'rolling_7d', 'rolling_30d', 'calendar_day', 'calendar_week', 'calendar_month'])
             AND calendar_tz IS NOT NULL AND week_start IS NOT NULL
             AND (asset_code IS NOT NULL OR ceiling_none)
        ELSE allowed_periods IS NULL AND calendar_tz IS NULL AND week_start IS NULL AND asset_code IS NULL END),
    CONSTRAINT rg_limit_configs_bonus_fields CHECK (CASE WHEN control_kind IN ('loss', 'wager')
        THEN count_bonus_funded IS NOT NULL ELSE count_bonus_funded IS NULL END),
    CONSTRAINT rg_limit_configs_loss_fields CHECK (CASE WHEN control_kind = 'loss'
        THEN loss_computation IS NOT NULL AND loss_may_go_negative_none IS NOT NULL
             AND (loss_may_go_negative IS NULL) = loss_may_go_negative_none
        ELSE loss_computation IS NULL AND loss_may_go_negative IS NULL AND loss_may_go_negative_none IS NULL END),
    CONSTRAINT rg_limit_configs_session_fields CHECK (CASE WHEN control_kind = 'session_duration'
        THEN post_session_break_none IS NOT NULL AND (post_session_break IS NULL) = post_session_break_none
             AND (post_session_break IS NULL OR rg_fixed_interval(post_session_break))
             AND concurrent_chain_aggregation_none IS NOT NULL
             AND (concurrent_chain_aggregation IS NULL) = concurrent_chain_aggregation_none
        ELSE post_session_break IS NULL AND post_session_break_none IS NULL
             AND concurrent_chain_aggregation IS NULL AND concurrent_chain_aggregation_none IS NULL END),
    CONSTRAINT rg_limit_configs_deposit_fields CHECK (CASE WHEN control_kind = 'deposit'
        THEN pending_intent_ttl_none IS NOT NULL AND (pending_intent_ttl IS NULL) = pending_intent_ttl_none
             AND (pending_intent_ttl IS NULL OR rg_fixed_interval(pending_intent_ttl))
        ELSE pending_intent_ttl IS NULL AND pending_intent_ttl_none IS NULL END),
    CONSTRAINT rg_limit_configs_time_out_fields CHECK (CASE WHEN control_kind = 'time_out'
        THEN allowed_durations_any IS NOT NULL
             AND CASE WHEN allowed_durations_any THEN allowed_durations IS NULL ELSE rg_valid_interval_set(allowed_durations) END
        ELSE allowed_durations IS NULL AND allowed_durations_any IS NULL END),
    CONSTRAINT rg_limit_configs_reality_check_fields CHECK (CASE WHEN control_kind = 'reality_check_interval'
        THEN rg_valid_interval_set(allowed_intervals) AND scope = 'account'
        ELSE allowed_intervals IS NULL END)
);
CREATE INDEX idx_rg_limit_configs_resolve
    ON rg_limit_configs (jurisdiction_code, control_kind, tenant_id, brand_id, asset_code, effective_from DESC);

-- The floor change-request guard (declared here: it uses the rg_limit_configs row type).
CREATE FUNCTION rg_limit_config_change_requests_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor    RECORD;
    v_proposed public.rg_limit_configs;
    v_hash     text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'rg_limit_config_change_requests is append-only: DELETE is not permitted' USING ERRCODE = 'RL009';
    END IF;
    SELECT * INTO v_actor FROM public.rg_actor_session();
    IF v_actor.scope <> 'platform' THEN
        RAISE EXCEPTION 'rg limit floor change requests are platform-only' USING ERRCODE = 'RL001';
    END IF;
    IF TG_OP = 'INSERT' THEN
        -- The proposal is a FLOOR row of exactly this request's scope; its hash is computed here.
        v_proposed := jsonb_populate_record(NULL::public.rg_limit_configs, NEW.proposed_config);
        IF v_proposed.tenant_id IS NOT NULL OR v_proposed.brand_id IS NOT NULL
           OR v_proposed.jurisdiction_code IS DISTINCT FROM NEW.jurisdiction_code
           OR v_proposed.control_kind IS DISTINCT FROM NEW.control_kind
           OR v_proposed.asset_code IS DISTINCT FROM NEW.asset_code
           OR v_proposed.reason_code IS DISTINCT FROM NEW.reason_code THEN
            RAISE EXCEPTION 'rg_limit_config_change_requests: the proposed row is not a floor of this request''s scope' USING ERRCODE = 'RL007';
        END IF;
        v_hash := public.rg_limit_config_payload_hash(v_proposed);
        IF NEW.payload_hash IS NOT NULL AND NEW.payload_hash <> v_hash THEN
            RAISE EXCEPTION 'rg_limit_config_change_requests: payload_hash is not the hash of the proposed row' USING ERRCODE = 'RL007';
        END IF;
        NEW.payload_hash := v_hash;
        NEW.requested_by := v_actor.actor;
        NEW.requested_by_person_id := v_actor.person_id;
        NEW.status := 'pending';
        NEW.decided_by := NULL; NEW.decided_by_person_id := NULL; NEW.decided_at := NULL;
        NEW.created_at := pg_catalog.clock_timestamp();
        NEW.expires_at := NEW.created_at + interval '72 hours';
        PERFORM public.actor_proof_require(v_actor.actor, 'platform', NULL, 'rg_limit_config:request', 'new',
            public.rg_limit_config_request_digest(NEW));
        RETURN NEW;
    END IF;
    -- UPDATE: only pending -> approved / rejected / cancelled; the content never changes.
    IF NEW.id <> OLD.id OR NEW.jurisdiction_code <> OLD.jurisdiction_code OR NEW.control_kind <> OLD.control_kind
       OR NEW.asset_code IS DISTINCT FROM OLD.asset_code OR NEW.payload_hash <> OLD.payload_hash
       OR NEW.proposed_config IS DISTINCT FROM OLD.proposed_config
       OR NEW.reason_code <> OLD.reason_code OR NEW.requested_by <> OLD.requested_by
       OR NEW.requested_by_person_id <> OLD.requested_by_person_id OR NEW.created_at <> OLD.created_at
       OR NEW.expires_at <> OLD.expires_at THEN
        RAISE EXCEPTION 'rg_limit_config_change_requests: the request content is immutable' USING ERRCODE = 'RL010';
    END IF;
    IF OLD.status <> 'pending' OR NEW.status NOT IN ('approved', 'rejected', 'cancelled') THEN
        RAISE EXCEPTION 'rg_limit_config_change_requests: % -> % is not a legal move', OLD.status, NEW.status USING ERRCODE = 'RL010';
    END IF;
    IF NEW.status = 'cancelled' THEN
        IF v_actor.actor <> OLD.requested_by THEN
            RAISE EXCEPTION 'rg_limit_config_change_requests: only the requester may cancel' USING ERRCODE = 'RL007';
        END IF;
    ELSE
        IF v_actor.person_id = OLD.requested_by_person_id THEN
            RAISE EXCEPTION 'rg_limit_config_change_requests: the requester''s Person may not decide the request (four-eyes)' USING ERRCODE = 'RL007';
        END IF;
        IF pg_catalog.clock_timestamp() >= OLD.expires_at THEN
            RAISE EXCEPTION 'rg_limit_config_change_requests: the request has expired' USING ERRCODE = 'RL010';
        END IF;
    END IF;
    NEW.decided_by := v_actor.actor;
    NEW.decided_by_person_id := v_actor.person_id;
    NEW.decided_at := pg_catalog.clock_timestamp();
    PERFORM public.actor_proof_require(v_actor.actor, 'platform', NULL,
        'rg_limit_config:' || CASE NEW.status WHEN 'approved' THEN 'approve' WHEN 'rejected' THEN 'reject' ELSE 'cancel' END,
        OLD.id::text, OLD.payload_hash);
    RETURN NEW;
END
$$;
CREATE TRIGGER rg_limit_config_change_requests_guard
    BEFORE INSERT OR UPDATE OR DELETE ON rg_limit_config_change_requests
    FOR EACH ROW EXECUTE FUNCTION rg_limit_config_change_requests_guard();
CREATE TRIGGER rg_limit_config_change_requests_no_truncate
    BEFORE TRUNCATE ON rg_limit_config_change_requests
    FOR EACH STATEMENT EXECUTE FUNCTION rg_deny_mutation();

-- The canonical payload of a configuration row (every content column; not id, change_request_id,
-- created_*). The floor four-eyes binds the approved request to EXACTLY this value, recomputed from
-- the inserted row. Arrays are order-independent (sorted; text in COLLATE "C"); intervals are fixed,
-- so epoch seconds are exact; timestamps are UTC with microseconds.
CREATE FUNCTION rg_limit_config_payload_hash(r rg_limit_configs) RETURNS text
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT public.k2_sha256_hex(public.k2_canonical(
        'rg_limit_config.v1', r.jurisdiction_code, r.tenant_id::text, r.brand_id::text, r.control_kind, r.asset_code,
        to_char(r.effective_from AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
        r.enabled::text, r.mandatory::text, r.ceiling::text, r.ceiling_none::text,
        (SELECT string_agg(x, ',' ORDER BY x COLLATE "C") FROM unnest(r.allowed_periods) AS x),
        r.calendar_tz, r.week_start::text, extract(epoch FROM r.increase_cooldown)::text,
        r.increase_requires_reconfirmation::text, r.scope, r.count_bonus_funded::text, r.loss_computation,
        r.loss_may_go_negative::text, r.loss_may_go_negative_none::text,
        extract(epoch FROM r.post_session_break)::text, r.post_session_break_none::text,
        r.concurrent_chain_aggregation::text, r.concurrent_chain_aggregation_none::text,
        extract(epoch FROM r.pending_intent_ttl)::text, r.pending_intent_ttl_none::text,
        (SELECT string_agg(extract(epoch FROM x)::text, ',' ORDER BY x) FROM unnest(r.allowed_durations) AS x),
        r.allowed_durations_any::text,
        (SELECT string_agg(extract(epoch FROM x)::text, ',' ORDER BY x) FROM unnest(r.allowed_intervals) AS x),
        r.reason_code, r.attestation_ref))
$$;

-- The first field in which child c is LOOSER than parent p (restrictive fields may only tighten;
-- protective option sets may only widen), or NULL. Both rows are of the same control_kind.
CREATE FUNCTION rg_limit_config_loosening(c rg_limit_configs, p rg_limit_configs) RETURNS text
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT CASE
        WHEN p.enabled AND NOT c.enabled THEN 'enabled'
        WHEN p.mandatory AND NOT c.mandatory THEN 'mandatory'
        WHEN p.ceiling_none IS FALSE AND (c.ceiling_none IS NOT FALSE OR c.ceiling > p.ceiling) THEN 'ceiling'
        WHEN p.increase_cooldown IS NOT NULL AND (c.increase_cooldown IS NULL OR c.increase_cooldown < p.increase_cooldown) THEN 'increase_cooldown'
        WHEN p.increase_requires_reconfirmation IS TRUE AND c.increase_requires_reconfirmation IS NOT TRUE THEN 'increase_requires_reconfirmation'
        WHEN p.count_bonus_funded IS TRUE AND c.count_bonus_funded IS NOT TRUE THEN 'count_bonus_funded'
        WHEN p.control_kind = 'loss' AND p.loss_may_go_negative IS NOT TRUE AND c.loss_may_go_negative IS TRUE THEN 'loss_may_go_negative'
        WHEN public.rg_scope_rank(c.scope) < public.rg_scope_rank(p.scope) THEN 'scope'
        WHEN p.post_session_break_none IS FALSE AND (c.post_session_break_none IS NOT FALSE OR c.post_session_break < p.post_session_break) THEN 'post_session_break'
        WHEN p.concurrent_chain_aggregation IS TRUE AND c.concurrent_chain_aggregation IS NOT TRUE THEN 'concurrent_chain_aggregation'
        WHEN p.pending_intent_ttl_none IS TRUE AND c.pending_intent_ttl_none IS NOT TRUE THEN 'pending_intent_ttl'
        WHEN p.pending_intent_ttl_none IS FALSE AND c.pending_intent_ttl_none IS FALSE AND c.pending_intent_ttl < p.pending_intent_ttl THEN 'pending_intent_ttl'
        WHEN p.allowed_periods IS NOT NULL AND NOT (c.allowed_periods @> p.allowed_periods) THEN 'allowed_periods'
        WHEN p.allowed_durations_any IS TRUE AND c.allowed_durations_any IS NOT TRUE THEN 'allowed_durations'
        WHEN p.allowed_durations_any IS FALSE AND c.allowed_durations_any IS FALSE AND NOT (c.allowed_durations @> p.allowed_durations) THEN 'allowed_durations'
        WHEN p.allowed_intervals IS NOT NULL AND NOT (c.allowed_intervals @> p.allowed_intervals) THEN 'allowed_intervals'
        ELSE NULL
    END
$$;

-- 5a. Actor and row-shape guard (first): forces created_by_* / created_at from the session.
CREATE FUNCTION rg_limit_configs_10_actor() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor RECORD;
BEGIN
    SELECT * INTO v_actor FROM public.rg_actor_session();
    IF NEW.tenant_id IS NULL THEN
        IF v_actor.scope <> 'platform' THEN
            RAISE EXCEPTION 'rg_limit_configs: a jurisdiction floor is written by a platform principal only' USING ERRCODE = 'RL001';
        END IF;
    ELSIF v_actor.scope <> 'tenant' OR v_actor.tenant IS DISTINCT FROM NEW.tenant_id THEN
        RAISE EXCEPTION 'rg_limit_configs: an operator row is written by a principal of the same tenant only' USING ERRCODE = 'RL001';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_timezone_names z WHERE z.name = NEW.calendar_tz) AND NEW.calendar_tz IS NOT NULL THEN
        RAISE EXCEPTION 'rg_limit_configs: calendar_tz % is not a known time zone', NEW.calendar_tz USING ERRCODE = 'RL011';
    END IF;
    NEW.created_by_actor_type := 'staff';
    NEW.created_by_actor_id := v_actor.actor;
    NEW.created_by_person_id := v_actor.person_id;
    NEW.created_at := pg_catalog.clock_timestamp();
    RETURN NEW;
END
$$;

-- 5b. No backdating, floor or operator row (security condition 9; the 5 s tolerance of 0049).
CREATE FUNCTION rg_limit_configs_20_no_backdate() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF NEW.effective_from < pg_catalog.clock_timestamp() - interval '5 seconds' THEN
        RAISE EXCEPTION 'rg_limit_configs: effective_from % is in the past; a configuration change takes effect now or later', NEW.effective_from
            USING ERRCODE = 'RL003';
    END IF;
    RETURN NEW;
END
$$;

-- 5c. Synthetic guard (security condition 8). Anything resembling a synthetic attestation is refused
-- unless it is exactly 'synthetic' AND rg_synthetic_config_allowed().
CREATE FUNCTION rg_limit_configs_30_synthetic() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF pg_catalog.strpos(pg_catalog.lower(NEW.attestation_ref), 'synthetic') > 0
       AND (NEW.attestation_ref <> 'synthetic' OR NOT public.rg_synthetic_config_allowed()) THEN
        RAISE EXCEPTION 'rg_limit_configs: a SYNTHETIC-NOT-LEGAL row is refused in environment % (app.environment=%)',
            public.rg_database_environment(), COALESCE(NULLIF(pg_catalog.current_setting('app.environment', true), ''), 'unset')
            USING ERRCODE = 'RL008';
    END IF;
    RETURN NEW;
END
$$;

-- 5d. Floor four-eyes (security condition 9): an approved, unexpired request of the same scope whose
-- payload_hash equals the hash recomputed from THIS row, decided by a different Person, and the signed
-- apply proof bound to (request id, recomputed hash). The UNIQUE change_request_id makes a request
-- single-use.
CREATE FUNCTION rg_limit_configs_40_floor_four_eyes() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_req  public.rg_limit_config_change_requests%ROWTYPE;
    v_hash text;
BEGIN
    IF NEW.tenant_id IS NOT NULL THEN
        IF NEW.change_request_id IS NOT NULL THEN
            RAISE EXCEPTION 'rg_limit_configs: an operator row carries no floor change request' USING ERRCODE = 'RL006';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.change_request_id IS NULL THEN
        RAISE EXCEPTION 'rg_limit_configs: a jurisdiction floor needs an approved change request' USING ERRCODE = 'RL006';
    END IF;
    SELECT * INTO v_req FROM public.rg_limit_config_change_requests r WHERE r.id = NEW.change_request_id FOR UPDATE;
    IF NOT FOUND OR v_req.status <> 'approved' OR pg_catalog.clock_timestamp() >= v_req.expires_at
       OR v_req.jurisdiction_code <> NEW.jurisdiction_code OR v_req.control_kind <> NEW.control_kind
       OR v_req.asset_code IS DISTINCT FROM NEW.asset_code THEN
        RAISE EXCEPTION 'rg_limit_configs: change request % is not an approved, unexpired request for this floor', NEW.change_request_id
            USING ERRCODE = 'RL006';
    END IF;
    IF v_req.decided_by_person_id IS NULL OR v_req.decided_by_person_id = v_req.requested_by_person_id THEN
        RAISE EXCEPTION 'rg_limit_configs: the request was not approved by a different Person' USING ERRCODE = 'RL007';
    END IF;
    v_hash := public.rg_limit_config_payload_hash(NEW);
    IF v_hash IS DISTINCT FROM v_req.payload_hash THEN
        RAISE EXCEPTION 'rg_limit_configs: the floor row is not the approved payload' USING ERRCODE = 'RL007';
    END IF;
    PERFORM public.actor_proof_require(NEW.created_by_actor_id, 'platform', NULL, 'rg_limit_config:apply',
        v_req.id::text, v_hash);
    RETURN NEW;
END
$$;

-- 5e. Operator rows: tighten-only / widen-only against every broader row in force at the row's own
-- effective_from (the floor rows of the jurisdiction for the row's asset and for all assets, and for a
-- brand row also its tenant's rows). A floor must be in force (fail closed: nothing to tighten).
CREATE FUNCTION rg_limit_configs_50_tighten_only() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    p       public.rg_limit_configs%ROWTYPE;
    v_field text;
    v_floor boolean := false;
    v_syn   boolean := public.rg_synthetic_config_allowed();
BEGIN
    IF NEW.tenant_id IS NULL THEN
        RETURN NEW;
    END IF;
    FOR p IN
        SELECT DISTINCT ON (c.tenant_id, c.brand_id, c.asset_code) c.*
          FROM public.rg_limit_configs c
         WHERE c.jurisdiction_code = NEW.jurisdiction_code AND c.control_kind = NEW.control_kind
           AND (c.asset_code IS NULL OR c.asset_code IS NOT DISTINCT FROM NEW.asset_code)
           AND (c.tenant_id IS NULL OR (NEW.brand_id IS NOT NULL AND c.tenant_id = NEW.tenant_id AND c.brand_id IS NULL))
           AND c.effective_from <= NEW.effective_from
           AND (c.attestation_ref <> 'synthetic' OR v_syn)
         ORDER BY c.tenant_id, c.brand_id, c.asset_code, c.effective_from DESC, c.created_at DESC
    LOOP
        IF p.tenant_id IS NULL THEN
            v_floor := true;
        END IF;
        v_field := public.rg_limit_config_loosening(NEW, p);
        IF v_field IS NOT NULL THEN
            RAISE EXCEPTION 'rg_limit_configs: the operator row loosens % of the % row in force', v_field,
                CASE WHEN p.tenant_id IS NULL THEN 'jurisdiction floor' ELSE 'tenant' END
                USING ERRCODE = 'RL004', DETAIL = format('field=%s parent=%s', v_field, p.id);
        END IF;
    END LOOP;
    IF NOT v_floor THEN
        RAISE EXCEPTION 'rg_limit_configs: no jurisdiction floor is in force for % / % at %; an operator row has nothing to tighten',
            NEW.jurisdiction_code, NEW.control_kind, NEW.effective_from USING ERRCODE = 'RL005';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER rg_limit_configs_10_actor BEFORE INSERT ON rg_limit_configs
    FOR EACH ROW EXECUTE FUNCTION rg_limit_configs_10_actor();
CREATE TRIGGER rg_limit_configs_20_no_backdate BEFORE INSERT ON rg_limit_configs
    FOR EACH ROW EXECUTE FUNCTION rg_limit_configs_20_no_backdate();
CREATE TRIGGER rg_limit_configs_30_synthetic BEFORE INSERT ON rg_limit_configs
    FOR EACH ROW EXECUTE FUNCTION rg_limit_configs_30_synthetic();
CREATE TRIGGER rg_limit_configs_40_floor_four_eyes BEFORE INSERT ON rg_limit_configs
    FOR EACH ROW EXECUTE FUNCTION rg_limit_configs_40_floor_four_eyes();
CREATE TRIGGER rg_limit_configs_50_tighten_only BEFORE INSERT ON rg_limit_configs
    FOR EACH ROW EXECUTE FUNCTION rg_limit_configs_50_tighten_only();
CREATE TRIGGER rg_limit_configs_immutable BEFORE UPDATE OR DELETE ON rg_limit_configs
    FOR EACH ROW EXECUTE FUNCTION rg_deny_mutation();
CREATE TRIGGER rg_limit_configs_no_truncate BEFORE TRUNCATE ON rg_limit_configs
    FOR EACH STATEMENT EXECUTE FUNCTION rg_deny_mutation();

ALTER TABLE rg_limit_configs ENABLE ROW LEVEL SECURITY;
ALTER TABLE rg_limit_configs FORCE ROW LEVEL SECURITY;
-- Floors are readable by every session; operator rows only by their own tenant.
CREATE POLICY rg_limit_configs_read ON rg_limit_configs
    FOR SELECT
    USING (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY rg_limit_configs_floor_insert ON rg_limit_configs
    FOR INSERT
    WITH CHECK (
        tenant_id IS NULL AND brand_id IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
CREATE POLICY rg_limit_configs_operator_insert ON rg_limit_configs
    FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- Read-time composition of the configuration in force at p_at for (jurisdiction, tenant, brand,
-- kind, asset): for each exact scope (floor / tenant / brand x all-assets / this asset) the latest row
-- with effective_from <= p_at; synthetic rows count as ABSENT unless rg_synthetic_config_allowed().
-- found is false when no floor is in force (rg_config_missing). Restrictive fields compose to the
-- strictest value, option sets by UNION, enabled / mandatory by OR, so a looser operator row that
-- bypassed the write trigger still cannot loosen anything. calendar_tz / week_start come from the
-- floor (the asset-specific floor first).
CREATE FUNCTION rg_limit_config_compose(
    p_jurisdiction text, p_tenant uuid, p_brand uuid, p_kind text, p_asset text, p_at timestamptz,
    OUT found boolean, OUT version_ids uuid[], OUT enabled boolean, OUT mandatory boolean,
    OUT ceiling numeric, OUT ceiling_none boolean, OUT allowed_periods text[], OUT calendar_tz text,
    OUT week_start smallint, OUT increase_cooldown interval, OUT increase_requires_reconfirmation boolean,
    OUT scope text, OUT count_bonus_funded boolean, OUT loss_computation text, OUT loss_may_go_negative boolean,
    OUT post_session_break interval, OUT concurrent_chain_aggregation boolean,
    OUT pending_intent_ttl interval, OUT pending_intent_ttl_none boolean,
    OUT allowed_durations interval[], OUT allowed_durations_any boolean, OUT allowed_intervals interval[]
) LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    WITH inforce AS (
        SELECT DISTINCT ON (c.tenant_id, c.brand_id, c.asset_code) c.*
          FROM public.rg_limit_configs c
         WHERE c.jurisdiction_code = p_jurisdiction AND c.control_kind = p_kind
           AND (c.asset_code IS NULL OR c.asset_code = p_asset)
           AND (c.tenant_id IS NULL OR (c.tenant_id = p_tenant AND (c.brand_id IS NULL OR c.brand_id = p_brand)))
           AND c.effective_from <= p_at
           AND (c.attestation_ref <> 'synthetic' OR public.rg_synthetic_config_allowed())
         ORDER BY c.tenant_id, c.brand_id, c.asset_code, c.effective_from DESC, c.created_at DESC
    ), rows AS (
        SELECT i.* FROM inforce i WHERE EXISTS (SELECT 1 FROM inforce f WHERE f.tenant_id IS NULL)
    )
    SELECT
        EXISTS (SELECT 1 FROM rows),
        COALESCE((SELECT array_agg(r.id ORDER BY r.id) FROM rows r), '{}'),
        (SELECT bool_or(r.enabled) FROM rows r),
        (SELECT bool_or(r.mandatory) FROM rows r),
        (SELECT min(r.ceiling) FROM rows r WHERE r.ceiling_none IS FALSE),
        (SELECT bool_and(r.ceiling_none) FROM rows r),
        (SELECT array_agg(DISTINCT x ORDER BY x) FROM rows r, unnest(r.allowed_periods) AS x),
        (SELECT r.calendar_tz FROM rows r WHERE r.tenant_id IS NULL ORDER BY r.asset_code NULLS LAST LIMIT 1),
        (SELECT r.week_start FROM rows r WHERE r.tenant_id IS NULL ORDER BY r.asset_code NULLS LAST LIMIT 1),
        (SELECT max(r.increase_cooldown) FROM rows r),
        (SELECT bool_or(r.increase_requires_reconfirmation) FROM rows r),
        (SELECT r.scope FROM rows r ORDER BY public.rg_scope_rank(r.scope) DESC LIMIT 1),
        (SELECT bool_or(r.count_bonus_funded) FROM rows r),
        (SELECT min(r.loss_computation) FROM rows r),
        (SELECT bool_and(r.loss_may_go_negative IS TRUE) FROM rows r WHERE r.control_kind = 'loss'),
        (SELECT max(r.post_session_break) FROM rows r WHERE r.post_session_break_none IS FALSE),
        (SELECT bool_or(r.concurrent_chain_aggregation IS TRUE) FROM rows r WHERE r.control_kind = 'session_duration'),
        CASE WHEN (SELECT bool_or(r.pending_intent_ttl_none) FROM rows r) THEN NULL
             ELSE (SELECT max(r.pending_intent_ttl) FROM rows r) END,
        (SELECT bool_or(r.pending_intent_ttl_none) FROM rows r),
        CASE WHEN (SELECT bool_or(r.allowed_durations_any) FROM rows r) THEN NULL
             ELSE (SELECT array_agg(DISTINCT x ORDER BY x) FROM rows r, unnest(r.allowed_durations) AS x) END,
        (SELECT bool_or(r.allowed_durations_any) FROM rows r),
        (SELECT array_agg(DISTINCT x ORDER BY x) FROM rows r, unnest(r.allowed_intervals) AS x)
$$;

-- =========================================================================
-- 6. player_rg_limit_changes (ADR 0115 2.2)
-- =========================================================================
CREATE TABLE player_rg_limit_changes (
    seq                     BIGSERIAL NOT NULL UNIQUE,
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    person_id               UUID NOT NULL REFERENCES persons (id) ON DELETE RESTRICT,
    player_account_id       UUID NOT NULL,
    brand_id                UUID NOT NULL,
    scope                   TEXT NOT NULL CHECK (scope IN ('account', 'tenant_person')),
    control_kind            TEXT NOT NULL CHECK (control_kind IN ('deposit', 'loss', 'wager', 'session_duration')),
    asset_code              TEXT NULL REFERENCES assets (code) ON DELETE RESTRICT,
    period_kind             TEXT NULL CHECK (period_kind IN ('rolling_24h', 'rolling_7d', 'rolling_30d', 'calendar_day', 'calendar_week', 'calendar_month')),
    value                   NUMERIC(38, 0) NULL CHECK (value > 0),
    change_kind             TEXT NOT NULL CHECK (change_kind IN ('initial', 'decrease', 'increase', 'removal', 'cancel_pending', 'confirm')),
    requires_confirmation   BOOLEAN NOT NULL,
    requested_at            TIMESTAMPTZ NOT NULL,
    effective_at            TIMESTAMPTZ NOT NULL,
    supersedes_change_id    UUID NULL REFERENCES player_rg_limit_changes (id) ON DELETE RESTRICT,
    source                  TEXT NOT NULL CHECK (source IN ('player_self_service', 'staff_on_player_request')),
    reason_code             TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64),
    evidence_ref            TEXT NULL CHECK (octet_length(evidence_ref) BETWEEN 1 AND 200),
    created_by_actor_type   TEXT NOT NULL CHECK (created_by_actor_type IN ('player', 'staff')),
    created_by_actor_id     UUID NOT NULL,
    config_version_ids      UUID[] NOT NULL,
    idempotency_key         TEXT NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 1 AND 128),
    request_hash            TEXT NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT player_rg_limit_changes_idempotency UNIQUE (tenant_id, player_account_id, idempotency_key),
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id) ON DELETE RESTRICT,
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id) ON DELETE RESTRICT,
    CONSTRAINT player_rg_limit_changes_source_actor CHECK ((source = 'player_self_service') = (created_by_actor_type = 'player')),
    CONSTRAINT player_rg_limit_changes_staff_evidence CHECK (source = 'player_self_service' OR evidence_ref IS NOT NULL),
    CONSTRAINT player_rg_limit_changes_key_shape CHECK (CASE WHEN control_kind = 'session_duration'
        THEN asset_code IS NULL AND period_kind IS NULL ELSE asset_code IS NOT NULL AND period_kind IS NOT NULL END),
    CONSTRAINT player_rg_limit_changes_supersedes CHECK ((change_kind IN ('cancel_pending', 'confirm')) = (supersedes_change_id IS NOT NULL)),
    CONSTRAINT player_rg_limit_changes_value CHECK (CASE change_kind
        WHEN 'removal' THEN value IS NULL
        WHEN 'cancel_pending' THEN true
        WHEN 'confirm' THEN true
        ELSE value IS NOT NULL END),
    CONSTRAINT player_rg_limit_changes_effective CHECK (effective_at >= requested_at)
);
CREATE INDEX idx_player_rg_limit_changes_person
    ON player_rg_limit_changes (tenant_id, person_id, control_kind, asset_code, period_kind, requested_at DESC, seq DESC);
CREATE INDEX idx_player_rg_limit_changes_account
    ON player_rg_limit_changes (tenant_id, player_account_id, control_kind, asset_code, period_kind, requested_at DESC, seq DESC);

-- THE realization rule (ADR 0115 2.2; one implementation, called by rg_limit_change_classify and by Go).
-- Requests of one key are ordered by (requested_at, seq). A row is realized iff it does not await a
-- confirmation and effective_at <= min(p_at, requested_at of the next request). The effective player
-- value is the latest realized row's value (NULL value = removed, no limit). has_row is false when no
-- row of the key has ever been realized. Runs with the CALLER's row visibility.
CREATE FUNCTION rg_limit_realized(
    p_tenant uuid, p_account uuid, p_person uuid, p_scope text, p_kind text, p_asset text, p_period text, p_at timestamptz,
    OUT has_row boolean, OUT value numeric, OUT change_id uuid
) LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    WITH k AS (
        SELECT c.id, c.value, c.requested_at, c.seq, c.effective_at, c.requires_confirmation,
               lead(c.requested_at) OVER (ORDER BY c.requested_at, c.seq) AS next_requested_at
          FROM public.player_rg_limit_changes c
         WHERE c.tenant_id = p_tenant AND c.scope = p_scope AND c.control_kind = p_kind
           AND c.asset_code IS NOT DISTINCT FROM p_asset AND c.period_kind IS NOT DISTINCT FROM p_period
           AND CASE WHEN p_scope = 'account' THEN c.player_account_id = p_account ELSE c.person_id = p_person END
           AND c.requested_at <= p_at
    ), realized AS (
        SELECT k.* FROM k
         WHERE NOT k.requires_confirmation
           AND k.effective_at <= LEAST(p_at, k.next_requested_at)
         ORDER BY k.requested_at DESC, k.seq DESC
         LIMIT 1
    )
    SELECT EXISTS (SELECT 1 FROM realized), (SELECT r.value FROM realized r), (SELECT r.id FROM realized r)
$$;

-- THE classification (ADR 0115 2.2, security condition 3; one implementation, called by the BEFORE
-- INSERT trigger and by Go). Given the request at instant p_at it returns the server-decided
-- change_kind, effective_at, requires_confirmation, the value the row carries and the configuration
-- versions used. Decreases, initial values and cancel_pending apply at p_at; increases and removals
-- at p_at + the composed increase_cooldown (strictest), and need configuration in force (RL023). A
-- staff-originated increase or removal always needs the player's own confirm (security condition 4).
CREATE FUNCTION rg_limit_change_classify(
    p_tenant uuid, p_account uuid, p_person uuid, p_brand uuid, p_jurisdiction text,
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
BEGIN
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
    SELECT * INTO v_cfg FROM public.rg_limit_config_compose(p_jurisdiction, p_tenant, p_brand, p_kind, p_asset, p_at);
    IF change_kind IN ('initial', 'decrease') THEN
        effective_at := p_at; requires_confirmation := false;
        config_version_ids := COALESCE(v_cfg.version_ids, '{}');
        RETURN;
    END IF;
    IF p_jurisdiction IS NULL OR NOT v_cfg.found OR v_cfg.increase_cooldown IS NULL THEN
        RAISE EXCEPTION 'rg limit change: no RG configuration is in force for this limit; an increase or removal is refused' USING ERRCODE = 'RL023';
    END IF;
    effective_at := p_at + v_cfg.increase_cooldown;
    requires_confirmation := v_cfg.increase_requires_reconfirmation OR p_source = 'staff_on_player_request';
    config_version_ids := v_cfg.version_ids;
END
$$;

-- The BEFORE INSERT trigger: session checks, forced columns, L0.4, server time, classification.
CREATE FUNCTION player_rg_limit_changes_classify() RETURNS trigger
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
    v_res       RECORD;
    v_intent    text := NEW.change_kind;
BEGIN
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
    SELECT * INTO v_res FROM public.rg_limit_change_classify(NEW.tenant_id, NEW.player_account_id, NEW.person_id, NEW.brand_id, v_jur,
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

CREATE TRIGGER player_rg_limit_changes_classify BEFORE INSERT ON player_rg_limit_changes
    FOR EACH ROW EXECUTE FUNCTION player_rg_limit_changes_classify();
CREATE TRIGGER player_rg_limit_changes_immutable BEFORE UPDATE OR DELETE ON player_rg_limit_changes
    FOR EACH ROW EXECUTE FUNCTION rg_deny_mutation();
CREATE TRIGGER player_rg_limit_changes_no_truncate BEFORE TRUNCATE ON player_rg_limit_changes
    FOR EACH STATEMENT EXECUTE FUNCTION rg_deny_mutation();

ALTER TABLE player_rg_limit_changes ENABLE ROW LEVEL SECURITY;
ALTER TABLE player_rg_limit_changes FORCE ROW LEVEL SECURITY;
-- Limit history never leaves its tenant: no platform-wide read.
CREATE POLICY staff_read ON player_rg_limit_changes
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY player_self_read ON player_rg_limit_changes
    FOR SELECT
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND (
            player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
            OR (scope = 'tenant_person' AND person_id = (
                SELECT pa.person_id FROM player_accounts pa
                 WHERE pa.id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
                   AND pa.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid))
        )
    );
CREATE POLICY staff_insert ON player_rg_limit_changes
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND source = 'staff_on_player_request'
    );
CREATE POLICY player_self_insert ON player_rg_limit_changes
    FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
        AND source = 'player_self_service'
        AND created_by_actor_type = 'player'
        AND created_by_actor_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
    );

-- =========================================================================
-- 7. player_restrictions: time_out (ADR 0115 2.3, security condition 10)
-- =========================================================================
ALTER TABLE player_restrictions DROP CONSTRAINT player_restrictions_restriction_type_check;
ALTER TABLE player_restrictions ADD CONSTRAINT player_restrictions_restriction_type_check
    CHECK (restriction_type IN ('self_exclusion', 'time_out'));
-- A time-out is a bounded break (never indefinite); it cannot be ended early (append-only, unchanged).
ALTER TABLE player_restrictions ADD CONSTRAINT player_restrictions_time_out_bounded
    CHECK (restriction_type <> 'time_out' OR ends_at IS NOT NULL);

DROP POLICY player_self_insert ON player_restrictions;
-- The self_exclusion branch is 0037's policy verbatim. The time_out branch: platform_person
-- (tenant_id / brand_id NULL) or tenant_person (tenant_id = the session's app.tenant_id, never a body
-- value; brand_id NULL). Every other column check is shared.
CREATE POLICY player_self_insert ON player_restrictions
    FOR INSERT
    WITH CHECK (
        (
            (tenant_id IS NULL AND brand_id IS NULL AND restriction_type = 'self_exclusion')
            OR (restriction_type = 'time_out' AND brand_id IS NULL
                AND (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid))
        )
        AND source = 'player_self_service'
        AND created_by_actor_type = 'player'
        AND created_by_actor_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
        AND player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
        AND person_id = (
            SELECT person_id FROM player_accounts
            WHERE id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
        )
    );

-- =========================================================================
-- 8. casino_launch_sessions.auth_session_root_started_at (ADR 0115 2.4; written by S4)
-- =========================================================================
ALTER TABLE casino_launch_sessions ADD COLUMN auth_session_root_started_at TIMESTAMPTZ NULL;

-- =========================================================================
-- 9. Grants to igaming_runtime (mirrored by deploy/init-app-role.sql). RLS + triggers bind.
-- =========================================================================
REVOKE ALL ON database_environment FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON database_environment FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON database_environment TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON rg_limit_config_change_requests FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON rg_limit_config_change_requests TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (status) ON rg_limit_config_change_requests TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON rg_limit_configs FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON rg_limit_configs TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON player_rg_limit_changes FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON player_rg_limit_changes TO igaming_runtime';
        EXECUTE 'GRANT USAGE ON SEQUENCE player_rg_limit_changes_seq_seq TO igaming_runtime';
    END IF;
END $$;

-- =========================================================================
-- 10. Indexes (ADR 0115 2.6, ledger-finance BLOCKER-1). See the header: pre-build CONCURRENTLY in
--     production; accepted here only if valid and identical (pre-flight), otherwise built now.
-- =========================================================================
CREATE INDEX IF NOT EXISTS idx_ledger_entries_tenant_player_asset_created
    ON ledger_entries (tenant_id, player_account_id, asset_code, created_at);
CREATE INDEX IF NOT EXISTS idx_deposit_intents_tenant_player_asset_created
    ON deposit_intents (tenant_id, player_account_id, asset_code, created_at);
