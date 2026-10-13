-- Reverses 0151 (ADR 0119 S4b; DEV/STAGING ONLY, per the migrate-down policy). Everything returns to 0142 exactly
-- (whole-schema snapshot test: internal/httpserver/staff_stepup_migration_integration_test.go). Staff sessions whose
-- mfa_at was moved by a step-up keep their value and their v2 seal; the S4b code that verifies them must be rolled back
-- with this migration (a pre-S4b binary does not verify v2 seals and refuses such a session on refresh).
SET LOCAL lock_timeout = '5s';

DROP POLICY staff_actor_self_lock ON staff_users;
DROP POLICY staff_actor_self_select ON staff_users;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE UPDATE (mfa_at, assurance_kid, assurance_seal) ON sessions FROM igaming_runtime';
    END IF;
END
$$;
