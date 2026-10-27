\i 00_setup.sql
\echo '=== E2 aml_screening_since body, p_at older than every version (worst case: every version qualifies as "later")'
EXPLAIN (ANALYZE, BUFFERS)
SELECT CASE WHEN COALESCE(pg_catalog.bool_or(NOT (v.lists @> :'vlists'::text[])), false) THEN 0 ELSE pg_catalog.min(v.rescreen_interval_seconds) END
  FROM (SELECT sp.id, sp.lists, sp.rescreen_interval_seconds, sp.created_at, GREATEST(sp.effective_from, sp.created_at) AS started
          FROM public.aml_screening_policies sp WHERE sp.tenant_id = :'tid' AND sp.licensing_jurisdiction_id = :'jur') v
 WHERE (v.started > now() - interval '100 days' OR FALSE)
   AND v.started <= pg_catalog.clock_timestamp()
   AND NOT EXISTS (SELECT 1 FROM public.aml_screening_policies w WHERE w.tenant_id = :'tid' AND w.licensing_jurisdiction_id = :'jur'
                    AND w.created_at > v.created_at AND GREATEST(w.effective_from, w.created_at) <= v.started);
\echo '=== E2b the same with p_at = 3 days ago (the realistic fresh-clear case)'
EXPLAIN (ANALYZE, BUFFERS)
SELECT CASE WHEN COALESCE(pg_catalog.bool_or(NOT (v.lists @> :'vlists'::text[])), false) THEN 0 ELSE pg_catalog.min(v.rescreen_interval_seconds) END
  FROM (SELECT sp.id, sp.lists, sp.rescreen_interval_seconds, sp.created_at, GREATEST(sp.effective_from, sp.created_at) AS started
          FROM public.aml_screening_policies sp WHERE sp.tenant_id = :'tid' AND sp.licensing_jurisdiction_id = :'jur') v
 WHERE (v.started > now() - interval '3 days')
   AND v.started <= pg_catalog.clock_timestamp()
   AND NOT EXISTS (SELECT 1 FROM public.aml_screening_policies w WHERE w.tenant_id = :'tid' AND w.licensing_jurisdiction_id = :'jur'
                    AND w.created_at > v.created_at AND GREATEST(w.effective_from, w.created_at) <= v.started);
\echo '=== END-TO-END: the real function'
\timing on
SELECT aml_screening_since(:'tid', :'jur', now() - interval '100 days', :'vlists'::text[], NULL);
SELECT aml_screening_since(:'tid', :'jur', now() - interval '100 days', :'vlists'::text[], NULL);
SELECT aml_screening_since(:'tid', :'jur', now() - interval '3 days', :'vlists'::text[], NULL);
SELECT aml_screening_since(:'tid', :'jur', now() - interval '3 days', :'vlists'::text[], NULL);
