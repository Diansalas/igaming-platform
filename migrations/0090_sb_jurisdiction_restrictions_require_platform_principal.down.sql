-- Clean inverse of
-- 0090_sb_jurisdiction_restrictions_require_platform_principal.up.sql:
-- drop the trigger, then the function (reverse order of creation) -
-- mirrors migration 0085's own down migration exactly.

DROP TRIGGER IF EXISTS sb_jurisdiction_restrictions_platform_principal ON sb_jurisdiction_restrictions;

DROP FUNCTION IF EXISTS sb_jurisdiction_restrictions_require_platform_principal();
