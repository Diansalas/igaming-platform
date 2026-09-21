-- Clean inverse of 0088_sportsbook_exposure_limits.up.sql, reverse order
-- of creation.

DROP INDEX IF EXISTS idx_sportsbook_bets_open_exposure;

DROP TRIGGER IF EXISTS sb_exposure_limits_no_truncate ON sb_exposure_limits;
DROP TRIGGER IF EXISTS sb_exposure_limits_deny_delete ON sb_exposure_limits;
DROP TRIGGER IF EXISTS sb_exposure_limits_immutable_identity ON sb_exposure_limits;

DROP POLICY IF EXISTS tenant_staff_scope ON sb_exposure_limits;

ALTER TABLE sb_exposure_limits NO FORCE ROW LEVEL SECURITY;
ALTER TABLE sb_exposure_limits DISABLE ROW LEVEL SECURITY;

DROP TABLE IF EXISTS sb_exposure_limits;
