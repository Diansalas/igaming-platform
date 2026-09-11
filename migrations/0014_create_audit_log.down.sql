DROP TRIGGER IF EXISTS audit_log_immutable ON audit_log;
DROP FUNCTION IF EXISTS audit_log_deny_mutation();
DROP TABLE IF EXISTS audit_log;
