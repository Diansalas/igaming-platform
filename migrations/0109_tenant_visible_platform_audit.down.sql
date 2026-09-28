-- ADR 0104 §3: the down migration refuses while any subject row exists,
-- or while any staff_users.display_name is non-NULL - both are considered
-- audit-adjacent historical state that must not be silently discarded.
-- The existence checks run with app.tenant_id unset (a platform-scoped
-- connection), so they can see platform-scope audit_log rows regardless
-- of which tenant's subject_tenant_id they carry.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM audit_log WHERE subject_tenant_id IS NOT NULL) THEN
        RAISE EXCEPTION 'migration 0109 down: refusing - audit_log has rows with subject_tenant_id set';
    END IF;
    IF EXISTS (SELECT 1 FROM staff_users WHERE display_name IS NOT NULL) THEN
        RAISE EXCEPTION 'migration 0109 down: refusing - staff_users has rows with display_name set';
    END IF;
END $$;

DROP TRIGGER IF EXISTS audit_log_subject_actor_guard ON audit_log;
DROP FUNCTION IF EXISTS audit_log_subject_actor_guard();
DROP POLICY IF EXISTS subject_tenant_read ON audit_log;
DROP INDEX IF EXISTS idx_audit_log_subject_tenant_time;
ALTER TABLE audit_log DROP CONSTRAINT IF EXISTS audit_log_subject_tenant_platform_only;
ALTER TABLE audit_log DROP COLUMN IF EXISTS subject_tenant_id;
ALTER TABLE staff_users DROP COLUMN IF EXISTS display_name;
