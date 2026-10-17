-- The actor-proof key schema of a LOGICALLY RESTORED database (F1, migration 0166; owner decision 5; ADR 0110 section 6, ADR 0122 section 10).
--
-- backup.sh dumps with --exclude-schema=actorproof_private: the backup role has no privilege on that schema, and a logical dump must never
-- carry the signing secret. restore.sh therefore runs THIS reviewed DDL (as the owner role, in one transaction, after pg_restore, only when the
-- restored database is at migration >= 0166 and the archive carried nothing of the schema) to recreate the key table EMPTY. Its catalog must be
-- IDENTICAL to what migrations 0120 + 0166 produce: verify.sh compares the FP|actorproof_private_md5 baseline line (source vs restored) and an
-- integration test compares this file against a migrated database. Change it only together with a migration that changes the table.
--
-- SRF1-1: the search_path is pinned (an archive restored just before cannot shadow anything used here), and the file ENDS by asserting that the
-- schema, the table and its columns are owner-only. A global DEFAULT PRIVILEGES entry of the owner carried by the archive (pg_dump emits those,
-- and restore.sh keeps them) would otherwise grant the new schema/table to another role; the assertion fails the restore instead.
--
-- LOAD-BEARING (SRF1-3): RLS ENABLED with NO policy. A role reachable only through SET ROLE has no BYPASSRLS and reads 0 rows. Never add a policy.
--
-- No key is ever written here: the restored platform fails closed (AP002; a production platform-api refuses to start) until
-- deploy/backup/rekey-actor-proof.sh provisions a NEW key.
\set ON_ERROR_STOP on
SET search_path = pg_catalog;

CREATE SCHEMA actorproof_private;
REVOKE ALL ON SCHEMA actorproof_private FROM PUBLIC;

CREATE TABLE actorproof_private.actor_proof_keys (
    kid        TEXT PRIMARY KEY CHECK (kid ~ '^[A-Za-z0-9._-]{1,32}$'),
    secret     BYTEA NOT NULL CHECK (octet_length(secret) >= 32),
    status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'retired')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    retired_at TIMESTAMPTZ NULL,
    CHECK ((status = 'retired') = (retired_at IS NOT NULL))
);
REVOKE ALL ON actorproof_private.actor_proof_keys FROM PUBLIC;
ALTER TABLE actorproof_private.actor_proof_keys ENABLE ROW LEVEL SECURITY;

DO $$
DECLARE
    v_bad text;
BEGIN
    SELECT string_agg(x, ', ') INTO v_bad FROM (
        SELECT 'schema:' || CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END || ':' || a.privilege_type AS x
          FROM pg_namespace n, LATERAL aclexplode(COALESCE(n.nspacl, acldefault('n', n.nspowner))) a
         WHERE n.nspname = 'actorproof_private' AND a.grantee <> n.nspowner
        UNION ALL
        SELECT 'table:' || CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END || ':' || a.privilege_type
          FROM pg_class c, LATERAL aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) a
         WHERE c.oid = 'actorproof_private.actor_proof_keys'::regclass AND a.grantee <> c.relowner
        UNION ALL
        SELECT 'column:' || at.attname FROM pg_attribute at
         WHERE at.attrelid = 'actorproof_private.actor_proof_keys'::regclass AND at.attacl IS NOT NULL) q;
    IF v_bad IS NOT NULL THEN
        RAISE EXCEPTION 'actorproof-private-schema.sql: the recreated key schema is not owner-only (%): a DEFAULT PRIVILEGES entry carried by the archive? the restore is refused',
            v_bad USING ERRCODE = 'APM03';
    END IF;
END;
$$;
