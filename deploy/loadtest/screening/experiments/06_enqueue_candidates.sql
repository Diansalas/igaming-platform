-- THROWAWAY EXPERIMENT. aml_screening_enqueue_due candidate query: production text vs the candidate with the guarded since() (03_gate_set.sql: capa_since_g
-- + capa_since_v2). Compares, for every (tenant, jurisdiction) whose latest trigger policy lists 'scheduled', the FULL candidate relation
-- (person_id, covered, last_any, before the LIMIT) and times both. Works on any capa_* database: ./run.sh explain fn-10k experiments/06_enqueue_candidates.sql
\set ON_ERROR_STOP on
\if :{?have03}\else\i 03_gate_set.sql\set have03 1\endif
CREATE FUNCTION capa_enq_old(p_tenant uuid, p_jur uuid) RETURNS TABLE (person_id uuid, last_any timestamptz, covered boolean) LANGUAGE plpgsql STABLE AS $$
DECLARE v_sp record; v_first timestamptz;
BEGIN
    SELECT sp.id, sp.rescreen_interval_seconds, sp.lists INTO v_sp FROM public.aml_screening_policies sp
     WHERE sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = p_jur AND sp.effective_from <= pg_catalog.clock_timestamp() ORDER BY sp.created_at DESC, sp.id DESC LIMIT 1;
    IF NOT FOUND THEN RETURN; END IF;
    SELECT COALESCE(min(tp.effective_from) FILTER (WHERE tp.gate_withdrawal), min(tp.effective_from)) INTO v_first
      FROM public.aml_screening_trigger_policies tp WHERE tp.tenant_id = p_tenant AND tp.licensing_jurisdiction_id = p_jur;
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
                       AND public.aml_screening_covers(LEAST(r.completed_at, r.screened_at), r.screened_interval_seconds, r.screened_lists, v_sp.rescreen_interval_seconds::double precision, v_sp.lists, public.aml_screening_since(p_tenant, p_jur, LEAST(r.claimed_at, r.completed_at, r.screened_at), v_sp.lists, r.claim_policy_id))
                    UNION ALL
                    SELECT 1 FROM public.aml_screening_results x
                     WHERE x.tenant_id = p_tenant AND x.person_id = pa.person_id AND x.outcome <> 'error' AND x.recorded_at >= v_first
                       AND public.aml_screening_covers(LEAST(x.recorded_at, x.screened_at), x.screened_interval_seconds, x.lists, v_sp.rescreen_interval_seconds::double precision, v_sp.lists, public.aml_screening_since(p_tenant, p_jur, LEAST(x.recorded_at, x.screened_at), v_sp.lists, NULL::uuid)))
          FROM (SELECT DISTINCT a.person_id FROM public.player_accounts a WHERE a.tenant_id = p_tenant) pa;
END $$;
CREATE FUNCTION capa_enq_new(p_tenant uuid, p_jur uuid) RETURNS TABLE (person_id uuid, last_any timestamptz, covered boolean) LANGUAGE plpgsql STABLE AS $$
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
          FROM (SELECT DISTINCT a.person_id FROM public.player_accounts a WHERE a.tenant_id = p_tenant) pa;
END $$;
CREATE TEMP TABLE e_old (tid uuid, jid uuid, person_id uuid, last_any timestamptz, covered boolean);
CREATE TEMP TABLE e_new (tid uuid, jid uuid, person_id uuid, last_any timestamptz, covered boolean);
CREATE TEMP TABLE e_t (tid uuid, jid uuid, ms_old numeric, ms_new numeric);
DO $$
DECLARE tn record; r record; t0 timestamptz; t1 timestamptz; t2 timestamptz;
BEGIN
  FOR tn IN SELECT id FROM public.tenants ORDER BY id LOOP
    PERFORM set_config('app.tenant_id', tn.id::text, true);
    FOR r IN SELECT tn.id AS tid, l.licensing_jurisdiction_id AS jid
               FROM (SELECT DISTINCT ON (x.licensing_jurisdiction_id) x.licensing_jurisdiction_id, x.events FROM public.aml_screening_trigger_policies x
                      WHERE x.tenant_id = tn.id AND x.effective_from <= clock_timestamp() ORDER BY x.licensing_jurisdiction_id, x.created_at DESC, x.id DESC) l
              WHERE 'scheduled' = ANY (l.events) LOOP
      t0 := clock_timestamp();
      INSERT INTO e_old SELECT r.tid, r.jid, o.* FROM public.capa_enq_old(r.tid, r.jid) o;
      t1 := clock_timestamp();
      INSERT INTO e_new SELECT r.tid, r.jid, o.* FROM public.capa_enq_new(r.tid, r.jid) o;
      t2 := clock_timestamp();
      INSERT INTO e_t VALUES (r.tid, r.jid, extract(epoch from t1 - t0) * 1000, extract(epoch from t2 - t1) * 1000);
    END LOOP;
  END LOOP;
END $$;
SELECT count(*) AS combos, round(sum(ms_old)) AS total_ms_old, round(sum(ms_new)) AS total_ms_new, round(sum(ms_old) / NULLIF(sum(ms_new), 0), 1) AS speedup FROM e_t;
SELECT (SELECT count(*) FROM e_old) AS rows_old, (SELECT count(*) FROM e_new) AS rows_new,
       (SELECT count(*) FROM (SELECT * FROM e_old EXCEPT SELECT * FROM e_new) q) AS only_in_old, (SELECT count(*) FROM (SELECT * FROM e_new EXCEPT SELECT * FROM e_old) q) AS only_in_new,
       (SELECT count(*) FROM e_old WHERE NOT covered) AS candidates_old, (SELECT count(*) FROM e_new WHERE NOT covered) AS candidates_new;
