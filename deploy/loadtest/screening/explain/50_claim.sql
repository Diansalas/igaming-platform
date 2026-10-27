\i 00_setup.sql
\echo '=== E7 claim: the due CTE (SKIP LOCKED) with a pending backlog; rolled back'
BEGIN;
SELECT aml_screening_enqueue_due(:'tid', 1000) AS enq \gset
EXPLAIN (ANALYZE, BUFFERS)
SELECT r.id AS rid FROM public.aml_screening_requests r
 WHERE r.tenant_id = :'tid' AND ((r.status = 'pending' AND r.next_attempt_at <= pg_catalog.clock_timestamp()) OR (r.status = 'claimed' AND r.lease_until <= pg_catalog.clock_timestamp()))
 ORDER BY r.next_attempt_at, r.id LIMIT 4 FOR UPDATE SKIP LOCKED;
\timing on
SELECT count(*) FROM aml_screening_request_claim(:'tid', 4, 120);
ROLLBACK;
