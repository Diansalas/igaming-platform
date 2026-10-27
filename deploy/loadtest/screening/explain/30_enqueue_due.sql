\i 00_setup.sql
-- the candidate query of aml_screening_enqueue_due (INSERT ... SELECT), v_bucket / v_sp from the data; wrapped in a transaction that is rolled back
SELECT floor(extract(epoch FROM clock_timestamp()) / :vint)::bigint AS bucket \gset
\echo '=== E3 enqueue_due candidate query, LIMIT 1000 (EXPLAIN ANALYZE of the INSERT, rolled back)'
BEGIN;
EXPLAIN (ANALYZE, BUFFERS)
INSERT INTO public.aml_screening_requests (tenant_id, person_id, licensing_jurisdiction_id, trigger, idempotency_key)
SELECT :'tid', c.person_id, :'jur', 'scheduled', 'sched:' || c.person_id::text || ':' || :bucket::text || ':' || :'vpol'::text
  FROM (
        SELECT pa.person_id,
               (SELECT max(g.t) FROM (
                    SELECT r.completed_at AS t FROM public.aml_screening_requests r
                     WHERE r.tenant_id = :'tid' AND r.person_id = pa.person_id AND r.status IN ('done', 'needs_officer') AND r.completed_at >= :'vfirst'
                    UNION ALL
                    SELECT x.recorded_at FROM public.aml_screening_results x
                     WHERE x.tenant_id = :'tid' AND x.person_id = pa.person_id AND x.outcome <> 'error' AND x.recorded_at >= :'vfirst') g) AS last_any,
               EXISTS (
                    SELECT 1 FROM public.aml_screening_requests r
                     WHERE r.tenant_id = :'tid' AND r.person_id = pa.person_id AND r.status IN ('done', 'needs_officer') AND r.completed_at >= :'vfirst'
                       AND public.aml_screening_covers(LEAST(r.completed_at, r.screened_at), r.screened_interval_seconds, r.screened_lists, :vint::double precision, :'vlists'::text[],
                           public.aml_screening_since(:'tid', :'jur', LEAST(r.claimed_at, r.completed_at, r.screened_at), :'vlists'::text[], r.claim_policy_id))
                    UNION ALL
                    SELECT 1 FROM public.aml_screening_results x
                     WHERE x.tenant_id = :'tid' AND x.person_id = pa.person_id AND x.outcome <> 'error' AND x.recorded_at >= :'vfirst'
                       AND public.aml_screening_covers(LEAST(x.recorded_at, x.screened_at), x.screened_interval_seconds, x.lists, :vint::double precision, :'vlists'::text[],
                           public.aml_screening_since(:'tid', :'jur', LEAST(x.recorded_at, x.screened_at), :'vlists'::text[], NULL::uuid))) AS covered
          FROM (SELECT DISTINCT a.person_id FROM public.player_accounts a WHERE a.tenant_id = :'tid') pa
       ) c
 WHERE NOT c.covered
   AND NOT EXISTS (SELECT 1 FROM public.aml_screening_requests o WHERE o.tenant_id = :'tid' AND o.person_id = c.person_id AND o.status IN ('pending', 'claimed'))
   AND NOT EXISTS (SELECT 1 FROM public.aml_screening_requests k WHERE k.tenant_id = :'tid'
                    AND k.idempotency_key = 'sched:' || c.person_id::text || ':' || :bucket::text || ':' || :'vpol'::text)
 ORDER BY c.last_any NULLS FIRST, c.person_id
 LIMIT 1000
ON CONFLICT (tenant_id, idempotency_key) DO NOTHING;
ROLLBACK;
\echo '=== END-TO-END: the real function (rolled back)'
\timing on
BEGIN; SELECT aml_screening_enqueue_due(:'tid', 1000); ROLLBACK;
BEGIN; SELECT aml_screening_enqueue_due(:'tid', 1000); ROLLBACK;
BEGIN; SELECT aml_screening_enqueue_due(:'tid', 100); ROLLBACK;
