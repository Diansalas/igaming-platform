-- Runs once against a freshly initialized Postgres cluster, as the
-- bootstrap superuser the official postgres image creates from
-- POSTGRES_USER. That bootstrap role is ALWAYS a superuser (initdb
-- requires it) and superusers ALWAYS bypass row-level security, even on
-- a table with FORCE ROW LEVEL SECURITY - so it must never be the role
-- platform-api or its tests actually connect as. This script creates a
-- second, ordinary role for that purpose and hands it ownership of the
-- application database.
--
-- See docs/decisions/0002-multi-tenancy-isolation-strategy.md and the
-- Stage 1 specialist review that caught the original config connecting
-- as the superuser directly (docs/active-stage.md).

CREATE ROLE igaming
    LOGIN
    PASSWORD 'igaming_dev_password'
    NOSUPERUSER
    NOCREATEDB
    NOCREATEROLE
    NOBYPASSRLS;

ALTER DATABASE igaming_platform_dev OWNER TO igaming;
ALTER SCHEMA public OWNER TO igaming;
