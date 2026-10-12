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
-- 6. money damage (the immutability trigger was dropped in step 3, which is what makes these writes possible):
--    one credit entry grows by 7 (SUM(debits) != SUM(credits), and the projection no longer equals the
--    recompute), and one projection row disappears (a row-count difference against the source).
BEGIN;
SELECT set_config('app.tenant_id', '00000000-0000-0000-0000-0000000000a1', true);
UPDATE ledger_entries SET amount = amount + 7
 WHERE id = (SELECT id FROM ledger_entries WHERE direction = 'credit' ORDER BY created_at, id LIMIT 1);
DELETE FROM wallet_balance_projection
 WHERE ledger_account_id = (SELECT ledger_account_id FROM wallet_balance_projection
                             WHERE account_type = 'player_cash' ORDER BY ledger_account_id DESC LIMIT 1);
COMMIT;
