-- MECHANISM CHECK (ops C1): aml_screening_request_finish for a clear/match calls aml_screening_assert_worker_provider, which needs a
-- screening_worker_heartbeats row younger than 10 minutes; the worker writes the heartbeat once at the START of RunPass. Here (private DB, table owner)
-- the heartbeat is aged to 11 minutes, a claimed request is finished -> expect AT001; a retry is still applied; after a fresh heartbeat the clear is applied.
-- ./run.sh explain race-2k experiments/11_heartbeat_expiry.sql
\set ON_ERROR_STOP off
\pset format unaligned
\pset tuples_only on
SELECT id AS tid FROM tenants WHERE status = 'active' ORDER BY created_at, slug LIMIT 1 \gset
SELECT set_config('app.tenant_id', :'tid', false) \gset
SELECT screening_worker_heartbeat('mock-screening', true);
SELECT aml_screening_enqueue_due(:'tid', 3) AS enq;
SELECT o_id AS rid, o_claim_token AS tok FROM aml_screening_request_claim(:'tid', 1, 600) LIMIT 1 \gset
SELECT claim_policy_id AS pol FROM aml_screening_requests WHERE id = :'rid' \gset
\echo '--- heartbeat aged to 11 minutes:'
UPDATE screening_worker_heartbeats SET last_seen_at = now() - interval '11 minutes' WHERE provider_id = 'mock-screening';
SELECT last_seen_at < now() - interval '10 minutes' AS heartbeat_is_older_than_10_min FROM screening_worker_heartbeats WHERE provider_id = 'mock-screening';
\echo '--- finish(clear) with the stale heartbeat (expect ERROR ... AT001 provider is not the one a running screening worker reported):'
SELECT * FROM aml_screening_request_finish(:'tid', :'rid', :'tok', 'clear', NULL, NULL, 'mock-screening', 'lt-x', repeat('a',64), now(), 0, NULL, :'pol');
\echo '--- finish(retry) with the stale heartbeat (a retry never makes a person pass; expect applied=t, status pending):'
SELECT * FROM aml_screening_request_finish(:'tid', :'rid', :'tok', 'retry', NULL, 'unavailable', 'mock-screening', NULL, NULL, NULL, 0, NULL, NULL);
\echo '--- fresh heartbeat, re-claim after making it due, finish(clear) (expect applied=t, status done):'
SELECT screening_worker_heartbeat('mock-screening', true);
SELECT o_id AS rid2, o_claim_token AS tok2 FROM aml_screening_request_claim(:'tid', 1, 600) LIMIT 1 \gset
SELECT claim_policy_id AS pol2 FROM aml_screening_requests WHERE id = :'rid2' \gset
SELECT * FROM aml_screening_request_finish(:'tid', :'rid2', :'tok2', 'clear', NULL, NULL, 'mock-screening', 'lt-y', repeat('b',64), now(), 0, NULL, :'pol2');
