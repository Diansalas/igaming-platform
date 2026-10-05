-- PRH-2 R2 (TRIGGER-SEARCH-PATH-1 / TRIGGER-SEARCH-PATH-1-UPGRADE; ADR 0108;
-- security review docs/plans/prh2-hardening-round/reviews/k3-delta-security.md
-- finding 1).
--
-- Invariant established: THE RUNTIME ROLE CANNOT CREATE TEMPORARY OBJECTS.
--
-- Root cause closed here: PostgreSQL grants TEMPORARY on every database to
-- PUBLIC by default, so igaming_runtime inherited it. A session may then put
-- pg_temp ahead of public in its search_path (or simply create a TEMP table
-- named like a real one - pg_temp is searched first for relations unless the
-- path says otherwise) and shadow every UNQUALIFIED table name used by a
-- function whose search_path is not pinned. The 0026..0113 guard/helper
-- functions are such functions; a TEMP shadow of staff_users /
-- staff_capability_grants / staff_capability_grant_requests forged a
-- two-person approval of a real compensating debit.
--
-- What this does: REVOKE TEMPORARY ON THE CURRENT DATABASE from PUBLIC and
-- from igaming_runtime (when that role exists). Revoking from the role alone
-- is a NO-OP while PUBLIC holds the privilege, which is why PUBLIC is revoked.
-- The database owner (the migration role) keeps TEMPORARY as the owner. The
-- database name is never hard-coded (current_database()).
--
-- This does NOT replace pinning search_path on the older functions
-- (TRIGGER-SEARCH-PATH-1 residual, defence in depth).
--
-- DEPLOYMENT: the role that runs migrations MUST own the database (or hold
-- TEMPORARY WITH GRANT OPTION). Otherwise REVOKE only raises a WARNING and
-- changes nothing; the end-state ASSERTION below then RAISES, so a silent
-- no-op is impossible. The same REVOKE is also in deploy/init-app-role.sql and
-- deploy/aws/sql/init-runtime-role.rds.sql so a freshly provisioned database
-- is safe before migrations run.

DO $$
DECLARE
    db text := current_database();
BEGIN
    EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC', db);

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM igaming_runtime', db);
    END IF;

    -- ASSERT the end state.
    IF EXISTS (
        SELECT 1
        FROM pg_database d,
             LATERAL aclexplode(COALESCE(d.datacl, acldefault('d', d.datdba))) a
        WHERE d.datname = db
          AND a.grantee = 0
          AND a.privilege_type = 'TEMPORARY'
    ) THEN
        RAISE EXCEPTION 'TEMP-REVOKE-1: PUBLIC still holds TEMPORARY on database % (the migration role must own the database)', db
            USING ERRCODE = 'insufficient_privilege';
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime')
       AND has_database_privilege('igaming_runtime', db, 'TEMPORARY') THEN
        RAISE EXCEPTION 'TEMP-REVOKE-1: igaming_runtime still holds TEMPORARY on database % (directly or through a role it is a member of)', db
            USING ERRCODE = 'insufficient_privilege';
    END IF;
END
$$;
