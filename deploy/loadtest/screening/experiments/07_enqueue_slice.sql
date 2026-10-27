-- THROWAWAY EXPERIMENT. Bounded scan: the guarded enqueue candidate (06) evaluated for ONE keyset slice of persons (person_id > cursor, LIMIT slice) per call.
-- Cost per call should depend on the slice, not on the tenant. The union of the slices over a full cycle must equal the full candidate relation.
\set ON_ERROR_STOP on
\i 06_enqueue_candidates.sql
CREATE FUNCTION capa_enq_slice(p_tenant uuid, p_jur uuid, p_after uuid, p_slice int) RETURNS TABLE (person_id uuid, last_any timestamptz, covered boolean) LANGUAGE sql STABLE AS $$
    SELECT o.* FROM public.capa_enq_new(p_tenant, p_jur) o
$$;
-- (the slice is applied inside a copy of capa_enq_new; a plain wrapper above would still evaluate every person)
DROP FUNCTION capa_enq_slice(uuid, uuid, uuid, int);
CREATE FUNCTION capa_enq_slice(p_tenant uuid, p_jur uuid, p_after uuid, p_slice int) RETURNS TABLE (person_id uuid, last_any timestamptz, covered boolean) LANGUAGE plpgsql STABLE AS $$
DECLARE v_sp record; v_first timestamptz; v_ls timestamptz; v_lid uuid;
BEGIN
    SELECT sp.id, sp.rescreen_interval_seconds, sp.lists INTO v_sp FROM public.aml_screening_policies sp
     WHERE sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = p_jur AND sp.effective_from <= pg_catalog.clock_timestamp() ORDER BY sp.created_at DESC, sp.id DESC LIMIT 1;
    IF NOT FOUND THEN RETURN; END IF;
    SELECT COALESCE(min(tp.effective_from) FILTER (WHERE tp.gate_withdrawal), min(tp.effective_from)) INTO v_first
      FROM public.aml_screening_trigger_policies tp WHERE tp.tenant_id = p_tenant AND tp.licensing_jurisdiction_id = p_jur;
    SELECT max(GREATEST(sp.effective_from, sp.created_at)) FILTER (WHERE GREATEST(sp.effective_from, sp.created_at) <= pg_catalog.clock_timestamp()),
           (array_agg(sp.id ORDER BY sp.created_at DESC, sp.id DESC) FILTER (WHERE GREATEST(sp.effective_from, sp.created_at) <= pg_catalog.clock_timestamp()))[1]
      INTO v_ls, v_lid FROM public.aml_screening_policies sp WHERE sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = p_jur;
    RETURN QUERY
        SELECT pa.person_id,
               (SELECT max(g.t) FROM (
                    SELECT r.completed_at AS t FROM public.aml_screening_requests r
                     WHERE r.tenant_id = p_tenant AND r.person_id = pa.person_id AND r.status IN ('done', 'needs_officer') AND r.completed_at >= v_first
                    UNION ALL
                    SELECT x.recorded_at FROM public.aml_screening_results x
                     WHERE x.tenant_id = p_tenant AND x.person_id = pa.person_id AND x.outcome <> 'error' AND x.recorded_at >= v_first) g),
               EXISTS (
                    SELECT 1 FROM public.aml_screening_requests r
                     WHERE r.tenant_id = p_tenant AND r.person_id = pa.person_id AND r.status IN ('done', 'needs_officer') AND r.completed_at >= v_first
                       AND public.aml_screening_covers(LEAST(r.completed_at, r.screened_at), r.screened_interval_seconds, r.screened_lists, v_sp.rescreen_interval_seconds::double precision, v_sp.lists, public.capa_since_g(p_tenant, p_jur, LEAST(r.claimed_at, r.completed_at, r.screened_at), v_sp.lists, r.claim_policy_id, v_ls, v_lid))
                    UNION ALL
                    SELECT 1 FROM public.aml_screening_results x
                     WHERE x.tenant_id = p_tenant AND x.person_id = pa.person_id AND x.outcome <> 'error' AND x.recorded_at >= v_first
                       AND public.aml_screening_covers(LEAST(x.recorded_at, x.screened_at), x.screened_interval_seconds, x.lists, v_sp.rescreen_interval_seconds::double precision, v_sp.lists, public.capa_since_g(p_tenant, p_jur, LEAST(x.recorded_at, x.screened_at), v_sp.lists, NULL::uuid, v_ls, v_lid)))
          FROM (SELECT DISTINCT a.person_id FROM public.player_accounts a WHERE a.tenant_id = p_tenant AND (p_after IS NULL OR a.person_id > p_after) ORDER BY a.person_id LIMIT p_slice) pa;
END $$;
SELECT id AS tid FROM tenants WHERE status = 'active' ORDER BY created_at, slug LIMIT 1 \gset
SELECT set_config('app.tenant_id', :'tid', false) \gset
SELECT licensing_jurisdiction_id AS jid FROM aml_screening_policies WHERE tenant_id = :'tid' LIMIT 1 \gset
\echo 'slice of 2000 persons, first call, then second call (cursor from the first slice):'
\timing on
SELECT count(*) AS rows, (array_agg(person_id ORDER BY person_id DESC))[1] AS last_person FROM capa_enq_slice(:'tid', :'jid', NULL, 2000) \gset
SELECT count(*) FROM capa_enq_slice(:'tid', :'jid', :'last_person', 2000);
SELECT count(*) FROM capa_enq_slice(:'tid', :'jid', NULL, 500);
\timing off
-- union of slices over a full cycle equals the full relation (equivalence)
CREATE TEMP TABLE sl (person_id uuid, last_any timestamptz, covered boolean);
DO $$
DECLARE c uuid := NULL; n int; tid uuid; jid uuid; mx uuid; batches int := 0;
BEGIN
  SELECT t.id INTO tid FROM public.tenants t WHERE t.status = 'active' ORDER BY t.created_at, t.slug LIMIT 1;
  PERFORM set_config('app.tenant_id', tid::text, true);
  SELECT licensing_jurisdiction_id INTO jid FROM public.aml_screening_policies WHERE tenant_id = tid LIMIT 1;
  LOOP
    INSERT INTO sl SELECT * FROM public.capa_enq_slice(tid, jid, c, 2000);
    GET DIAGNOSTICS n = ROW_COUNT;
    EXIT WHEN n = 0;
    SELECT person_id INTO c FROM sl ORDER BY person_id DESC LIMIT 1;
    batches := batches + 1;
  END LOOP;
  RAISE NOTICE 'batches=%', batches;
END $$;
SELECT (SELECT count(*) FROM sl) AS slice_rows, (SELECT count(*) FROM e_new) AS full_rows,
       (SELECT count(*) FROM (SELECT person_id, last_any, covered FROM sl EXCEPT SELECT person_id, last_any, covered FROM e_new) q) AS only_slices,
       (SELECT count(*) FROM (SELECT person_id, last_any, covered FROM e_new EXCEPT SELECT person_id, last_any, covered FROM sl) q) AS only_full;
