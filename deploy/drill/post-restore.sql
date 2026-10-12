-- Post-restore re-application of the DATABASE-LEVEL privileges that neither pg_dump (without
-- --create) nor CREATE DATABASE ... TEMPLATE carries across. Mirrors deploy/init-app-role.sql and
-- migration 0116 (docs/security/runtime-role-separation.md, "TEMPORARY privilege": "A restore or
-- recreate loses the ACL and `migrate up` will not re-apply 0116").
--
-- Run as the DATABASE OWNER (the migration role) connected to the RESTORED database. The database name
-- is never hard-coded. Idempotent. It creates no role and changes no role attribute or password:
-- it only GRANTs/REVOKEs privileges ON the restored database, which the owner is entitled to do.
\set ON_ERROR_STOP on
DO $$
DECLARE
    db text := current_database();
BEGIN
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO igaming_runtime', db);
    EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC', db);
    EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM igaming_runtime', db);
    -- schema USAGE is a schema ACL (carried by a dump, repeated here so the step is self-sufficient)
    EXECUTE 'GRANT USAGE ON SCHEMA public TO igaming_runtime';

    IF has_database_privilege('igaming_runtime', db, 'TEMPORARY') THEN
        RAISE EXCEPTION 'post-restore: igaming_runtime still holds TEMPORARY on %', db;
    END IF;
END
$$;
