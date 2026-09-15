DROP TRIGGER IF EXISTS risk_rules_no_truncate ON risk_rules;
DROP TRIGGER IF EXISTS risk_rules_deny_delete ON risk_rules;
DROP TRIGGER IF EXISTS risk_rules_immutable_core ON risk_rules;
DROP FUNCTION IF EXISTS risk_rules_enforce_immutability();
DROP TABLE IF EXISTS risk_rules;

-- One-way door once any staff_users row uses role='risk_manager' -
-- identical documented precedent to migration 0039's own
-- identity_review_required down-migration note: re-adding the narrower
-- CHECK constraint fails outright (rather than silently corrupting data)
-- if such a row exists, since the constraint is validated against all
-- existing rows immediately. Delete or re-role any risk_manager staff
-- account before running this down migration on an environment where
-- one was ever created.
ALTER TABLE staff_users DROP CONSTRAINT staff_users_role_check;
ALTER TABLE staff_users ADD CONSTRAINT staff_users_role_check
    CHECK (role IN ('platform_admin', 'tenant_admin', 'support', 'compliance', 'finance'));
