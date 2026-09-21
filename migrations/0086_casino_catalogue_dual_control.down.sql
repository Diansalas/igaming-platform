-- Reverses migration 0086. Clean inverse: drops the casino_games
-- dual-control trigger/function, the consume function, both new tables'
-- triggers/functions/policies, disables RLS (NO FORCE first, mirroring
-- migration 0044's own down migration convention), then drops the tables
-- (approvals before requests, due to the FK).

DROP TRIGGER IF EXISTS casino_games_dual_control ON casino_games;
DROP FUNCTION IF EXISTS casino_games_enforce_dual_control();
DROP FUNCTION IF EXISTS casino_catalogue_change_consume_approved_request(UUID, TEXT);

DROP TRIGGER IF EXISTS casino_catalogue_change_approvals_deny_self_approval ON casino_catalogue_change_approvals;
DROP FUNCTION IF EXISTS casino_catalogue_change_approvals_deny_self_approval();
DROP TRIGGER IF EXISTS casino_catalogue_change_approvals_no_truncate ON casino_catalogue_change_approvals;
DROP TRIGGER IF EXISTS casino_catalogue_change_approvals_immutable ON casino_catalogue_change_approvals;

DROP TRIGGER IF EXISTS casino_catalogue_change_requests_platform_principal ON casino_catalogue_change_requests;
DROP FUNCTION IF EXISTS casino_catalogue_change_requests_require_platform_principal();
DROP TRIGGER IF EXISTS casino_catalogue_change_requests_no_truncate ON casino_catalogue_change_requests;
DROP TRIGGER IF EXISTS casino_catalogue_change_requests_deny_delete ON casino_catalogue_change_requests;
DROP TRIGGER IF EXISTS casino_catalogue_change_requests_immutable ON casino_catalogue_change_requests;
DROP FUNCTION IF EXISTS casino_catalogue_change_requests_enforce_immutability();

DROP POLICY IF EXISTS platform_admin_scope ON casino_catalogue_change_approvals;
ALTER TABLE casino_catalogue_change_approvals NO FORCE ROW LEVEL SECURITY;
ALTER TABLE casino_catalogue_change_approvals DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS platform_admin_scope ON casino_catalogue_change_requests;
ALTER TABLE casino_catalogue_change_requests NO FORCE ROW LEVEL SECURITY;
ALTER TABLE casino_catalogue_change_requests DISABLE ROW LEVEL SECURITY;

DROP TABLE IF EXISTS casino_catalogue_change_approvals;
DROP TABLE IF EXISTS casino_catalogue_change_requests;
