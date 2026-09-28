-- ADR 0104 §3: the down migration refuses while any subject row exists,
-- or while any staff_users.display_name is non-NULL - both are considered
-- audit-adjacent historical state that must not be silently discarded.
--
-- G1-C1 (security review, 2026-09-28; orchestrator choice (a)): staff_users
-- is FORCE ROW LEVEL SECURITY with the dual_scope_isolation policy
-- (migration 0011) - a connection with app.tenant_id UNSET only ever sees
-- PLATFORM staff (tenant_id IS NULL) rows. A single EXISTS check run with
-- no GUC set would therefore be blind to every TENANT staff member's
-- display_name and let the column be dropped while real tenant data still
-- existed, silently. The check below first covers platform staff (no GUC
-- set - the platform arm of dual_scope_isolation), then loops over every
-- tenant, setting app.tenant_id for the duration of that tenant's own
-- check so dual_scope_isolation's tenant arm admits its rows - exactly
-- how any other genuinely tenant-scoped read in this codebase is done,
-- just repeated per tenant since this check needs ALL tenants, not one.
-- `tenants` itself carries no tenant-id-scoped RLS restriction on SELECT
-- (`tenants_read`'s only condition is app.player_account_id IS NULL), so
-- it can be read here with no GUC set.
--
-- The audit_log check is unaffected by this issue: a subject row's own
-- CHECK constraint (audit_log_subject_tenant_platform_only) forces
-- tenant_id IS NULL on any row with subject_tenant_id set, so the
-- platform arm of audit_log's OWN dual_scope_isolation policy (also
-- keyed on app.tenant_id being unset) already sees every such row with no
-- GUC set - there is no analogous per-tenant gap there.
DO $$
DECLARE
    v_ids UUID[];
    v_id  UUID;
BEGIN
    IF EXISTS (SELECT 1 FROM audit_log WHERE subject_tenant_id IS NOT NULL) THEN
        RAISE EXCEPTION 'migration 0109 down: refusing - audit_log has rows with subject_tenant_id set';
    END IF;

    -- Platform staff (tenant_id IS NULL): visible with no GUC set.
    IF EXISTS (SELECT 1 FROM staff_users WHERE display_name IS NOT NULL) THEN
        RAISE EXCEPTION 'migration 0109 down: refusing - staff_users has rows with display_name set';
    END IF;

    -- R-1 (security re-review, 2026-09-28): snapshot every tenant id
    -- FIRST, into a plain array, rather than a `FOR ... IN SELECT`
    -- cursor loop that would lazily re-evaluate `tenants` (and re-apply
    -- its own RLS under whatever app.tenant_id this loop has ALREADY set
    -- by a later iteration) each time it fetches the next row. This
    -- works correctly either way ONLY because `tenants_read`'s own
    -- policy (migration 0077) admits every non-player session
    -- regardless of app.tenant_id - snapshotting first removes that
    -- dependency from this loop's correctness entirely, so it keeps
    -- working even if `tenants_read` ever tightens.
    SELECT array_agg(id) INTO v_ids FROM tenants;

    -- Every tenant's own staff: one pass per tenant, with app.tenant_id
    -- set to make dual_scope_isolation admit that tenant's rows.
    FOREACH v_id IN ARRAY COALESCE(v_ids, ARRAY[]::UUID[]) LOOP
        PERFORM set_config('app.tenant_id', v_id::text, true);
        IF EXISTS (SELECT 1 FROM staff_users WHERE display_name IS NOT NULL) THEN
            PERFORM set_config('app.tenant_id', '', true);
            RAISE EXCEPTION 'migration 0109 down: refusing - staff_users has rows with display_name set';
        END IF;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
END $$;

DROP TRIGGER IF EXISTS audit_log_subject_actor_guard ON audit_log;
DROP FUNCTION IF EXISTS audit_log_subject_actor_guard();
DROP POLICY IF EXISTS subject_tenant_read ON audit_log;
DROP INDEX IF EXISTS idx_audit_log_subject_tenant_time;
ALTER TABLE audit_log DROP CONSTRAINT IF EXISTS audit_log_subject_tenant_platform_only;
ALTER TABLE audit_log DROP COLUMN IF EXISTS subject_tenant_id;
ALTER TABLE staff_users DROP COLUMN IF EXISTS display_name;
