-- ADR 0116 SLICE S3 (part 1 of 3): AML access foundation. IDENTITY / COMPLIANCE / SECURITY MIGRATION: FOR
-- `security` (blocking), `identity-compliance`, `ledger-finance`, `qa`, `code-reviewer` REVIEW BEFORE MERGE.
--
-- NUMBERING: 0140, allocated by the orchestrator. 0137-0139 are RESERVED for ADR 0119 (staff MFA / lifecycle);
-- until that branch merges `migrate verify` on this branch reports the 0137-0139 gap (the 0135 precedent). The
-- 0132 numbering in ADR 0116 is stale and updated in the ADR implementation record. This file depends on nothing in
-- 0137-0139.
--
-- NO LEGAL VALUE. Nothing here seeds a threshold, a retention period, a rule or a screening policy. NO AML VENDOR.
--
-- What it adds (ADR 0116 s3.1 B1, s6 B4), and only this:
--   0. Pre-flight (AM098): refuses if any object this file creates already exists, or if the K1 catalogue CHECKs are
--      not 0124's.
--   1. The K1 capability `aml_officer` (ADR 0099 catalogue row + operation_kind `aml_officer`, class
--      `outside_mandatory_class`: it is NOT a financial approval operation). It is created ONLY by the existing
--      proof-bound four-eyes capability grant flow (0112 / 0120). Role `aml_officer` is NOT added to any auth role.
--   2. NO new request guard: the K1 flow is already four-eyes and proof-bound. A tenant-scope officer is REQUESTED by a
--      tenant principal of the same tenant (a platform session cannot read tenant staff rows, 0112) and DECIDED only by a
--      distinct PLATFORM approver (R-7), who must also be a different Person from the requester and the grantee (R-4); a
--      platform officer (acting, bounded by acting_grant_max_lifetime) is requested by a platform principal (R-6). A tenant
--      admin therefore can never create or assign the role by themselves: their request is inert until a platform co-approval,
--      and `staff:manage` cannot set it (it is a capability, not a role). The eligible tenant grantee role is `compliance`.
--   3. aml_owner_scope_of_tenant(), aml_scope_compatible(), aml_officer_quorum() and aml_authorize(): the ONE
--      authorisation primitive every AML SECURITY DEFINER function (0141, 0142) calls FIRST. aml_authorize resolves
--      the actor from the session (financial_actor_session(): tenant | platform_acting only), re-checks IN SQL that the
--      actor's own staff row holds an in-force aml_officer grant of the matching scope for the target tenant, and then
--      consumes a SIGNED actor proof (actor_proof_require, ADR 0110) bound to (actor, scope, tenant, operation,
--      target, payload hash). No GUC grants anything by itself: a session that forges every GUC but cannot sign a
--      proof fails AP001..AP005 (THREAT-MODEL-ARBITRARY-SQL-1).
--
-- DEVIATION (disclosed): ADR 0116 s6 says "at least 2 distinct person_ids holding aml_officer PER OWNER SCOPE". The
-- K1 grant is keyed by (tenant, grantee, capability) and a platform grantee's grant is per tenant and bounded by
-- acting_grant_max_lifetime (0112 R-13, 4 hours by default). The quorum is therefore counted PER TENANT per owner scope:
-- distinct Persons with an in-force aml_officer grant of grantee_scope 'tenant' (owner scope 'tenant') or 'platform'
-- (owner scope 'platform') on that tenant. Re-granting platform officers within the lifetime bound is an operations
-- matter (docs/runbooks/aml-officer-bootstrap.md).
--
-- SQLSTATE class AM (new; ERRCODE grep over migrations, Go and docs found no AM code; SF is reserved by ADR 0119):
--   AM001 actor session / officer role / proof prerequisite refused     AM002 owner scope or tenant mismatch
--   AM003 officer quorum not met                                        AM004 invalid argument / shape
--   AM005 append-only table: UPDATE / DELETE / TRUNCATE refused         AM006 hold lift refused
--   AM007 hold person / account binding refused (C2)                    AM010 case state machine refused
--   AM011 four-eyes binding refused                                     AM012 stale or invalid proposal
--   AM013 note refused
--   AM026 session tenant is not the requested tenant                    AM040 not found (also: out of the actor's scope)
--   AM098 up pre-flight refused                                         AM099 down refused
-- AP0xx comes from the 0120 verifier unchanged.
--
-- The 0120 verifier (actor_proof_require) is NOT replaced here: it admits any `<family>:<verb>` operation with a tenant
-- for scope tenant / platform_acting, and ADR 0119 may also replace it (a second CREATE OR REPLACE would silently drop
-- one of the two families). The AML admission rule (scope tenant | platform_acting only, closed operation names) is
-- enforced by aml_authorize and mirrored by internal/actorproof.Claims.validate.

SET LOCAL lock_timeout = '5s';

-- =========================================================================
-- 0. Pre-flight
-- =========================================================================
DO $$
DECLARE
    v_def text;
    v_n   int;
BEGIN
    SELECT count(*) INTO v_n FROM pg_catalog.pg_proc
     WHERE pronamespace = 'public'::regnamespace AND proname LIKE 'aml\_%';
    IF v_n > 0 THEN
        RAISE EXCEPTION 'migration 0140 pre-flight: % aml_* function(s) already exist', v_n USING ERRCODE = 'AM098';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_class WHERE relnamespace = 'public'::regnamespace AND relname LIKE 'aml\_%') THEN
        RAISE EXCEPTION 'migration 0140 pre-flight: an aml_* relation already exists' USING ERRCODE = 'AM098';
    END IF;
    IF EXISTS (SELECT 1 FROM public.financial_capability_catalogue WHERE capability = 'aml_officer' OR operation_kind = 'aml_officer')
       OR EXISTS (SELECT 1 FROM public.financial_control_classifications WHERE operation_kind = 'aml_officer') THEN
        RAISE EXCEPTION 'migration 0140 pre-flight: the aml_officer capability / classification already exists' USING ERRCODE = 'AM098';
    END IF;
    SELECT pg_catalog.pg_get_constraintdef(oid) INTO v_def FROM pg_catalog.pg_constraint
     WHERE conrelid = 'public.financial_capability_catalogue'::regclass AND conname = 'financial_capability_catalogue_action_check';
    IF v_def IS DISTINCT FROM 'CHECK ((action = ANY (ARRAY[''initiate''::text, ''request''::text, ''approve''::text])))' THEN
        RAISE EXCEPTION 'migration 0140 pre-flight: financial_capability_catalogue_action_check is not the 0112 definition (%)', v_def USING ERRCODE = 'AM098';
    END IF;
    SELECT pg_catalog.pg_get_constraintdef(oid) INTO v_def FROM pg_catalog.pg_constraint
     WHERE conrelid = 'public.financial_capability_catalogue'::regclass AND conname = 'financial_capability_catalogue_operation_kind_check';
    IF v_def IS DISTINCT FROM 'CHECK ((operation_kind = ANY (ARRAY[''ledger_adjustment''::text, ''payment_force_resolve''::text, ''withdrawal_hold_resolution''::text])))' THEN
        RAISE EXCEPTION 'migration 0140 pre-flight: financial_capability_catalogue_operation_kind_check is not the 0124 definition (%)', v_def USING ERRCODE = 'AM098';
    END IF;
    IF to_regprocedure('public.financial_actor_session()') IS NULL
       OR to_regprocedure('public.actor_proof_require(uuid, text, uuid, text, text, text)') IS NULL
       OR to_regprocedure('public.k2_sha256_hex(text)') IS NULL THEN
        RAISE EXCEPTION 'migration 0140 pre-flight: a 0112 / 0113 / 0120 dependency is missing' USING ERRCODE = 'AM098';
    END IF;
END
$$;

-- =========================================================================
-- 1. The aml_officer K1 capability (reference rows, migration-written; the 0124 "seed with FORCE lifted and restored
--    inside this transaction" pattern)
-- =========================================================================
ALTER TABLE financial_capability_catalogue DROP CONSTRAINT financial_capability_catalogue_action_check;
ALTER TABLE financial_capability_catalogue ADD CONSTRAINT financial_capability_catalogue_action_check
    CHECK (action IN ('initiate', 'request', 'approve', 'officer'));
ALTER TABLE financial_capability_catalogue DROP CONSTRAINT financial_capability_catalogue_operation_kind_check;
ALTER TABLE financial_capability_catalogue ADD CONSTRAINT financial_capability_catalogue_operation_kind_check
    CHECK (operation_kind IN ('ledger_adjustment', 'payment_force_resolve', 'withdrawal_hold_resolution', 'aml_officer'));

ALTER TABLE financial_control_classifications NO FORCE ROW LEVEL SECURITY;
ALTER TABLE financial_capability_catalogue NO FORCE ROW LEVEL SECURITY;
INSERT INTO financial_control_classifications (operation_kind, class, governed_since)
VALUES ('aml_officer', 'outside_mandatory_class', now());
-- eligible tenant role: `compliance` (a tenant grantee must be a compliance staff member); a platform grantee must be
-- platform_admin (0112 R-9) and its grant is bounded by acting_grant_max_lifetime (R-13).
INSERT INTO financial_capability_catalogue (capability, operation_kind, action, eligible_tenant_roles, platform_grantee_allowed)
VALUES ('aml_officer', 'aml_officer', 'officer', '{compliance}', true);
ALTER TABLE financial_control_classifications FORCE ROW LEVEL SECURITY;
ALTER TABLE financial_capability_catalogue FORCE ROW LEVEL SECURITY;

-- =========================================================================
-- 2. Scope helpers and the authorisation primitive
-- =========================================================================

-- The owner scope of a tenant's AML data, from the licensing model (ADR 0006): our licence -> 'platform'; the tenant's
-- own licence -> 'tenant'. A tenant with no resolvable model fails closed.
CREATE FUNCTION aml_owner_scope_of_tenant(p_tenant uuid) RETURNS text
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v text;
BEGIN
    SELECT t.expected_licensee INTO v FROM public.tenants t WHERE t.id = p_tenant;
    IF v IS NULL THEN
        RAISE EXCEPTION 'aml: the tenant has no resolvable owner scope' USING ERRCODE = 'AM002';
    END IF;
    RETURN v;
END
$$;

-- A tenant-scope actor reaches only owner_scope 'tenant' rows; a platform_acting actor reaches both (within the tenant it
-- is acting in; the tenant equality is checked by aml_authorize).
CREATE FUNCTION aml_scope_compatible(p_actor_scope text, p_owner_scope text) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT CASE p_actor_scope
               WHEN 'tenant' THEN p_owner_scope = 'tenant'
               WHEN 'platform_acting' THEN p_owner_scope IN ('platform', 'tenant')
               ELSE false
           END
$$;

-- The quorum readiness count (ADR 0116 s6, deviation above): distinct Persons with an in-force aml_officer grant of the
-- owner scope's grantee scope on the tenant. Runs under the caller's own RLS (tenant session: the tenant's grants;
-- platform_acting session: the acting tenant's grants). The person comes from the request row (0112 captured it at
-- request time precisely because a platform session cannot read a tenant staff row).
CREATE FUNCTION aml_officer_quorum(p_tenant uuid, p_owner_scope text) RETURNS integer
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT count(DISTINCT r.grantee_person_id)::integer
      FROM public.staff_capability_grants g
      JOIN public.staff_capability_grant_requests r ON r.id = g.request_id
     WHERE g.tenant_id = p_tenant
       AND g.capability = 'aml_officer'
       AND g.revoked_at IS NULL
       AND g.valid_from <= pg_catalog.now()
       AND (g.valid_until IS NULL OR pg_catalog.now() < g.valid_until)
       AND g.grantee_scope = CASE p_owner_scope WHEN 'tenant' THEN 'tenant' WHEN 'platform' THEN 'platform' ELSE '?' END
$$;

CREATE FUNCTION aml_assert_quorum(p_tenant uuid, p_owner_scope text) RETURNS void
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF public.aml_officer_quorum(p_tenant, p_owner_scope) < 2 THEN
        RAISE EXCEPTION 'aml: the officer quorum (at least 2 distinct Persons) is not met for this owner scope' USING ERRCODE = 'AM003';
    END IF;
END
$$;

-- aml_authorize: the first statement of every AML definer function. Invoker function (it must see the CALLER's session);
-- EXECUTE is revoked from PUBLIC and the runtime role below, so only the definer functions (running as the owner) reach it.
CREATE FUNCTION aml_authorize(
    p_tenant uuid, p_operation text, p_target text, p_payload_hash text,
    OUT o_actor uuid, OUT o_scope text, OUT o_person_id uuid
) RETURNS record
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v           record;
    v_grant_scope text;
BEGIN
    IF p_tenant IS NULL OR p_operation IS NULL OR p_target IS NULL OR p_payload_hash IS NULL THEN
        RAISE EXCEPTION 'aml: authorisation arguments are required' USING ERRCODE = 'AM001';
    END IF;
    IF p_operation !~ '^aml_[a-z_]+:[a-z_]+$' THEN
        RAISE EXCEPTION 'aml: not an AML operation name' USING ERRCODE = 'AM001';
    END IF;
    -- Actor resolution: tenant | platform | platform_acting, else CG001 / CG020 (fail closed on any mixed GUC shape).
    SELECT * INTO v FROM public.financial_actor_session();
    IF v.scope = 'tenant' THEN
        v_grant_scope := 'tenant';
    ELSIF v.scope = 'platform_acting' THEN
        v_grant_scope := 'platform';
    ELSE
        RAISE EXCEPTION 'aml: this session shape may not use AML functions' USING ERRCODE = 'AM001';
    END IF;
    IF v.tenant IS DISTINCT FROM p_tenant THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AM002';
    END IF;
    IF v.person_id IS NULL THEN
        RAISE EXCEPTION 'aml: the officer has no linked person' USING ERRCODE = 'AM001';
    END IF;
    -- The SQL re-check of the role (ADR 0116 s3.1 step 2): the actor's OWN grant row, in force, of the matching scope.
    IF NOT EXISTS (
        SELECT 1 FROM public.staff_capability_grants g
         WHERE g.tenant_id = p_tenant AND g.grantee_staff_id = v.actor AND g.capability = 'aml_officer'
           AND g.grantee_scope = v_grant_scope AND g.revoked_at IS NULL
           AND g.valid_from <= pg_catalog.now() AND (g.valid_until IS NULL OR pg_catalog.now() < g.valid_until)
    ) THEN
        RAISE EXCEPTION 'aml: the actor does not hold an in-force aml_officer grant' USING ERRCODE = 'AM001';
    END IF;
    -- The signed proof (0120): bound to this actor, scope, tenant, operation, target and payload.
    PERFORM public.actor_proof_require(v.actor, v.scope, v.tenant, p_operation, p_target, p_payload_hash);
    o_actor := v.actor; o_scope := v.scope; o_person_id := v.person_id;
END
$$;

-- A row visible to the actor must be in the actor's scope; otherwise it is reported exactly like a missing row (AM040).
CREATE FUNCTION aml_assert_row_scope(p_actor_scope text, p_owner_scope text) RETURNS void
    LANGUAGE plpgsql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF p_owner_scope IS NULL OR NOT public.aml_scope_compatible(p_actor_scope, p_owner_scope) THEN
        RAISE EXCEPTION 'aml: not found' USING ERRCODE = 'AM040';
    END IF;
END
$$;

-- =========================================================================
-- 3. Grants. Helper functions are not callable by the runtime role; the definer API of 0141 / 0142 is.
-- =========================================================================
REVOKE ALL ON FUNCTION aml_owner_scope_of_tenant(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_scope_compatible(text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_officer_quorum(uuid, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_assert_quorum(uuid, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_authorize(uuid, text, text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_assert_row_scope(text, text) FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        REVOKE ALL ON FUNCTION aml_owner_scope_of_tenant(uuid) FROM igaming_runtime;
        REVOKE ALL ON FUNCTION aml_scope_compatible(text, text) FROM igaming_runtime;
        REVOKE ALL ON FUNCTION aml_officer_quorum(uuid, text) FROM igaming_runtime;
        REVOKE ALL ON FUNCTION aml_assert_quorum(uuid, text) FROM igaming_runtime;
        REVOKE ALL ON FUNCTION aml_authorize(uuid, text, text, text) FROM igaming_runtime;
        REVOKE ALL ON FUNCTION aml_assert_row_scope(text, text) FROM igaming_runtime;
    END IF;
END
$$;

COMMENT ON FUNCTION aml_authorize(uuid, text, text, text) IS 'ADR 0116 s3.1: the first statement of every AML definer function: actor from the session, SQL re-check of the aml_officer grant, signed actor proof. No GUC grants anything by itself.';
COMMENT ON FUNCTION aml_officer_quorum(uuid, text) IS 'ADR 0116 s6 B4: distinct Persons holding an in-force aml_officer grant of the owner scope on the tenant (per-tenant deviation recorded in migration 0140).';
