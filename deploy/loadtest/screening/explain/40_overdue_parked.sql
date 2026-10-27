\i 00_setup.sql
SELECT 3600 AS v_alert \gset
\echo '=== E4 gate_overdue outer: DISTINCT persons LIMIT 5000 + per-person function call (function internals are not visible; see E1)'
EXPLAIN (ANALYZE, BUFFERS)
SELECT pa.person_id FROM (SELECT DISTINCT a.person_id FROM public.player_accounts a WHERE a.tenant_id = :'tid' LIMIT 5000) pa
 WHERE public.aml_screening_gate_denies(:'tid', pa.person_id, 'withdrawal');
\echo '=== E4b gate_overdue per-denied-person anchor subquery'
EXPLAIN (ANALYZE, BUFFERS)
SELECT min(r.created_at) FROM public.aml_screening_requests r
 WHERE r.tenant_id = :'tid' AND r.person_id = :'xp' AND r.status NOT IN ('done', 'cancelled') AND NOT public.aml_screening_request_resolved(r.id);
\echo '=== E5 parked_overdue: the batch (next 500 distinct persons after a cursor, person_id order)'
EXPLAIN (ANALYZE, BUFFERS)
SELECT DISTINCT a.person_id FROM public.player_accounts a WHERE a.tenant_id = :'tid' AND a.person_id > '80000000-0000-0000-0000-000000000000'::uuid ORDER BY a.person_id LIMIT 500;
\echo '=== E5b parked_overdue: the aml_enforcement_decisions window read for denied persons'
SELECT ARRAY(SELECT person_id FROM (SELECT DISTINCT a.person_id FROM public.player_accounts a WHERE a.tenant_id = :'tid' ORDER BY a.person_id LIMIT 500) q WHERE public.aml_screening_gate_denies(:'tid', q.person_id, 'withdrawal'))::text AS denied_ids \gset
EXPLAIN (ANALYZE, BUFFERS)
SELECT x.person_id, x.decided_at, (x.hold_outcome = 'held' AND x.hold_id IS NULL AND NOT x.allowed AND x.kyc_outcome IN ('passed', 'not_required')) AS parked, x.id
  FROM public.aml_enforcement_decisions x
 WHERE x.tenant_id = :'tid' AND x.operation = 'withdrawal_payout' AND x.decided_at >= now() - interval '7 days'
   AND x.person_id = ANY (:'denied_ids'::uuid[]);
\echo '=== E5c parked_overdue: open requests of denied persons'
EXPLAIN (ANALYZE, BUFFERS)
SELECT r.person_id, r.status, r.created_at, public.aml_screening_request_resolved(r.id) AS res FROM public.aml_screening_requests r
 WHERE r.tenant_id = :'tid' AND r.status NOT IN ('done', 'cancelled') AND r.person_id = ANY (:'denied_ids'::uuid[]);
\echo '=== E6 parked_summary: latest decision per person in the 7-day window'
EXPLAIN (ANALYZE, BUFFERS)
SELECT DISTINCT ON (d.person_id) d.person_id, d.hold_outcome, d.hold_id, d.kyc_outcome, d.allowed FROM public.aml_enforcement_decisions d
 WHERE d.tenant_id = :'tid' AND d.operation = 'withdrawal_payout' AND d.decided_at >= now() - interval '7 days' ORDER BY d.person_id, d.decided_at DESC, d.id DESC;
\echo '=== END-TO-END: real functions'
\timing on
SELECT * FROM aml_screening_parked_overdue(:'tid', clock_timestamp() - interval '7 days', NULL, 500);
SELECT * FROM aml_screening_parked_overdue(:'tid', clock_timestamp() - interval '7 days', NULL, 500);
SELECT * FROM aml_screening_parked_overdue(:'tid', clock_timestamp() - interval '7 days', NULL, 5000);
SELECT aml_screening_gate_overdue(:'tid', 5000);
SELECT aml_screening_gate_overdue(:'tid', 5000);
SELECT * FROM aml_screening_parked_summary(:'tid', clock_timestamp() - interval '7 days');
