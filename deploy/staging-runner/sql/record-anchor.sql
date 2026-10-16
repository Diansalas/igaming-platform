-- staging-runner mode record-anchor (STAGING-PREP-3, SRP3-1): record the write-once database_environment anchor (migration 0135) as the
-- TABLE OWNER. Variables: -v anchor=staging|development|test. Never 'production': recording production is the production provisioning
-- runbook's step, not this runner's.
--
-- One transaction, the table locked EXCLUSIVE so two runs cannot both see "no row". Idempotent: the same value already recorded is a no-op; a
-- different value is refused (the row can never change; the database trigger would refuse it too).
--
-- FRESHNESS (SRP3R-1): "no row" is the database's definition of PRODUCTION, so this script must not turn an unanchored database that holds data
-- into a seedable one. It INSERTS only when the database is demonstrably fresh: no tenant row and no player account / ledger entry statistics.
-- Method (RLS cannot hide it): `tenants` is readable by the owner without a tenant scope (policy tenants_read), and every tenant staff, player and
-- ledger row carries a tenant_id that references a tenant, so zero tenants means zero tenant staff, players and ledger entries; as a second signal
-- pg_stat_user_tables.n_live_tup of player_accounts / ledger_entries must be 0 (statistics, so it can lag a second behind; it only ever ADDS a
-- refusal). Platform admins (tenant_id NULL, e.g. from seed-admin) are allowed. The idempotent / conflict answers do not need freshness.
-- Prints ANCHOR|state|<state>:
--   inserted | already_recorded_same | conflict_other_value:<value> | refused_value | refused_live_data | no_table
\set ON_ERROR_STOP on

BEGIN;
SELECT CASE WHEN to_regclass('public.database_environment') IS NULL THEN 1 ELSE 0 END AS no_table \gset
\if :no_table
  ROLLBACK;
  SELECT 'ANCHOR|state|no_table';
  \q
\endif
SELECT CASE WHEN :'anchor' IN ('staging', 'development', 'test') THEN 0 ELSE 1 END AS bad_value \gset
\if :bad_value
  ROLLBACK;
  SELECT 'ANCHOR|state|refused_value';
  \q
\endif
SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '30s';
LOCK TABLE database_environment IN EXCLUSIVE MODE;
SELECT COALESCE((SELECT environment FROM database_environment LIMIT 1), '') AS current_anchor \gset
SELECT CASE WHEN :'current_anchor' = '' THEN 'none' WHEN :'current_anchor' = :'anchor' THEN 'same' ELSE 'other' END AS anchor_state \gset
SELECT (:'anchor_state' = 'none') AS do_insert \gset
SELECT ((SELECT count(*) FROM tenants) > 0
        OR COALESCE((SELECT sum(n_live_tup) FROM pg_stat_user_tables WHERE relname IN ('player_accounts', 'ledger_entries')), 0) > 0) AS has_live_data \gset
\if :do_insert
\if :has_live_data
  ROLLBACK;
  SELECT 'ANCHOR|state|refused_live_data';
  \q
\endif
  INSERT INTO database_environment (environment) VALUES (:'anchor');
  COMMIT;
  SELECT 'ANCHOR|state|inserted';
\else
  ROLLBACK;
  SELECT CASE WHEN :'anchor_state' = 'same' THEN 'ANCHOR|state|already_recorded_same'
              ELSE 'ANCHOR|state|conflict_other_value:' || :'current_anchor' END;
\endif
SELECT 'ANCHOR|row|' || environment || '|by=' || recorded_by FROM database_environment;
