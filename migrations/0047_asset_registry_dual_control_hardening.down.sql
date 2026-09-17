-- Reverses migration 0047, restoring migrations 0044/0045's original
-- function bodies, CHECK constraint and policy shape verbatim.
--
-- What reversing this migration actually means, stated plainly rather
-- than left implicit: it reinstates three confirmed, exploited defects -
-- an inert four-eyes person check, an un-dual-controlled platform-wide
-- layer-7 grant, and a DELETE-permitting policy on asset_authorizations.
-- It exists because the migration chain must round-trip, not because
-- rolling back is a safe operational choice.
--
-- Rows created while 0047 was in force are NOT rewritten (ordinary data,
-- the same posture 0044's own down migration takes): a pending
-- asset_change_requests row whose operation is
-- 'platform_operation_eligibility' would violate the narrowed CHECK
-- constraint restored below, so those rows are cancelled first. Applied
-- ones are history and are left exactly as they are - the narrowed CHECK
-- is NOT VALID-free only because there are no legal applied rows of that
-- operation in a pre-0047 world to preserve.

-- ----------------------------------------------------------------------
-- FIX 3 reversal
-- ----------------------------------------------------------------------

DROP TRIGGER IF EXISTS asset_operation_eligibility_no_truncate ON asset_operation_eligibility;
DROP TRIGGER IF EXISTS asset_authorizations_no_truncate ON asset_authorizations;
DROP FUNCTION IF EXISTS asset_authorization_deny_truncate();

DROP POLICY IF EXISTS tenant_isolation_update ON asset_authorizations;
DROP POLICY IF EXISTS tenant_isolation_insert ON asset_authorizations;
DROP POLICY IF EXISTS tenant_isolation_read ON asset_authorizations;

CREATE POLICY tenant_isolation ON asset_authorizations
    FOR ALL
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

-- ----------------------------------------------------------------------
-- FIX 2 reversal
-- ----------------------------------------------------------------------

-- Migration 0045's asset_operation_eligibility_enforce_narrowing() was
-- never modified by 0047, so nothing to restore there. Only the added
-- dual-control trigger is removed.
DROP TRIGGER IF EXISTS asset_operation_eligibility_dual_control ON asset_operation_eligibility;
DROP FUNCTION IF EXISTS asset_operation_eligibility_enforce_dual_control();

-- Migration 0044's original 2-argument consume, restored as a standalone
-- implementation (it must not delegate to a 3-argument form that is about
-- to be dropped).
CREATE OR REPLACE FUNCTION asset_change_consume_approved_request(p_asset_code TEXT, p_operation TEXT) RETURNS UUID AS $$
DECLARE
    v_request_id UUID;
BEGIN
    SELECT r.id INTO v_request_id
      FROM asset_change_requests r
     WHERE r.asset_code = p_asset_code
       AND r.operation = p_operation
       AND r.state = 'pending'
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
        RAISE EXCEPTION 'assets: % of asset % requires a pending asset_change_requests row approved by a DIFFERENT platform principal (four-eyes, ADR 0037 C.5.3)', p_operation, p_asset_code;
    END IF;

    UPDATE asset_change_requests
       SET state = 'applied',
           applied_at = now(),
           applied_by_principal_id = NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid
     WHERE id = v_request_id;

    RETURN v_request_id;
END;
$$ LANGUAGE plpgsql;

DROP FUNCTION IF EXISTS asset_change_consume_approved_request(TEXT, TEXT, JSONB);

-- Cancel any request of the operation type that is about to become
-- illegal, so restoring the narrowed CHECK cannot fail (ADD CONSTRAINT
-- validates every existing row, and DDL is not subject to RLS, so a row
-- this migration cannot SEE would still block it). `state` is the one
-- column asset_change_requests_enforce_immutability() allows to change,
-- and 'cancelled' is one of its legal terminal values.
--
-- NO FORCE is needed for the UPDATE itself: asset_change_requests'
-- platform_admin_scope policy requires app.platform_admin_principal_id,
-- which the migration runner (db.Pool.WithoutTenant) does not set, so
-- without this the UPDATE would silently match zero rows. NO FORCE
-- restores the ordinary table-owner bypass for the duration of this
-- transaction only, and FORCE is re-established immediately.
ALTER TABLE asset_change_requests NO FORCE ROW LEVEL SECURITY;

UPDATE asset_change_requests
   SET state = 'cancelled'
 WHERE operation = 'platform_operation_eligibility'
   AND state = 'pending';

ALTER TABLE asset_change_requests FORCE ROW LEVEL SECURITY;

ALTER TABLE asset_change_requests
    DROP CONSTRAINT IF EXISTS asset_change_requests_eligibility_payload_check;

ALTER TABLE asset_change_requests
    DROP CONSTRAINT IF EXISTS asset_change_requests_operation_check;

-- NOT VALID, deliberately, and this is the one place where reversing
-- 0047 cannot be byte-for-byte faithful. The constraint is enforced on
-- every INSERT and UPDATE exactly as migration 0044 defined it, so the
-- pre-0047 rule is fully back in force for all new rows. What NOT VALID
-- skips is re-validating rows that already exist - and the only such rows
-- are APPLIED (historical) platform_operation_eligibility requests
-- created while 0047 was in force. Validating them is impossible without
-- either destroying audit history (asset_change_requests carries a
-- deny-delete trigger for exactly that reason) or rewriting an immutable
-- column to something untrue. Migration 0044's own down file already
-- states this class of limitation for the seven seeded asset rows; this
-- is the same honesty applied here rather than a silent data rewrite.
ALTER TABLE asset_change_requests
    ADD CONSTRAINT asset_change_requests_operation_check
    CHECK (operation IN ('create', 'activate', 'platform_authorize')) NOT VALID;

COMMENT ON COLUMN asset_change_requests.operation IS NULL;

-- ----------------------------------------------------------------------
-- FIX 1 reversal - migration 0044's original bodies, restored verbatim
-- ----------------------------------------------------------------------

CREATE OR REPLACE FUNCTION asset_change_requests_require_platform_principal() RETURNS TRIGGER AS $$
DECLARE
    v_is_platform_scoped BOOLEAN;
BEGIN
    SELECT su.tenant_id IS NULL INTO v_is_platform_scoped
      FROM staff_users su
     WHERE su.id = NEW.requested_by_principal_id;
    IF NOT FOUND OR NOT v_is_platform_scoped THEN
        RAISE EXCEPTION 'asset_change_requests: requesting principal % is not a platform-scoped staff principal (ADR 0037 C.1: layers 1-3 are never reachable by a tenant-scoped role)', NEW.requested_by_principal_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION asset_change_approvals_deny_self_approval() RETURNS TRIGGER AS $$
DECLARE
    v_requester_principal_id UUID;
    v_requester_person_id    UUID;
    v_approver_person_id     UUID;
    v_approver_is_platform   BOOLEAN;
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

    SELECT su.tenant_id IS NULL, su.person_id INTO v_approver_is_platform, v_approver_person_id
      FROM staff_users su
     WHERE su.id = NEW.approver_principal_id;
    IF NOT FOUND OR NOT v_approver_is_platform THEN
        RAISE EXCEPTION 'asset_change_approvals: approving principal % is not a platform-scoped staff principal', NEW.approver_principal_id;
    END IF;

    SELECT su.person_id INTO v_requester_person_id
      FROM staff_users su
     WHERE su.id = v_requester_principal_id;

    IF v_requester_person_id IS NOT NULL
        AND v_approver_person_id IS NOT NULL
        AND v_requester_person_id = v_approver_person_id
    THEN
        RAISE EXCEPTION 'asset_change_approvals: approver resolves to the same person as the requester (self-approval through a second staff account)';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
