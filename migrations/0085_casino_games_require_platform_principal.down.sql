-- Clean inverse of 0085_casino_games_require_platform_principal.up.sql:
-- drop the trigger, then the function (reverse order of creation).

DROP TRIGGER IF EXISTS casino_games_platform_principal ON casino_games;

DROP FUNCTION IF EXISTS casino_games_require_platform_principal();
