-- Down for 0112 (ADR 0099 §10.7). Refuses (CG099) while any row exists in
-- 0112's own request/approval/grant tables - a capability grant is a
-- security-relevant fact and must never be silently dropped by a schema
-- rollback. Otherwise: drop the triggers/policies added on EXISTING
-- tables first (restoring their pre-0112 effective policy set exactly,
-- the A-17 baseline), then the new tables, then the functions.
--
-- All three tables carry FORCE ROW LEVEL SECURITY, which - per this
-- repository's own established precedent (migration 0110's down file,
-- same reasoning, itself citing migration 0100's) - means even the
-- migration role (owner, never granted BYPASSRLS per
-- deploy/init-app-role.sql) is subject to every policy. A migration-time
-- guard with no app.tenant_id/app.principal_id/app.platform_admin_
-- principal_id GUC set would therefore see ZERO rows from every one of
-- these tables' T/P/A policies regardless of what they actually hold,
-- making the guard below a silent no-op. DISABLE ROW LEVEL SECURITY for
-- the duration of this transactional DDL is safe for the identical
-- reason: if the guard raises, the whole transaction (including these
-- ALTERs) rolls back and FORCE RLS is restored exactly as it was; if the
-- guard does not fire, every one of these tables is dropped immediately
-- afterward anyway, so its RLS posture stops mattering.
ALTER TABLE staff_capability_grant_requests DISABLE ROW LEVEL SECURITY;
ALTER TABLE staff_capability_grant_approvals DISABLE ROW LEVEL SECURITY;
ALTER TABLE staff_capability_grants DISABLE ROW LEVEL SECURITY;

DO $$
DECLARE
    v_count BIGINT;
BEGIN
    SELECT count(*) INTO v_count FROM staff_capability_grant_requests;
    IF v_count > 0 THEN
        RAISE EXCEPTION 'migration 0112 down: refusing - % row(s) exist in staff_capability_grant_requests', v_count USING ERRCODE = 'CG099';
    END IF;
    SELECT count(*) INTO v_count FROM staff_capability_grant_approvals;
    IF v_count > 0 THEN
        RAISE EXCEPTION 'migration 0112 down: refusing - % row(s) exist in staff_capability_grant_approvals', v_count USING ERRCODE = 'CG099';
    END IF;
    SELECT count(*) INTO v_count FROM staff_capability_grants;
    IF v_count > 0 THEN
        RAISE EXCEPTION 'migration 0112 down: refusing - % row(s) exist in staff_capability_grants', v_count USING ERRCODE = 'CG099';
    END IF;
END $$;

-- --- existing-table triggers/policies added by 0112, in reverse order ---

DROP TRIGGER IF EXISTS audit_log_acting_actor ON audit_log;
DROP FUNCTION IF EXISTS audit_log_acting_actor();

DROP POLICY IF EXISTS acting_insert ON audit_log;
DROP POLICY IF EXISTS acting_fence_delete ON audit_log;
DROP POLICY IF EXISTS acting_fence_update ON audit_log;
DROP POLICY IF EXISTS acting_fence_insert ON audit_log;
DROP POLICY IF EXISTS acting_fence_select ON audit_log;

DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['sessions', 'login_attempts', 'persons', 'player_restrictions', 'risk_rules']
    LOOP
        EXECUTE format('DROP POLICY IF EXISTS acting_fence_delete ON %I', t);
        EXECUTE format('DROP POLICY IF EXISTS acting_fence_update ON %I', t);
        EXECUTE format('DROP POLICY IF EXISTS acting_fence_insert ON %I', t);
        EXECUTE format('DROP POLICY IF EXISTS acting_fence_select ON %I', t);
    END LOOP;
END $$;

DROP POLICY IF EXISTS acting_lock ON staff_users;
DROP POLICY IF EXISTS acting_read ON staff_users;
DROP POLICY IF EXISTS acting_fence_delete ON staff_users;
DROP POLICY IF EXISTS acting_fence_update ON staff_users;
DROP POLICY IF EXISTS acting_fence_insert ON staff_users;
DROP POLICY IF EXISTS acting_fence_select ON staff_users;

-- --- new tables, in reverse dependency order ---

DROP TABLE IF EXISTS staff_capability_grants;
DROP FUNCTION IF EXISTS staff_capability_grants_guard();

DROP TABLE IF EXISTS staff_capability_grant_approvals;
DROP FUNCTION IF EXISTS staff_capability_grant_approvals_require_grant();
DROP FUNCTION IF EXISTS staff_capability_grant_approvals_apply_to_request();
DROP FUNCTION IF EXISTS staff_capability_grant_approvals_guard();

DROP TABLE IF EXISTS staff_capability_grant_requests;
DROP FUNCTION IF EXISTS staff_capability_grant_requests_guard();

DROP TABLE IF EXISTS financial_capability_settings;
DROP TABLE IF EXISTS financial_governance_permissions;
DROP TABLE IF EXISTS financial_capability_catalogue;

-- --- functions ---

DROP FUNCTION IF EXISTS financial_actor_session();
DROP FUNCTION IF EXISTS financial_acting_session_open();
DROP FUNCTION IF EXISTS financial_acting_session_valid();
DROP FUNCTION IF EXISTS staff_capability_grant_in_force(uuid, uuid, text, timestamptz);
DROP FUNCTION IF EXISTS financial_acting_gucs_exact();
DROP FUNCTION IF EXISTS financial_acting_gucs_present();
