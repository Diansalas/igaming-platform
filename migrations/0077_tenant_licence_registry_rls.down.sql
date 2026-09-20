-- Reverses migration 0077. Unlike migration 0075's down migration
-- (which refuses to roll back while jurisdiction_precedence_configs holds
-- rows, because that table is a Stage 4I addition that may hold real
-- policy-authoring history with no prior existence to "restore"),
-- `tenants`, `licences` and `jurisdictions` are core, pre-existing tables
-- that have held rows since migration 0001/0002 and will always continue
-- to. There is no "empty table" precondition to protect and no history
-- introduced by this migration to lose - migration 0044's down migration
-- (assets, a similarly always-populated core table) is the correct
-- precedent, not 0075's. Restores the exact pre-migration posture:
-- relrowsecurity=false, relforcerowsecurity=false, zero policies.

DROP TRIGGER IF EXISTS jurisdictions_deny_truncate ON jurisdictions;
DROP FUNCTION IF EXISTS jurisdictions_deny_truncate();

DROP TRIGGER IF EXISTS licences_deny_truncate ON licences;
DROP FUNCTION IF EXISTS licences_deny_truncate();

DROP TRIGGER IF EXISTS tenants_deny_truncate ON tenants;
DROP FUNCTION IF EXISTS tenants_deny_truncate();

DROP INDEX IF EXISTS uq_tenants_exclusive_own_licence;

DROP POLICY IF EXISTS jurisdictions_platform_admin_update ON jurisdictions;
DROP POLICY IF EXISTS jurisdictions_platform_admin_insert ON jurisdictions;
DROP POLICY IF EXISTS jurisdictions_read ON jurisdictions;
ALTER TABLE jurisdictions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE jurisdictions DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS licences_platform_admin_update ON licences;
DROP POLICY IF EXISTS licences_platform_admin_insert ON licences;
DROP POLICY IF EXISTS licences_read ON licences;
ALTER TABLE licences NO FORCE ROW LEVEL SECURITY;
ALTER TABLE licences DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenants_platform_admin_delete ON tenants;
DROP POLICY IF EXISTS tenants_platform_admin_update ON tenants;
DROP POLICY IF EXISTS tenants_platform_admin_insert ON tenants;
DROP POLICY IF EXISTS tenants_read ON tenants;
ALTER TABLE tenants NO FORCE ROW LEVEL SECURITY;
ALTER TABLE tenants DISABLE ROW LEVEL SECURITY;
