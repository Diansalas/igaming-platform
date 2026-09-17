-- Stage 4H-B0-R6, Workstream A (architect) - REVIEW FIX MIGRATION.
--
-- Closes three confirmed defects that independent security and code
-- review found in migrations 0044/0045 (this workstream's own prior
-- work). Nothing here is new scope: every change makes an already-stated
-- control actually true. 0044 and 0045 are deliberately NOT edited in
-- place - they are applied migrations, and rewriting history would make
-- the schema a different thing from what the migration chain says it is.
--
-- ======================================================================
-- FIX 1 (P1, launch-blocking) - the person-identity half of four-eyes
-- was unconditionally inert.
-- ======================================================================
-- 0044's asset_change_approvals_deny_self_approval() only compared
-- requester person to approver person when BOTH staff_users.person_id
-- values were non-NULL. Security traced every path that can create a
-- `platform_admin` account and established, as a verified fact rather
-- than a suspicion, that NO code path in this platform can set person_id
-- on one:
--
--   * cmd/seed-admin/main.go calls identity.CreateStaffUser(..., nil) -
--     the personID argument is literally nil for the platform_admin it
--     mints;
--   * internal/httpserver/admin_routes.go's staff-creation role allowlist
--     does not include platform_admin at all, so the person-carrying
--     creation path cannot produce one;
--   * the person-link remediation route is tenant-scoped, and under
--     staff_users' dual_scope_isolation RLS (migration 0011) a
--     tenant-scoped transaction cannot see a tenant_id IS NULL row, so it
--     can never link a platform account.
--
-- So the person check could never fire, and dual control degraded to "two
-- distinct staff UUIDs" - which one operator defeats by running
-- seed-admin twice. Security reproduced the whole bypass live (file ->
-- self-authorize through the second account -> create -> activate, one
-- human throughout).
--
-- The platform already has the correct pattern, and it is NEWER than the
-- one 0044 mirrored: migration 0034's withdrawal_approvals_enforce_
-- governance() REFUSES a decision outright when the approver's person_id
-- IS NULL or the approver's staff account is not `active`. Migration 0029
-- - which 0044 copied - is that function's since-superseded predecessor.
-- 0044 therefore mirrored a withdrawn version of the platform's own
-- precedent. Both 0044 trigger functions are brought in line with 0034
-- here.
--
-- ----------------------------------------------------------------------
-- *** DEPLOYMENT ORDERING DEPENDENCY - READ BEFORE APPLYING ***
-- ----------------------------------------------------------------------
-- Applied ALONE, with no way to ever set person_id on a platform-scoped
-- staff account, this migration makes the Asset Registry PERMANENTLY
-- UNUSABLE: no platform_admin could satisfy the requester check, so no
-- asset could ever be created, activated, platform-authorized, or granted
-- a platform-wide operation-eligibility default again. That is
-- fail-closed (correct direction) but it is a total outage of the
-- registry's administrative surface, not a graceful degradation.
--
-- This migration MUST land together with, or after, a path that can
-- person-link platform-scoped staff. That path is owned by
-- `identity-compliance` and is NOT built here (different package,
-- different owner): the candidates are a personID argument on
-- cmd/seed-admin, and a platform-scoped person-link route that runs under
-- db.Pool.WithoutTenant/WithPlatformAdmin so staff_users' dual-scope RLS
-- lets it see tenant_id IS NULL rows.
--
-- Verification before deploy - this query must return zero rows, or the
-- registry is bricked for that account:
--
--   SELECT id, email FROM staff_users
--    WHERE tenant_id IS NULL AND status = 'active' AND person_id IS NULL;
--
-- ======================================================================
-- FIX 2 (P2) - the layer-7 platform-wide eligibility grant had no
-- four-eyes representation at all.
-- ======================================================================
-- 0044's asset_change_requests.operation CHECK allowed exactly
-- ('create','activate','platform_authorize'), so "grant a platform-wide
-- default operation-eligibility row" - ADR 0037 §C.5.1 op 9, which §C.5.3
-- explicitly names as dual-controlled - was not even expressible as a
-- request. Meanwhile 0045's own comment (its lines 266-269) asserted that
-- platform-wide rows are "dual-controlled at the API level", which was
-- false: a single compromised platform-admin credential could flip a
-- platform-wide eligibility gate for every tenant on the platform in one
-- call. Code review and security both found this independently.
--
-- Closed by a fourth operation type plus a payload-matched consume, so
-- one approval authorizes exactly one (asset, operation, product) grant,
-- once - not "any layer-7 grant for this asset".
--
-- ======================================================================
-- FIX 3 (P2) - asset_authorizations RLS permitted DELETE, contradicting
-- 0045's own stated intent.
-- ======================================================================
-- 0045 wrote "No DELETE policy, deliberately" and then created
-- `tenant_isolation` as FOR ALL - which includes DELETE. Security proved
-- the consequence live: deleting a brand-level `eligible = false` row
-- silently promotes that brand from denied to allowed, because
-- CheckEligibility treats an ABSENT layer-5 row as "inherit the tenant
-- answer" (ADR 0037 §A.5's nullable-narrowing pattern). The narrowing
-- trigger cannot catch it: it is BEFORE INSERT OR UPDATE, and a DELETE
-- fires no BEFORE INSERT/UPDATE trigger. asset_operation_eligibility was
-- already correct (its policies are split per command, with none for
-- DELETE); asset_authorizations is brought to the same shape here.

-- ----------------------------------------------------------------------
-- FIX 1: person-linked, active, platform-scoped on BOTH sides
-- ----------------------------------------------------------------------

-- Requester side. Three conditions now, exactly mirroring migration
-- 0034's order and wording: the principal must resolve to a staff_users
-- row, that row must be platform-scoped, it must carry a confirmed Person
-- linkage, and it must be active.
--
-- Note there is no is_automated_approval-style exemption here, and no
-- "no staff row resolved" service-identity carve-out of the kind
-- migration 0034 has: unlike a withdrawal auto-approval, no service
-- identity files or decides asset registry changes. ADR 0037 §C.1 is
-- explicit that layers 1-3 are reachable only by a platform-scoped human
-- principal, so "cannot be resolved to a staff row" is a refusal, full
-- stop.
CREATE OR REPLACE FUNCTION asset_change_requests_require_platform_principal() RETURNS TRIGGER AS $$
DECLARE
    v_tenant_id UUID;
    v_person_id UUID;
    v_status    TEXT;
BEGIN
    SELECT su.tenant_id, su.person_id, su.status
      INTO v_tenant_id, v_person_id, v_status
      FROM staff_users su
     WHERE su.id = NEW.requested_by_principal_id;

    -- FOUND, checked immediately after the SELECT INTO, is the platform's
    -- existing idiom for this (migrations 0034 and 0044 both use it). A
    -- "SELECT ..., true INTO ..., v_found" variant would be wrong: with no
    -- matching row PL/pgSQL sets every INTO target to NULL, and
    -- `IF NOT NULL` is not taken - the check would fail open.
    IF NOT FOUND THEN
        RAISE EXCEPTION 'asset_change_requests: requesting principal % cannot be resolved to a staff account and is not eligible to request an asset change', NEW.requested_by_principal_id;
    END IF;

    IF v_tenant_id IS NOT NULL THEN
        RAISE EXCEPTION 'asset_change_requests: requesting principal % is not a platform-scoped staff principal (ADR 0037 C.1: layers 1-3 are never reachable by a tenant-scoped role)', NEW.requested_by_principal_id;
    END IF;

    -- THE Fix-1 condition on this side. Without a confirmed Person
    -- linkage the four-eyes control cannot tell two staff accounts held
    -- by one human apart from two humans, so an unlinked principal is
    -- refused rather than silently exempted (migration 0034's rule for
    -- withdrawal decisions, applied to asset registry decisions).
    IF v_person_id IS NULL THEN
        RAISE EXCEPTION 'asset_change_requests: requesting principal % has no confirmed Person linkage and is not eligible to request an asset change (four-eyes cannot be evaluated without it)', NEW.requested_by_principal_id;
    END IF;

    IF v_status IS DISTINCT FROM 'active' THEN
        RAISE EXCEPTION 'asset_change_requests: requesting staff account % is not active', NEW.requested_by_principal_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Approver side. Same three conditions for the approver, PLUS the
-- requester's own person_id is re-resolved and required non-NULL: a
-- request filed before this migration existed can still be pending with
-- an unlinked requester, and comparing against NULL is exactly the inert
-- check being fixed. The person comparison is now unconditional - it can
-- no longer be skipped by either side being NULL, because neither side
-- is allowed to be NULL.
CREATE OR REPLACE FUNCTION asset_change_approvals_deny_self_approval() RETURNS TRIGGER AS $$
DECLARE
    v_requester_principal_id UUID;
    v_requester_person_id    UUID;
    v_approver_person_id     UUID;
    v_approver_tenant_id     UUID;
    v_approver_status        TEXT;
BEGIN
    SELECT r.requested_by_principal_id INTO v_requester_principal_id
      FROM asset_change_requests r
     WHERE r.id = NEW.request_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'asset_change_approvals: request % is not visible in this scope', NEW.request_id;
    END IF;

    IF NEW.approver_principal_id = v_requester_principal_id THEN
        RAISE EXCEPTION 'asset_change_approvals: the requesting principal may not approve or reject its own asset change (self-approval)';
    END IF;

    SELECT su.tenant_id, su.person_id, su.status
      INTO v_approver_tenant_id, v_approver_person_id, v_approver_status
      FROM staff_users su
     WHERE su.id = NEW.approver_principal_id;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'asset_change_approvals: approver identity % cannot be resolved and is not eligible to decide asset changes', NEW.approver_principal_id;
    END IF;

    IF v_approver_tenant_id IS NOT NULL THEN
        RAISE EXCEPTION 'asset_change_approvals: approving principal % is not a platform-scoped staff principal', NEW.approver_principal_id;
    END IF;

    IF v_approver_person_id IS NULL THEN
        RAISE EXCEPTION 'asset_change_approvals: approver has no confirmed Person linkage and is not eligible to decide asset changes (four-eyes cannot be evaluated without it)';
    END IF;

    IF v_approver_status IS DISTINCT FROM 'active' THEN
        RAISE EXCEPTION 'asset_change_approvals: approver staff account is not active';
    END IF;

    SELECT su.person_id INTO v_requester_person_id
      FROM staff_users su
     WHERE su.id = v_requester_principal_id;

    IF v_requester_person_id IS NULL THEN
        RAISE EXCEPTION 'asset_change_approvals: the requesting principal has no confirmed Person linkage, so this request cannot be four-eyes approved (file a new request from a person-linked principal)';
    END IF;

    -- Unconditional now. Two staff accounts held by one human are one
    -- human, and that is no longer a check that quietly does nothing.
    IF v_requester_person_id = v_approver_person_id THEN
        RAISE EXCEPTION 'asset_change_approvals: approver resolves to the same person as the requester (self-approval through a second staff account)';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ----------------------------------------------------------------------
-- FIX 2: a fourth dual-controlled operation - the layer-7 platform-wide
-- eligibility GRANT
-- ----------------------------------------------------------------------
-- Only the GRANT direction. Revoking a platform-wide default
-- (eligible = false) stays single-actor, for the same reason suspend and
-- revoke do at layers 2-3 (ADR 0037 §C.5.3): the fail-closed direction
-- must never wait for a second approver.

ALTER TABLE asset_change_requests
    DROP CONSTRAINT IF EXISTS asset_change_requests_operation_check;

ALTER TABLE asset_change_requests
    ADD CONSTRAINT asset_change_requests_operation_check
    CHECK (operation IN ('create', 'activate', 'platform_authorize', 'platform_operation_eligibility'));

-- The approver must approve the EXACT layer-7 fact that will be written,
-- not "some eligibility change for this asset" - the same reasoning that
-- makes 0044 carry the identity payload for 'create'. eligibility_product
-- uses '*' for "every product" rather than a JSON null, matching the
-- COALESCE(product, '*') convention 0045's own unique indexes already
-- use, so payload matching is a plain text comparison with no
-- NULL-semantics trap.
ALTER TABLE asset_change_requests
    ADD CONSTRAINT asset_change_requests_eligibility_payload_check
    CHECK (
        operation <> 'platform_operation_eligibility'
        OR (
            payload ? 'eligibility_operation'
            AND payload ? 'eligibility_product'
            AND payload ->> 'eligibility_operation' IN (
                'deposit', 'withdrawal', 'wagering', 'settlement', 'conversion', 'reporting'
            )
            AND NULLIF(payload ->> 'eligibility_product', '') IS NOT NULL
        )
    );

COMMENT ON COLUMN asset_change_requests.operation IS 'Which dual-controlled operation this request authorizes. create/activate/platform_authorize are layers 1-3 (ADR 0037 §C.5.3); platform_operation_eligibility is the layer-7 platform-wide-default GRANT (§C.5.1 op 9), added by migration 0047 after review found it had no four-eyes representation at all. Revocations are never filed here - turning something off is deliberately single-actor.';

-- A payload-matched consume. The 2-argument form is kept (it is what
-- assets_enforce_dual_control calls) and now delegates, so there remains
-- exactly ONE place that marks a request applied - which is what makes
-- "one approval authorizes one mutation, once" true rather than
-- duplicated.
CREATE OR REPLACE FUNCTION asset_change_consume_approved_request(
    p_asset_code TEXT, p_operation TEXT, p_payload_match JSONB
) RETURNS UUID AS $$
DECLARE
    v_request_id UUID;
BEGIN
    SELECT r.id INTO v_request_id
      FROM asset_change_requests r
     WHERE r.asset_code = p_asset_code
       AND r.operation = p_operation
       AND r.state = 'pending'
       -- JSONB containment: every key/value in p_payload_match must be
       -- present in the approved payload. '{}' contains-matches
       -- everything, so the 2-arg delegation below behaves exactly as
       -- migration 0044's original did.
       AND r.payload @> p_payload_match
       AND EXISTS (
           SELECT 1
             FROM asset_change_approvals a
            WHERE a.request_id = r.id
              AND a.decision = 'approve'
              AND a.approver_principal_id <> r.requested_by_principal_id
       )
       AND NOT EXISTS (
           SELECT 1 FROM asset_change_approvals a
            WHERE a.request_id = r.id AND a.decision = 'reject'
       )
     ORDER BY r.requested_at
     FOR UPDATE
     LIMIT 1;

    IF v_request_id IS NULL THEN
        RAISE EXCEPTION 'assets: % of asset % requires a pending asset_change_requests row (matching %) approved by a DIFFERENT platform principal (four-eyes, ADR 0037 C.5.3)', p_operation, p_asset_code, p_payload_match;
    END IF;

    UPDATE asset_change_requests
       SET state = 'applied',
           applied_at = now(),
           applied_by_principal_id = NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid
     WHERE id = v_request_id;

    RETURN v_request_id;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION asset_change_consume_approved_request(
    p_asset_code TEXT, p_operation TEXT
) RETURNS UUID AS $$
BEGIN
    RETURN asset_change_consume_approved_request(p_asset_code, p_operation, '{}'::jsonb);
END;
$$ LANGUAGE plpgsql;

-- Dual control on the platform-wide GRANT, in its own AFTER trigger.
--
-- WHY AFTER, AND WHY NOT INSIDE THE EXISTING BEFORE TRIGGER - this is
-- load-bearing, not a style choice. Layer-7 rows are written with
-- INSERT ... ON CONFLICT DO UPDATE (the partial unique indexes are what
-- guarantee one fact per (scope, asset, operation, product) under
-- concurrency). PostgreSQL fires the BEFORE INSERT trigger FIRST, and
-- only then detects the conflict and runs the UPDATE path, firing
-- BEFORE UPDATE as well - so a single upsert fires the BEFORE trigger
-- TWICE, and side effects of the first firing are not undone. Consuming
-- an approval there would either demand two approvals for one logical
-- grant, or consume one and then fail. An AFTER INSERT OR UPDATE row
-- trigger fires exactly ONCE for the row that actually survives the
-- statement, with the true OLD for the conflict case - which is the only
-- place this decision can be made correctly. A RAISE here still aborts
-- the whole statement, so the control is no weaker for being AFTER.
--
-- Migration 0045's asset_operation_eligibility_enforce_narrowing() is
-- deliberately left untouched: it is pure validation with no side
-- effects, so running it twice on an upsert is harmless.
CREATE FUNCTION asset_operation_eligibility_enforce_dual_control() RETURNS TRIGGER AS $$
DECLARE
    v_needs_dual_control BOOLEAN;
BEGIN
    -- Tenant-scoped rows are not dual-controlled (they can only narrow),
    -- and a revocation is never dual-controlled (fail-closed direction,
    -- ADR 0037 §C.5.3's asymmetry).
    IF NEW.tenant_id IS NOT NULL OR NOT NEW.eligible THEN
        RETURN NULL;
    END IF;

    -- Required whenever this statement actually PRODUCES a platform-wide
    -- grant that did not exist before. An idempotent re-write of an
    -- already-eligible row needs no new approval (mirroring
    -- assets_enforce_dual_control's off->on-transition rule), but a
    -- statement that re-points the row at a different asset/operation/
    -- product, or promotes a tenant row to a platform row, does -
    -- otherwise one approval could be laundered into a grant nobody
    -- approved.
    IF TG_OP = 'INSERT' THEN
        v_needs_dual_control := true;
    ELSE
        v_needs_dual_control := NOT OLD.eligible
            OR OLD.tenant_id IS NOT NULL
            OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
            OR NEW.operation IS DISTINCT FROM OLD.operation
            OR NEW.product IS DISTINCT FROM OLD.product;
    END IF;

    IF v_needs_dual_control THEN
        PERFORM asset_change_consume_approved_request(
            NEW.asset_code,
            'platform_operation_eligibility',
            jsonb_build_object(
                'eligibility_operation', NEW.operation,
                'eligibility_product', COALESCE(NEW.product, '*')
            )
        );
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER asset_operation_eligibility_dual_control
    AFTER INSERT OR UPDATE ON asset_operation_eligibility
    FOR EACH ROW EXECUTE FUNCTION asset_operation_eligibility_enforce_dual_control();

-- ----------------------------------------------------------------------
-- FIX 3: asset_authorizations - split the FOR ALL policy per command, so
-- DELETE genuinely has no policy
-- ----------------------------------------------------------------------
-- Same predicate as 0045's tenant_isolation, split across SELECT/INSERT/
-- UPDATE and pointedly absent for DELETE. The existing player_read
-- SELECT policy is untouched (policies for one command are OR-ed, so
-- read behaviour is byte-for-byte what it was).
--
-- With FORCE ROW LEVEL SECURITY and no DELETE policy, a DELETE from any
-- application connection - tenant-scoped, platform-admin-scoped or
-- player-scoped - sees zero rows and therefore removes nothing. Revoking
-- an authorization is `eligible = false`, which leaves the fact, its
-- reason_code and its actor visible.
--
-- What deliberately still works: the tenant_id ... ON DELETE CASCADE that
-- 0045 declared. PostgreSQL runs referential-integrity actions with RLS
-- bypassed, so deleting a tenant still removes that tenant's
-- authorization rows. That is the schema's own declared intent and this
-- fix is not allowed to silently change it - which is also why no
-- BEFORE DELETE deny-trigger is added here (one would contradict the
-- cascade, unlike `assets`, which declares no cascade and does carry
-- such a trigger).

DROP POLICY IF EXISTS tenant_isolation ON asset_authorizations;

CREATE POLICY tenant_isolation_read ON asset_authorizations
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE POLICY tenant_isolation_insert ON asset_authorizations
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE POLICY tenant_isolation_update ON asset_authorizations
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

-- TRUNCATE is the same erasure in bulk, and RLS does not apply to it at
-- all (it is an owner/privilege-level operation, and the application role
-- owns these tables). Migration 0044 already carries exactly this
-- statement-level guard for `assets`, `asset_change_requests` and
-- `asset_change_approvals`; 0045's two tables were left without one.
-- Closing that here is the same finding at statement scope, not a new
-- control: without it, one TRUNCATE erases every authorization and
-- eligibility row on the platform and every absent-row inherit/deny turns
-- into an inherit.
CREATE FUNCTION asset_authorization_deny_truncate() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '%: TRUNCATE is not permitted - authorization configuration is revoked with eligible = false, never erased (removing a denial row silently widens eligibility)', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER asset_authorizations_no_truncate
    BEFORE TRUNCATE ON asset_authorizations
    FOR EACH STATEMENT EXECUTE FUNCTION asset_authorization_deny_truncate();

CREATE TRIGGER asset_operation_eligibility_no_truncate
    BEFORE TRUNCATE ON asset_operation_eligibility
    FOR EACH STATEMENT EXECUTE FUNCTION asset_authorization_deny_truncate();
