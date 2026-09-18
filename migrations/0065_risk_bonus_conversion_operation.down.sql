-- One-way door once any risk_rules row uses operation='bonus_conversion'
-- - identical documented precedent to migration 0041's own
-- staff_users_role_check down-migration note: re-adding the narrower
-- CHECK constraint fails outright (rather than silently corrupting data)
-- if such a row exists, since the constraint is validated against all
-- existing rows immediately. Delete or re-scope any bonus_conversion
-- risk_rules row before running this down migration on an environment
-- where one was ever created.
ALTER TABLE risk_rules DROP CONSTRAINT risk_rules_operation_check;
ALTER TABLE risk_rules ADD CONSTRAINT risk_rules_operation_check
    CHECK (operation IN ('casino_launch', 'casino_bet', 'deposit', 'withdrawal', 'sportsbook_bet', 'bonus_grant'));
