-- 0181 (lic10): (A) residence determination maker-checker (audit G8, owner question Q21) and (B) automatic AML screening
-- triggers (audit G1). COMPLIANCE / SECURITY MIGRATION: FOR `security` (BLOCKING), `identity-compliance`, `qa`,
-- `code-reviewer` REVIEW BEFORE MERGE. No integration test of this file has been run by its author (the database harness was
-- refused in the authoring session): the owner runs internal/residenceapproval, internal/kyc and internal/aml integration tests
-- and `go test ./internal/db` before merge.
--
-- NUMBERING: 0181 (0180 belongs to the parallel `operating bases` work and is not touched here). It does NOT replace
-- licensing_change_classify / licensing_change_row_matches and adds no licensing change kind, so it cannot collide with 0180.
--
-- NO LEGAL VALUE, NO SEED, NO DEFAULT THRESHOLD. Every table below starts EMPTY. An absent residence policy means mode
-- `none`, which is TODAY'S behaviour (a single reviewer's determination is evidence at once); nothing changes for an existing
-- tenant until an authorised administrator configures a stricter mode.
--
-- (A) Residence determination maker-checker
--   residence_approval_policies          per-tenant policy VERSIONS: mode none | into_open_market | all. A change to a STRICTER
--                                        mode is filed and active at once (one actor); a change to a LESS strict mode is filed
--                                        PENDING and becomes active only after two approvals by two distinct Persons, neither
--                                        the requester's (residence_approval_policy_approvals). The rule "stricter = 0
--                                        approvals" is a table CHECK, not application logic.
--   residence_determination_checks       one row per reviewer determination (kyc_verifications.verified_residence_set_at),
--                                        written by an AFTER trigger on kyc_verifications in the SAME statement: it SNAPSHOTS the
--                                        mode in force and whether this determination needs a second Person. A determination
--                                        that needs approval and has none is NOT market evidence (kyc.ReadMarketResidenceEvidence
--                                        excludes it). The approval is bound to the exact determination (verification id and
--                                        determined_at) and needs a different staff member AND a different Person.
--   All writes go through OWNER-owned SECURITY DEFINER functions that re-resolve the actor from the session
--   (financial_actor_session) and verify a signed actor proof (actor_proof_require, ADR 0110). The runtime role has no write
--   privilege on any of these tables. Tenant sessions can read only their own determination checks.
--   into_open_market relies on the application's open-market evaluation, passed as the transaction-local setting
--   app.residence_into_open_market ('true' | 'false'); with the mode in force and the setting MISSING the determination is
--   REFUSED (fail closed). The database cannot evaluate the operating-market ladder itself. CORRECTION (lic18, security S3): mode
--   `all` does not read that setting, but it is NOT free of application dependence. It still relies on (1) the application recording
--   a determination only through the reviewer path (a tenant session cannot prove that verified_residence_set_by is the reviewer; the
--   guard below only forces a change of the residence columns to be a NEW, later determination with its own check row), and (2) every
--   consumer of residence for a market decision reading ONLY kyc.ReadMarketResidenceEvidence (kyc.GetVerifiedResidence is
--   unfiltered by approval; it has no production caller today and must not gain one for a market decision).
--
-- (B) AML screening triggers: see the section further down.
--
-- SQLSTATE class RD (residence determination): RD001 session / actor, RD002 invalid input or tenant mismatch, RD003 not pending
-- / stale / no change, RD004 four-eyes refused, RD005 append-only, RD006 no linked Person, RD010 fail-closed refusal, RD040 not
-- found, RD098 pre-flight, RD099 down refused. SQLSTATE class AT (AML trigger): see section B.

SET LOCAL lock_timeout = '5s';

-- =========================================================================
-- 0. Pre-flight
-- =========================================================================
DO $$
BEGIN
    IF to_regclass('public.kyc_verifications') IS NULL OR to_regprocedure('public.financial_actor_session()') IS NULL
       OR to_regprocedure('public.actor_proof_require(uuid, text, uuid, text, text, text)') IS NULL
       OR to_regclass('public.aml_screening_results') IS NULL OR to_regclass('public.operating_bases') IS NULL
       OR to_regprocedure('public.rg_database_environment()') IS NULL THEN
        RAISE EXCEPTION 'migration 0181 pre-flight: migrations 0040, 0112, 0120, 0135 and 0154 are not applied' USING ERRCODE = 'RD098';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_class WHERE relnamespace = 'public'::regnamespace
                AND relname IN ('residence_approval_policies', 'residence_approval_policy_approvals', 'residence_determination_checks',
                                'aml_screening_trigger_policies', 'aml_screening_requests', 'screening_worker_heartbeats')) THEN
        RAISE EXCEPTION 'migration 0181 pre-flight: a lic10 table already exists' USING ERRCODE = 'RD098';
    END IF;
END
$$;

-- =========================================================================
-- A1. Helpers
-- =========================================================================
CREATE FUNCTION residence_mode_rank(p text) RETURNS integer
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog
AS $$ SELECT CASE p WHEN 'none' THEN 0 WHEN 'into_open_market' THEN 1 WHEN 'all' THEN 2 END $$;

CREATE FUNCTION residence_mc_deny_mutation() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION 'residence maker-checker: % on % is not permitted (append-only compliance history)', TG_OP, TG_TABLE_NAME USING ERRCODE = 'RD005';
END
$$;

-- =========================================================================
-- A2. Tables
-- =========================================================================
CREATE TABLE residence_approval_policies (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    mode                    TEXT NOT NULL CHECK (mode IN ('none', 'into_open_market', 'all')),
    previous_mode           TEXT NOT NULL CHECK (previous_mode IN ('none', 'into_open_market', 'all')),
    status                  TEXT NOT NULL CHECK (status IN ('pending', 'active')),
    required_approvals      SMALLINT NOT NULL CHECK (required_approvals IN (0, 2)),
    reason_code             TEXT NOT NULL CHECK (reason_code ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$'),
    payload_hash            TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    requested_by            UUID NOT NULL,
    requested_by_person_id  UUID NOT NULL,
    requested_by_scope      TEXT NOT NULL CHECK (requested_by_scope = 'tenant'),
    requested_at            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    activated_at            TIMESTAMPTZ NULL,
    UNIQUE (tenant_id, id),
    CHECK (mode <> previous_mode),
    -- The governance rule, in the database: a change to a STRICTER mode needs no approval and is active at once; a change to a
    -- LESS strict mode needs two approvals and starts pending. Application code cannot file it any other way.
    CHECK ((public.residence_mode_rank(mode) > public.residence_mode_rank(previous_mode)) = (required_approvals = 0)),
    CHECK ((status = 'active') = (activated_at IS NOT NULL)),
    CHECK (required_approvals > 0 OR status = 'active')
);
CREATE INDEX residence_approval_policies_effective ON residence_approval_policies (tenant_id, activated_at DESC, id DESC) WHERE status = 'active';

CREATE TABLE residence_approval_policy_approvals (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL,
    policy_id           UUID NOT NULL,
    approver            UUID NOT NULL,
    approver_person_id  UUID NOT NULL,
    approver_scope      TEXT NOT NULL CHECK (approver_scope = 'tenant'),
    reason_code         TEXT NOT NULL CHECK (reason_code ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$'),
    approved_at         TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (policy_id, approver_person_id),
    FOREIGN KEY (tenant_id, policy_id) REFERENCES residence_approval_policies (tenant_id, id) ON DELETE RESTRICT
);

CREATE TABLE residence_determination_checks (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL,
    verification_id           UUID NOT NULL,
    determined_at             TIMESTAMPTZ NOT NULL,
    determined_by             UUID NOT NULL,
    determined_by_person_id   UUID NULL,
    -- S3: the country the check was made for. The evidence read requires it to equal the verification's CURRENT country, so a
    -- determination changed under an existing check can never inherit its approval.
    country                   TEXT NOT NULL CHECK (country ~ '^[A-Z]{2}$'),
    -- Snapshot of the policy mode in force when the determination was made. A later change of policy does not retro-actively
    -- change an existing determination (tightening applies to FUTURE determinations; see the lic10 record).
    policy_mode               TEXT NOT NULL CHECK (policy_mode IN ('none', 'into_open_market', 'all')),
    approval_required         BOOLEAN NOT NULL,
    approved_by               UUID NULL,
    approved_by_person_id     UUID NULL,
    approved_at               TIMESTAMPTZ NULL,
    approval_reason_code      TEXT NULL CHECK (approval_reason_code ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$'),
    created_at                TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (verification_id, determined_at),
    FOREIGN KEY (verification_id, tenant_id) REFERENCES kyc_verifications (id, tenant_id) ON DELETE RESTRICT,
    CHECK (policy_mode <> 'none' OR NOT approval_required),
    CHECK (policy_mode <> 'all' OR approval_required),
    CHECK ((approved_by IS NULL) = (approved_at IS NULL) AND (approved_by IS NULL) = (approved_by_person_id IS NULL)
           AND (approved_by IS NULL) = (approval_reason_code IS NULL)),
    CHECK (approved_by IS NULL OR approval_required)
);
CREATE INDEX residence_determination_checks_pending ON residence_determination_checks (tenant_id, determined_at DESC)
    WHERE approval_required AND approved_at IS NULL;

-- RLS: enabled; the owner (definer functions) bypasses it, every other role is denied. Tenant sessions additionally
-- READ their own determination checks (the evidence read and the pending list). No other policy, no write privilege.
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['residence_approval_policies', 'residence_approval_policy_approvals', 'residence_determination_checks'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        -- ENABLE (not FORCE) and NO policy for the owner: the table owner (the role of the SECURITY DEFINER functions) is not
        -- subject to RLS, every other role is denied by default and in addition holds no privilege. A role-specific permissive
        -- owner policy would be satisfiable by the KYC worker identity's catalogue gate (internal/db
        -- kyc_worker_catalogue_gate_integration_test.go) and is therefore not used; the same owner-access idiom as the licensing
        -- tables. The tenant read policy below is unsatisfiable without app.tenant_id.
        EXECUTE format('REVOKE ALL ON %I FROM PUBLIC', t);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
            EXECUTE format('REVOKE ALL ON %I FROM igaming_runtime', t);
        END IF;
        EXECUTE format('CREATE TRIGGER %I BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION residence_mc_deny_mutation()', t || '_no_truncate', t);
        EXECUTE format('CREATE TRIGGER %I BEFORE DELETE ON %I FOR EACH ROW EXECUTE FUNCTION residence_mc_deny_mutation()', t || '_no_delete', t);
    END LOOP;
    -- The policy and the approvals tables are never UPDATEd except by the guarded pending -> active transition (policies) and the
    -- guarded approval columns (checks); the approvals table is purely append-only.
    CREATE TRIGGER residence_approval_policy_approvals_no_update BEFORE UPDATE ON residence_approval_policy_approvals
        FOR EACH ROW EXECUTE FUNCTION residence_mc_deny_mutation();
END
$$;
CREATE POLICY residence_checks_tenant_read ON residence_determination_checks FOR SELECT
    USING (tenant_id = NULLIF(pg_catalog.current_setting('app.tenant_id', true), '')::uuid);
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        GRANT SELECT ON residence_determination_checks TO igaming_runtime;
    END IF;
END
$$;

-- =========================================================================
-- A3. Guards (the second, independent check behind the definer functions)
-- =========================================================================
CREATE FUNCTION residence_approval_policies_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor record;
    v_n     integer;
    v_p     integer;
BEGIN
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF TG_OP = 'INSERT' THEN
        IF NEW.requested_by IS DISTINCT FROM v_actor.actor OR NEW.requested_by_scope IS DISTINCT FROM v_actor.scope
           OR NEW.requested_by_person_id IS DISTINCT FROM v_actor.person_id OR NEW.tenant_id IS DISTINCT FROM v_actor.tenant THEN
            RAISE EXCEPTION 'residence policy: the requester columns are not the proven session actor' USING ERRCODE = 'RD001';
        END IF;
        IF NEW.required_approvals = 0 AND NEW.status <> 'active' OR NEW.required_approvals > 0 AND NEW.status <> 'pending' THEN
            RAISE EXCEPTION 'residence policy: a stricter change is active at once, a less strict one is filed pending' USING ERRCODE = 'RD003';
        END IF;
        RETURN NEW;
    END IF;
    -- UPDATE: only pending -> active, with the approvals actually recorded (count of DISTINCT Persons, none the requester).
    IF ROW(NEW.id, NEW.tenant_id, NEW.mode, NEW.previous_mode, NEW.required_approvals, NEW.reason_code, NEW.payload_hash,
           NEW.requested_by, NEW.requested_by_person_id, NEW.requested_by_scope, NEW.requested_at)
       IS DISTINCT FROM ROW(OLD.id, OLD.tenant_id, OLD.mode, OLD.previous_mode, OLD.required_approvals, OLD.reason_code, OLD.payload_hash,
                            OLD.requested_by, OLD.requested_by_person_id, OLD.requested_by_scope, OLD.requested_at)
       OR OLD.status <> 'pending' OR NEW.status <> 'active' THEN
        RAISE EXCEPTION 'residence policy: only a pending policy may become active' USING ERRCODE = 'RD005';
    END IF;
    SELECT count(DISTINCT a.approver_person_id), count(*) FILTER (WHERE a.approver_person_id = OLD.requested_by_person_id OR a.approver = OLD.requested_by)
      INTO v_n, v_p
      FROM public.residence_approval_policy_approvals a WHERE a.policy_id = OLD.id AND a.tenant_id = OLD.tenant_id;
    IF v_n < OLD.required_approvals OR v_p > 0 THEN
        RAISE EXCEPTION 'residence policy: the required approvals by distinct Persons (not the requester) are not recorded' USING ERRCODE = 'RD004';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER residence_approval_policies_10_guard BEFORE INSERT OR UPDATE ON residence_approval_policies
    FOR EACH ROW EXECUTE FUNCTION residence_approval_policies_guard();

CREATE FUNCTION residence_approval_policy_approvals_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor record;
    v_pol   record;
BEGIN
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.approver IS DISTINCT FROM v_actor.actor OR NEW.approver_scope IS DISTINCT FROM v_actor.scope
       OR NEW.approver_person_id IS DISTINCT FROM v_actor.person_id OR NEW.tenant_id IS DISTINCT FROM v_actor.tenant THEN
        RAISE EXCEPTION 'residence policy approval: the approver columns are not the proven session actor' USING ERRCODE = 'RD001';
    END IF;
    SELECT * INTO v_pol FROM public.residence_approval_policies p WHERE p.id = NEW.policy_id AND p.tenant_id = NEW.tenant_id;
    IF NOT FOUND OR v_pol.status <> 'pending' THEN
        RAISE EXCEPTION 'residence policy approval: the policy is not pending' USING ERRCODE = 'RD003';
    END IF;
    IF NEW.approver = v_pol.requested_by OR NEW.approver_person_id = v_pol.requested_by_person_id THEN
        RAISE EXCEPTION 'residence policy approval: the requester (staff or Person) may not approve their own request' USING ERRCODE = 'RD004';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER residence_approval_policy_approvals_10_guard BEFORE INSERT ON residence_approval_policy_approvals
    FOR EACH ROW EXECUTE FUNCTION residence_approval_policy_approvals_guard();

CREATE FUNCTION residence_determination_checks_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor record;
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- Inserted only by the kyc_verifications trigger (definer); the determination must be the verification's current one.
        IF NOT EXISTS (SELECT 1 FROM public.kyc_verifications v WHERE v.id = NEW.verification_id AND v.tenant_id = NEW.tenant_id
                        AND v.verified_residence_set_at = NEW.determined_at AND v.verified_residence_set_by = NEW.determined_by
                        AND v.verified_residence_country = NEW.country) THEN
            RAISE EXCEPTION 'residence determination: the check does not describe the verification''s current determination' USING ERRCODE = 'RD003';
        END IF;
        IF NEW.approved_by IS NOT NULL THEN
            RAISE EXCEPTION 'residence determination: a check is created unapproved' USING ERRCODE = 'RD003';
        END IF;
        RETURN NEW;
    END IF;
    -- UPDATE: exactly the approval columns, NULL -> value, once, by the proven session actor, a different staff member AND a
    -- different, linked Person than the determiner.
    IF ROW(NEW.id, NEW.tenant_id, NEW.verification_id, NEW.determined_at, NEW.determined_by, NEW.determined_by_person_id, NEW.country,
           NEW.policy_mode, NEW.approval_required, NEW.created_at)
       IS DISTINCT FROM ROW(OLD.id, OLD.tenant_id, OLD.verification_id, OLD.determined_at, OLD.determined_by, OLD.determined_by_person_id, OLD.country,
                            OLD.policy_mode, OLD.approval_required, OLD.created_at)
       OR OLD.approved_by IS NOT NULL OR NEW.approved_by IS NULL OR NOT OLD.approval_required THEN
        RAISE EXCEPTION 'residence determination: only an unapproved, approval-requiring check may be approved, once' USING ERRCODE = 'RD005';
    END IF;
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.approved_by IS DISTINCT FROM v_actor.actor OR NEW.approved_by_person_id IS DISTINCT FROM v_actor.person_id
       OR v_actor.scope IS DISTINCT FROM 'tenant' OR v_actor.tenant IS DISTINCT FROM NEW.tenant_id THEN
        RAISE EXCEPTION 'residence determination: the approver columns are not the proven tenant session actor' USING ERRCODE = 'RD001';
    END IF;
    IF NEW.approved_by_person_id IS NULL OR OLD.determined_by_person_id IS NULL THEN
        RAISE EXCEPTION 'residence determination: both the determiner and the approver must be linked to a Person' USING ERRCODE = 'RD006';
    END IF;
    IF NEW.approved_by = OLD.determined_by OR NEW.approved_by_person_id = OLD.determined_by_person_id THEN
        RAISE EXCEPTION 'residence determination: the determiner (staff or Person) may not approve their own determination' USING ERRCODE = 'RD004';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER residence_determination_checks_10_guard BEFORE INSERT OR UPDATE ON residence_determination_checks
    FOR EACH ROW EXECUTE FUNCTION residence_determination_checks_guard();

-- =========================================================================
-- A4. The effective mode, the actor check and the definer API
-- =========================================================================

-- The mode in force for a tenant: the latest ACTIVE version, or `none` (today's behaviour) when no version was ever active.
CREATE FUNCTION residence_policy_mode(p_tenant uuid) RETURNS text
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_mode text;
BEGIN
    SELECT p.mode INTO v_mode FROM public.residence_approval_policies p
     WHERE p.tenant_id = p_tenant AND p.status = 'active' ORDER BY p.activated_at DESC, p.id DESC LIMIT 1;
    RETURN COALESCE(v_mode, 'none');
END
$$;

-- The actor check shared by the policy functions: a TENANT staff session (role compliance) of exactly p_tenant, with a linked
-- Person and a valid signed proof for exactly this operation, target and payload. (A platform_acting path is NOT built: it would
-- widen the acting-session allow-list of ADR 0099 / A-16; platform staff run a platform-licensed tenant through that tenant's own
-- compliance accounts.)
CREATE FUNCTION residence_authorize_policy_actor(p_tenant uuid, p_operation text, p_target text, p_hash text,
    OUT o_actor uuid, OUT o_scope text, OUT o_person_id uuid)
    RETURNS record
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v record;
BEGIN
    IF p_tenant IS NULL OR p_operation IS NULL OR p_target IS NULL OR p_hash IS NULL THEN
        RAISE EXCEPTION 'residence policy: authorisation arguments are required' USING ERRCODE = 'RD002';
    END IF;
    SELECT * INTO v FROM public.financial_actor_session();
    IF v.scope IS DISTINCT FROM 'tenant' THEN
        RAISE EXCEPTION 'residence policy: this session shape may not use residence policy functions' USING ERRCODE = 'RD001';
    END IF;
    IF v.tenant IS DISTINCT FROM p_tenant THEN
        RAISE EXCEPTION 'residence policy: the session tenant is not the requested tenant' USING ERRCODE = 'RD002';
    END IF;
    IF v.role IS DISTINCT FROM 'compliance' THEN
        RAISE EXCEPTION 'residence policy: this role may not change the residence approval policy' USING ERRCODE = 'RD001';
    END IF;
    IF v.person_id IS NULL THEN
        RAISE EXCEPTION 'residence policy: the actor has no linked Person' USING ERRCODE = 'RD006';
    END IF;
    PERFORM public.actor_proof_require(v.actor, v.scope, v.tenant, p_operation, p_target, p_hash);
    o_actor := v.actor; o_scope := v.scope; o_person_id := v.person_id;
END
$$;

-- Files a policy change. Stricter than the mode in force: active at once. Less strict: pending, two approvals needed.
CREATE FUNCTION residence_policy_set(p_tenant uuid, p_mode text, p_reason_code text)
RETURNS TABLE (o_policy_id uuid, o_status text, o_required_approvals integer, o_previous_mode text)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a      record;
    v_prev text;
    v_req  integer;
    v_id   uuid;
    v_hash text;
BEGIN
    IF p_mode IS NULL OR p_mode NOT IN ('none', 'into_open_market', 'all') OR p_reason_code IS NULL
       OR p_reason_code !~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$' THEN
        RAISE EXCEPTION 'residence policy: invalid mode or reason code' USING ERRCODE = 'RD002';
    END IF;
    v_hash := public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_mode, p_reason_code));
    SELECT * INTO a FROM public.residence_authorize_policy_actor(p_tenant, 'residence_policy:set', 'new', v_hash);
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('residence_policy:' || p_tenant::text, 0));
    v_prev := public.residence_policy_mode(p_tenant);
    IF p_mode = v_prev THEN
        RAISE EXCEPTION 'residence policy: the requested mode is already in force' USING ERRCODE = 'RD003';
    END IF;
    v_req := CASE WHEN public.residence_mode_rank(p_mode) > public.residence_mode_rank(v_prev) THEN 0 ELSE 2 END;
    INSERT INTO public.residence_approval_policies (tenant_id, mode, previous_mode, status, required_approvals, reason_code, payload_hash,
                                                    requested_by, requested_by_person_id, requested_by_scope, activated_at)
    VALUES (p_tenant, p_mode, v_prev, CASE WHEN v_req = 0 THEN 'active' ELSE 'pending' END, v_req, p_reason_code, v_hash,
            a.o_actor, a.o_person_id, a.o_scope, CASE WHEN v_req = 0 THEN pg_catalog.clock_timestamp() END)
    RETURNING id INTO v_id;
    RETURN QUERY SELECT v_id, CASE WHEN v_req = 0 THEN 'active' ELSE 'pending' END, v_req, v_prev;
END
$$;

-- Records one approval of a PENDING (less strict) policy; the approval that reaches the required count activates it, in the
-- same transaction.
CREATE FUNCTION residence_policy_approve(p_tenant uuid, p_policy uuid, p_reason_code text)
RETURNS TABLE (o_status text, o_approvals integer, o_required_approvals integer)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a      record;
    v_pol  record;
    v_n    integer;
    v_hash text;
BEGIN
    IF p_policy IS NULL OR p_reason_code IS NULL OR p_reason_code !~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$' THEN
        RAISE EXCEPTION 'residence policy: invalid approval arguments' USING ERRCODE = 'RD002';
    END IF;
    v_hash := public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_policy::text, p_reason_code));
    SELECT * INTO a FROM public.residence_authorize_policy_actor(p_tenant, 'residence_policy:approve', p_policy::text, v_hash);
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('residence_policy:' || p_tenant::text, 0));
    SELECT * INTO v_pol FROM public.residence_approval_policies p WHERE p.id = p_policy AND p.tenant_id = p_tenant FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'residence policy: not found' USING ERRCODE = 'RD040';
    END IF;
    IF v_pol.status <> 'pending' THEN
        RAISE EXCEPTION 'residence policy: the request is not pending' USING ERRCODE = 'RD003';
    END IF;
    -- A pending loosening expires like the other governed requests (72 hours): it must be refiled.
    IF v_pol.requested_at < pg_catalog.clock_timestamp() - interval '72 hours' THEN
        RAISE EXCEPTION 'residence policy: the request expired (72 hours); refile it' USING ERRCODE = 'RD003';
    END IF;
    -- A request is only valid against the mode it was filed against: if the mode in force changed since, it must be refiled.
    IF v_pol.previous_mode IS DISTINCT FROM public.residence_policy_mode(p_tenant) THEN
        RAISE EXCEPTION 'residence policy: the mode in force changed since this request was filed; refile it' USING ERRCODE = 'RD003';
    END IF;
    IF a.o_actor = v_pol.requested_by OR a.o_person_id = v_pol.requested_by_person_id THEN
        RAISE EXCEPTION 'residence policy: the requester may not approve their own request' USING ERRCODE = 'RD004';
    END IF;
    IF EXISTS (SELECT 1 FROM public.residence_approval_policy_approvals x WHERE x.policy_id = p_policy AND x.approver_person_id = a.o_person_id) THEN
        RAISE EXCEPTION 'residence policy: this Person has already approved this request' USING ERRCODE = 'RD004';
    END IF;
    INSERT INTO public.residence_approval_policy_approvals (tenant_id, policy_id, approver, approver_person_id, approver_scope, reason_code)
    VALUES (p_tenant, p_policy, a.o_actor, a.o_person_id, a.o_scope, p_reason_code);
    SELECT count(DISTINCT x.approver_person_id) INTO v_n FROM public.residence_approval_policy_approvals x WHERE x.policy_id = p_policy;
    IF v_n >= v_pol.required_approvals THEN
        UPDATE public.residence_approval_policies SET status = 'active', activated_at = pg_catalog.clock_timestamp() WHERE id = p_policy;
        RETURN QUERY SELECT 'active'::text, v_n, v_pol.required_approvals::integer;
    ELSE
        RETURN QUERY SELECT 'pending'::text, v_n, v_pol.required_approvals::integer;
    END IF;
END
$$;

-- Reads the policy versions of a tenant (newest first) with the number of approvals recorded. No proof (a read); a tenant
-- session bound to exactly p_tenant. Discloses no determination and no player.
CREATE FUNCTION residence_policy_list(p_tenant uuid)
RETURNS TABLE (o_policy_id uuid, o_mode text, o_previous_mode text, o_status text, o_required_approvals integer, o_approvals integer,
               o_reason_code text, o_requested_by uuid, o_requested_by_scope text, o_requested_at timestamptz, o_activated_at timestamptz,
               o_effective_mode text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v record;
BEGIN
    SELECT * INTO v FROM public.financial_actor_session();
    IF v.scope IS DISTINCT FROM 'tenant' OR v.tenant IS DISTINCT FROM p_tenant THEN
        RAISE EXCEPTION 'residence policy: the session may not read this tenant''s policy' USING ERRCODE = 'RD002';
    END IF;
    RETURN QUERY
        SELECT p.id, p.mode, p.previous_mode, p.status, p.required_approvals::integer,
               (SELECT count(*)::integer FROM public.residence_approval_policy_approvals x WHERE x.policy_id = p.id),
               p.reason_code, p.requested_by, p.requested_by_scope, p.requested_at, p.activated_at, public.residence_policy_mode(p_tenant)
          FROM public.residence_approval_policies p WHERE p.tenant_id = p_tenant
         ORDER BY p.requested_at DESC, p.id DESC LIMIT 200;
END
$$;

-- Approves ONE reviewer determination (bound to the verification AND the exact determined_at the approver saw). Tenant scope
-- only (the verification rows are tenant-RLS data). The second Person must differ from the determiner in staff AND Person.
CREATE FUNCTION residence_determination_approve(p_tenant uuid, p_verification uuid, p_determined_at timestamptz, p_reason_code text)
RETURNS TABLE (o_check_id uuid)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v         record;
    v_chk     record;
    v_hash    text;
    v_subject uuid;
BEGIN
    IF p_verification IS NULL OR p_determined_at IS NULL OR p_reason_code IS NULL OR p_reason_code !~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$' THEN
        RAISE EXCEPTION 'residence determination: invalid approval arguments' USING ERRCODE = 'RD002';
    END IF;
    v_hash := public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_verification::text,
        to_char(p_determined_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), p_reason_code));
    SELECT * INTO v FROM public.financial_actor_session();
    IF v.scope IS DISTINCT FROM 'tenant' OR v.tenant IS DISTINCT FROM p_tenant THEN
        RAISE EXCEPTION 'residence determination: only a staff session of the verification''s own tenant may approve' USING ERRCODE = 'RD001';
    END IF;
    IF v.role IS DISTINCT FROM 'compliance' THEN
        RAISE EXCEPTION 'residence determination: only a compliance officer may approve a determination' USING ERRCODE = 'RD001';
    END IF;
    IF v.person_id IS NULL THEN
        RAISE EXCEPTION 'residence determination: the approver has no linked Person' USING ERRCODE = 'RD006';
    END IF;
    PERFORM public.actor_proof_require(v.actor, v.scope, v.tenant, 'residence_determination:approve', p_verification::text, v_hash);
    -- The verification row is the authority on which determination is current (a later determination replaces it).
    SELECT k.person_id INTO v_subject FROM public.kyc_verifications k
     WHERE k.id = p_verification AND k.tenant_id = p_tenant AND k.verified_residence_set_at = p_determined_at FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'residence determination: not found or no longer current' USING ERRCODE = 'RD040';
    END IF;
    -- L4: neither the approver nor the determiner may be the person the verification is ABOUT.
    IF v.person_id = v_subject THEN
        RAISE EXCEPTION 'residence determination: an approver may not approve a determination about their own Person' USING ERRCODE = 'RD004';
    END IF;
    SELECT * INTO v_chk FROM public.residence_determination_checks c
     WHERE c.verification_id = p_verification AND c.tenant_id = p_tenant AND c.determined_at = p_determined_at FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'residence determination: no approval record exists for this determination' USING ERRCODE = 'RD040';
    END IF;
    IF NOT v_chk.approval_required OR v_chk.approved_at IS NOT NULL THEN
        RAISE EXCEPTION 'residence determination: nothing to approve' USING ERRCODE = 'RD003';
    END IF;
    IF v_chk.determined_by_person_id IS NOT DISTINCT FROM v_subject THEN
        RAISE EXCEPTION 'residence determination: a determination about the determiner''s own Person cannot be approved' USING ERRCODE = 'RD004';
    END IF;
    UPDATE public.residence_determination_checks
       SET approved_by = v.actor, approved_by_person_id = v.person_id, approved_at = pg_catalog.clock_timestamp(), approval_reason_code = p_reason_code
     WHERE id = v_chk.id;
    RETURN QUERY SELECT v_chk.id;
END
$$;

-- =========================================================================
-- A5. The determination trigger on kyc_verifications
-- =========================================================================
-- Fires when a verification row GAINS or CHANGES a reviewer determination (verified_residence_set_at changes to a non-null
-- value). It SNAPSHOTS the policy mode in force and decides whether the determination needs a second Person:
--   none              -> not required (today's behaviour);
--   all               -> required;
--   into_open_market  -> required iff the application declared (app.residence_into_open_market = 'true') that the country is
--                        served by the brand; the setting MISSING or malformed REFUSES the determination (fail closed).
-- The determiner's Person is read from staff_users; if it cannot be read it is recorded NULL and the determination can then
-- never be approved (it stays pending: fail closed).
CREATE FUNCTION kyc_verifications_residence_check() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_mode   text;
    v_flag   text;
    v_req    boolean;
    v_person uuid;
BEGIN
    IF NEW.verified_residence_set_at IS NULL OR NEW.verified_residence_set_by IS NULL OR NEW.verified_residence_country IS NULL THEN
        RETURN NEW;
    END IF;
    -- lic23 SN-6: a determination is stamped by the database clock (ReviewVerification uses now()); a future-dated time would block
    -- every later determination (RD005, the next one must be later). A small skew is tolerated.
    IF NEW.verified_residence_set_at > pg_catalog.clock_timestamp() + interval '2 minutes' THEN
        RAISE EXCEPTION 'residence determination: the determination time may not be in the future' USING ERRCODE = 'RD005';
    END IF;
    v_mode := public.residence_policy_mode(NEW.tenant_id);
    v_flag := NULLIF(pg_catalog.current_setting('app.residence_into_open_market', true), '');
    PERFORM pg_catalog.set_config('app.residence_into_open_market', '', true);
    IF v_mode = 'all' THEN
        v_req := true;
    ELSIF v_mode = 'into_open_market' THEN
        IF v_flag IS NULL OR v_flag NOT IN ('true', 'false') THEN
            RAISE EXCEPTION 'residence determination: the open-market evaluation is missing while the policy mode is into_open_market (fail closed)' USING ERRCODE = 'RD010';
        END IF;
        v_req := v_flag = 'true';
    ELSE
        v_req := false;
    END IF;
    SELECT su.person_id INTO v_person FROM public.staff_users su WHERE su.id = NEW.verified_residence_set_by AND su.tenant_id = NEW.tenant_id;
    INSERT INTO public.residence_determination_checks (tenant_id, verification_id, determined_at, determined_by, determined_by_person_id, country, policy_mode, approval_required)
    VALUES (NEW.tenant_id, NEW.id, NEW.verified_residence_set_at, NEW.verified_residence_set_by, v_person, NEW.verified_residence_country, v_mode, v_req);
    RETURN NEW;
END
$$;
-- S3: the residence columns of a verification change ONLY as a new determination (verified_residence_set_at advances together with
-- them). Without this a writer could change the country (or the determiner) under an unchanged set_at and keep the old check's
-- approval. NOTE (recorded limit): the reviewer's session is a plain tenant session, so the database cannot prove that
-- verified_residence_set_by is the reviewer; the determiner Person on the check is read from staff_users.
CREATE FUNCTION kyc_verifications_residence_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF NEW.verified_residence_set_at IS NOT DISTINCT FROM OLD.verified_residence_set_at THEN
        RAISE EXCEPTION 'residence determination: the residence columns change only as a new determination (verified_residence_set_at must advance with them)' USING ERRCODE = 'RD005';
    END IF;
    IF NEW.verified_residence_set_at > pg_catalog.clock_timestamp() + interval '2 minutes' THEN
        RAISE EXCEPTION 'residence determination: the determination time may not be in the future' USING ERRCODE = 'RD005';
    END IF;
    IF NEW.verified_residence_set_at IS NOT NULL AND OLD.verified_residence_set_at IS NOT NULL AND NEW.verified_residence_set_at <= OLD.verified_residence_set_at THEN
        RAISE EXCEPTION 'residence determination: a new determination must be later than the one it replaces' USING ERRCODE = 'RD005';
    END IF;
    RETURN NEW;
END
$$;
-- AFTER, not BEFORE: the table's own CHECK constraints (the column pairs) are evaluated first and keep their own error. Backdating
-- set_at alone (no residence column changes) is not guarded here: the check row is keyed by set_at, so a moved set_at orphans the
-- check and the evidence read excludes the determination (fail closed).
CREATE TRIGGER kyc_verifications_residence_guard AFTER UPDATE ON kyc_verifications
    FOR EACH ROW WHEN (ROW(NEW.verified_residence_country, NEW.verified_residence_set_by, NEW.verified_residence_source)
                       IS DISTINCT FROM ROW(OLD.verified_residence_country, OLD.verified_residence_set_by, OLD.verified_residence_source))
    EXECUTE FUNCTION kyc_verifications_residence_guard();
CREATE TRIGGER kyc_verifications_residence_check_upd AFTER UPDATE ON kyc_verifications
    FOR EACH ROW WHEN (NEW.verified_residence_set_at IS NOT NULL AND NEW.verified_residence_set_at IS DISTINCT FROM OLD.verified_residence_set_at)
    EXECUTE FUNCTION kyc_verifications_residence_check();
CREATE TRIGGER kyc_verifications_residence_check_ins AFTER INSERT ON kyc_verifications
    FOR EACH ROW WHEN (NEW.verified_residence_set_at IS NOT NULL)
    EXECUTE FUNCTION kyc_verifications_residence_check();

-- =========================================================================
-- A6. Grants (mirrored by deploy/init-app-role.sql)
-- =========================================================================
DO $$
DECLARE
    f text;
BEGIN
    FOREACH f IN ARRAY ARRAY['residence_mode_rank(text)', 'residence_mc_deny_mutation()', 'residence_approval_policies_guard()',
                             'residence_approval_policy_approvals_guard()', 'residence_determination_checks_guard()',
                             'residence_policy_mode(uuid)', 'residence_authorize_policy_actor(uuid, text, text, text)',
                             'kyc_verifications_residence_check()', 'kyc_verifications_residence_guard()'] LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
            EXECUTE format('REVOKE ALL ON FUNCTION %s FROM igaming_runtime', f);
        END IF;
    END LOOP;
    FOREACH f IN ARRAY ARRAY['residence_policy_set(uuid, text, text)', 'residence_policy_approve(uuid, uuid, text)', 'residence_policy_list(uuid)',
                             'residence_determination_approve(uuid, uuid, timestamptz, text)'] LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
            EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO igaming_runtime', f);
        END IF;
    END LOOP;
END
$$;
-- residence_mode_rank is referenced by a CHECK constraint, which runs with the privileges of the inserting role: it must stay
-- executable. It is a pure IMMUTABLE function of its argument.
GRANT EXECUTE ON FUNCTION residence_mode_rank(text) TO PUBLIC;

COMMENT ON TABLE residence_approval_policies IS '0181 (lic10, G8/Q21): per-tenant residence determination maker-checker policy VERSIONS (none | into_open_market | all). No seed: an absent row is mode none (the pre-0181 behaviour). A stricter change is active at once; a less strict one needs two approvals by distinct Persons.';
COMMENT ON TABLE residence_determination_checks IS '0181 (lic10): one row per reviewer residence determination, with the policy mode snapshot and whether a second Person must approve it. A determination that requires approval and has none is NOT market evidence (kyc.ReadMarketResidenceEvidence).';

-- ##########################################################################################################################
-- (B) AML screening triggers (audit G1). PARTIALLY IMPLEMENTED; the only provider is the MOCK.
-- ##########################################################################################################################
-- What this adds, and only this:
--   B1  aml_screening_trigger_policies   governed per-tenant, per-jurisdiction configuration of WHICH events trigger a screening
--                                        (registration, kyc_approval, scheduled) and whether withdrawal / payout is GATED while a
--                                        required screening is outstanding. Authored exactly like aml_screening_policies: a
--                                        platform_acting aml_officer, the officer quorum, a signed actor proof, a legal review
--                                        reference; NO default, NO seed. Needs an existing screening policy for the jurisdiction
--                                        (the lists, the re-screen interval and the backfill grace are THAT policy's values).
--   B2  aml_screening_requests           the durable queue. The DATABASE unique key (tenant_id, idempotency_key) makes every
--                                        enqueue idempotent; a state-machine trigger makes the lifecycle (pending, claimed, done,
--                                        needs_officer, failed_terminal) the only one possible. A potential / confirmed match is
--                                        NEVER cleared by the system: it stays needs_officer until an officer records the result
--                                        (aml_screening_record) under the SAME idempotency key (which also places the hold, with
--                                        the officer quorum and proof, exactly as today).
--   B3  enqueue triggers on player_accounts (registration) and kyc_verifications (approval), and the scheduled enqueue function
--       (periodic re-screen and backfill, one request per person per re-screen interval).
--   B4  the worker API: claim (SKIP LOCKED, lease) and finish (claim-token CAS). Session-tenant bound, no proof: the worker is not
--       an officer. LIMIT (recorded for security review): like the KYC submission outbox, a holder of the runtime credential and a
--       tenant session could call finish; a system-actor attestation (ADR 0116 F1) is NOT built.
--   B5  the GATE: aml_hold_active (the existing AML gate point of every money boundary, migration 0141) additionally answers
--       "held" for withdrawal and payout when the tenant's effective trigger policy has gate_withdrawal = true and the person has
--       NO fresh completed clear (inside the screening policy's re-screen interval; a pending, retrying, failed, error or stale
--       result is not one), or has a system-found match that no officer has recorded yet. An existing person is exempt during
--       the screening policy's backfill grace. The public rendering of a held answer is the existing indistinguishable
--       account_review (no tipping off). Deposit, casino and sportsbook stakes are NOT gated by this predicate.
--   B6  the alert Kind aml.screening_attention (a match awaiting an officer, or a request that failed terminally).
-- NOT BUILT (and not claimed): transaction monitoring and its rules (R-AML-MONITORING stays failing), a first-deposit and a
-- withdrawal-request TRIGGER (a denied withdrawal rolls its own request back, so a request-time enqueue cannot persist; the gate
-- plus the scheduler cover that need), the system-actor hold placement, a real provider.
-- SQLSTATE class AT (AML trigger): AT001 invalid input, AT002 not found / session tenant mismatch, AT003 illegal transition,
-- AT004 configuration precondition, AT005 append-only.

-- B6. The alert Kind (the 0141 / 0170 pattern: a temporary literal single-Kind FOR INSERT policy, created and dropped in one block).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.alert_kinds WHERE kind IN ('aml.screening_attention', 'aml.screening_gate_trapped')) THEN
        RAISE EXCEPTION 'migration 0181 pre-flight: an AML screening alert kind already exists' USING ERRCODE = 'RD098';
    END IF;
    CREATE POLICY alert_kinds_seed_0181 ON alert_kinds
        FOR INSERT TO CURRENT_USER
        WITH CHECK (kind IN ('aml.screening_attention', 'aml.screening_gate_trapped'));
    INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject,
                             in_tx_raisable_by_tenant, allowed_keys, raise_mode)
    VALUES ('aml.screening_attention', 'p2', 'platform', false, true, true,
            ARRAY['request_id', 'reason', 'trigger'], 'in_tx'),
           -- lic18 (H-1 f): persons have been held at the withdrawal gate longer than the policy's alert threshold (trapped funds).
           ('aml.screening_gate_trapped', 'p1', 'platform', false, true, true,
            ARRAY['persons'], 'in_tx');
    DROP POLICY alert_kinds_seed_0181 ON alert_kinds;
END
$$;

-- H-1 (a): the screening-worker heartbeat. A gating trigger policy can be authored ONLY while a screening worker has reported in
-- recently; production never starts the worker (the MOCK provider is refused there), so the gate cannot be enabled there. Platform-level
-- (not tenant data): RLS enabled, no policy and no privilege for the runtime role; the owner (the definer functions) bypasses it.
-- LIMIT: the heartbeat function is runtime-callable and proof-less, like the worker API; it is a configuration guard, not an
-- attestation (see the lic18 record for the pre-real-provider requirements).
CREATE TABLE screening_worker_heartbeats (
    provider_id  TEXT PRIMARY KEY CHECK (provider_id ~ '^[a-z0-9_.-]{1,64}$'),
    synthetic    BOOLEAN NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL
);
ALTER TABLE screening_worker_heartbeats ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON screening_worker_heartbeats FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        REVOKE ALL ON screening_worker_heartbeats FROM igaming_runtime;
    END IF;
END
$$;

CREATE FUNCTION screening_worker_heartbeat(p_provider text, p_synthetic boolean) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF p_provider IS NULL OR p_provider !~ '^[a-z0-9_.-]{1,64}$' OR p_synthetic IS NULL THEN
        RAISE EXCEPTION 'screening worker: invalid heartbeat' USING ERRCODE = 'AT001';
    END IF;
    INSERT INTO public.screening_worker_heartbeats (provider_id, synthetic, last_seen_at)
    VALUES (p_provider, p_synthetic, pg_catalog.clock_timestamp())
    ON CONFLICT (provider_id) DO UPDATE SET last_seen_at = EXCLUDED.last_seen_at, synthetic = EXCLUDED.synthetic;
END
$$;

-- Has a screening worker reported in within the last 10 minutes (an ENGINEERING bound, not a compliance value)? Owner-only helper.
CREATE FUNCTION screening_worker_available() RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT EXISTS (SELECT 1 FROM public.screening_worker_heartbeats h WHERE h.last_seen_at > pg_catalog.clock_timestamp() - interval '10 minutes')
$$;

-- B1/B2. Tables
CREATE TABLE aml_screening_trigger_policies (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    licensing_jurisdiction_id UUID NOT NULL REFERENCES jurisdictions (id) ON DELETE RESTRICT,
    -- Closed vocabulary; may be empty (no automatic trigger). No default.
    events                    TEXT[] NOT NULL CHECK (cardinality(events) <= 3 AND events <@ ARRAY['registration', 'kyc_approval', 'scheduled']::text[]),
    gate_withdrawal           BOOLEAN NOT NULL,
    -- H-1 (f): a gating policy states after how many seconds a held person raises the `aml.screening_gate_trapped` alert. No default.
    gate_alert_after_seconds  BIGINT NULL CHECK (gate_alert_after_seconds IS NULL OR gate_alert_after_seconds BETWEEN 60 AND 2592000),
    effective_from            TIMESTAMPTZ NOT NULL,
    legal_review_reference    TEXT NOT NULL CHECK (octet_length(legal_review_reference) BETWEEN 1 AND 200
                                                   AND legal_review_reference !~ '[\x01-\x1F\x7F]'),
    reason_code               TEXT NOT NULL CHECK (reason_code ~ '^[a-z0-9_]{1,64}$'),
    created_by                UUID NOT NULL,
    created_by_person_id      UUID NOT NULL,
    created_by_scope          TEXT NOT NULL CHECK (created_by_scope = 'platform_acting'),
    created_at                TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, id),
    -- H-1 (b): a gate needs the SCHEDULED re-screen, otherwise a clear goes stale unattended and holds the person for ever.
    CHECK (NOT gate_withdrawal OR 'scheduled' = ANY (events)),
    CHECK (gate_withdrawal = (gate_alert_after_seconds IS NOT NULL))
);
CREATE INDEX aml_screening_trigger_policies_effective ON aml_screening_trigger_policies (tenant_id, licensing_jurisdiction_id, effective_from DESC, created_at DESC);

CREATE TABLE aml_screening_requests (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    person_id                 UUID NOT NULL REFERENCES persons (id) ON DELETE RESTRICT,
    licensing_jurisdiction_id UUID NOT NULL REFERENCES jurisdictions (id) ON DELETE RESTRICT,
    trigger                   TEXT NOT NULL CHECK (trigger IN ('registration', 'kyc_approval', 'scheduled')),
    idempotency_key           TEXT NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9_.:-]{1,128}$'),
    status                    TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'claimed', 'done', 'needs_officer', 'failed_terminal', 'cancelled')),
    attempts                  INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at           TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    claim_token               UUID NULL,
    lease_until               TIMESTAMPTZ NULL,
    last_error_code           TEXT NULL CHECK (last_error_code IN ('timeout', 'unavailable', 'invalid_response', 'rate_limited', 'unknown')),
    outcome                   TEXT NULL CHECK (outcome IN ('clear', 'potential_match', 'confirmed')),
    match_lists               TEXT[] NULL CHECK (match_lists IS NULL OR (cardinality(match_lists) BETWEEN 1 AND 2 AND match_lists <@ ARRAY['sanctions', 'pep']::text[])),
    provider_id               TEXT NULL CHECK (provider_id ~ '^[a-z0-9_.-]{1,64}$'),
    provider_reference        TEXT NULL CHECK (provider_reference ~ '^[A-Za-z0-9_.:-]{1,128}$'),
    -- C7: the per-tenant pseudonymous reference the vendor saw. Never persons.id / player_account_id.
    subject_ref               TEXT NULL CHECK (subject_ref ~ '^[0-9a-f]{64}$'),
    screened_at               TIMESTAMPTZ NULL,
    completed_at              TIMESTAMPTZ NULL,
    -- lic23 (N-2): the identity of the match as the provider states it (sha256 of the sorted list:vendor-match-reference pairs; NULL
    -- when the provider gave no stable reference). A later match with the SAME fingerprint for the same person, whose earlier
    -- occurrence an officer already reviewed and cleared, is recorded as reviewed_from_request_id instead of holding the person again.
    match_fingerprint         TEXT NULL CHECK (match_fingerprint ~ '^[0-9a-f]{64}$'),
    reviewed_from_request_id  UUID NULL,
    -- lic23 (N-4): an officer's governed cancellation of a stale pending / claimed request (the replacement is a new request).
    cancelled_by              UUID NULL,
    cancelled_by_person_id    UUID NULL,
    cancel_reason_code        TEXT NULL CHECK (cancel_reason_code ~ '^[a-z0-9_]{1,64}$'),
    replaced_by_request_id    UUID NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, idempotency_key),
    UNIQUE (tenant_id, id),
    CHECK ((status = 'claimed') = (claim_token IS NOT NULL AND lease_until IS NOT NULL)),
    CHECK ((claim_token IS NULL) = (lease_until IS NULL)),
    CHECK ((status IN ('done', 'needs_officer', 'failed_terminal')) = (completed_at IS NOT NULL)),
    -- FAIL CLOSED, in the database: only a COMPLETED clear is done; a match is never done; a failure is never an outcome.
    CHECK (status <> 'done' OR (outcome = 'clear' AND match_lists IS NULL AND provider_id IS NOT NULL AND screened_at IS NOT NULL)),
    CHECK (status <> 'needs_officer' OR (outcome IN ('potential_match', 'confirmed') AND match_lists IS NOT NULL AND provider_id IS NOT NULL AND screened_at IS NOT NULL)),
    CHECK (status NOT IN ('pending', 'claimed', 'failed_terminal', 'cancelled') OR (outcome IS NULL AND match_lists IS NULL)),
    CHECK (status <> 'failed_terminal' OR last_error_code IS NOT NULL),
    CHECK (reviewed_from_request_id IS NULL OR (status = 'needs_officer' AND match_fingerprint IS NOT NULL)),
    CHECK ((status = 'cancelled') = (cancelled_by IS NOT NULL AND cancelled_by_person_id IS NOT NULL AND cancel_reason_code IS NOT NULL)),
    CHECK (status <> 'cancelled' OR (outcome IS NULL AND match_lists IS NULL AND completed_at IS NULL)),
    FOREIGN KEY (tenant_id, reviewed_from_request_id) REFERENCES aml_screening_requests (tenant_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, replaced_by_request_id) REFERENCES aml_screening_requests (tenant_id, id) ON DELETE RESTRICT
);
CREATE INDEX aml_screening_requests_due ON aml_screening_requests (tenant_id, next_attempt_at) WHERE status IN ('pending', 'claimed');
CREATE INDEX aml_screening_requests_person ON aml_screening_requests (tenant_id, person_id, status, completed_at DESC);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['aml_screening_trigger_policies', 'aml_screening_requests'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('REVOKE ALL ON %I FROM PUBLIC', t);
        EXECUTE format($f$CREATE POLICY aml_owner_session ON %I FOR ALL TO CURRENT_USER
                          USING (tenant_id = public.aml_session_tenant()) WITH CHECK (tenant_id = public.aml_session_tenant())$f$, t);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
            EXECUTE format('REVOKE ALL ON %I FROM igaming_runtime', t);
        END IF;
    END LOOP;
    -- The policy table is append-only (the 0154 shape); the queue is a state machine guarded by its own trigger and is never deleted.
    EXECUTE 'CREATE TRIGGER aml_screening_trigger_policies_append_only BEFORE UPDATE OR DELETE ON aml_screening_trigger_policies FOR EACH ROW EXECUTE FUNCTION aml_deny_mutation()';
    EXECUTE 'CREATE TRIGGER aml_screening_trigger_policies_no_truncate BEFORE TRUNCATE ON aml_screening_trigger_policies FOR EACH STATEMENT EXECUTE FUNCTION aml_deny_mutation()';
    EXECUTE 'CREATE TRIGGER aml_screening_requests_no_delete BEFORE DELETE ON aml_screening_requests FOR EACH ROW EXECUTE FUNCTION aml_deny_mutation()';
    EXECUTE 'CREATE TRIGGER aml_screening_requests_no_truncate BEFORE TRUNCATE ON aml_screening_requests FOR EACH STATEMENT EXECUTE FUNCTION aml_deny_mutation()';
END
$$;

-- Guards
CREATE FUNCTION aml_screening_trigger_policies_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor record;
BEGIN
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.created_by IS DISTINCT FROM v_actor.actor OR NEW.created_by_scope IS DISTINCT FROM v_actor.scope
       OR NEW.created_by_person_id IS DISTINCT FROM v_actor.person_id THEN
        RAISE EXCEPTION 'aml: the trigger policy author columns are not the proven session actor' USING ERRCODE = 'AT001';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER aml_screening_trigger_policies_10_guard BEFORE INSERT ON aml_screening_trigger_policies
    FOR EACH ROW EXECUTE FUNCTION aml_screening_trigger_policies_guard();

CREATE FUNCTION aml_screening_requests_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'pending' OR NEW.attempts <> 0 OR NEW.claim_token IS NOT NULL THEN
            RAISE EXCEPTION 'aml: a screening request is created pending, unclaimed, with no attempt' USING ERRCODE = 'AT003';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM public.player_accounts pa WHERE pa.tenant_id = NEW.tenant_id AND pa.person_id = NEW.person_id) THEN
            RAISE EXCEPTION 'aml: the person has no account in this tenant' USING ERRCODE = 'AT001';
        END IF;
        RETURN NEW;
    END IF;
    -- UPDATE: the identity of the request never changes.
    IF ROW(NEW.id, NEW.tenant_id, NEW.person_id, NEW.licensing_jurisdiction_id, NEW.trigger, NEW.idempotency_key, NEW.created_at)
       IS DISTINCT FROM ROW(OLD.id, OLD.tenant_id, OLD.person_id, OLD.licensing_jurisdiction_id, OLD.trigger, OLD.idempotency_key, OLD.created_at) THEN
        RAISE EXCEPTION 'aml: the identity of a screening request is immutable' USING ERRCODE = 'AT005';
    END IF;
    -- Terminal states are final.
    IF OLD.status IN ('done', 'needs_officer', 'failed_terminal', 'cancelled') THEN
        RAISE EXCEPTION 'aml: a % screening request is final', OLD.status USING ERRCODE = 'AT005';
    END IF;
    -- lic23 (N-4): a stale pending / claimed request is cancelled only by the governed officer function, which stamps the officer
    -- columns and releases any lease in the same statement.
    IF OLD.status IN ('pending', 'claimed') AND NEW.status = 'cancelled' AND NEW.attempts = OLD.attempts AND NEW.claim_token IS NULL THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'pending' AND NEW.status = 'claimed' AND NEW.attempts = OLD.attempts + 1 THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'claimed' AND NEW.status = 'claimed' AND NEW.attempts = OLD.attempts + 1 AND OLD.lease_until <= pg_catalog.clock_timestamp()
       AND NEW.claim_token IS DISTINCT FROM OLD.claim_token THEN
        RETURN NEW;  -- a lease that expired is reclaimed under a NEW token
    END IF;
    IF OLD.status = 'claimed' AND NEW.status = 'pending' AND NEW.attempts = OLD.attempts AND NEW.last_error_code IS NOT NULL THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'claimed' AND NEW.status IN ('done', 'needs_officer', 'failed_terminal') AND NEW.attempts = OLD.attempts THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'aml: illegal screening request transition % -> %', OLD.status, NEW.status USING ERRCODE = 'AT003';
END
$$;
CREATE TRIGGER aml_screening_requests_10_guard BEFORE INSERT OR UPDATE ON aml_screening_requests
    FOR EACH ROW EXECUTE FUNCTION aml_screening_requests_guard();

-- lic23 helpers (owner-only, invoker; reached through the definer API functions and the gate).
-- N-4 / SN-5: the NON-forgeable environment anchor (migration 0135). An unrecorded anchor reads as 'production', so a gate can be
-- enabled (and a worker result accepted) only where the database owner recorded development, staging or test.
CREATE FUNCTION aml_screening_gate_environment_ok() RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$ SELECT public.rg_database_environment() IN ('development', 'staging', 'test') $$;

-- SN-2: has an officer RESOLVED this system-found request? Reviewed automatically (lic23 N-2) counts; otherwise a non-error officer
-- result under the request's key counts only if it was recorded AFTER the request completed and, for a sanctions-class request
-- (confirmed, or a sanctions list), only if it is itself a sanctions match (a clear never resolves it).
CREATE FUNCTION aml_screening_request_resolved(p_request uuid) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT EXISTS (
        SELECT 1 FROM public.aml_screening_requests r
         WHERE r.id = p_request
           AND (r.reviewed_from_request_id IS NOT NULL
                OR EXISTS (SELECT 1 FROM public.aml_screening_results x
                            WHERE x.tenant_id = r.tenant_id AND x.person_id = r.person_id AND x.idempotency_key = r.idempotency_key
                              AND x.outcome <> 'error' AND (r.completed_at IS NULL OR x.recorded_at >= r.completed_at)
                              AND (NOT (r.outcome = 'confirmed' OR COALESCE('sanctions' = ANY (r.match_lists), false))
                                   OR x.outcome = 'confirmed' OR (x.outcome = 'potential_match' AND COALESCE('sanctions' = ANY (x.match_lists), false))))))
$$;

-- N-2: was this request's match REVIEWED AND CLEARED by an officer? A clear (never possible for a sanctions-class match), or a
-- recorded match whose hold has since been LIFTED (a sanctions hold only by the proof-bound four-eyes approval, aml_hold_lift /
-- aml_hold_lifts_guard). Returns the time from which the person counts as reviewed-cleared (NULL when not).
CREATE FUNCTION aml_screening_review_cleared_at(p_request uuid) RETURNS timestamptz
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT max(CASE WHEN x.outcome = 'clear' THEN x.recorded_at ELSE l.lifted_at END)
      FROM public.aml_screening_requests r
      JOIN public.aml_screening_results x ON x.tenant_id = r.tenant_id AND x.person_id = r.person_id AND x.idempotency_key = r.idempotency_key
      LEFT JOIN public.aml_hold_lifts l ON l.tenant_id = x.tenant_id AND l.hold_id = x.hold_id
     WHERE r.id = p_request AND x.outcome <> 'error' AND (r.completed_at IS NULL OR x.recorded_at >= r.completed_at)
       AND (x.outcome = 'clear' OR (x.outcome IN ('potential_match', 'confirmed') AND l.id IS NOT NULL))
$$;

-- B3. Enqueue (internal; invoker, owner-only; always reached through a definer trigger or a definer API function)
-- The jurisdictions a tenant's screening policies may be authored for: the one of its licence and those of its APPROVED, open operating
-- bases (lic9). Owner-only helper (invoker; reached through definer functions).
CREATE FUNCTION aml_screening_jurisdictions(p_tenant uuid) RETURNS SETOF uuid
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT l.jurisdiction_id FROM public.tenants t JOIN public.licences l ON l.id = t.licence_id WHERE t.id = p_tenant
    UNION
    SELECT COALESCE(b.jurisdiction_id, bl.jurisdiction_id) FROM public.operating_bases b
      LEFT JOIN public.licences bl ON bl.id = b.licence_id
     WHERE b.tenant_id = p_tenant AND b.status = 'active' AND b.effective_to IS NULL AND b.change_request_id IS NOT NULL
$$;

CREATE FUNCTION aml_screening_enqueue(p_tenant uuid, p_person uuid, p_trigger text, p_key text) RETURNS uuid
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_jur uuid;
    v_id  uuid;
BEGIN
    -- The latest effective version per jurisdiction of the tenant; the first jurisdiction whose policy lists the event.
    SELECT l.licensing_jurisdiction_id INTO v_jur
      FROM (SELECT DISTINCT ON (tp.licensing_jurisdiction_id) tp.licensing_jurisdiction_id, tp.events
              FROM public.aml_screening_trigger_policies tp
             WHERE tp.tenant_id = p_tenant AND tp.effective_from <= pg_catalog.clock_timestamp()
               AND tp.licensing_jurisdiction_id IN (SELECT public.aml_screening_jurisdictions(p_tenant))
             ORDER BY tp.licensing_jurisdiction_id, tp.created_at DESC, tp.id DESC) l
     WHERE p_trigger = ANY (l.events)
     ORDER BY l.licensing_jurisdiction_id LIMIT 1;
    IF v_jur IS NULL THEN
        RETURN NULL;  -- no policy for this event: nothing is triggered
    END IF;
    INSERT INTO public.aml_screening_requests (tenant_id, person_id, licensing_jurisdiction_id, trigger, idempotency_key)
    VALUES (p_tenant, p_person, v_jur, p_trigger, p_key)
    ON CONFLICT (tenant_id, idempotency_key) DO NOTHING
    RETURNING id INTO v_id;
    RETURN v_id;
END
$$;

CREATE FUNCTION player_accounts_aml_enqueue() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    PERFORM public.aml_screening_enqueue(NEW.tenant_id, NEW.person_id, 'registration', 'reg:' || NEW.id::text);
    RETURN NEW;
END
$$;
CREATE TRIGGER player_accounts_aml_enqueue AFTER INSERT ON player_accounts
    FOR EACH ROW EXECUTE FUNCTION player_accounts_aml_enqueue();

CREATE FUNCTION kyc_verifications_aml_enqueue() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    PERFORM public.aml_screening_enqueue(NEW.tenant_id, NEW.person_id, 'kyc_approval', 'kyc:' || NEW.id::text);
    RETURN NEW;
END
$$;
CREATE TRIGGER kyc_verifications_aml_enqueue_upd AFTER UPDATE OF status ON kyc_verifications
    FOR EACH ROW WHEN (NEW.status = 'approved' AND OLD.status IS DISTINCT FROM 'approved')
    EXECUTE FUNCTION kyc_verifications_aml_enqueue();
CREATE TRIGGER kyc_verifications_aml_enqueue_ins AFTER INSERT ON kyc_verifications
    FOR EACH ROW WHEN (NEW.status = 'approved')
    EXECUTE FUNCTION kyc_verifications_aml_enqueue();

-- B1. The officer API for the trigger policy
CREATE FUNCTION aml_screening_trigger_policy_set(
    p_tenant uuid, p_jurisdiction uuid, p_events text[], p_gate_withdrawal boolean, p_gate_alert_after_seconds bigint,
    p_effective_from timestamptz, p_legal_ref text, p_reason_code text
) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a        record;
    v_id     uuid;
    v_events text;
    v_cfg    boolean;
    v_tenant text;
    v_off    boolean;
BEGIN
    IF p_events IS NULL OR p_gate_withdrawal IS NULL OR p_jurisdiction IS NULL OR p_effective_from IS NULL THEN
        RAISE EXCEPTION 'aml: a trigger policy needs its jurisdiction, events, gate flag and effective time' USING ERRCODE = 'AT001';
    END IF;
    SELECT COALESCE(string_agg(x, ',' ORDER BY x), '') INTO v_events FROM (SELECT DISTINCT unnest(p_events) AS x) u;
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_screening_trigger_policy:set', 'new',
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_jurisdiction::text, v_events, p_gate_withdrawal::text, p_gate_alert_after_seconds::text,
            to_char(p_effective_from AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), p_legal_ref, p_reason_code)));
    IF a.o_scope <> 'platform_acting' THEN
        RAISE EXCEPTION 'aml: screening trigger configuration is authored by a platform officer' USING ERRCODE = 'AM002';
    END IF;
    PERFORM public.aml_assert_quorum(p_tenant, public.aml_owner_scope_of_tenant(p_tenant));
    -- H-1 (d): a version takes effect now or soon, never in the past (a past date would wipe prior clears and reset the grace) and
    -- never far ahead. A small tolerance absorbs clock skew between the caller and the database.
    IF p_effective_from < pg_catalog.clock_timestamp() - interval '2 minutes' OR p_effective_from > pg_catalog.clock_timestamp() + interval '30 days' THEN
        RAISE EXCEPTION 'aml: a trigger policy takes effect between now (2 minutes tolerance) and 30 days ahead' USING ERRCODE = 'AT004';
    END IF;
    IF p_gate_withdrawal THEN
        -- H-1 (b): a gate needs the scheduled re-screen; (f): and a stated alert threshold; (a): and a running screening worker.
        IF NOT ('scheduled' = ANY (p_events)) THEN
            RAISE EXCEPTION 'aml: a withdrawal gate needs the scheduled re-screen event' USING ERRCODE = 'AT004';
        END IF;
        IF p_gate_alert_after_seconds IS NULL THEN
            RAISE EXCEPTION 'aml: a withdrawal gate needs gate_alert_after_seconds' USING ERRCODE = 'AT004';
        END IF;
        IF NOT public.screening_worker_available() THEN
            RAISE EXCEPTION 'aml: no screening worker is running in this environment, so a withdrawal gate cannot be enabled (it would trap funds)' USING ERRCODE = 'AT004';
        END IF;
        -- N-4 / SN-5: the heartbeat is forgeable by a holder of the runtime credential; the database environment anchor is not. A gate
        -- cannot be enabled where the anchor says production or records nothing (an unrecorded anchor is production).
        IF NOT public.aml_screening_gate_environment_ok() THEN
            RAISE EXCEPTION 'aml: a withdrawal gate cannot be enabled in a database whose environment anchor is production or unrecorded' USING ERRCODE = 'AT004';
        END IF;
    ELSIF p_gate_alert_after_seconds IS NOT NULL THEN
        RAISE EXCEPTION 'aml: gate_alert_after_seconds belongs to a gating policy only' USING ERRCODE = 'AT004';
    END IF;
    -- N-1: a NON-gating version with no events (the way to switch a gate off) is allowed for any jurisdiction that already has a trigger
    -- policy for this tenant, whatever the licence or operating bases are today: after a licence rebind or a basis closure the gate
    -- loop still evaluates that jurisdiction, so it must stay possible to switch it off. Quorum, proof and the legal review reference
    -- above still apply.
    v_off := NOT p_gate_withdrawal AND v_events = ''
             AND EXISTS (SELECT 1 FROM public.aml_screening_trigger_policies t WHERE t.tenant_id = p_tenant AND t.licensing_jurisdiction_id = p_jurisdiction);
    IF NOT v_off THEN
    -- The policy jurisdiction must be one of the tenant's licence or approved operating bases.
    IF NOT EXISTS (SELECT 1 FROM public.aml_screening_jurisdictions(p_tenant) j WHERE j = p_jurisdiction) THEN
        RAISE EXCEPTION 'aml: the trigger policy jurisdiction must be the jurisdiction of the tenant''s licence or of an approved operating basis' USING ERRCODE = 'AT004';
    END IF;
    -- The lists, the re-screen interval and the backfill grace are the SCREENING POLICY's: it must exist first.
    -- N-5: and it must be IN EFFECT at or before this version's effective time (a future-dated screening policy supplies no interval yet).
    IF NOT EXISTS (SELECT 1 FROM public.aml_screening_policies sp WHERE sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = p_jurisdiction
                    AND sp.effective_from <= p_effective_from) THEN
        RAISE EXCEPTION 'aml: set the screening policy for this jurisdiction, in effect at or before the trigger policy, first' USING ERRCODE = 'AT004';
    END IF;
    -- Same SR7-3 rule as the screening policy: the tenant must be configured to operate in the jurisdiction.
    v_tenant := pg_catalog.current_setting('app.tenant_id', true);
    PERFORM pg_catalog.set_config('app.tenant_id', p_tenant::text, true);
    SELECT EXISTS (SELECT 1 FROM public.tenant_jurisdiction_configs c
                    WHERE c.tenant_id = p_tenant AND c.jurisdiction_id = p_jurisdiction
                      AND (c.effective_to IS NULL OR c.effective_to > pg_catalog.clock_timestamp())) INTO v_cfg;
    PERFORM pg_catalog.set_config('app.tenant_id', COALESCE(v_tenant, ''), true);
    IF NOT v_cfg THEN
        RAISE EXCEPTION 'aml: the tenant is not configured to operate in this jurisdiction' USING ERRCODE = 'AT004';
    END IF;
    END IF;
    INSERT INTO public.aml_screening_trigger_policies (tenant_id, licensing_jurisdiction_id, events, gate_withdrawal, gate_alert_after_seconds, effective_from,
                                                       legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope)
    VALUES (p_tenant, p_jurisdiction, COALESCE(pg_catalog.string_to_array(NULLIF(v_events, ''), ','), ARRAY[]::text[]), p_gate_withdrawal,
            p_gate_alert_after_seconds, p_effective_from, p_legal_ref, p_reason_code, a.o_actor, a.o_person_id, a.o_scope)
    RETURNING id INTO v_id;
    RETURN v_id;
END
$$;

CREATE FUNCTION aml_screening_trigger_policy_effective(p_tenant uuid, p_jurisdiction uuid)
RETURNS TABLE (policy_id uuid, events text[], gate_withdrawal boolean, gate_alert_after_seconds bigint, effective_from timestamptz)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF p_tenant IS NULL OR p_jurisdiction IS NULL THEN
        RAISE EXCEPTION 'aml: invalid trigger policy read arguments' USING ERRCODE = 'AT001';
    END IF;
    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AT002';
    END IF;
    RETURN QUERY
        SELECT p.id, COALESCE(p.events, ARRAY[]::text[]), p.gate_withdrawal, p.gate_alert_after_seconds, p.effective_from
          FROM public.aml_screening_trigger_policies p
         WHERE p.tenant_id = p_tenant AND p.licensing_jurisdiction_id = p_jurisdiction AND p.effective_from <= pg_catalog.clock_timestamp()
         ORDER BY p.created_at DESC, p.id DESC
         LIMIT 1;
END
$$;

-- Officer read of a person's screening requests (so an officer can find the key a needs_officer request must be resolved under).
CREATE FUNCTION aml_screening_request_list(p_tenant uuid, p_person uuid)
RETURNS TABLE (o_request_id uuid, o_trigger text, o_idempotency_key text, o_status text, o_attempts integer, o_outcome text,
               o_match_lists text[], o_last_error_code text, o_completed_at timestamptz, o_resolved boolean, o_created_at timestamptz)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a record;
BEGIN
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_screening_request:read', p_person::text,
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_person::text)));
    PERFORM public.aml_assert_not_subject(a.o_person_id, p_person);
    PERFORM public.aml_assert_row_scope(a.o_scope, public.aml_owner_scope_of_tenant(p_tenant));
    RETURN QUERY
        SELECT r.id, r.trigger, r.idempotency_key, r.status, r.attempts, r.outcome, r.match_lists, r.last_error_code, r.completed_at,
               public.aml_screening_request_resolved(r.id),
               r.created_at
          FROM public.aml_screening_requests r
         WHERE r.tenant_id = p_tenant AND r.person_id = p_person
         ORDER BY r.created_at, r.id;
END
$$;

-- B4. The scheduled enqueue (periodic re-screen and backfill): at most one request per person per re-screen interval.
CREATE FUNCTION aml_screening_enqueue_due(p_tenant uuid, p_limit integer) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_tp       record;
    v_sp       record;
    v_first    timestamptz;
    v_bucket   bigint;
    v_n        integer;
    v_total    integer := 0;
BEGIN
    IF p_tenant IS NULL OR p_limit IS NULL OR p_limit < 1 OR p_limit > 1000 THEN
        RAISE EXCEPTION 'aml: invalid scheduled enqueue arguments' USING ERRCODE = 'AT001';
    END IF;
    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AT002';
    END IF;
    FOR v_tp IN
        SELECT l.licensing_jurisdiction_id AS jur
          FROM (SELECT DISTINCT ON (tp.licensing_jurisdiction_id) tp.licensing_jurisdiction_id, tp.events
                  FROM public.aml_screening_trigger_policies tp
                 WHERE tp.tenant_id = p_tenant AND tp.effective_from <= pg_catalog.clock_timestamp()
                 ORDER BY tp.licensing_jurisdiction_id, tp.created_at DESC, tp.id DESC) l
         WHERE 'scheduled' = ANY (l.events)
    LOOP
        SELECT sp.rescreen_interval_seconds INTO v_sp FROM public.aml_screening_policies sp
         WHERE sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = v_tp.jur AND sp.effective_from <= pg_catalog.clock_timestamp()
         ORDER BY sp.created_at DESC, sp.id DESC LIMIT 1;
        IF NOT FOUND THEN
            CONTINUE;  -- no re-screen interval: nothing is due for this jurisdiction
        END IF;
        -- H-1 (d): clears count from the FIRST version of this jurisdiction's policy, so a new version never re-queues everyone.
        -- lic23 N-3: the SAME baseline the gate uses (the first GATING version), so a person cleared under an earlier non-gating version is
        -- re-enqueued exactly when the gate would hold them. A jurisdiction that never gated falls back to its first version.
        SELECT COALESCE(min(tp.effective_from) FILTER (WHERE tp.gate_withdrawal), min(tp.effective_from)) INTO v_first
          FROM public.aml_screening_trigger_policies tp
         WHERE tp.tenant_id = p_tenant AND tp.licensing_jurisdiction_id = v_tp.jur;
        v_bucket := pg_catalog.floor(pg_catalog.date_part('epoch', pg_catalog.clock_timestamp()) / v_sp.rescreen_interval_seconds)::bigint;
        INSERT INTO public.aml_screening_requests (tenant_id, person_id, licensing_jurisdiction_id, trigger, idempotency_key)
        SELECT p_tenant, c.person_id, v_tp.jur, 'scheduled', 'sched:' || c.person_id::text || ':' || v_bucket::text
          FROM (
                SELECT pa.person_id,
                       (SELECT max(g.t) FROM (
                            SELECT r.completed_at AS t FROM public.aml_screening_requests r
                             WHERE r.tenant_id = p_tenant AND r.person_id = pa.person_id AND r.status IN ('done', 'needs_officer') AND r.completed_at >= v_first
                            UNION ALL
                            SELECT x.recorded_at FROM public.aml_screening_results x
                             WHERE x.tenant_id = p_tenant AND x.person_id = pa.person_id AND x.outcome <> 'error' AND x.recorded_at >= v_first) g) AS last_done
                  FROM (SELECT DISTINCT a.person_id FROM public.player_accounts a WHERE a.tenant_id = p_tenant) pa
               ) c
         WHERE (c.last_done IS NULL OR c.last_done <= pg_catalog.clock_timestamp() - pg_catalog.make_interval(secs => v_sp.rescreen_interval_seconds::double precision))
           AND NOT EXISTS (SELECT 1 FROM public.aml_screening_requests o WHERE o.tenant_id = p_tenant AND o.person_id = c.person_id AND o.status IN ('pending', 'claimed'))
         ORDER BY c.last_done NULLS FIRST, c.person_id
         LIMIT (p_limit - v_total)
        ON CONFLICT (tenant_id, idempotency_key) DO NOTHING;
        GET DIAGNOSTICS v_n = ROW_COUNT;
        v_total := v_total + v_n;
        EXIT WHEN v_total >= p_limit;
    END LOOP;
    RETURN v_total;
END
$$;

-- B4. Claim (SKIP LOCKED + lease; an expired lease is reclaimed under a NEW token and counts as an attempt).
CREATE FUNCTION aml_screening_request_claim(p_tenant uuid, p_limit integer, p_lease_seconds integer)
RETURNS TABLE (o_id uuid, o_person_id uuid, o_jurisdiction_id uuid, o_trigger text, o_idempotency_key text, o_attempts integer, o_claim_token uuid)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF p_tenant IS NULL OR p_limit IS NULL OR p_limit < 1 OR p_limit > 200 OR p_lease_seconds IS NULL OR p_lease_seconds < 5 OR p_lease_seconds > 3600 THEN
        RAISE EXCEPTION 'aml: invalid claim arguments' USING ERRCODE = 'AT001';
    END IF;
    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AT002';
    END IF;
    RETURN QUERY
        WITH due AS (
            SELECT r.id AS rid FROM public.aml_screening_requests r
             WHERE r.tenant_id = p_tenant
               AND ((r.status = 'pending' AND r.next_attempt_at <= pg_catalog.clock_timestamp())
                    OR (r.status = 'claimed' AND r.lease_until <= pg_catalog.clock_timestamp()))
             ORDER BY r.next_attempt_at, r.id
             LIMIT p_limit
             FOR UPDATE SKIP LOCKED
        )
        UPDATE public.aml_screening_requests u
           SET status = 'claimed', attempts = u.attempts + 1, claim_token = gen_random_uuid(),
               lease_until = pg_catalog.clock_timestamp() + pg_catalog.make_interval(secs => p_lease_seconds::double precision),
               updated_at = pg_catalog.clock_timestamp()
          FROM due
         WHERE u.id = due.rid
        RETURNING u.id, u.person_id, u.licensing_jurisdiction_id, u.trigger, u.idempotency_key, u.attempts, u.claim_token;
END
$$;

-- S2 (cheap): a clear or a match may name only a provider that a screening worker has reported in with recently. Owner-only helper.
CREATE FUNCTION aml_screening_assert_worker_provider(p_provider text) RETURNS void
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM public.screening_worker_heartbeats h
                    WHERE h.provider_id = p_provider AND h.last_seen_at > pg_catalog.clock_timestamp() - interval '10 minutes') THEN
        RAISE EXCEPTION 'aml: the provider is not the one a running screening worker reported' USING ERRCODE = 'AT001';
    END IF;
    -- SN-5: the heartbeat alone is forgeable; a worker result (clear or match) is accepted only where the non-forgeable database
    -- environment anchor is development, staging or test.
    IF NOT public.aml_screening_gate_environment_ok() THEN
        RAISE EXCEPTION 'aml: a worker screening result is not accepted in a database whose environment anchor is production or unrecorded' USING ERRCODE = 'AT004';
    END IF;
END
$$;

-- B4. Finish (claim-token CAS). p_result: clear | potential_match | confirmed | retry | terminal.
CREATE FUNCTION aml_screening_request_finish(
    p_tenant uuid, p_id uuid, p_claim_token uuid, p_result text, p_match_lists text[], p_error_code text,
    p_provider_id text, p_provider_reference text, p_subject_ref text, p_screened_at timestamptz, p_backoff_seconds integer,
    p_match_fingerprint text
) RETURNS TABLE (o_applied boolean, o_status text, o_reviewed boolean)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v     record;
    v_rev uuid;
BEGIN
    IF p_tenant IS NULL OR p_id IS NULL OR p_claim_token IS NULL OR p_result IS NULL
       OR p_result NOT IN ('clear', 'potential_match', 'confirmed', 'retry', 'terminal') THEN
        RAISE EXCEPTION 'aml: invalid finish arguments' USING ERRCODE = 'AT001';
    END IF;
    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AT002';
    END IF;
    SELECT * INTO v FROM public.aml_screening_requests r WHERE r.id = p_id AND r.tenant_id = p_tenant FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'aml: screening request not found' USING ERRCODE = 'AT002';
    END IF;
    IF v.status <> 'claimed' OR v.claim_token IS DISTINCT FROM p_claim_token THEN
        RETURN QUERY SELECT false, v.status, false;  -- someone else holds it, or it already ended: nothing is applied
        RETURN;
    END IF;
    IF p_result = 'clear' THEN
        IF p_match_lists IS NOT NULL OR p_provider_id IS NULL OR p_screened_at IS NULL OR p_provider_reference IS NULL THEN
            RAISE EXCEPTION 'aml: a clear carries no match and names its provider, its provider reference and time' USING ERRCODE = 'AT001';
        END IF;
        PERFORM public.aml_screening_assert_worker_provider(p_provider_id);
        UPDATE public.aml_screening_requests SET status = 'done', outcome = 'clear', provider_id = p_provider_id, provider_reference = p_provider_reference,
               subject_ref = p_subject_ref, screened_at = p_screened_at, completed_at = pg_catalog.clock_timestamp(), claim_token = NULL, lease_until = NULL,
               last_error_code = NULL, updated_at = pg_catalog.clock_timestamp() WHERE id = p_id;
        RETURN QUERY SELECT true, 'done'::text, false;
    ELSIF p_result IN ('potential_match', 'confirmed') THEN
        IF p_match_lists IS NULL OR p_provider_id IS NULL OR p_screened_at IS NULL THEN
            RAISE EXCEPTION 'aml: a match names its list, provider and time' USING ERRCODE = 'AT001';
        END IF;
        PERFORM public.aml_screening_assert_worker_provider(p_provider_id);
        IF p_match_fingerprint IS NOT NULL AND p_match_fingerprint !~ '^[0-9a-f]{64}$' THEN
            RAISE EXCEPTION 'aml: malformed match fingerprint' USING ERRCODE = 'AT001';
        END IF;
        -- lic23 N-2: the SAME match (same provider-stated identity) for the same person that an officer already REVIEWED AND CLEARED is
        -- recorded as such instead of holding the person again for ever. A different or new hit still needs an officer.
        v_rev := NULL;
        IF p_match_fingerprint IS NOT NULL THEN
            SELECT COALESCE(q.reviewed_from_request_id, q.id) INTO v_rev FROM public.aml_screening_requests q
             WHERE q.tenant_id = p_tenant AND q.person_id = v.person_id AND q.id <> p_id AND q.status = 'needs_officer'
               AND q.match_fingerprint = p_match_fingerprint
               AND (q.reviewed_from_request_id IS NOT NULL OR public.aml_screening_review_cleared_at(q.id) IS NOT NULL)
             ORDER BY q.completed_at DESC LIMIT 1;
        END IF;
        UPDATE public.aml_screening_requests SET status = 'needs_officer', outcome = p_result, match_lists = p_match_lists, provider_id = p_provider_id,
               provider_reference = p_provider_reference, subject_ref = p_subject_ref, screened_at = p_screened_at,
               match_fingerprint = p_match_fingerprint, reviewed_from_request_id = v_rev,
               completed_at = pg_catalog.clock_timestamp(), claim_token = NULL, lease_until = NULL, last_error_code = NULL,
               updated_at = pg_catalog.clock_timestamp() WHERE id = p_id;
        RETURN QUERY SELECT true, 'needs_officer'::text, v_rev IS NOT NULL;
    ELSIF p_result = 'retry' THEN
        IF p_error_code IS NULL OR p_backoff_seconds IS NULL OR p_backoff_seconds < 0 OR p_backoff_seconds > 86400 THEN
            RAISE EXCEPTION 'aml: a retry names its error code and a bounded backoff' USING ERRCODE = 'AT001';
        END IF;
        UPDATE public.aml_screening_requests SET status = 'pending', next_attempt_at = pg_catalog.clock_timestamp() + pg_catalog.make_interval(secs => p_backoff_seconds::double precision),
               last_error_code = p_error_code, claim_token = NULL, lease_until = NULL, updated_at = pg_catalog.clock_timestamp() WHERE id = p_id;
        RETURN QUERY SELECT true, 'pending'::text, false;
    ELSE
        IF p_error_code IS NULL THEN
            RAISE EXCEPTION 'aml: a terminal failure names its error code' USING ERRCODE = 'AT001';
        END IF;
        UPDATE public.aml_screening_requests SET status = 'failed_terminal', last_error_code = p_error_code, completed_at = pg_catalog.clock_timestamp(),
               claim_token = NULL, lease_until = NULL, updated_at = pg_catalog.clock_timestamp() WHERE id = p_id;
        RETURN QUERY SELECT true, 'failed_terminal'::text, false;
    END IF;
END
$$;

-- S1: aml_screening_record = migration 0155's body PLUS the binding of a system-found request. When a request exists under the key:
-- the record must be for ITS person and trigger; an `error` never resolves a needs_officer request; resolving one needs the officer
-- quorum; and a SANCTIONS-class match (confirmed, or a sanctions list) can be resolved ONLY by recording a match (which places the
-- hold, lifted later only by the proof-bound four-eyes approval), never by a single officer's clear.
CREATE OR REPLACE FUNCTION aml_screening_record(
    p_tenant uuid, p_person uuid, p_idempotency_key text, p_trigger text, p_provider_id text, p_provider_reference text,
    p_subject_ref text, p_lists text[], p_outcome text, p_match_lists text[], p_error_code text, p_screened_at timestamptz,
    p_decision text, p_hold_reason_code text
) RETURNS TABLE (o_result_id uuid, o_hold_id uuid, o_replayed boolean)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a        record;
    v_owner  text;
    v_lists  text;
    v_match  text;
    v_prior  record;
    v_req    record;
    v_hold   uuid;
    v_result uuid;
    v_hash   text;
    v_class  text;
BEGIN
    SELECT string_agg(x, ',' ORDER BY x) INTO v_lists FROM unnest(p_lists) AS x;
    SELECT string_agg(x, ',' ORDER BY x) INTO v_match FROM unnest(p_match_lists) AS x;
    v_hash := public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_person::text, p_idempotency_key, p_trigger, p_provider_id, p_provider_reference,
            p_subject_ref, v_lists, p_outcome, v_match, p_error_code,
            to_char(p_screened_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), p_decision, p_hold_reason_code));
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_screening:record', 'new', v_hash);
    v_owner := public.aml_owner_scope_of_tenant(p_tenant);
    IF public.aml_actor_owner_scope(a.o_scope) IS DISTINCT FROM v_owner THEN
        RAISE EXCEPTION 'aml: the actor does not own this tenant''s AML scope' USING ERRCODE = 'AM002';
    END IF;
    PERFORM public.aml_assert_not_subject(a.o_person_id, p_person);
    PERFORM public.aml_person_lock(p_tenant, p_person);
    SELECT r.id, r.hold_id, r.payload_hash INTO v_prior
      FROM public.aml_screening_results r WHERE r.tenant_id = p_tenant AND r.idempotency_key = p_idempotency_key;
    IF FOUND THEN
        IF v_prior.payload_hash <> v_hash THEN
            RAISE EXCEPTION 'aml: the idempotency key was used with different content' USING ERRCODE = 'AM004';
        END IF;
        RETURN QUERY SELECT v_prior.id, v_prior.hold_id, true;
        RETURN;
    END IF;
    SELECT q.person_id, q.trigger, q.status, q.outcome, q.match_lists INTO v_req
      FROM public.aml_screening_requests q WHERE q.tenant_id = p_tenant AND q.idempotency_key = p_idempotency_key;
    IF FOUND THEN
        IF v_req.person_id <> p_person OR v_req.trigger <> p_trigger THEN
            RAISE EXCEPTION 'aml: the key belongs to a screening request of another person or trigger' USING ERRCODE = 'AM004';
        END IF;
        -- SN-2: only a request that is waiting for an officer (a match, or a terminal failure) can be resolved by a record under its
        -- key. A pending / claimed / done / cancelled request is not: a record there would pre-resolve a later result.
        IF v_req.status NOT IN ('needs_officer', 'failed_terminal') THEN
            RAISE EXCEPTION 'aml: the key belongs to a screening request that is not awaiting an officer' USING ERRCODE = 'AM004';
        END IF;
        IF v_req.status = 'needs_officer' THEN
            IF p_outcome = 'error' THEN
                RAISE EXCEPTION 'aml: an error result cannot resolve a match awaiting an officer' USING ERRCODE = 'AM004';
            END IF;
            PERFORM public.aml_assert_quorum(p_tenant, v_owner);
            IF (v_req.outcome = 'confirmed' OR COALESCE('sanctions' = ANY (v_req.match_lists), false))
               AND NOT (p_outcome = 'confirmed' OR (p_outcome = 'potential_match' AND COALESCE('sanctions' = ANY (p_match_lists), false))) THEN
                RAISE EXCEPTION 'aml: a sanctions-class match is resolved only by recording a sanctions match (a hold lifted by four-eyes), never by a clear' USING ERRCODE = 'AM006';
            END IF;
        END IF;
    ELSIF p_idempotency_key ~ '^(reg|kyc|sched|repl):' THEN
        -- SN-2: those key prefixes belong to the automatic screening queue; an officer record may not squat a key the system will use
        -- (reg:<account>, kyc:<verification>, sched:<person>:<bucket>, repl:<person>:<id>).
        RAISE EXCEPTION 'aml: this key prefix is reserved for the automatic screening queue' USING ERRCODE = 'AM004';
    END IF;
    IF p_decision <> 'none' THEN
        IF p_hold_reason_code IS NULL OR p_hold_reason_code !~ '^[a-z0-9_]{1,64}$' THEN
            RAISE EXCEPTION 'aml: a hold decision needs a reason code' USING ERRCODE = 'AM004';
        END IF;
        IF p_decision = 'hold_all_value_movement' AND NOT (p_outcome = 'confirmed' OR COALESCE('sanctions' = ANY (p_match_lists), false)) THEN
            RAISE EXCEPTION 'aml: all-value-movement is the decision of a confirmed outcome or a sanctions-list match only' USING ERRCODE = 'AM004';
        END IF;
        v_class := CASE WHEN COALESCE('sanctions' = ANY (p_match_lists), false)
                        THEN CASE WHEN p_outcome = 'confirmed' THEN 'confirmed_sanctions' ELSE 'sanctions_potential' END
                        ELSE 'manual' END;
        PERFORM public.aml_assert_quorum(p_tenant, v_owner);
        INSERT INTO public.aml_holds (tenant_id, owner_scope, person_id, player_account_id, hold_scope, reason_class, reason_code,
                                      placed_by, placed_by_person_id, placed_by_scope)
        VALUES (p_tenant, v_owner, p_person, NULL,
                CASE p_decision WHEN 'hold_all_value_movement' THEN 'all_value_movement' ELSE 'withdrawal_and_payout' END,
                v_class,
                p_hold_reason_code, a.o_actor, a.o_person_id, a.o_scope)
        RETURNING id INTO v_hold;
    END IF;
    INSERT INTO public.aml_screening_results (tenant_id, owner_scope, person_id, idempotency_key, trigger, provider_id, provider_reference,
                                              subject_ref, lists, outcome, match_lists, error_code, screened_at, decision, hold_id,
                                              hold_reason_class, payload_hash, recorded_by, recorded_by_person_id, recorded_by_scope)
    VALUES (p_tenant, v_owner, p_person, p_idempotency_key, p_trigger, p_provider_id, p_provider_reference, p_subject_ref, p_lists,
            p_outcome, p_match_lists, p_error_code, p_screened_at, p_decision, v_hold, v_class, v_hash, a.o_actor, a.o_person_id, a.o_scope)
    RETURNING id INTO v_result;
    RETURN QUERY SELECT v_result, v_hold, false;
END
$$;

-- B5. The gate predicate and the extended aml_hold_active.
CREATE FUNCTION aml_screening_gate_denies(p_tenant uuid, p_person uuid, p_operation text) RETURNS boolean
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_tp         record;
    v_sp         record;
    v_reg        timestamptz;
    v_interval   double precision;
    v_grace      double precision;
    v_first      timestamptz;
    v_last_match timestamptz;
BEGIN
    IF p_operation NOT IN ('withdrawal', 'payout') THEN
        RETURN false;
    END IF;
    -- L1 / S1: a match the system found and no officer has RESOLVED holds the person whatever the policy says now (a new version
    -- with the gate off, a licence rebind or a removed licence must not release it). A resolution is a NON-ERROR result of the same
    -- person under the request's key; an `error` never resolves.
    -- SN-2: "resolved" is aml_screening_request_resolved (recorded after the request, and a sanctions-class match only by a sanctions
    -- match); N-2: a re-flag of a match an officer already reviewed and cleared is resolved by that review.
    IF EXISTS (SELECT 1 FROM public.aml_screening_requests r
                WHERE r.tenant_id = p_tenant AND r.person_id = p_person AND r.status = 'needs_officer'
                  AND NOT public.aml_screening_request_resolved(r.id)) THEN
        RETURN true;
    END IF;
    SELECT min(pa.created_at) INTO v_reg FROM public.player_accounts pa WHERE pa.tenant_id = p_tenant AND pa.person_id = p_person;
    -- Every jurisdiction whose LATEST effective trigger policy gates withdrawal (the policy is the tenant's, not tied to the licence
    -- that exists today: an unreadable or removed licence cannot release a gate, L2).
    FOR v_tp IN
        SELECT l.jur FROM (SELECT DISTINCT ON (tp.licensing_jurisdiction_id) tp.licensing_jurisdiction_id AS jur, tp.gate_withdrawal
                             FROM public.aml_screening_trigger_policies tp
                            WHERE tp.tenant_id = p_tenant AND tp.effective_from <= pg_catalog.clock_timestamp()
                            ORDER BY tp.licensing_jurisdiction_id, tp.created_at DESC, tp.id DESC) l
         WHERE l.gate_withdrawal
    LOOP
        -- H-1 (d) / L3: the baseline is the FIRST gating version of this jurisdiction, so a new version never resets the grace and
        -- never discards clears.
        SELECT min(tp.effective_from) INTO v_first FROM public.aml_screening_trigger_policies tp
         WHERE tp.tenant_id = p_tenant AND tp.licensing_jurisdiction_id = v_tp.jur AND tp.gate_withdrawal;
        SELECT sp.rescreen_interval_seconds, sp.backfill_grace_seconds INTO v_sp FROM public.aml_screening_policies sp
         WHERE sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = v_tp.jur AND sp.effective_from <= pg_catalog.clock_timestamp()
         ORDER BY sp.created_at DESC, sp.id DESC LIMIT 1;
        IF FOUND THEN
            v_interval := v_sp.rescreen_interval_seconds::double precision;
            v_grace := v_sp.backfill_grace_seconds::double precision;
        ELSE
            v_interval := NULL;
            v_grace := 0;
        END IF;
        IF v_reg IS NULL THEN
            RETURN true;  -- an unknown person is never "screened"
        END IF;
        -- An existing person is not held for a missing result during the backfill grace after the gate FIRST took effect (C1).
        IF v_reg < v_first AND pg_catalog.clock_timestamp() < v_first + pg_catalog.make_interval(secs => v_grace) THEN
            CONTINUE;
        END IF;
        -- The latest match the system found that no earlier review covers (an auto-reviewed re-flag is not a new finding).
        SELECT max(r.completed_at) INTO v_last_match FROM public.aml_screening_requests r
         WHERE r.tenant_id = p_tenant AND r.person_id = p_person AND r.status = 'needs_officer' AND r.reviewed_from_request_id IS NULL;
        -- Only a FRESH screening that cleared the person, NEWER than the latest match, satisfies the policy: a completed clear, an
        -- auto-reviewed re-flag (N-2), an officer's clear, or an officer-recorded match whose hold was LIFTED (a sanctions hold only by
        -- the proof-bound four-eyes approval) from the lift time. A pending, retrying, failed, error or stale result does not.
        -- Officer results are timed by the database clock (recorded_at / lifted_at), never by the officer's screened_at.
        IF NOT EXISTS (
            SELECT 1 FROM public.aml_screening_requests r
             WHERE r.tenant_id = p_tenant AND r.person_id = p_person AND r.status = 'done' AND r.outcome = 'clear'
               AND r.completed_at >= v_first AND (v_last_match IS NULL OR r.completed_at > v_last_match)
               AND (v_interval IS NULL OR r.completed_at > pg_catalog.clock_timestamp() - pg_catalog.make_interval(secs => v_interval))
            UNION ALL
            SELECT 1 FROM public.aml_screening_requests r
             WHERE r.tenant_id = p_tenant AND r.person_id = p_person AND r.status = 'needs_officer' AND r.reviewed_from_request_id IS NOT NULL
               AND r.completed_at >= v_first AND (v_last_match IS NULL OR r.completed_at > v_last_match)
               AND (v_interval IS NULL OR r.completed_at > pg_catalog.clock_timestamp() - pg_catalog.make_interval(secs => v_interval))
            UNION ALL
            SELECT 1 FROM public.aml_screening_results x
             WHERE x.tenant_id = p_tenant AND x.person_id = p_person AND x.outcome = 'clear'
               AND x.recorded_at >= v_first AND (v_last_match IS NULL OR x.recorded_at > v_last_match)
               AND (v_interval IS NULL OR x.recorded_at > pg_catalog.clock_timestamp() - pg_catalog.make_interval(secs => v_interval))
            UNION ALL
            SELECT 1 FROM public.aml_screening_results x
              JOIN public.aml_hold_lifts l ON l.tenant_id = x.tenant_id AND l.hold_id = x.hold_id
             WHERE x.tenant_id = p_tenant AND x.person_id = p_person AND x.outcome IN ('potential_match', 'confirmed')
               AND l.lifted_at >= v_first AND (v_last_match IS NULL OR l.lifted_at > v_last_match)
               AND (v_interval IS NULL OR l.lifted_at > pg_catalog.clock_timestamp() - pg_catalog.make_interval(secs => v_interval))
        ) THEN
            RETURN true;
        END IF;
    END LOOP;
    RETURN false;
END
$$;

-- H-1 (f): how many persons have been held at the withdrawal gate for longer than the policy's alert threshold (trapped funds)?
-- Session-tenant bound, proof-less, returns a COUNT only. Scans at most p_limit persons.
CREATE FUNCTION aml_screening_gate_overdue(p_tenant uuid, p_limit integer) RETURNS integer
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_alert integer;
    v_first timestamptz;
    v_n     integer;
BEGIN
    IF p_tenant IS NULL OR p_limit IS NULL OR p_limit < 1 OR p_limit > 20000 THEN
        RAISE EXCEPTION 'aml: invalid overdue arguments' USING ERRCODE = 'AT001';
    END IF;
    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AT002';
    END IF;
    SELECT min(l.gate_alert_after_seconds), min(f.first_from) INTO v_alert, v_first
      FROM (SELECT DISTINCT ON (tp.licensing_jurisdiction_id) tp.licensing_jurisdiction_id AS jur, tp.gate_withdrawal, tp.gate_alert_after_seconds
              FROM public.aml_screening_trigger_policies tp
             WHERE tp.tenant_id = p_tenant AND tp.effective_from <= pg_catalog.clock_timestamp()
             ORDER BY tp.licensing_jurisdiction_id, tp.created_at DESC, tp.id DESC) l
      JOIN LATERAL (SELECT min(t2.effective_from) AS first_from FROM public.aml_screening_trigger_policies t2
                     WHERE t2.tenant_id = p_tenant AND t2.licensing_jurisdiction_id = l.jur AND t2.gate_withdrawal) f ON true
     WHERE l.gate_withdrawal;
    IF v_alert IS NULL THEN
        RETURN 0;
    END IF;
    SELECT count(*)::integer INTO v_n FROM (
        SELECT pa.person_id FROM (SELECT DISTINCT a.person_id FROM public.player_accounts a WHERE a.tenant_id = p_tenant LIMIT p_limit) pa
         WHERE public.aml_screening_gate_denies(p_tenant, pa.person_id, 'withdrawal')
           AND COALESCE((SELECT min(r.created_at) FROM public.aml_screening_requests r
                          WHERE r.tenant_id = p_tenant AND r.person_id = pa.person_id AND r.status NOT IN ('done', 'cancelled')
                            AND NOT public.aml_screening_request_resolved(r.id)), v_first)
               < pg_catalog.clock_timestamp() - pg_catalog.make_interval(secs => v_alert::double precision)
    ) q;
    RETURN v_n;
END
$$;

-- lic23 (N-9): how many persons are PARKED right now at the payout gate ONLY because a required automatic screening is outstanding
-- (their latest withdrawal_payout enforcement decision in the window was a screening-only hold: held, no hold row, KYC passed or not
-- required)? Counts only, session-tenant bound, proof-less. It is readable by a loop that is NOT the screening worker (the payments
-- sweeper), so a dead screening worker shows as a growing parked count even though it can no longer raise its own alert.
CREATE FUNCTION aml_screening_parked_summary(p_tenant uuid, p_since timestamptz) RETURNS TABLE (o_persons integer, o_evaluations integer)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF p_tenant IS NULL OR p_since IS NULL THEN
        RAISE EXCEPTION 'aml: invalid parked summary arguments' USING ERRCODE = 'AT001';
    END IF;
    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AT002';
    END IF;
    RETURN QUERY
        WITH latest AS (
            SELECT DISTINCT ON (d.person_id) d.person_id, d.hold_outcome, d.hold_id, d.kyc_outcome, d.allowed
              FROM public.aml_enforcement_decisions d
             WHERE d.tenant_id = p_tenant AND d.operation = 'withdrawal_payout' AND d.decided_at >= p_since
             ORDER BY d.person_id, d.decided_at DESC, d.id DESC
        )
        SELECT (SELECT count(*)::integer FROM latest l
                 WHERE l.hold_outcome = 'held' AND l.hold_id IS NULL AND NOT l.allowed AND l.kyc_outcome IN ('passed', 'not_required')),
               (SELECT count(*)::integer FROM public.aml_enforcement_decisions d
                 WHERE d.tenant_id = p_tenant AND d.operation = 'withdrawal_payout' AND d.decided_at >= p_since
                   AND d.hold_outcome = 'held' AND d.hold_id IS NULL AND NOT d.allowed AND d.kyc_outcome IN ('passed', 'not_required'));
END
$$;

-- lic23 (N-4): the officer's governed way to un-stick a person whose screening is not progressing: a stale pending / claimed request
-- is CANCELLED and a fresh request is queued for the worker, or, for a person with NO request at all, a fresh request is queued.
-- Officer quorum, owner scope, a signed actor proof and a reason code; the cancelled row keeps the officer columns and the id of its
-- replacement (that IS the audit record), and the proof nonce is consumed. A request that is stuck on a match or a terminal failure
-- is RESOLVED (aml_screening_record), never replaced.
CREATE FUNCTION aml_screening_request_replace(p_tenant uuid, p_person uuid, p_request uuid, p_reason_code text)
RETURNS TABLE (o_new_request_id uuid, o_cancelled_request_id uuid)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a      record;
    v_req  record;
    v_jur  uuid;
    v_new  uuid;
    v_owner text;
BEGIN
    IF p_person IS NULL OR p_reason_code IS NULL OR p_reason_code !~ '^[a-z0-9_]{1,64}$' THEN
        RAISE EXCEPTION 'aml: a replacement needs a person and a reason code' USING ERRCODE = 'AT001';
    END IF;
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_screening_request:replace', COALESCE(p_request::text, 'new'),
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_person::text, p_request::text, p_reason_code)));
    v_owner := public.aml_owner_scope_of_tenant(p_tenant);
    IF public.aml_actor_owner_scope(a.o_scope) IS DISTINCT FROM v_owner THEN
        RAISE EXCEPTION 'aml: the actor does not own this tenant''s AML scope' USING ERRCODE = 'AM002';
    END IF;
    PERFORM public.aml_assert_not_subject(a.o_person_id, p_person);
    PERFORM public.aml_assert_quorum(p_tenant, v_owner);
    PERFORM public.aml_person_lock(p_tenant, p_person);
    IF p_request IS NOT NULL THEN
        SELECT * INTO v_req FROM public.aml_screening_requests r WHERE r.id = p_request AND r.tenant_id = p_tenant AND r.person_id = p_person FOR UPDATE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'aml: screening request not found' USING ERRCODE = 'AM040';
        END IF;
        IF v_req.status NOT IN ('pending', 'claimed') THEN
            RAISE EXCEPTION 'aml: only a pending or claimed request is replaced; a match or a terminal failure is resolved' USING ERRCODE = 'AT003';
        END IF;
        v_jur := v_req.licensing_jurisdiction_id;
    ELSE
        IF EXISTS (SELECT 1 FROM public.aml_screening_requests r WHERE r.tenant_id = p_tenant AND r.person_id = p_person AND r.status IN ('pending', 'claimed')) THEN
            RAISE EXCEPTION 'aml: the person has a pending request: name it to replace it' USING ERRCODE = 'AT003';
        END IF;
        SELECT l.jur INTO v_jur FROM (SELECT DISTINCT ON (tp.licensing_jurisdiction_id) tp.licensing_jurisdiction_id AS jur, tp.gate_withdrawal
                                        FROM public.aml_screening_trigger_policies tp
                                       WHERE tp.tenant_id = p_tenant AND tp.effective_from <= pg_catalog.clock_timestamp()
                                       ORDER BY tp.licensing_jurisdiction_id, tp.created_at DESC, tp.id DESC) l
         WHERE l.gate_withdrawal ORDER BY l.jur LIMIT 1;
        IF v_jur IS NULL THEN
            RAISE EXCEPTION 'aml: no trigger policy gates withdrawals for this tenant, so there is nothing to queue a screening for' USING ERRCODE = 'AT004';
        END IF;
    END IF;
    INSERT INTO public.aml_screening_requests (tenant_id, person_id, licensing_jurisdiction_id, trigger, idempotency_key)
    VALUES (p_tenant, p_person, v_jur, 'scheduled', 'repl:' || p_person::text || ':' || gen_random_uuid()::text)
    RETURNING id INTO v_new;
    IF p_request IS NOT NULL THEN
        UPDATE public.aml_screening_requests SET status = 'cancelled', claim_token = NULL, lease_until = NULL, cancelled_by = a.o_actor,
               cancelled_by_person_id = a.o_person_id, cancel_reason_code = p_reason_code, replaced_by_request_id = v_new,
               updated_at = pg_catalog.clock_timestamp() WHERE id = p_request;
    END IF;
    RETURN QUERY SELECT v_new, p_request;
END
$$;

-- aml_hold_active: migration 0141's body plus ONE additional disjunct. Same signature, attributes, grants and session-tenant bind.
CREATE OR REPLACE FUNCTION aml_hold_active(p_tenant uuid, p_person uuid, p_account uuid, p_operation text) RETURNS boolean
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF p_tenant IS NULL OR p_person IS NULL OR p_operation IS NULL OR NOT public.aml_valid_operation(p_operation) THEN
        RAISE EXCEPTION 'aml: invalid hold evaluation arguments' USING ERRCODE = 'AM004';
    END IF;
    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AM026';
    END IF;
    RETURN EXISTS (
        SELECT 1 FROM public.aml_holds h
         WHERE h.tenant_id = p_tenant AND h.person_id = p_person
           AND (h.player_account_id IS NULL OR h.player_account_id = p_account)
           AND (h.hold_scope = 'all_value_movement' OR p_operation IN ('withdrawal', 'payout', 'manual_adjustment'))
           AND NOT EXISTS (SELECT 1 FROM public.aml_hold_lifts l WHERE l.hold_id = h.id)
    ) OR public.aml_screening_gate_denies(p_tenant, p_person, p_operation);
END
$$;
COMMENT ON FUNCTION aml_hold_active(uuid, uuid, uuid, text) IS 'ADR 0116 s5.1: the ONLY hold read a gate performs. SECURITY DEFINER, STABLE, search_path pinned, session-tenant bound (AM026). Since 0181 (lic10) it also answers true for withdrawal / payout when a required automatic screening is outstanding (aml_screening_gate_denies).';

-- B7. Grants (mirrored by deploy/init-app-role.sql)
DO $$
DECLARE
    f text;
BEGIN
    FOREACH f IN ARRAY ARRAY['aml_screening_trigger_policies_guard()', 'aml_screening_requests_guard()', 'aml_screening_enqueue(uuid, uuid, text, text)',
                             'aml_screening_gate_denies(uuid, uuid, text)', 'player_accounts_aml_enqueue()', 'kyc_verifications_aml_enqueue()',
                             'aml_screening_jurisdictions(uuid)', 'aml_screening_assert_worker_provider(text)', 'screening_worker_available()',
                             'aml_screening_gate_environment_ok()', 'aml_screening_request_resolved(uuid)', 'aml_screening_review_cleared_at(uuid)'] LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
            EXECUTE format('REVOKE ALL ON FUNCTION %s FROM igaming_runtime', f);
        END IF;
    END LOOP;
    FOREACH f IN ARRAY ARRAY[
        'aml_screening_trigger_policy_set(uuid, uuid, text[], boolean, bigint, timestamptz, text, text)',
        'aml_screening_gate_overdue(uuid, integer)', 'screening_worker_heartbeat(text, boolean)',
        'aml_screening_trigger_policy_effective(uuid, uuid)', 'aml_screening_request_list(uuid, uuid)',
        'aml_screening_enqueue_due(uuid, integer)', 'aml_screening_request_claim(uuid, integer, integer)',
        'aml_screening_request_finish(uuid, uuid, uuid, text, text[], text, text, text, text, timestamptz, integer, text)',
        'aml_screening_request_replace(uuid, uuid, uuid, text)', 'aml_screening_parked_summary(uuid, timestamptz)'
    ] LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
            EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO igaming_runtime', f);
        END IF;
    END LOOP;
END
$$;

COMMENT ON TABLE aml_screening_trigger_policies IS '0181 (lic10, G1): per tenant and licensing jurisdiction VERSIONS of which events trigger an automatic screening and whether withdrawal / payout is gated. No default, no seed; legal values are configuration.';
COMMENT ON TABLE aml_screening_requests IS '0181 (lic10, G1): the durable automatic-screening queue. DB-unique idempotency per (tenant, key); a state-machine trigger; a match is never cleared by the system (needs_officer until an officer records the result under the same key).';
