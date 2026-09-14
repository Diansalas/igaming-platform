DROP TRIGGER IF EXISTS player_restrictions_no_truncate ON player_restrictions;
DROP TRIGGER IF EXISTS player_restrictions_immutable ON player_restrictions;
DROP FUNCTION IF EXISTS player_restrictions_deny_mutation();
DROP TABLE IF EXISTS player_restrictions;
