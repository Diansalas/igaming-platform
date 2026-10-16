-- staging-runner backup-restore-drill, phase "load" identity (plan 8.2). Run as the master by modes/backup-restore-drill.sh.
--
-- restore.sh --phase load runs the archive, so it refuses any identity that is a superuser, CREATEROLE, BYPASSRLS, REPLICATION or a member
-- of a pg_* / rds_* role, and requires it to be the owner role or a member of it. This creates that identity: a NOSUPERUSER login that is a
-- member of the owner role (-v owner_role), with a client-computed SCRAM verifier (RN_LOADER_VERIFIER; the clear password never reaches the
-- server or any statement text) and a short validity. DISPOSABLE: dropped at the end of the drill.
--
-- UNVERIFIED ON RDS: the master (and therefore the owner role `igaming`) is a member of rds_superuser, so a role created IN ROLE igaming is,
-- transitively, a member of rds_superuser too, and restore.sh will refuse it. That outcome is the expected BLOCKED finding of plan 8.2
-- ("if RDS cannot provide one this step is BLOCKED and the finding goes back to the architect"); the drill records the exact refusal.
\set ON_ERROR_STOP on
\getenv loader_verifier RN_LOADER_VERIFIER

DROP ROLE IF EXISTS igaming_restore_loader;
CREATE ROLE igaming_restore_loader LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 2
    IN ROLE :"owner_role" PASSWORD :'loader_verifier' VALID UNTIL :'valid_until';
