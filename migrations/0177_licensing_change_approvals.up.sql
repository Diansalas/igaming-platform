-- 0177: licensing / jurisdiction maker-checker (MKT-DUAL-1; audit gaps G-1, G-2, G-9; triage L-10, L-24, L-25).
-- SECURITY-SENSITIVE (governed configuration writes, signed actor proof): FOR `security`, `architect`, `code-reviewer`, `qa`
-- REVIEW BEFORE MERGE. Proposed ADR text: docs/plans/lic2-maker-checker-record.md.
--
-- NUMBERING: 0177. 0176 belongs to the parallel licence-lifecycle slice (licence status / dates / evidence). This migration does
-- NOT touch any `licences` column, the licence status / date / evidence writers, or a licences UPDATE path. It adds ONE
-- BEFORE INSERT guard on `licences` (licence creation goes through the primitive). MERGE NOTE: both 0176 and 0177 may replace
-- actor_proof_require (CREATE OR REPLACE); whichever lands second must carry BOTH added blocks (see the record, section 9).
--
-- WHAT THIS ADDS
--   1. A GENERIC two-person approval primitive for licensing / jurisdiction configuration writes:
--        licensing_change_kinds      the CLOSED kind registry (the extension hook: a later migration adds a row plus a
--                                    licensing_change_classify / licensing_change_row_matches branch; an unknown kind fails
--                                    closed in both functions);
--        licensing_change_requests   append-only request rows (state machine requested -> approved -> executed, or
--                                    rejected / cancelled / expired; content immutable; payload hash computed HERE);
--        licensing_change_approvals  append-only approval rows (one per Person per request).
--      WIDENING kinds (open / enable / raise / bind / create) need kind.widening_approvals approvals (2 in this slice), each by a
--      distinct Person who is neither the requester's Person nor the requester's staff account, each at most 24 h old at
--      execution, each by an ACTIVE staff account whose role is in kind.approver_roles (interim, HDR-M-1 pending:
--      platform_admin only). NARROWING kinds (close / disable / withdraw at a terminal rung / unbind / narrow a configuration)
--      need 0 approvals when a platform principal requests them (single actor with a reason code, executed in the SAME
--      transaction as the request: fail-closed asymmetry, same as launch suspend) and 1 platform approval when a tenant
--      principal requests them (tenant staff may only REQUEST and read).
--   2. Signed actor proof (ADR 0110) on every request, approval, reject, cancel and execute: a new operation family
--      licensing_change:{request,approve,reject,cancel,execute}; approve / reject / execute are platform scope only.
--   3. A GOVERNED-WRITE GUARD on licence_country_ceilings, operating_country_policies, tenant_jurisdiction_configs (INSERT and
--      the effective_to close), licences (INSERT) and tenants (licence_id set on INSERT or changed on UPDATE): a write by any
--      role other than the table owner needs an EXECUTED licensing change request of the matching kind whose execution
--      happened in THIS transaction (executed_txid = txid_current()) and whose payload matches the written row (LC040).
--      The table owner (the migration / provisioning role) keeps its path, exactly like the ADR 0112 LA021 / LF2 owner path:
--      it is how synthetic staging / development seeding (cmd/seed-market, cmd/seed-staging-world, which run as the owner
--      and refuse production in Go) and test fixtures provision rows; an owner can disable triggers anyway, so a guard against
--      the owner would be theatre.
--   4. tenant_jurisdiction_configs: the 0005 `FOR ALL` tenant policy is replaced by tenant (and platform) SELECT, platform-only
--      INSERT / close, and owner-only tenant-scope fixture writes; an append-only trigger (only effective_to may be set, once,
--      to now() for a non-owner); one open version per (tenant, jurisdiction); TRUNCATE denied.
--
-- SQLSTATEs (class LC): LC001 session / actor; LC002 tenant scope may not request this kind / subject; LC010 payload or kind
-- invalid; LC011 duplicate open request (also the unique index, 23505); LC012 request content is immutable; LC013 illegal
-- state move or expired; LC020 approvals not satisfied (count, freshness, Person, role); LC021 a 0-approval execution not by
-- the requester in the request's own transaction; LC022 the requester's Person may not reject; LC023 only the requester may
-- cancel; LC024 re-classified as widening at execution; LC030 approver not eligible; LC031 request not open; LC032 four-eyes
-- (requester / same Person); LC040 governed table written outside an executed request of this transaction; LC041
-- tenant_jurisdiction_configs is append-only; LC095 down refused while requests exist; LC098 pre-flight; LC099 append-only.
-- Every function pins search_path; none but the (replaced) verifier is SECURITY DEFINER. No role, password or grant to any
-- role other than igaming_runtime is changed.

SET LOCAL lock_timeout = '5s';

-- =========================================================================
-- 0. Pre-flight
-- =========================================================================
DO $$
BEGIN
    IF to_regclass('public.licensing_change_requests') IS NOT NULL OR to_regclass('public.licensing_change_kinds') IS NOT NULL
       OR to_regclass('public.licensing_change_approvals') IS NOT NULL THEN
        RAISE EXCEPTION 'migration 0177 pre-flight: a licensing_change table already exists' USING ERRCODE = 'LC098';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE schemaname = 'public' AND tablename = 'tenant_jurisdiction_configs'
                    AND policyname = 'tenant_isolation' AND cmd = 'ALL') THEN
        RAISE EXCEPTION 'migration 0177 pre-flight: tenant_jurisdiction_configs.tenant_isolation is not the 0005 policy' USING ERRCODE = 'LC098';
    END IF;
END
$$;

-- More than one open version per (tenant, jurisdiction) would make the new unique index fail: refuse with a clear message.
DO $$
DECLARE
    v_dups bigint;
BEGIN
    ALTER TABLE tenant_jurisdiction_configs NO FORCE ROW LEVEL SECURITY;
    SELECT count(*) INTO v_dups FROM (
        SELECT 1 FROM tenant_jurisdiction_configs WHERE effective_to IS NULL GROUP BY tenant_id, jurisdiction_id HAVING count(*) > 1) d;
    ALTER TABLE tenant_jurisdiction_configs FORCE ROW LEVEL SECURITY;
    IF v_dups > 0 THEN
        RAISE EXCEPTION 'migration 0177 pre-flight: % (tenant, jurisdiction) pairs have more than one open tenant_jurisdiction_configs version; close the older ones first', v_dups
            USING ERRCODE = 'LC098';
    END IF;
END
$$;

-- =========================================================================
-- 1. The signed actor proof verifier: 0175's body plus the licensing_change block (and the platform-scope operation list).
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
    -- 0177 (licensing / jurisdiction maker-checker, MKT-DUAL-1): licensing_change:* is provable only for scope 'platform'
    -- (NULL tenant) or, for request / cancel only, scope 'tenant' (with a tenant). approve / reject / execute are platform
    -- only. The operation set is closed.
    IF p_operation LIKE 'licensing\_change:%' THEN
        IF p_operation NOT IN ('licensing_change:request', 'licensing_change:approve', 'licensing_change:reject',
                               'licensing_change:cancel', 'licensing_change:execute')
           OR p_scope NOT IN ('platform', 'tenant')
           OR (p_operation IN ('licensing_change:approve', 'licensing_change:reject', 'licensing_change:execute') AND p_scope <> 'platform') THEN
            RAISE EXCEPTION 'actor_proof: this licensing_change operation is not provable for this scope' USING ERRCODE = 'AP004';
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
                                         OR p_operation LIKE 'staff\_lifecycle:%'
                                         OR p_operation LIKE 'licensing\_change:%') THEN
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
-- =========================================================================
-- 2. Helpers
-- =========================================================================

-- The session actor of a request / approval / decision: exactly a TENANT principal (app.tenant_id + app.principal_id, an
-- ACTIVE staff row of that tenant) or exactly a PLATFORM principal (app.platform_admin_principal_id alone, an ACTIVE platform
-- staff row); never an acting / player / service / mixed session (LC001). The Person is required (four-eyes needs it).
CREATE FUNCTION licensing_change_actor(
    OUT actor uuid, OUT scope text, OUT tenant uuid, OUT person_id uuid, OUT role text
) LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_tenant    text := NULLIF(current_setting('app.tenant_id', true), '');
    v_principal text := NULLIF(current_setting('app.principal_id', true), '');
    v_platform  text := NULLIF(current_setting('app.platform_admin_principal_id', true), '');
    v_player    text := NULLIF(current_setting('app.player_account_id', true), '');
    v_service   text := NULLIF(current_setting('app.platform_service_id', true), '');
    rec RECORD;
BEGIN
    IF v_player IS NOT NULL OR v_service IS NOT NULL OR public.financial_acting_gucs_present() THEN
        RAISE EXCEPTION 'licensing change: this session may not act on licensing changes' USING ERRCODE = 'LC001';
    END IF;
    IF v_tenant IS NOT NULL AND v_principal IS NOT NULL AND v_platform IS NULL THEN
        SELECT su.id, su.person_id, su.status, su.role INTO rec
          FROM public.staff_users su WHERE su.id = v_principal::uuid AND su.tenant_id = v_tenant::uuid;
        IF NOT FOUND OR rec.status <> 'active' OR rec.person_id IS NULL THEN
            RAISE EXCEPTION 'licensing change: the tenant principal is not an active staff member of the tenant with a Person' USING ERRCODE = 'LC001';
        END IF;
        actor := rec.id; scope := 'tenant'; tenant := v_tenant::uuid; person_id := rec.person_id; role := rec.role;
        RETURN;
    END IF;
    IF v_platform IS NOT NULL AND v_tenant IS NULL AND v_principal IS NULL THEN
        SELECT su.id, su.person_id, su.status, su.role INTO rec
          FROM public.staff_users su WHERE su.id = v_platform::uuid AND su.tenant_id IS NULL;
        IF NOT FOUND OR rec.status <> 'active' OR rec.person_id IS NULL THEN
            RAISE EXCEPTION 'licensing change: the platform principal is not an active platform staff member with a Person' USING ERRCODE = 'LC001';
        END IF;
        actor := rec.id; scope := 'platform'; tenant := NULL; person_id := rec.person_id; role := rec.role;
        RETURN;
    END IF;
    RAISE EXCEPTION 'licensing change: the session is not a valid tenant or platform principal session' USING ERRCODE = 'LC001';
END
$$;

-- The object's key set is exactly p_keys (every key present, no other key).
CREATE FUNCTION licensing_change_keys_are(p jsonb, p_keys text[]) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT pg_catalog.jsonb_typeof(p) = 'object'
       AND (SELECT COALESCE(pg_catalog.array_agg(k ORDER BY k COLLATE "C"), '{}'::text[]) FROM pg_catalog.jsonb_object_keys(p) k)
           = (SELECT pg_catalog.array_agg(k ORDER BY k COLLATE "C") FROM pg_catalog.unnest(p_keys) k)
$$;

CREATE FUNCTION licensing_change_is_uuid(p jsonb) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT COALESCE(pg_catalog.jsonb_typeof(p) = 'string'
       AND (p #>> '{}') ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$', false)
$$;

-- A non-blank, bounded string with no control character.
CREATE FUNCTION licensing_change_is_text(p jsonb, p_max int) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT COALESCE(pg_catalog.jsonb_typeof(p) = 'string'
       AND pg_catalog.btrim(p #>> '{}') <> '' AND pg_catalog.octet_length(p #>> '{}') <= p_max
       AND (p #>> '{}') !~ '[[:cntrl:]]', false)
$$;

-- JSON null, or licensing_change_is_text.
CREATE FUNCTION licensing_change_is_opt_text(p jsonb, p_max int) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT COALESCE(pg_catalog.jsonb_typeof(p) = 'null', false) OR public.licensing_change_is_text(p, p_max)
$$;

-- A JSON array of at most p_max_items distinct bounded strings (64 octets each).
CREATE FUNCTION licensing_change_is_text_array(p jsonb, p_max_items int) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT COALESCE(pg_catalog.jsonb_typeof(p) = 'array'
       AND pg_catalog.jsonb_array_length(p) <= p_max_items
       AND NOT EXISTS (SELECT 1 FROM pg_catalog.jsonb_array_elements(p) e WHERE NOT public.licensing_change_is_text(e, 64))
       AND (SELECT count(DISTINCT e) FROM pg_catalog.jsonb_array_elements(p) e) = pg_catalog.jsonb_array_length(p), false)
$$;

-- What every proof of a request binds: the kind, the canonical jsonb text of the payload and the reason code. jsonb's text
-- form is canonical (keys ordered, whitespace normalised), so equal payloads hash equally; the hash is computed by the
-- database only and read back by the application before it signs.
CREATE FUNCTION licensing_change_payload_hash(p_kind text, p_payload jsonb, p_reason_code text) RETURNS text
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT public.k2_sha256_hex(public.k2_canonical('licensing_change:v1', p_kind, p_payload::text, p_reason_code))
$$;

-- =========================================================================
-- 3. The kind registry (closed; the extension hook)
-- =========================================================================
CREATE TABLE licensing_change_kinds (
    kind               TEXT PRIMARY KEY CHECK (kind ~ '^[a-z][a-z_]{2,63}$'),
    platform_only      BOOLEAN NOT NULL,
    widening_approvals INT NOT NULL CHECK (widening_approvals BETWEEN 1 AND 5),
    approver_roles     TEXT[] NOT NULL CHECK (pg_catalog.cardinality(approver_roles) >= 1),
    description        TEXT NOT NULL CHECK (pg_catalog.btrim(description) <> '')
);
COMMENT ON TABLE licensing_change_kinds IS '0177: the CLOSED registry of licensing / jurisdiction change kinds (MKT-DUAL-1). Written by migrations only (no write policy, no runtime write grant). widening_approvals and approver_roles are the INTERIM answer to HDR-M-1 (platform_admin only, 2 approvals); changing them is a reviewed migration. A new kind (for example the licence-lifecycle slice''s reinstate / extend-expiry) needs a row here AND a branch in licensing_change_classify and licensing_change_row_matches; without both it fails closed (LC010 / LC040).';

INSERT INTO licensing_change_kinds (kind, platform_only, widening_approvals, approver_roles, description) VALUES
    ('licence_create', true, 2, ARRAY['platform_admin'], 'Create a licences row (always widening).'),
    ('tenant_licence_bind', true, 2, ARRAY['platform_admin'], 'Bind or rebind tenants.licence_id to a licence (always widening).'),
    ('tenant_licence_unbind', true, 2, ARRAY['platform_admin'], 'Clear tenants.licence_id (always narrowing: single actor).'),
    ('licence_ceiling_version', true, 2, ARRAY['platform_admin'], 'Append a licence_country_ceilings version (widening iff enabled + active).'),
    ('operating_policy_version', false, 2, ARRAY['platform_admin'], 'Append an operating_country_policies version (widening iff enabled + active, or withdrawn at an inheriting brand / operation rung).'),
    ('tenant_jurisdiction_config_version', false, 2, ARRAY['platform_admin'], 'Set (close + append) or close a tenant_jurisdiction_configs version (narrowing iff it only removes currencies / payment methods, adds geo blocks, keeps every ruleset id, or closes).');

ALTER TABLE licensing_change_kinds ENABLE ROW LEVEL SECURITY;
ALTER TABLE licensing_change_kinds FORCE ROW LEVEL SECURITY;
CREATE POLICY licensing_change_kinds_read ON licensing_change_kinds FOR SELECT USING (
    (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
     OR (NULLIF(current_setting('app.tenant_id', true), '') IS NOT NULL AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL))
    AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    AND NOT financial_acting_gucs_present()
);

-- =========================================================================
-- 4. Payload validation and classification (per kind; an unknown kind fails closed)
-- =========================================================================
-- Returns widening and the subject tenant / licence. VOLATILE: the configuration kind locks the open version it compares
-- against, so a narrowing decision cannot race a concurrent version change.
CREATE FUNCTION licensing_change_classify(p_kind text, p jsonb,
    OUT widening boolean, OUT subject_tenant uuid, OUT subject_licence uuid)
    LANGUAGE plpgsql VOLATILE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_open   public.tenant_jurisdiction_configs%ROWTYPE;
    v_state  text := p ->> 'state';
    v_status text := p ->> 'status';
    v_scope  text := p ->> 'scope';
BEGIN
    IF p_kind = 'licence_create' THEN
        IF NOT public.licensing_change_keys_are(p, ARRAY['jurisdiction_id', 'licensee', 'licence_number', 'permitted_products', 'permitted_markets'])
           OR NOT public.licensing_change_is_uuid(p -> 'jurisdiction_id')
           OR NOT COALESCE(p ->> 'licensee' IN ('platform', 'tenant'), false)
           OR NOT public.licensing_change_is_text(p -> 'licence_number', 200)
           OR NOT public.licensing_change_is_text_array(p -> 'permitted_products', 64)
           OR NOT public.licensing_change_is_text_array(p -> 'permitted_markets', 64) THEN
            RAISE EXCEPTION 'licensing change: invalid licence_create payload' USING ERRCODE = 'LC010';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM public.jurisdictions j WHERE j.id = (p ->> 'jurisdiction_id')::uuid) THEN
            RAISE EXCEPTION 'licensing change: unknown jurisdiction' USING ERRCODE = 'LC010';
        END IF;
        widening := true;
        RETURN;
    END IF;

    IF p_kind IN ('tenant_licence_bind', 'tenant_licence_unbind') THEN
        IF (p_kind = 'tenant_licence_bind' AND (NOT public.licensing_change_keys_are(p, ARRAY['tenant_id', 'licence_id'])
                                                OR NOT public.licensing_change_is_uuid(p -> 'licence_id')))
           OR (p_kind = 'tenant_licence_unbind' AND NOT public.licensing_change_keys_are(p, ARRAY['tenant_id']))
           OR NOT public.licensing_change_is_uuid(p -> 'tenant_id') THEN
            RAISE EXCEPTION 'licensing change: invalid % payload', p_kind USING ERRCODE = 'LC010';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM public.tenants t WHERE t.id = (p ->> 'tenant_id')::uuid) THEN
            RAISE EXCEPTION 'licensing change: unknown tenant' USING ERRCODE = 'LC010';
        END IF;
        subject_tenant := (p ->> 'tenant_id')::uuid;
        IF p_kind = 'tenant_licence_bind' THEN
            IF NOT EXISTS (SELECT 1 FROM public.licences l WHERE l.id = (p ->> 'licence_id')::uuid) THEN
                RAISE EXCEPTION 'licensing change: unknown licence' USING ERRCODE = 'LC010';
            END IF;
            subject_licence := (p ->> 'licence_id')::uuid;
            widening := true;
        ELSE
            widening := false;
        END IF;
        RETURN;
    END IF;

    IF p_kind IN ('licence_ceiling_version', 'operating_policy_version') THEN
        IF (p_kind = 'licence_ceiling_version'
                AND (NOT public.licensing_change_keys_are(p, ARRAY['licence_id', 'country_code', 'state', 'status', 'authorization_reference'])
                     OR NOT public.licensing_change_is_uuid(p -> 'licence_id')))
           OR (p_kind = 'operating_policy_version'
                AND (NOT public.licensing_change_keys_are(p, ARRAY['tenant_id', 'scope', 'brand_id', 'operation_code', 'product_code',
                                                                    'country_code', 'state', 'status', 'authorization_reference'])
                     OR NOT public.licensing_change_is_uuid(p -> 'tenant_id')
                     OR NOT COALESCE(v_scope IN ('tenant', 'brand', 'operation'), false)
                     OR NOT (COALESCE(pg_catalog.jsonb_typeof(p -> 'brand_id') = 'null', false) OR public.licensing_change_is_uuid(p -> 'brand_id'))
                     OR NOT public.licensing_change_is_opt_text(p -> 'operation_code', 64)
                     OR NOT public.licensing_change_is_opt_text(p -> 'product_code', 64)
                     OR (v_scope = 'tenant' AND (p ->> 'brand_id' IS NOT NULL OR p ->> 'operation_code' IS NOT NULL OR p ->> 'product_code' IS NOT NULL))
                     OR (v_scope = 'brand' AND (p ->> 'brand_id' IS NULL OR p ->> 'operation_code' IS NOT NULL OR p ->> 'product_code' IS NOT NULL))
                     OR (v_scope = 'operation' AND p ->> 'operation_code' IS NULL)))
           OR NOT COALESCE((p ->> 'country_code') ~ '^[A-Z]{2}$', false)
           OR NOT COALESCE(v_state IN ('enabled', 'disabled'), false)
           OR NOT COALESCE(v_status IN ('active', 'withdrawn'), false)
           OR NOT public.licensing_change_is_opt_text(p -> 'authorization_reference', 500)
           OR (v_status = 'active' AND v_state = 'enabled' AND p ->> 'authorization_reference' IS NULL)
           OR (v_status = 'withdrawn' AND v_state <> 'disabled')
           OR (p_kind = 'operating_policy_version' AND v_status = 'withdrawn' AND v_scope IN ('brand', 'operation')
               AND p ->> 'authorization_reference' IS NULL) THEN
            RAISE EXCEPTION 'licensing change: invalid % payload', p_kind USING ERRCODE = 'LC010';
        END IF;
        IF p_kind = 'licence_ceiling_version' THEN
            IF NOT EXISTS (SELECT 1 FROM public.licences l WHERE l.id = (p ->> 'licence_id')::uuid) THEN
                RAISE EXCEPTION 'licensing change: unknown licence' USING ERRCODE = 'LC010';
            END IF;
            subject_licence := (p ->> 'licence_id')::uuid;
            -- A withdrawn or absent ceiling resolves not_permitted_by_licence: only enabled + active widens.
            widening := (v_state = 'enabled' AND v_status = 'active');
        ELSE
            IF NOT EXISTS (SELECT 1 FROM public.tenants t WHERE t.id = (p ->> 'tenant_id')::uuid) THEN
                RAISE EXCEPTION 'licensing change: unknown tenant' USING ERRCODE = 'LC010';
            END IF;
            subject_tenant := (p ->> 'tenant_id')::uuid;
            -- ADR 0045 3.5-A AMENDMENT-2: absence at the brand / operation rung inherits, so a withdrawal there widens.
            widening := (v_state = 'enabled' AND v_status = 'active')
                        OR (v_status = 'withdrawn' AND v_scope IN ('brand', 'operation'));
        END IF;
        RETURN;
    END IF;

    IF p_kind = 'tenant_jurisdiction_config_version' THEN
        IF NOT public.licensing_change_keys_are(p, ARRAY['tenant_id', 'jurisdiction_id', 'action', 'kyc_ruleset_id', 'aml_ruleset_id',
                                                          'rg_ruleset_id', 'reporting_ruleset_id', 'allowed_currencies',
                                                          'allowed_payment_methods', 'geo_block_list'])
           OR NOT public.licensing_change_is_uuid(p -> 'tenant_id')
           OR NOT public.licensing_change_is_uuid(p -> 'jurisdiction_id')
           OR NOT COALESCE(p ->> 'action' IN ('set', 'close'), false)
           OR (p ->> 'action' = 'set' AND (
                   NOT public.licensing_change_is_opt_text(p -> 'kyc_ruleset_id', 128)
                OR NOT public.licensing_change_is_opt_text(p -> 'aml_ruleset_id', 128)
                OR NOT public.licensing_change_is_opt_text(p -> 'rg_ruleset_id', 128)
                OR NOT public.licensing_change_is_opt_text(p -> 'reporting_ruleset_id', 128)
                OR NOT public.licensing_change_is_text_array(p -> 'allowed_currencies', 64)
                OR NOT public.licensing_change_is_text_array(p -> 'allowed_payment_methods', 64)
                OR NOT public.licensing_change_is_text_array(p -> 'geo_block_list', 300)))
           OR (p ->> 'action' = 'close' AND (
                   p ->> 'kyc_ruleset_id' IS NOT NULL OR p ->> 'aml_ruleset_id' IS NOT NULL OR p ->> 'rg_ruleset_id' IS NOT NULL
                OR p ->> 'reporting_ruleset_id' IS NOT NULL OR p ->> 'allowed_currencies' IS NOT NULL
                OR p ->> 'allowed_payment_methods' IS NOT NULL OR p ->> 'geo_block_list' IS NOT NULL)) THEN
            RAISE EXCEPTION 'licensing change: invalid tenant_jurisdiction_config_version payload' USING ERRCODE = 'LC010';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM public.tenants t WHERE t.id = (p ->> 'tenant_id')::uuid)
           OR NOT EXISTS (SELECT 1 FROM public.jurisdictions j WHERE j.id = (p ->> 'jurisdiction_id')::uuid) THEN
            RAISE EXCEPTION 'licensing change: unknown tenant or jurisdiction' USING ERRCODE = 'LC010';
        END IF;
        subject_tenant := (p ->> 'tenant_id')::uuid;
        -- A platform session (the executor, and a platform requester) locks the open version; a tenant session has no UPDATE
        -- policy on the table (FOR UPDATE would hide the row) and reads it plainly: its classification is re-done, locked,
        -- by the platform executor (LC024).
        IF NULLIF(pg_catalog.current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL THEN
            SELECT * INTO v_open FROM public.tenant_jurisdiction_configs c
             WHERE c.tenant_id = subject_tenant AND c.jurisdiction_id = (p ->> 'jurisdiction_id')::uuid AND c.effective_to IS NULL
             ORDER BY c.effective_from DESC LIMIT 1 FOR UPDATE;
        ELSE
            SELECT * INTO v_open FROM public.tenant_jurisdiction_configs c
             WHERE c.tenant_id = subject_tenant AND c.jurisdiction_id = (p ->> 'jurisdiction_id')::uuid AND c.effective_to IS NULL
             ORDER BY c.effective_from DESC LIMIT 1;
        END IF;
        IF p ->> 'action' = 'close' THEN
            IF NOT FOUND THEN
                RAISE EXCEPTION 'licensing change: there is no open configuration to close' USING ERRCODE = 'LC010';
            END IF;
            widening := false;   -- no configuration in effect: H-3 and the AML jurisdiction check refuse (fail closed)
            RETURN;
        END IF;
        widening := NOT (FOUND
            AND v_open.kyc_ruleset_id IS NOT DISTINCT FROM p ->> 'kyc_ruleset_id'
            AND v_open.aml_ruleset_id IS NOT DISTINCT FROM p ->> 'aml_ruleset_id'
            AND v_open.rg_ruleset_id IS NOT DISTINCT FROM p ->> 'rg_ruleset_id'
            AND v_open.reporting_ruleset_id IS NOT DISTINCT FROM p ->> 'reporting_ruleset_id'
            AND (p -> 'allowed_currencies') <@ v_open.allowed_currencies
            AND (p -> 'allowed_payment_methods') <@ v_open.allowed_payment_methods
            AND v_open.geo_block_list <@ (p -> 'geo_block_list'));
        RETURN;
    END IF;

    RAISE EXCEPTION 'licensing change: kind % has no classification (closed registry)', p_kind USING ERRCODE = 'LC010';
END
$$;

-- Does the written row (p_new; p_old on UPDATE) match an executed request of this kind and payload? An unknown table or kind
-- is false (fail closed).
CREATE FUNCTION licensing_change_row_matches(p_kind text, p jsonb, p_reason text, p_subject uuid,
    p_table text, p_op text, p_new jsonb, p_old jsonb) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT COALESCE(CASE
        WHEN p_table = 'licences' AND p_op = 'INSERT' THEN
            p_kind = 'licence_create'
            AND p_new ->> 'jurisdiction_id' = p ->> 'jurisdiction_id' AND p_new ->> 'licensee' = p ->> 'licensee'
            AND p_new ->> 'licence_number' = p ->> 'licence_number'
            AND p_new -> 'permitted_products' = p -> 'permitted_products' AND p_new -> 'permitted_markets' = p -> 'permitted_markets'
            AND p_new ->> 'status' = 'active' AND p_new ->> 'issued_at' IS NULL AND p_new ->> 'expires_at' IS NULL
        WHEN p_table = 'tenants' THEN
            p_subject::text = p_new ->> 'id'
            AND ((p_kind = 'tenant_licence_bind' AND p_new ->> 'licence_id' = p ->> 'licence_id')
                 OR (p_kind = 'tenant_licence_unbind' AND p_op = 'UPDATE' AND p_new ->> 'licence_id' IS NULL))
        WHEN p_table = 'licence_country_ceilings' AND p_op = 'INSERT' THEN
            p_kind = 'licence_ceiling_version'
            AND p_new ->> 'licence_id' = p ->> 'licence_id' AND p_new ->> 'country_code' = p ->> 'country_code'
            AND p_new ->> 'state' = p ->> 'state' AND p_new ->> 'status' = p ->> 'status'
            AND (p_new ->> 'authorization_reference') IS NOT DISTINCT FROM (p ->> 'authorization_reference')
            AND p_new ->> 'reason_code' = p_reason
        WHEN p_table = 'licence_country_ceilings' AND p_op = 'UPDATE' THEN
            p_kind = 'licence_ceiling_version'
            AND p_old ->> 'licence_id' = p ->> 'licence_id' AND p_old ->> 'country_code' = p ->> 'country_code'
        WHEN p_table = 'operating_country_policies' AND p_op = 'INSERT' THEN
            p_kind = 'operating_policy_version'
            AND p_new ->> 'tenant_id' = p ->> 'tenant_id' AND p_new ->> 'scope_kind' = p ->> 'scope'
            AND (p_new ->> 'brand_id') IS NOT DISTINCT FROM (p ->> 'brand_id')
            AND (p_new ->> 'operation_code') IS NOT DISTINCT FROM (p ->> 'operation_code')
            AND (p_new ->> 'product_code') IS NOT DISTINCT FROM (p ->> 'product_code')
            AND p_new ->> 'country_code' = p ->> 'country_code'
            AND p_new ->> 'state' = p ->> 'state' AND p_new ->> 'status' = p ->> 'status'
            AND (p_new ->> 'authorization_reference') IS NOT DISTINCT FROM (p ->> 'authorization_reference')
            AND p_new ->> 'reason_code' = p_reason
        WHEN p_table = 'operating_country_policies' AND p_op = 'UPDATE' THEN
            p_kind = 'operating_policy_version'
            AND p_old ->> 'tenant_id' = p ->> 'tenant_id' AND p_old ->> 'scope_kind' = p ->> 'scope'
            AND (p_old ->> 'brand_id') IS NOT DISTINCT FROM (p ->> 'brand_id')
            AND (p_old ->> 'operation_code') IS NOT DISTINCT FROM (p ->> 'operation_code')
            AND (p_old ->> 'product_code') IS NOT DISTINCT FROM (p ->> 'product_code')
            AND p_old ->> 'country_code' = p ->> 'country_code'
        WHEN p_table = 'tenant_jurisdiction_configs' AND p_op = 'INSERT' THEN
            p_kind = 'tenant_jurisdiction_config_version' AND p ->> 'action' = 'set'
            AND p_new ->> 'tenant_id' = p ->> 'tenant_id' AND p_new ->> 'jurisdiction_id' = p ->> 'jurisdiction_id'
            AND (p_new ->> 'kyc_ruleset_id') IS NOT DISTINCT FROM (p ->> 'kyc_ruleset_id')
            AND (p_new ->> 'aml_ruleset_id') IS NOT DISTINCT FROM (p ->> 'aml_ruleset_id')
            AND (p_new ->> 'rg_ruleset_id') IS NOT DISTINCT FROM (p ->> 'rg_ruleset_id')
            AND (p_new ->> 'reporting_ruleset_id') IS NOT DISTINCT FROM (p ->> 'reporting_ruleset_id')
            AND p_new -> 'allowed_currencies' = p -> 'allowed_currencies'
            AND p_new -> 'allowed_payment_methods' = p -> 'allowed_payment_methods'
            AND p_new -> 'geo_block_list' = p -> 'geo_block_list'
        WHEN p_table = 'tenant_jurisdiction_configs' AND p_op = 'UPDATE' THEN
            p_kind = 'tenant_jurisdiction_config_version'
            AND p_old ->> 'tenant_id' = p ->> 'tenant_id' AND p_old ->> 'jurisdiction_id' = p ->> 'jurisdiction_id'
        ELSE false
    END, false)
$$;

-- =========================================================================
-- 5. licensing_change_requests / licensing_change_approvals
-- =========================================================================
CREATE TABLE licensing_change_requests (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind                   TEXT NOT NULL REFERENCES licensing_change_kinds (kind) ON DELETE RESTRICT,
    payload                JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'object' AND octet_length(payload::text) <= 32768),
    payload_hash           TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    reason_code            TEXT NOT NULL CHECK (reason_code ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$'),
    note                   TEXT NULL CHECK (note IS NULL OR (btrim(note) <> '' AND octet_length(note) <= 1000 AND note !~ '[[:cntrl:]]')),
    widening               BOOLEAN NOT NULL,
    required_approvals     INT NOT NULL CHECK (required_approvals BETWEEN 0 AND 5),
    subject_tenant_id      UUID NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    subject_licence_id     UUID NULL REFERENCES licences (id) ON DELETE RESTRICT,
    requested_by           UUID NOT NULL,
    requested_by_person_id UUID NOT NULL,
    requested_scope        TEXT NOT NULL CHECK (requested_scope IN ('platform', 'tenant')),
    requested_tenant_id    UUID NULL,
    status                 TEXT NOT NULL CHECK (status IN ('requested', 'approved', 'executed', 'rejected', 'cancelled', 'expired')),
    created_at             TIMESTAMPTZ NOT NULL,
    created_txid           BIGINT NOT NULL,
    expires_at             TIMESTAMPTZ NOT NULL,
    decided_by             UUID NULL,
    decided_by_person_id   UUID NULL,
    decided_at             TIMESTAMPTZ NULL,
    decision_reason_code   TEXT NULL CHECK (decision_reason_code IS NULL OR decision_reason_code ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$'),
    executed_by            UUID NULL,
    executed_at            TIMESTAMPTZ NULL,
    executed_txid          BIGINT NULL,
    CONSTRAINT licensing_change_requests_scope_tenant CHECK ((requested_scope = 'tenant') = (requested_tenant_id IS NOT NULL)),
    CONSTRAINT licensing_change_requests_executed CHECK ((status = 'executed') = (executed_by IS NOT NULL AND executed_at IS NOT NULL AND executed_txid IS NOT NULL)),
    CONSTRAINT licensing_change_requests_narrowing_single CHECK (widening OR required_approvals <= 1),
    CONSTRAINT licensing_change_requests_widening_two CHECK (NOT widening OR required_approvals >= 1)
);
CREATE UNIQUE INDEX uq_licensing_change_requests_open_payload ON licensing_change_requests (payload_hash) WHERE status = 'requested';
CREATE INDEX idx_licensing_change_requests_status ON licensing_change_requests (status, created_at DESC);
CREATE INDEX idx_licensing_change_requests_subject_tenant ON licensing_change_requests (subject_tenant_id, created_at DESC);
CREATE INDEX idx_licensing_change_requests_executed_txid ON licensing_change_requests (executed_txid) WHERE executed_txid IS NOT NULL;
COMMENT ON TABLE licensing_change_requests IS '0177: append-only licensing / jurisdiction change requests (MKT-DUAL-1, G-1 / G-2 / G-9). Content is immutable; only the state columns move, through the guard''s closed state machine, each move proof-bound (ADR 0110). An executed request is what admits the governed-table write of its own transaction (LC040).';

CREATE TABLE licensing_change_approvals (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id         UUID NOT NULL REFERENCES licensing_change_requests (id) ON DELETE RESTRICT,
    approver_id        UUID NOT NULL,
    approver_person_id UUID NOT NULL,
    payload_hash       TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    reason_code        TEXT NOT NULL CHECK (reason_code ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$'),
    decided_at         TIMESTAMPTZ NOT NULL,
    CONSTRAINT uq_licensing_change_approvals_person UNIQUE (request_id, approver_person_id),
    CONSTRAINT uq_licensing_change_approvals_staff UNIQUE (request_id, approver_id)
);
COMMENT ON TABLE licensing_change_approvals IS '0177: append-only approvals of licensing change requests, one per Person per request, each proof-bound (licensing_change:approve).';

-- Approvals that count: approve rows on the request''s own payload hash, at most 24 h old, by distinct Persons that are neither
-- the requester''s Person nor the requester''s account, each by an ACTIVE platform staff account whose role is one of the
-- kind''s approver roles and whose Person is still the recorded one.
CREATE FUNCTION licensing_change_fresh_approvals(p_request uuid) RETURNS int
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT count(DISTINCT a.approver_person_id)::int
      FROM public.licensing_change_approvals a
      JOIN public.licensing_change_requests r ON r.id = a.request_id
      JOIN public.licensing_change_kinds k ON k.kind = r.kind
      JOIN public.staff_users s ON s.id = a.approver_id
     WHERE a.request_id = p_request
       AND a.payload_hash = r.payload_hash
       AND a.decided_at > pg_catalog.clock_timestamp() - interval '24 hours'
       AND a.approver_person_id <> r.requested_by_person_id
       AND a.approver_id <> r.requested_by
       AND s.person_id = a.approver_person_id AND s.status = 'active' AND s.tenant_id IS NULL
       AND s.role = ANY (k.approver_roles)
$$;

-- The request guard: INSERT derives everything that matters (actor, scope, classification, approvals needed, hash, times) and
-- consumes the requester's proof; UPDATE is the closed state machine, each move checked and (except expiry) proof-bound.
CREATE FUNCTION licensing_change_requests_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor RECORD;
    v_kind  public.licensing_change_kinds%ROWTYPE;
    v_cls   RECORD;
    v_now   timestamptz := pg_catalog.clock_timestamp();
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'licensing_change_requests is append-only: DELETE is not permitted' USING ERRCODE = 'LC099';
    END IF;

    IF TG_OP = 'INSERT' THEN
        SELECT * INTO v_actor FROM public.licensing_change_actor();
        SELECT * INTO v_kind FROM public.licensing_change_kinds k WHERE k.kind = NEW.kind;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'licensing change: unknown kind' USING ERRCODE = 'LC010';
        END IF;
        IF v_actor.scope = 'tenant' AND (v_kind.platform_only OR v_actor.role NOT IN ('tenant_admin', 'compliance')) THEN
            RAISE EXCEPTION 'licensing change: a tenant session may not request %', NEW.kind USING ERRCODE = 'LC002';
        END IF;
        IF v_actor.scope = 'platform' AND v_actor.role <> 'platform_admin' THEN
            RAISE EXCEPTION 'licensing change: only a platform_admin requests from the platform scope' USING ERRCODE = 'LC001';
        END IF;
        SELECT * INTO v_cls FROM public.licensing_change_classify(NEW.kind, NEW.payload);
        IF v_actor.scope = 'tenant' AND v_cls.subject_tenant IS DISTINCT FROM v_actor.tenant THEN
            RAISE EXCEPTION 'licensing change: a tenant session requests changes of its own tenant only' USING ERRCODE = 'LC002';
        END IF;
        NEW.widening := v_cls.widening;
        NEW.subject_tenant_id := v_cls.subject_tenant;
        NEW.subject_licence_id := v_cls.subject_licence;
        NEW.required_approvals := CASE WHEN v_cls.widening THEN v_kind.widening_approvals
                                       WHEN v_actor.scope = 'tenant' THEN 1 ELSE 0 END;
        NEW.payload_hash := public.licensing_change_payload_hash(NEW.kind, NEW.payload, NEW.reason_code);
        NEW.requested_by := v_actor.actor;
        NEW.requested_by_person_id := v_actor.person_id;
        NEW.requested_scope := v_actor.scope;
        NEW.requested_tenant_id := v_actor.tenant;
        NEW.status := 'requested';
        NEW.created_at := v_now;
        NEW.created_txid := pg_catalog.txid_current();
        NEW.expires_at := v_now + interval '72 hours';
        NEW.decided_by := NULL; NEW.decided_by_person_id := NULL; NEW.decided_at := NULL; NEW.decision_reason_code := NULL;
        NEW.executed_by := NULL; NEW.executed_at := NULL; NEW.executed_txid := NULL;
        PERFORM public.actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant, 'licensing_change:request', 'new', NEW.payload_hash);
        RETURN NEW;
    END IF;

    -- UPDATE: the content never changes.
    IF (pg_catalog.to_jsonb(NEW) - ARRAY['status', 'decided_by', 'decided_by_person_id', 'decided_at', 'decision_reason_code',
                                          'executed_by', 'executed_at', 'executed_txid'])
       IS DISTINCT FROM
       (pg_catalog.to_jsonb(OLD) - ARRAY['status', 'decided_by', 'decided_by_person_id', 'decided_at', 'decision_reason_code',
                                          'executed_by', 'executed_at', 'executed_txid']) THEN
        RAISE EXCEPTION 'licensing_change_requests: the request content is immutable' USING ERRCODE = 'LC012';
    END IF;
    -- Every state column is re-derived below from OLD; a caller value is never kept.
    NEW.decided_by := OLD.decided_by; NEW.decided_by_person_id := OLD.decided_by_person_id; NEW.decided_at := OLD.decided_at;
    NEW.decision_reason_code := CASE WHEN NEW.status = 'rejected' THEN NEW.decision_reason_code ELSE OLD.decision_reason_code END;
    NEW.executed_by := OLD.executed_by; NEW.executed_at := OLD.executed_at; NEW.executed_txid := OLD.executed_txid;

    -- Expiry needs no proof (it only closes a request) and no particular session.
    IF OLD.status = 'requested' AND NEW.status = 'expired' THEN
        IF v_now < OLD.expires_at THEN
            RAISE EXCEPTION 'licensing_change_requests: the request has not expired' USING ERRCODE = 'LC013';
        END IF;
        RETURN NEW;
    END IF;

    SELECT * INTO v_actor FROM public.licensing_change_actor();
    SELECT * INTO v_kind FROM public.licensing_change_kinds k WHERE k.kind = OLD.kind;

    IF NEW.status = 'cancelled' THEN
        IF OLD.status <> 'requested' THEN
            RAISE EXCEPTION 'licensing_change_requests: % -> cancelled is not a legal move', OLD.status USING ERRCODE = 'LC013';
        END IF;
        IF v_actor.actor <> OLD.requested_by THEN
            RAISE EXCEPTION 'licensing_change_requests: only the requester may cancel' USING ERRCODE = 'LC023';
        END IF;
        NEW.decided_by := v_actor.actor; NEW.decided_by_person_id := v_actor.person_id; NEW.decided_at := v_now;
        PERFORM public.actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant, 'licensing_change:cancel', OLD.id::text, OLD.payload_hash);
        RETURN NEW;
    END IF;

    IF NEW.status = 'rejected' THEN
        IF OLD.status <> 'requested' OR v_now >= OLD.expires_at THEN
            RAISE EXCEPTION 'licensing_change_requests: % -> rejected is not a legal move (or the request expired)', OLD.status USING ERRCODE = 'LC013';
        END IF;
        IF v_actor.scope <> 'platform' OR NOT (v_actor.role = ANY (v_kind.approver_roles)) THEN
            RAISE EXCEPTION 'licensing_change_requests: only an eligible platform approver rejects' USING ERRCODE = 'LC030';
        END IF;
        IF v_actor.actor = OLD.requested_by OR v_actor.person_id = OLD.requested_by_person_id THEN
            RAISE EXCEPTION 'licensing_change_requests: the requester''s Person may not decide the request' USING ERRCODE = 'LC022';
        END IF;
        IF NEW.decision_reason_code IS NULL THEN
            RAISE EXCEPTION 'licensing_change_requests: a rejection carries a reason code' USING ERRCODE = 'LC010';
        END IF;
        NEW.decided_by := v_actor.actor; NEW.decided_by_person_id := v_actor.person_id; NEW.decided_at := v_now;
        PERFORM public.actor_proof_require(v_actor.actor, 'platform', NULL, 'licensing_change:reject', OLD.id::text, OLD.payload_hash);
        RETURN NEW;
    END IF;

    IF NEW.status = 'approved' THEN
        IF OLD.status <> 'requested' OR v_now >= OLD.expires_at OR OLD.required_approvals = 0 THEN
            RAISE EXCEPTION 'licensing_change_requests: % -> approved is not a legal move (or the request expired)', OLD.status USING ERRCODE = 'LC013';
        END IF;
        IF v_actor.scope <> 'platform' THEN
            RAISE EXCEPTION 'licensing_change_requests: only a platform session records the approved state' USING ERRCODE = 'LC030';
        END IF;
        IF public.licensing_change_fresh_approvals(OLD.id) < OLD.required_approvals THEN
            RAISE EXCEPTION 'licensing_change_requests: the request does not hold % fresh approvals by distinct eligible Persons', OLD.required_approvals
                USING ERRCODE = 'LC020';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.status = 'executed' THEN
        IF v_now >= OLD.expires_at THEN
            RAISE EXCEPTION 'licensing_change_requests: the request has expired' USING ERRCODE = 'LC013';
        END IF;
        IF v_actor.scope <> 'platform' OR v_actor.role <> 'platform_admin' THEN
            RAISE EXCEPTION 'licensing_change_requests: only a platform_admin session executes' USING ERRCODE = 'LC030';
        END IF;
        IF OLD.required_approvals = 0 THEN
            IF OLD.status <> 'requested' OR v_actor.actor <> OLD.requested_by OR OLD.created_txid <> pg_catalog.txid_current() THEN
                RAISE EXCEPTION 'licensing_change_requests: a single-actor change executes only by its requester, in its own transaction' USING ERRCODE = 'LC021';
            END IF;
        ELSE
            IF OLD.status <> 'approved' THEN
                RAISE EXCEPTION 'licensing_change_requests: % -> executed is not a legal move', OLD.status USING ERRCODE = 'LC013';
            END IF;
            IF public.licensing_change_fresh_approvals(OLD.id) < OLD.required_approvals THEN
                RAISE EXCEPTION 'licensing_change_requests: the request does not hold % fresh approvals by distinct eligible Persons', OLD.required_approvals
                    USING ERRCODE = 'LC020';
            END IF;
        END IF;
        -- Re-classify against the state NOW (the configuration kind compares with the open version, locked): a change that
        -- was narrowing when requested but would widen now needs the widening approvals it does not hold.
        SELECT * INTO v_cls FROM public.licensing_change_classify(OLD.kind, OLD.payload);
        IF v_cls.widening AND NOT OLD.widening THEN
            RAISE EXCEPTION 'licensing_change_requests: the change now widens; file a new request' USING ERRCODE = 'LC024';
        END IF;
        PERFORM public.actor_proof_require(v_actor.actor, 'platform', NULL, 'licensing_change:execute', OLD.id::text, OLD.payload_hash);
        NEW.executed_by := v_actor.actor; NEW.executed_at := v_now; NEW.executed_txid := pg_catalog.txid_current();
        RETURN NEW;
    END IF;

    RAISE EXCEPTION 'licensing_change_requests: % -> % is not a legal move', OLD.status, NEW.status USING ERRCODE = 'LC013';
END
$$;
CREATE TRIGGER licensing_change_requests_guard
    BEFORE INSERT OR UPDATE OR DELETE ON licensing_change_requests
    FOR EACH ROW EXECUTE FUNCTION licensing_change_requests_guard();

CREATE FUNCTION licensing_change_deny_mutation() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted', TG_TABLE_NAME, TG_OP USING ERRCODE = 'LC099';
END
$$;
CREATE TRIGGER licensing_change_requests_no_truncate
    BEFORE TRUNCATE ON licensing_change_requests
    FOR EACH STATEMENT EXECUTE FUNCTION licensing_change_deny_mutation();

-- The approval guard: an eligible platform approver, a distinct Person, an open request (locked), the request's hash, the
-- approver's proof.
CREATE FUNCTION licensing_change_approvals_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor RECORD;
    v_req   public.licensing_change_requests%ROWTYPE;
    v_roles text[];
    v_now   timestamptz := pg_catalog.clock_timestamp();
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'licensing_change_approvals is append-only: % is not permitted', TG_OP USING ERRCODE = 'LC099';
    END IF;
    SELECT * INTO v_actor FROM public.licensing_change_actor();
    IF v_actor.scope <> 'platform' THEN
        RAISE EXCEPTION 'licensing change: only a platform session approves' USING ERRCODE = 'LC030';
    END IF;
    SELECT * INTO v_req FROM public.licensing_change_requests r WHERE r.id = NEW.request_id FOR UPDATE;
    IF NOT FOUND OR v_req.status <> 'requested' OR v_now >= v_req.expires_at OR v_req.required_approvals = 0 THEN
        RAISE EXCEPTION 'licensing change: the request is not open for approval' USING ERRCODE = 'LC031';
    END IF;
    SELECT k.approver_roles INTO v_roles FROM public.licensing_change_kinds k WHERE k.kind = v_req.kind;
    IF NOT (v_actor.role = ANY (v_roles)) THEN
        RAISE EXCEPTION 'licensing change: the approver''s role may not approve this kind' USING ERRCODE = 'LC030';
    END IF;
    IF v_actor.actor = v_req.requested_by OR v_actor.person_id = v_req.requested_by_person_id THEN
        RAISE EXCEPTION 'licensing change: the requester''s Person may not approve the request (four-eyes)' USING ERRCODE = 'LC032';
    END IF;
    NEW.approver_id := v_actor.actor;
    NEW.approver_person_id := v_actor.person_id;
    NEW.payload_hash := v_req.payload_hash;
    NEW.decided_at := v_now;
    PERFORM public.actor_proof_require(v_actor.actor, 'platform', NULL, 'licensing_change:approve', v_req.id::text, v_req.payload_hash);
    RETURN NEW;
END
$$;
CREATE TRIGGER licensing_change_approvals_guard
    BEFORE INSERT OR UPDATE OR DELETE ON licensing_change_approvals
    FOR EACH ROW EXECUTE FUNCTION licensing_change_approvals_guard();
CREATE TRIGGER licensing_change_approvals_no_truncate
    BEFORE TRUNCATE ON licensing_change_approvals
    FOR EACH STATEMENT EXECUTE FUNCTION licensing_change_deny_mutation();

-- =========================================================================
-- 6. The governed-write guard (LC040)
-- =========================================================================
-- A write to a governed table by any role but the table owner needs an executed licensing change request of the matching
-- kind and payload, executed in THIS transaction. The executor of an operating_country_policies version binds app.tenant_id to
-- the subject tenant (the table's tenant RLS); the request rows are platform-scoped, so the tenant GUC is cleared for the read
-- and restored (the ADR 0112 launch_subject_status_guard technique).
CREATE FUNCTION licensing_change_governed_write_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_prev_tenant text;
    v_n           bigint;
    v_old         jsonb;
BEGIN
    IF public.launch_is_table_owner() THEN
        RETURN NEW;
    END IF;
    IF TG_TABLE_NAME = 'tenants' THEN
        IF TG_OP = 'UPDATE' AND NEW.licence_id IS NOT DISTINCT FROM OLD.licence_id THEN
            RETURN NEW;
        END IF;
        IF TG_OP = 'INSERT' AND NEW.licence_id IS NULL THEN
            RETURN NEW;
        END IF;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        v_old := pg_catalog.to_jsonb(OLD);
    END IF;
    v_prev_tenant := NULLIF(pg_catalog.current_setting('app.tenant_id', true), '');
    IF v_prev_tenant IS NOT NULL AND NULLIF(pg_catalog.current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL THEN
        PERFORM pg_catalog.set_config('app.tenant_id', '', true);
    ELSE
        v_prev_tenant := NULL;
    END IF;
    SELECT count(*) INTO v_n
      FROM public.licensing_change_requests r
     WHERE r.status = 'executed' AND r.executed_txid = pg_catalog.txid_current()
       AND public.licensing_change_row_matches(r.kind, r.payload, r.reason_code, r.subject_tenant_id,
                                               TG_TABLE_NAME, TG_OP, pg_catalog.to_jsonb(NEW), v_old);
    IF v_prev_tenant IS NOT NULL THEN
        PERFORM pg_catalog.set_config('app.tenant_id', v_prev_tenant, true);
    END IF;
    IF v_n < 1 THEN
        RAISE EXCEPTION '%: a % needs a licensing change request of the matching kind executed in this transaction (MKT-DUAL-1, migration 0177)', TG_TABLE_NAME, TG_OP
            USING ERRCODE = 'LC040';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER zz_licensing_change_governed BEFORE INSERT OR UPDATE ON licence_country_ceilings
    FOR EACH ROW EXECUTE FUNCTION licensing_change_governed_write_guard();
CREATE TRIGGER zz_licensing_change_governed BEFORE INSERT OR UPDATE ON operating_country_policies
    FOR EACH ROW EXECUTE FUNCTION licensing_change_governed_write_guard();
CREATE TRIGGER zz_licensing_change_governed BEFORE INSERT OR UPDATE ON tenant_jurisdiction_configs
    FOR EACH ROW EXECUTE FUNCTION licensing_change_governed_write_guard();
CREATE TRIGGER zz_licensing_change_governed BEFORE INSERT ON licences
    FOR EACH ROW EXECUTE FUNCTION licensing_change_governed_write_guard();
CREATE TRIGGER zz_licensing_change_governed BEFORE INSERT OR UPDATE ON tenants
    FOR EACH ROW EXECUTE FUNCTION licensing_change_governed_write_guard();

-- =========================================================================
-- 7. tenant_jurisdiction_configs: append-only, one open version, narrowed RLS (G-9)
-- =========================================================================
CREATE FUNCTION tenant_jurisdiction_configs_append_only() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'tenant_jurisdiction_configs is append-only: TRUNCATE is not permitted' USING ERRCODE = 'LC041';
    END IF;
    IF TG_OP = 'INSERT' THEN
        -- The executor's versions start now and open; only the owner (fixtures, provisioning) may date a row.
        IF NOT public.launch_is_table_owner() THEN
            NEW.effective_from := now();
            NEW.effective_to := NULL;
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.effective_to IS NOT NULL THEN
        RAISE EXCEPTION 'tenant_jurisdiction_configs: a closed version may not be reopened or re-closed' USING ERRCODE = 'LC041';
    END IF;
    IF NEW.effective_to IS NULL THEN
        RAISE EXCEPTION 'tenant_jurisdiction_configs: only effective_to may change, and only to close the version' USING ERRCODE = 'LC041';
    END IF;
    IF NOT public.launch_is_table_owner() THEN
        NEW.effective_to := now();
    END IF;
    IF (pg_catalog.to_jsonb(NEW) - 'effective_to') IS DISTINCT FROM (pg_catalog.to_jsonb(OLD) - 'effective_to') THEN
        RAISE EXCEPTION 'tenant_jurisdiction_configs: only effective_to may change after insert' USING ERRCODE = 'LC041';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER tenant_jurisdiction_configs_append_only BEFORE INSERT OR UPDATE ON tenant_jurisdiction_configs
    FOR EACH ROW EXECUTE FUNCTION tenant_jurisdiction_configs_append_only();
CREATE TRIGGER tenant_jurisdiction_configs_deny_truncate BEFORE TRUNCATE ON tenant_jurisdiction_configs
    FOR EACH STATEMENT EXECUTE FUNCTION tenant_jurisdiction_configs_append_only();

CREATE UNIQUE INDEX uq_tenant_jurisdiction_configs_open ON tenant_jurisdiction_configs (tenant_id, jurisdiction_id) WHERE effective_to IS NULL;

-- 0005's FOR ALL tenant policy let any tenant-scoped runtime session write the table (G-9). Reads keep exactly the 0005
-- predicate; writes are platform-only (the executor) plus the owner's tenant-scope fixture path.
DROP POLICY tenant_isolation ON tenant_jurisdiction_configs;
CREATE POLICY tjc_tenant_read ON tenant_jurisdiction_configs FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tjc_platform_read ON tenant_jurisdiction_configs FOR SELECT
    USING (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tjc_platform_insert ON tenant_jurisdiction_configs FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
                AND NOT financial_acting_gucs_present());
CREATE POLICY tjc_platform_close ON tenant_jurisdiction_configs FOR UPDATE
    USING (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present())
    WITH CHECK (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
                AND NOT financial_acting_gucs_present());
-- FORCE RLS binds the owner too: its tenant-scope fixture writes need these (strict owner equality, never igaming_runtime).
CREATE POLICY tjc_owner_tenant_insert ON tenant_jurisdiction_configs FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid AND launch_is_table_owner());
CREATE POLICY tjc_owner_tenant_close ON tenant_jurisdiction_configs FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid AND launch_is_table_owner())
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid AND launch_is_table_owner());

-- =========================================================================
-- 8. RLS of the request / approval tables
-- =========================================================================
ALTER TABLE licensing_change_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE licensing_change_requests FORCE ROW LEVEL SECURITY;
ALTER TABLE licensing_change_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE licensing_change_approvals FORCE ROW LEVEL SECURITY;

-- Platform principal shape (the launch p_pred).
CREATE POLICY platform_scope_select ON licensing_change_requests FOR SELECT
    USING (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY platform_scope_insert ON licensing_change_requests FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
                AND NOT financial_acting_gucs_present());
CREATE POLICY platform_scope_update ON licensing_change_requests FOR UPDATE
    USING (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present())
    WITH CHECK (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
                AND NOT financial_acting_gucs_present());
-- Tenant principal shape, bound to the SUBJECT tenant (the ADR 0104 projection: a tenant reads the requests about itself,
-- files its own, and cancels / expires them; it never decides one).
CREATE POLICY tenant_subject_select ON licensing_change_requests FOR SELECT
    USING (subject_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_subject_insert ON licensing_change_requests FOR INSERT
    WITH CHECK (subject_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
                AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
                AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
                AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_subject_update ON licensing_change_requests FOR UPDATE
    USING (subject_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present())
    WITH CHECK (subject_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
                AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
                AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
                AND NOT financial_acting_gucs_present());

CREATE POLICY platform_scope_select ON licensing_change_approvals FOR SELECT
    USING (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY platform_scope_insert ON licensing_change_approvals FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
                AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_subject_select ON licensing_change_approvals FOR SELECT
    USING (EXISTS (SELECT 1 FROM licensing_change_requests r
                    WHERE r.id = licensing_change_approvals.request_id
                      AND r.subject_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());

-- ADR 0099 acting fence on the three tables.
CREATE POLICY acting_fence_select ON licensing_change_requests AS RESTRICTIVE FOR SELECT USING (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_insert ON licensing_change_requests AS RESTRICTIVE FOR INSERT WITH CHECK (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_update ON licensing_change_requests AS RESTRICTIVE FOR UPDATE USING (NOT financial_acting_gucs_present()) WITH CHECK (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_delete ON licensing_change_requests AS RESTRICTIVE FOR DELETE USING (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_select ON licensing_change_approvals AS RESTRICTIVE FOR SELECT USING (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_insert ON licensing_change_approvals AS RESTRICTIVE FOR INSERT WITH CHECK (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_update ON licensing_change_approvals AS RESTRICTIVE FOR UPDATE USING (NOT financial_acting_gucs_present()) WITH CHECK (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_delete ON licensing_change_approvals AS RESTRICTIVE FOR DELETE USING (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_select ON licensing_change_kinds AS RESTRICTIVE FOR SELECT USING (NOT financial_acting_gucs_present());

-- =========================================================================
-- 9. Grants to igaming_runtime (mirrored in deploy/init-app-role.sql)
-- =========================================================================
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON licensing_change_kinds FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON licensing_change_kinds TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON licensing_change_requests FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON licensing_change_requests TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (status, decided_by, decided_by_person_id, decided_at, decision_reason_code, executed_by, executed_at, executed_txid) ON licensing_change_requests TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON licensing_change_approvals FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON licensing_change_approvals TO igaming_runtime';
    END IF;
END
$$;
