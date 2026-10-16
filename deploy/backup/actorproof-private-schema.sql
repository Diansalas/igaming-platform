-- The actor-proof key schema of a LOGICALLY RESTORED database (F1, migration 0166; owner decision 5; ADR 0110 section 6, ADR 0122 section 10).
--
-- backup.sh dumps with --exclude-schema=actorproof_private: the backup role has no privilege on that schema, and a logical dump must never
-- carry the signing secret. restore.sh therefore runs THIS reviewed DDL (as the owner role, after pg_restore, only when the restored database
-- is at migration >= 0166 and the archive carried nothing of the schema) to recreate the key table EMPTY. Its catalog must be IDENTICAL to
-- what migrations 0120 + 0166 produce: verify.sh compares the FP|actorproof_private_md5 baseline line (source vs restored) and an integration
-- test compares this file against a migrated database. Change it only together with a migration that changes the table.
--
-- No key is ever written here: the restored platform fails closed (AP002; a production platform-api refuses to start) until
-- deploy/backup/rekey-actor-proof.sh provisions a NEW key.
\set ON_ERROR_STOP on

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
