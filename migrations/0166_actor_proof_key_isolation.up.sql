-- F1 of the platform-freeze work list (SRBX-3 / residual R1 of docs/plans/backup-exclude-actor-secret-record.md; owner decision 5,
-- 2026-10-16: actor-proof private material must not be exposed through backups; ADR 0110 section 6, ADR 0122 section 10).
--
-- PROBLEM. The actor-proof HMAC secrets (ADR 0110) lived in public.actor_proof_keys. igaming_backup holds table-level SELECT on every
-- public table (pg_dump takes LOCK TABLE ... ACCESS SHARE on every table it dumps, which needs a table privilege, so a column grant broke the
-- dump), so a stolen backup CREDENTIAL could read the signing key. With a stolen runtime credential as well, that defeats the database-enforced
-- four-eyes rule (forged proofs for any actor; ADR 0110 T1/T8 context).
--
-- MECHANISM (option a of the SRBX-3 review). The key table moves, rows and all, into a DEDICATED schema `actorproof_private`:
--   * owned by the migration/owner role; NO privilege for PUBLIC, igaming_runtime or igaming_backup on the schema (no USAGE: nothing in it can
--     even be named) and no privilege on the table for anyone but the owner (the SELECT that the backup role's blanket grant gave it is REVOKED);
--   * pg_dump runs with --exclude-schema=actorproof_private (deploy/backup/backup.sh), so it neither locks nor reads the table and the backup role
--     needs no privilege on it; a restore recreates the EMPTY table from deploy/backup/actorproof-private-schema.sql (reviewed DDL, identical
--     catalog; asserted by verify.sh's FP|actorproof_private_md5 line and an integration test);
--   * the secret is reachable ONLY through the existing SECURITY DEFINER verifier actor_proof_require (its body is 0139's, with the one key
--     lookup pointed at the new table) and the existence probe actor_proof_key_active (0120's body, same change). search_path stays pinned to
--     pg_catalog, public, pg_temp (the private schema is NOT on it: the reference is schema-qualified), so no caller-controlled object can be
--     substituted;
--   * NO new function, view or grant: the backup role does not even see the key ids any more (the backup baseline stops listing them; verify.sh
--     asserts the key table's state as the OWNER role). A non-secret view or inventory function was considered and rejected: a view in public
--     would be re-granted to igaming_runtime by the init scripts' blanket "ALL TABLES IN SCHEMA public" grant and would make the logical
--     restore depend on the excluded table during pg_restore; an inventory function would be new reachable surface for no security gain.
-- Proof semantics are unchanged: same token format, same AP001..AP005 rules and order, same nonce table (public.actor_proof_nonces holds no
-- secret and stays where it is), same triggers. Nothing else reads the key table.
--
-- REFUSALS (APM01; nothing is changed): the key table is not owned by the migrating role; the schema already exists; the table carries a grant
-- to anyone other than its owner and igaming_backup's SELECT, or any column-level grant, or any dependent view/rule/trigger/policy or
-- SQL-standard (BEGIN ATOMIC) function body (an unexpected path to the secret: investigate first); igaming_backup's SELECT on the key table differs
-- from its SELECT on actor_proof_nonces (the blanket grant covers both alike; the down migration reconstructs the key grant from the nonce
-- grant); after the move, the new schema or the table carries any non-owner ACL entry (e.g. inherited from a global DEFAULT PRIVILEGES entry).
-- A database without the key table (0120 not applied) fails at the first LOCK TABLE (42P01), also changing nothing.
--
-- LOAD-BEARING (SRF1-3): the key table keeps RLS ENABLED with NO policy. A role reachable only through SET ROLE (e.g. pg_read_all_data granted
-- NOINHERIT) has no BYPASSRLS and therefore reads 0 rows. Never add a policy, never disable RLS on it; the backup baseline's FP line pins the flag.
--
-- NOT changed / residual: the owner (migration) role and a superuser can still read the key (ADR 0110 section 9.4, T7). Physical base
-- backups, snapshots, the WAL archive and PITR copies contain the table's data files and so the key (inside the cloud trust boundary;
-- docs/runbooks/backup-restore-dr.md).

LOCK TABLE public.actor_proof_keys IN ACCESS EXCLUSIVE MODE;

DO $$
DECLARE
    v_rel     oid := 'public.actor_proof_keys'::regclass;
    v_nonces  oid := 'public.actor_proof_nonces'::regclass;
    v_backup  oid := (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = 'igaming_backup');
    v_bad     text;
    v_n       bigint;
    v_bk_keys boolean;
    v_bk_non  boolean;
BEGIN
    IF pg_catalog.to_regnamespace('actorproof_private') IS NOT NULL THEN
        RAISE EXCEPTION 'migration 0166 refused: schema actorproof_private already exists (created outside this migration?); nothing was changed'
            USING ERRCODE = 'APM01';
    END IF;
    IF (SELECT c.relowner FROM pg_catalog.pg_class c WHERE c.oid = v_rel) <> (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user) THEN
        RAISE EXCEPTION 'migration 0166 refused: public.actor_proof_keys is not owned by the migrating role %', current_user
            USING ERRCODE = 'APM01';
    END IF;
    SELECT pg_catalog.string_agg(DISTINCT CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(a.grantee) END || ':' || a.privilege_type, ', ')
      INTO v_bad
      FROM pg_catalog.pg_class c, LATERAL pg_catalog.aclexplode(COALESCE(c.relacl, pg_catalog.acldefault('r', c.relowner))) a
     WHERE c.oid = v_rel AND a.grantee <> c.relowner
       AND NOT COALESCE(a.grantee = v_backup AND a.privilege_type = 'SELECT', false);   -- COALESCE: no backup role means no exception
    IF v_bad IS NOT NULL THEN
        RAISE EXCEPTION 'migration 0166 refused: public.actor_proof_keys carries unexpected grants (%); the signing key may have been exposed: investigate and revoke first',
            v_bad USING ERRCODE = 'APM01';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_attribute WHERE attrelid = v_rel AND attacl IS NOT NULL) THEN
        RAISE EXCEPTION 'migration 0166 refused: public.actor_proof_keys carries column-level grants; investigate and revoke first'
            USING ERRCODE = 'APM01';
    END IF;
    SELECT count(*) INTO v_n FROM pg_catalog.pg_depend d
     WHERE d.refclassid = 'pg_catalog.pg_class'::regclass AND d.refobjid = v_rel
       AND d.classid IN ('pg_catalog.pg_rewrite'::regclass, 'pg_catalog.pg_trigger'::regclass, 'pg_catalog.pg_policy'::regclass,
                         'pg_catalog.pg_proc'::regclass);   -- pg_proc: SQL-standard (BEGIN ATOMIC) bodies record the table dependency (SRF1-2)
    IF v_n > 0 THEN
        RAISE EXCEPTION 'migration 0166 refused: % view/rule/trigger/policy/function object(s) depend on public.actor_proof_keys (an unexpected path to the secret); investigate first', v_n
            USING ERRCODE = 'APM01';
    END IF;
    v_bk_keys := v_backup IS NOT NULL AND EXISTS (SELECT 1 FROM pg_catalog.pg_class c, LATERAL pg_catalog.aclexplode(c.relacl) a
                                                   WHERE c.oid = v_rel AND a.grantee = v_backup AND a.privilege_type = 'SELECT');
    v_bk_non  := v_backup IS NOT NULL AND EXISTS (SELECT 1 FROM pg_catalog.pg_class c, LATERAL pg_catalog.aclexplode(c.relacl) a
                                                   WHERE c.oid = v_nonces AND a.grantee = v_backup AND a.privilege_type = 'SELECT');
    IF v_bk_keys IS DISTINCT FROM v_bk_non THEN
        RAISE EXCEPTION 'migration 0166 refused: igaming_backup SELECT on actor_proof_keys (%) differs from its SELECT on actor_proof_nonces (%); re-run deploy/backup/init-backup-role.sql first',
            v_bk_keys, v_bk_non USING ERRCODE = 'APM01';
    END IF;
END;
$$;

CREATE SCHEMA actorproof_private;
REVOKE ALL ON SCHEMA actorproof_private FROM PUBLIC;

ALTER TABLE public.actor_proof_keys SET SCHEMA actorproof_private;

-- Only the owner keeps a privilege on the key table. The backup role's SELECT (blanket "ALL TABLES IN SCHEMA public" grant of
-- init-backup-role.sql) is removed; the runtime role and PUBLIC never had one (0120), which is re-asserted.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'igaming_backup') THEN
        REVOKE ALL ON actorproof_private.actor_proof_keys FROM igaming_backup;
        REVOKE ALL ON SCHEMA actorproof_private FROM igaming_backup;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'igaming_runtime') THEN
        REVOKE ALL ON actorproof_private.actor_proof_keys FROM igaming_runtime;
        REVOKE ALL ON SCHEMA actorproof_private FROM igaming_runtime;
    END IF;
END;
$$;
REVOKE ALL ON actorproof_private.actor_proof_keys FROM PUBLIC;

-- SRF1-1(d): assert the isolation instead of assuming it. CREATE SCHEMA may inherit a global DEFAULT PRIVILEGES entry of the owner for a grantee
-- other than the two roles revoked above; any non-owner ACL entry left on the schema, the table or a column refuses the migration.
DO $$
DECLARE
    v_bad text;
BEGIN
    SELECT pg_catalog.string_agg(x, ', ') INTO v_bad FROM (
        SELECT 'schema:' || CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(a.grantee) END || ':' || a.privilege_type AS x
          FROM pg_catalog.pg_namespace n, LATERAL pg_catalog.aclexplode(COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))) a
         WHERE n.nspname = 'actorproof_private' AND a.grantee <> n.nspowner
        UNION ALL
        SELECT 'table:' || CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(a.grantee) END || ':' || a.privilege_type
          FROM pg_catalog.pg_class c, LATERAL pg_catalog.aclexplode(COALESCE(c.relacl, pg_catalog.acldefault('r', c.relowner))) a
         WHERE c.oid = 'actorproof_private.actor_proof_keys'::regclass AND a.grantee <> c.relowner
        UNION ALL
        SELECT 'column:' || at.attname FROM pg_catalog.pg_attribute at
         WHERE at.attrelid = 'actorproof_private.actor_proof_keys'::regclass AND at.attacl IS NOT NULL) q;
    IF v_bad IS NOT NULL THEN
        RAISE EXCEPTION 'migration 0166 refused: the key schema is not owner-only after the move (%): a DEFAULT PRIVILEGES entry of the owner? investigate and remove it first',
            v_bad USING ERRCODE = 'APM01';
    END IF;
END;
$$;

-- -------------------------------------------------------------------------
-- The verifier: 0139's body verbatim except the key lookup, which reads actorproof_private.actor_proof_keys.
-- CREATE OR REPLACE keeps the owner, EXECUTE grants (igaming_runtime only), SECURITY DEFINER and the pinned search_path.
-- -------------------------------------------------------------------------
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

-- Startup/health probe (0120's body; only the table reference changes): is this kid an ACTIVE key? Discloses no key material.
CREATE OR REPLACE FUNCTION actor_proof_key_active(p_kid text) RETURNS boolean AS $$
    SELECT EXISTS (SELECT 1 FROM actorproof_private.actor_proof_keys k WHERE k.kid = p_kid AND k.status = 'active');
$$ LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp;
