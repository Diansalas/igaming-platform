-- EXPERIMENT (private scratch database): how do the parked reads scale with the size of aml_enforcement_decisions (HYPOTHESIS: they read every
-- 'withdrawal_payout' decision of the 7-day window, so their cost follows decision VOLUME, not the batch)? Inserts N synthetic decisions over the last 7 days
-- for the 10k persons of fn-10k, then times the reads; then adds a candidate index and times again. Run: ./run.sh explain fn-10k experiments/08_decisions_scale.sql
\set ON_ERROR_STOP on
\pset pager off
SELECT id AS tid FROM tenants WHERE status = 'active' ORDER BY created_at, slug LIMIT 1 \gset
SELECT set_config('app.tenant_id', :'tid', false) \gset
SELECT aml_screening_gate_overdue(:'tid', 5000) AS warm \gset
CREATE TEMP TABLE accs AS SELECT row_number() OVER (ORDER BY id) AS rn, id AS aid, person_id AS pid FROM player_accounts WHERE tenant_id = :'tid';
SELECT count(*) AS n_accounts FROM accs \gset
\echo '--- baseline decisions rows:'
SELECT count(*) FROM aml_enforcement_decisions;
\set rows 1000000
\echo '--- insert :rows decisions (70% parked-shaped: held, no hold id, not allowed)'
INSERT INTO aml_enforcement_decisions (tenant_id, player_account_id, person_id, operation, kyc_outcome, hold_outcome, public_outcome, allowed, correlation_id, decided_at)
SELECT :'tid', a.aid, a.pid, 'withdrawal_payout', 'passed', CASE WHEN g % 10 < 7 THEN 'held' ELSE 'none' END, CASE WHEN g % 10 < 7 THEN 'review' ELSE 'passed' END, g % 10 >= 7,
       gen_random_uuid(), now() - (random() * 7) * interval '1 day'
  FROM generate_series(1, :rows) g JOIN accs a ON a.rn = 1 + (g % :n_accounts);
VACUUM (ANALYZE) aml_enforcement_decisions;
SELECT pg_size_pretty(pg_total_relation_size('aml_enforcement_decisions')) AS decisions_size;
\timing on
\echo '--- 1M decisions, existing indexes: parked_summary (x2), parked_overdue 500 first batch (x2), parked_overdue 5000'
SELECT * FROM aml_screening_parked_summary(:'tid', clock_timestamp() - interval '7 days');
SELECT * FROM aml_screening_parked_summary(:'tid', clock_timestamp() - interval '7 days');
SELECT * FROM aml_screening_parked_overdue(:'tid', clock_timestamp() - interval '7 days', NULL, 500);
SELECT * FROM aml_screening_parked_overdue(:'tid', clock_timestamp() - interval '7 days', NULL, 500);
SELECT * FROM aml_screening_parked_overdue(:'tid', clock_timestamp() - interval '7 days', NULL, 5000);
\timing off
EXPLAIN (ANALYZE, BUFFERS) SELECT DISTINCT ON (d.person_id) d.person_id, d.hold_outcome, d.hold_id, d.kyc_outcome, d.allowed FROM aml_enforcement_decisions d
 WHERE d.tenant_id = :'tid' AND d.operation = 'withdrawal_payout' AND d.decided_at >= now() - interval '7 days' ORDER BY d.person_id, d.decided_at DESC, d.id DESC;
\echo '--- add 3M more (4M total)'
INSERT INTO aml_enforcement_decisions (tenant_id, player_account_id, person_id, operation, kyc_outcome, hold_outcome, public_outcome, allowed, correlation_id, decided_at)
SELECT :'tid', a.aid, a.pid, 'withdrawal_payout', 'passed', CASE WHEN g % 10 < 7 THEN 'held' ELSE 'none' END, CASE WHEN g % 10 < 7 THEN 'review' ELSE 'passed' END, g % 10 >= 7,
       gen_random_uuid(), now() - (random() * 7) * interval '1 day'
  FROM generate_series(1, 3000000) g JOIN accs a ON a.rn = 1 + (g % :n_accounts);
VACUUM (ANALYZE) aml_enforcement_decisions;
SELECT pg_size_pretty(pg_total_relation_size('aml_enforcement_decisions')) AS decisions_size;
\timing on
SELECT * FROM aml_screening_parked_summary(:'tid', clock_timestamp() - interval '7 days');
SELECT * FROM aml_screening_parked_summary(:'tid', clock_timestamp() - interval '7 days');
SELECT * FROM aml_screening_parked_overdue(:'tid', clock_timestamp() - interval '7 days', NULL, 500);
SELECT * FROM aml_screening_parked_overdue(:'tid', clock_timestamp() - interval '7 days', NULL, 500);
SELECT * FROM aml_screening_parked_overdue(:'tid', clock_timestamp() - interval '7 days', NULL, 5000);
\echo '--- candidate index (private DB only): (tenant_id, person_id, decided_at DESC) WHERE operation = withdrawal_payout'
CREATE INDEX capa_decisions_person ON aml_enforcement_decisions (tenant_id, person_id, decided_at DESC) WHERE operation = 'withdrawal_payout';
ANALYZE aml_enforcement_decisions;
SELECT * FROM aml_screening_parked_summary(:'tid', clock_timestamp() - interval '7 days');
SELECT * FROM aml_screening_parked_summary(:'tid', clock_timestamp() - interval '7 days');
SELECT * FROM aml_screening_parked_overdue(:'tid', clock_timestamp() - interval '7 days', NULL, 500);
SELECT * FROM aml_screening_parked_overdue(:'tid', clock_timestamp() - interval '7 days', NULL, 500);
SELECT * FROM aml_screening_parked_overdue(:'tid', clock_timestamp() - interval '7 days', NULL, 5000);
\timing off
