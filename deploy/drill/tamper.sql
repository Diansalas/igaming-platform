-- NEGATIVE CONTROL (drill-only): deliberately damage the RESTORED scratch database so the driver can
-- prove verify.sql / `migrate verify` actually FAIL on the defects they claim to catch (a verifier
-- that cannot fail proves nothing). Only ever run against the drill's own scratch copy (the driver
-- passes its restored database). Never touches ledger rows (they are append-only by trigger).
\set ON_ERROR_STOP on
-- 1. weaken RLS on a tenant table
ALTER TABLE wallets NO FORCE ROW LEVEL SECURITY;
-- 2. re-open the TEMP shadowing hole (migration 0116)
DO $$ BEGIN EXECUTE format('GRANT TEMPORARY ON DATABASE %I TO PUBLIC', current_database()); END $$;
-- 3. drop a ledger guard trigger
DROP TRIGGER ledger_entries_immutable ON ledger_entries;
-- 4. corrupt a recorded migration checksum
UPDATE schema_migrations SET checksum = 'tampered-by-drill-negative-control' WHERE version = (SELECT min(version) FROM schema_migrations);
-- 5. over-grant the runtime role
GRANT TRUNCATE ON ledger_entries TO igaming_runtime;
