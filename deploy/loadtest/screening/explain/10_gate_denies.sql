\i 00_setup.sql
\echo '=== E1 gate_denies: the big "is there a covering fresh clear" NOT EXISTS for a STALE person (inline reconstruction)'
EXPLAIN (ANALYZE, BUFFERS)
SELECT NOT EXISTS (
    SELECT 1 FROM public.aml_screening_requests r
     WHERE r.tenant_id = :'tid' AND r.person_id = :'xp' AND r.status = 'done' AND r.outcome = 'clear'
       AND r.completed_at >= :'vfirst'
       AND public.aml_screening_covers(LEAST(r.completed_at, r.screened_at), r.screened_interval_seconds, r.screened_lists, :vint::double precision, :'vlists'::text[],
              public.aml_screening_since(:'tid', :'jur', LEAST(r.claimed_at, r.completed_at, r.screened_at), :'vlists'::text[], r.claim_policy_id))
    UNION ALL
    SELECT 1 FROM public.aml_screening_requests r
     WHERE r.tenant_id = :'tid' AND r.person_id = :'xp' AND r.status = 'needs_officer' AND r.reviewed_from_request_id IS NOT NULL
       AND r.completed_at >= :'vfirst'
    UNION ALL
    SELECT 1 FROM public.aml_screening_results x
     WHERE x.tenant_id = :'tid' AND x.person_id = :'xp' AND x.outcome = 'clear' AND x.recorded_at >= :'vfirst'
       AND public.aml_screening_covers(LEAST(x.recorded_at, x.screened_at), x.screened_interval_seconds, x.lists, :vint::double precision, :'vlists'::text[],
              public.aml_screening_since(:'tid', :'jur', LEAST(x.recorded_at, x.screened_at), :'vlists'::text[], NULL::uuid))
    UNION ALL
    SELECT 1 FROM public.aml_screening_results x JOIN public.aml_hold_lifts l ON l.tenant_id = x.tenant_id AND l.hold_id = x.hold_id
     WHERE x.tenant_id = :'tid' AND x.person_id = :'xp' AND x.outcome IN ('potential_match', 'confirmed') AND l.lifted_at >= :'vfirst'
);
\echo '=== E1b gate_denies: the always-on needs_officer EXISTS (person with an unresolved match is denied first)'
EXPLAIN (ANALYZE, BUFFERS)
SELECT EXISTS (SELECT 1 FROM public.aml_screening_requests r WHERE r.tenant_id = :'tid' AND r.person_id = :'xp' AND r.status = 'needs_officer'
                AND NOT public.aml_screening_request_resolved(r.id));
\echo '=== E1c gate_denies: registration lookup, trigger-policy DISTINCT ON, first gating version, latest screening policy'
EXPLAIN (ANALYZE, BUFFERS) SELECT min(pa.created_at) FROM public.player_accounts pa WHERE pa.tenant_id = :'tid' AND pa.person_id = :'xp';
EXPLAIN (ANALYZE, BUFFERS) SELECT l.jur FROM (SELECT DISTINCT ON (tp.licensing_jurisdiction_id) tp.licensing_jurisdiction_id AS jur, tp.gate_withdrawal
   FROM public.aml_screening_trigger_policies tp WHERE tp.tenant_id = :'tid' AND tp.effective_from <= pg_catalog.clock_timestamp()
   ORDER BY tp.licensing_jurisdiction_id, tp.created_at DESC, tp.id DESC) l WHERE l.gate_withdrawal;
EXPLAIN (ANALYZE, BUFFERS) SELECT sp.rescreen_interval_seconds, sp.backfill_grace_seconds, sp.lists FROM public.aml_screening_policies sp
 WHERE sp.tenant_id = :'tid' AND sp.licensing_jurisdiction_id = :'jur' AND sp.effective_from <= pg_catalog.clock_timestamp() ORDER BY sp.created_at DESC, sp.id DESC LIMIT 1;
\echo '=== E1d the same E1 for a CLEAR person'
EXPLAIN (ANALYZE, BUFFERS)
SELECT 1 FROM public.aml_screening_requests r
 WHERE r.tenant_id = :'tid' AND r.person_id = :'cp' AND r.status = 'done' AND r.outcome = 'clear' AND r.completed_at >= :'vfirst'
   AND public.aml_screening_covers(LEAST(r.completed_at, r.screened_at), r.screened_interval_seconds, r.screened_lists, :vint::double precision, :'vlists'::text[],
          public.aml_screening_since(:'tid', :'jur', LEAST(r.claimed_at, r.completed_at, r.screened_at), :'vlists'::text[], r.claim_policy_id));
\echo '=== END-TO-END: the real function, same person (reconstruction vs function)'
\timing on
SELECT aml_screening_gate_denies(:'tid', :'xp', 'withdrawal') AS stale_denied;
SELECT aml_screening_gate_denies(:'tid', :'xp', 'withdrawal') AS stale_denied;
SELECT aml_screening_gate_denies(:'tid', :'cp', 'withdrawal') AS clear_denied;
SELECT aml_screening_gate_denies(:'tid', :'cp', 'withdrawal') AS clear_denied;
