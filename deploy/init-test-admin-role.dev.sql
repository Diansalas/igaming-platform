-- DEV/CI ONLY - never run against staging or production.
--
-- Stage 10 W0 (docs/testing/testing-strategy.md "Scratch databases"):
-- igaming_test_admin exists only so internal/testsupport/scratchdb can
-- CREATE and DROP throwaway databases for migration and RLS integration
-- tests. It is deliberately NOT part of deploy/init-app-role.sql (the
-- deployment path), and it never replaces the application roles:
--
--   * CREATEDB, but NOSUPERUSER and NOBYPASSRLS.
--   * Member of igaming, so it can create databases OWNER igaming and drop
--     them again. Every test assertion still connects as igaming, which
--     stays NOCREATEDB (docs/security/runtime-role-separation.md §2).
--
-- The password is a local development placeholder of the same kind as
-- deploy/init-app-role.sql's; it is not a secret. Safe to re-run.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_test_admin') THEN
        CREATE ROLE igaming_test_admin
            LOGIN
            PASSWORD 'igaming_test_admin_dev_password'
            NOSUPERUSER
            CREATEDB
            NOCREATEROLE
            NOBYPASSRLS;
    END IF;
END
$$;

GRANT igaming TO igaming_test_admin;
