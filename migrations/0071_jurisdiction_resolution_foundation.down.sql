-- Reverses migration 0071.

DROP TABLE IF EXISTS jurisdiction_precedence_configs;

DROP TRIGGER IF EXISTS jurisdiction_resolution_active_updated_at ON jurisdiction_resolution_active;
DROP FUNCTION IF EXISTS jurisdiction_resolution_active_touch_updated_at();
DROP TABLE IF EXISTS jurisdiction_resolution_active;

DROP TRIGGER IF EXISTS jurisdiction_resolutions_deny_truncate ON jurisdiction_resolutions;
DROP TRIGGER IF EXISTS jurisdiction_resolutions_immutable ON jurisdiction_resolutions;
DROP FUNCTION IF EXISTS jurisdiction_resolutions_deny_mutation();
DROP TABLE IF EXISTS jurisdiction_resolutions;
