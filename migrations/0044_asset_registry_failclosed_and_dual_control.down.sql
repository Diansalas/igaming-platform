-- Reverses migration 0044. Note what a down migration CANNOT undo: the
-- explicit active/platform_authorized values written to the seven seeded
-- rows are ordinary data, so restoring DEFAULT true is the only
-- meaningful reversal of the schema-level half (the seven rows keep
-- active = true, which is exactly what they had before 0044 ran).

DROP TRIGGER IF EXISTS assets_dual_control ON assets;
DROP FUNCTION IF EXISTS assets_enforce_dual_control();
DROP FUNCTION IF EXISTS asset_change_consume_approved_request(TEXT, TEXT);

DROP TRIGGER IF EXISTS assets_no_truncate ON assets;
DROP TRIGGER IF EXISTS assets_deny_delete ON assets;
DROP TRIGGER IF EXISTS assets_immutable_identity ON assets;
DROP FUNCTION IF EXISTS assets_enforce_immutable_identity();

DROP POLICY IF EXISTS assets_platform_admin_delete_visibility ON assets;
DROP POLICY IF EXISTS assets_platform_admin_update ON assets;
DROP POLICY IF EXISTS assets_platform_admin_insert ON assets;
DROP POLICY IF EXISTS assets_read ON assets;
ALTER TABLE assets NO FORCE ROW LEVEL SECURITY;
ALTER TABLE assets DISABLE ROW LEVEL SECURITY;

DROP TRIGGER IF EXISTS asset_change_approvals_deny_self_approval ON asset_change_approvals;
DROP FUNCTION IF EXISTS asset_change_approvals_deny_self_approval();
DROP TRIGGER IF EXISTS asset_change_approvals_no_truncate ON asset_change_approvals;
DROP TRIGGER IF EXISTS asset_change_approvals_immutable ON asset_change_approvals;

DROP TRIGGER IF EXISTS asset_change_requests_platform_principal ON asset_change_requests;
DROP FUNCTION IF EXISTS asset_change_requests_require_platform_principal();
DROP TRIGGER IF EXISTS asset_change_requests_no_truncate ON asset_change_requests;
DROP TRIGGER IF EXISTS asset_change_requests_deny_delete ON asset_change_requests;
DROP TRIGGER IF EXISTS asset_change_requests_immutable ON asset_change_requests;
DROP FUNCTION IF EXISTS asset_change_requests_enforce_immutability();

DROP TABLE IF EXISTS asset_change_approvals;
DROP TABLE IF EXISTS asset_change_requests;

ALTER TABLE assets DROP COLUMN IF EXISTS updated_at;
ALTER TABLE assets DROP COLUMN IF EXISTS platform_authorized;
ALTER TABLE assets ALTER COLUMN active SET DEFAULT true;
