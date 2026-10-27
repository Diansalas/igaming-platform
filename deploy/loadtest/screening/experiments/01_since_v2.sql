-- THROWAWAY EXPERIMENT (private scratch database only; not a migration, not production code).
-- Candidate: aml_screening_since without the O(V^2) correlated NOT EXISTS.
--   original:  keep version v when NOT EXISTS (w: same tenant+jurisdiction, w.created_at > v.created_at AND started(w) <= started(v))
--   candidate: the same predicate as a window minimum:  min(started) over the versions created STRICTLY later  >  started(v)  (or none)
-- Everything else (the outer filter, the CASE) is unchanged. Equivalence is checked by a randomised differential test below.
\set ON_ERROR_STOP on
CREATE FUNCTION capa_since_v2(p_tenant uuid, p_jur uuid, p_at timestamptz, p_cur_lists text[], p_after_policy uuid) RETURNS bigint
LANGUAGE sql SET search_path TO 'pg_catalog', 'public', 'pg_temp' AS $$
    SELECT CASE WHEN COALESCE(pg_catalog.bool_or(NOT (v.lists @> COALESCE(p_cur_lists, ARRAY[]::text[]))), false) THEN 0
                ELSE pg_catalog.min(v.rescreen_interval_seconds) END
      FROM (SELECT sp.id, sp.lists, sp.rescreen_interval_seconds, sp.created_at, GREATEST(sp.effective_from, sp.created_at) AS started,
                   pg_catalog.min(GREATEST(sp.effective_from, sp.created_at)) OVER (ORDER BY sp.created_at DESC
                        RANGE BETWEEN UNBOUNDED PRECEDING AND '1 microsecond'::interval PRECEDING) AS min_later_started
              FROM public.aml_screening_policies sp
             WHERE sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = p_jur) v
     WHERE (v.started > p_at
            OR (p_after_policy IS NOT NULL
                AND (v.created_at, v.id) > (SELECT cp.created_at, cp.id FROM public.aml_screening_policies cp WHERE cp.tenant_id = p_tenant AND cp.id = p_after_policy)))
       AND v.started <= pg_catalog.clock_timestamp()
       AND (v.min_later_started IS NULL OR v.min_later_started > v.started)
$$;
