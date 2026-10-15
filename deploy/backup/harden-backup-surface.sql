-- Narrow what a STOLEN igaming_backup credential can abuse (A12 security review M1). Run once per target cluster/database by
-- the bootstrap superuser (it changes privileges on pg_catalog functions and on the maintenance databases); NOT applied to any
-- shared cluster by anything in this repository. The platform itself uses none of the revoked functions as a role other than
-- `igaming` (migration owner) and `igaming_runtime` (advisory locks in reconciliation and the status gates), which are granted
-- EXECUTE back explicitly; no other role needs them.
--
-- Why: an authenticated session of the backup role could (verified on the drill cluster) take session-level advisory locks that
-- stall migrations, withdrawals or reconciliation (the lock keys are deterministic), write unbounded WAL with
-- pg_logical_emit_message in a read-only transaction, create large objects, NOTIFY, and connect to the maintenance databases.
-- LOCK TABLE ... IN ACCESS SHARE MODE, LISTEN, ALTER ROLE ... PASSWORD/SET on itself and RESET of its own session defaults remain
-- possible for any login role and are RESIDUALS covered by detection (ADR 0122 section 9), and on RDS by IAM database
-- authentication. Idempotent. NOTE: a dump of a hardened database DOES carry these pg_catalog ACLs, but a non-superuser restore cannot apply them,
-- so the script must be re-run on every restored cluster (drilled). Feasibility on RDS (REVOKE on pg_catalog functions) is UNVERIFIED.
\set ON_ERROR_STOP on

DO $$
DECLARE
    f record;
    grantees text := (SELECT string_agg(quote_ident(rolname), ', ') FROM pg_roles WHERE rolname IN ('igaming', 'igaming_runtime'));
BEGIN
    FOR f IN
        SELECT p.oid::regprocedure AS sig
          FROM pg_proc p
         WHERE p.pronamespace = 'pg_catalog'::regnamespace
           -- PUBLIC can execute: either the default (NULL ACL) or an explicit PUBLIC entry (a non-superuser pg_restore of a dump that carries
           -- these ACLs cannot apply them but materialises the default ACL, so a NULL test alone would miss it); already-restricted functions stay as is
           AND (p.proacl IS NULL OR EXISTS (SELECT 1 FROM aclexplode(p.proacl) a WHERE a.grantee = 0 AND a.privilege_type = 'EXECUTE'))
           AND (p.proname LIKE 'pg\_advisory\_%' OR p.proname LIKE 'pg\_try\_advisory\_%'
                OR p.proname IN ('pg_logical_emit_message', 'pg_notify', 'lo_create', 'lo_creat', 'lo_from_bytea', 'lo_put', 'lo_open',
                                 'lo_unlink', 'lowrite', 'lo_truncate', 'lo_truncate64', 'lo_close', 'lo_lseek', 'lo_lseek64',
                                 'loread', 'lo_tell', 'lo_tell64', 'lo_get'))
    LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f.sig);
        IF grantees IS NOT NULL THEN
            EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO %s', f.sig, grantees);
        END IF;
    END LOOP;
END
$$;

-- A login role with no business there must not be able to connect to the maintenance databases (PUBLIC has CONNECT by default).
-- Operators who need them (restore, provisioning) are granted CONNECT explicitly by the environment's provisioning.
REVOKE CONNECT ON DATABASE postgres FROM PUBLIC;
REVOKE CONNECT ON DATABASE template1 FROM PUBLIC;
