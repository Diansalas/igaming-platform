-- staging-runner mode record-anchor (STAGING-PREP-3, SRP3-1): record the write-once database_environment anchor (migration 0135) as the
-- TABLE OWNER. Variables: -v anchor=staging|development|test. Never 'production': recording production is the production provisioning
-- runbook's step, not this runner's.
--
-- One transaction, the table locked EXCLUSIVE so two runs cannot both see "no row". Idempotent: the same value already recorded is a no-op; a
-- different value is refused (the row can never change; the database trigger would refuse it too). Prints ANCHOR|state|<state>:
--   inserted | already_recorded_same | conflict_other_value:<value> | refused_value | no_table
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
SELECT :'anchor_state' = 'none' AS do_insert \gset
\if :do_insert
  INSERT INTO database_environment (environment) VALUES (:'anchor');
  COMMIT;
  SELECT 'ANCHOR|state|inserted';
\else
  ROLLBACK;
  SELECT CASE WHEN :'anchor_state' = 'same' THEN 'ANCHOR|state|already_recorded_same'
              ELSE 'ANCHOR|state|conflict_other_value:' || :'current_anchor' END;
\endif
SELECT 'ANCHOR|row|' || environment || '|by=' || recorded_by FROM database_environment;
