-- THROWAWAY EXPERIMENT (private scratch database only; not a migration, not production code).
-- Candidate: evaluate aml_screening_gate_denies for a SET of persons in one SQL statement (instead of one plpgsql call per person), for the
-- batch consumers (aml_screening_gate_overdue, aml_screening_parked_overdue). The decision logic is a statement-for-statement transcription of
-- aml_screening_gate_denies (migrations 0192-0194) with the plpgsql loop over gating jurisdictions turned into EXISTS over a jurisdiction set:
--   denied  <=>  open_match  OR  EXISTS gating jurisdiction j: ( v_reg IS NULL  OR  ( NOT grace_skip(j) AND NOT covered(person, j) ) )
-- It uses capa_since_v2 (01_since_v2.sql). Equivalence is checked by 04_gate_equiv.sql (randomised) and by a full-tenant comparison.
\set ON_ERROR_STOP on
\i 01_since_v2.sql
-- Guard: aml_screening_since can only return a non-NULL value when SOME version that has started (started <= now) either started after p_at, or was
-- created after the claim-time version p_after. If p_at is not before the latest started version and the screening was claimed under the latest started
-- version (or has no claim version), the answer is NULL without reading the policy table per row. (Plain SQL, no SET clause, so the planner inlines it;
-- the production functions carry SET search_path, a real change would write the CASE inline.)
CREATE FUNCTION capa_since_g(p_tenant uuid, p_jur uuid, p_at timestamptz, p_lists text[], p_after uuid, p_last_started timestamptz, p_last_id uuid) RETURNS bigint
LANGUAGE sql AS $$
    SELECT CASE WHEN p_at >= p_last_started AND (p_after IS NULL OR p_after = p_last_id) THEN NULL::bigint
                ELSE public.capa_since_v2(p_tenant, p_jur, p_at, p_lists, p_after) END
$$;
CREATE FUNCTION capa_gate_denied_set(p_tenant uuid, p_persons uuid[]) RETURNS TABLE (person_id uuid)
LANGUAGE sql STABLE SET search_path TO 'pg_catalog', 'public', 'pg_temp' AS $$
WITH pers AS (SELECT DISTINCT u.person_id FROM unnest(p_persons) AS u(person_id)),
gj AS (
    SELECT l.jur FROM (SELECT DISTINCT ON (tp.licensing_jurisdiction_id) tp.licensing_jurisdiction_id AS jur, tp.gate_withdrawal
                         FROM public.aml_screening_trigger_policies tp
                        WHERE tp.tenant_id = p_tenant AND tp.effective_from <= pg_catalog.clock_timestamp()
                        ORDER BY tp.licensing_jurisdiction_id, tp.created_at DESC, tp.id DESC) l
     WHERE l.gate_withdrawal),
gp AS (
    SELECT gj.jur,
           (SELECT min(tp.effective_from) FROM public.aml_screening_trigger_policies tp
             WHERE tp.tenant_id = p_tenant AND tp.licensing_jurisdiction_id = gj.jur AND tp.gate_withdrawal) AS v_first,
           s.rescreen_interval_seconds::double precision AS v_interval,
           COALESCE(s.backfill_grace_seconds, 0)::double precision AS v_grace,
           s.lists AS v_lists, m.last_started, m.last_id
      FROM gj LEFT JOIN LATERAL (SELECT sp.rescreen_interval_seconds, sp.backfill_grace_seconds, sp.lists FROM public.aml_screening_policies sp
                                  WHERE sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = gj.jur AND sp.effective_from <= pg_catalog.clock_timestamp()
                                  ORDER BY sp.created_at DESC, sp.id DESC LIMIT 1) s ON true
      LEFT JOIN LATERAL (SELECT pg_catalog.max(GREATEST(sp.effective_from, sp.created_at)) FILTER (WHERE GREATEST(sp.effective_from, sp.created_at) <= pg_catalog.clock_timestamp()) AS last_started,
                                (pg_catalog.array_agg(sp.id ORDER BY sp.created_at DESC, sp.id DESC) FILTER (WHERE GREATEST(sp.effective_from, sp.created_at) <= pg_catalog.clock_timestamp()))[1] AS last_id
                           FROM public.aml_screening_policies sp WHERE sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = gj.jur) m ON true),
base AS (
    SELECT p.person_id,
           EXISTS (SELECT 1 FROM public.aml_screening_requests r WHERE r.tenant_id = p_tenant AND r.person_id = p.person_id AND r.status = 'needs_officer'
                    AND NOT public.aml_screening_request_resolved(r.id)) AS open_match,
           (SELECT min(pa.created_at) FROM public.player_accounts pa WHERE pa.tenant_id = p_tenant AND pa.person_id = p.person_id) AS v_reg,
           (SELECT max(r.completed_at) FROM public.aml_screening_requests r WHERE r.tenant_id = p_tenant AND r.person_id = p.person_id
               AND r.status = 'needs_officer' AND r.reviewed_from_request_id IS NULL) AS v_last_match
      FROM pers p),
base2 AS (
    SELECT b.*, COALESCE((SELECT pg_catalog.bool_or(r.outcome = 'confirmed' OR COALESCE('sanctions' = ANY (r.match_lists), false))
                            FROM public.aml_screening_requests r
                           WHERE r.tenant_id = p_tenant AND r.person_id = b.person_id AND r.status = 'needs_officer' AND r.reviewed_from_request_id IS NULL
                             AND r.completed_at = b.v_last_match), false) AS v_sanc
      FROM base b)
SELECT b.person_id FROM base2 b
 WHERE b.open_match
    OR (b.v_reg IS NULL AND EXISTS (SELECT 1 FROM gp))
    OR EXISTS (
        SELECT 1 FROM gp
         WHERE b.v_reg IS NOT NULL
           AND NOT (b.v_reg < gp.v_first AND pg_catalog.clock_timestamp() < gp.v_first + pg_catalog.make_interval(secs => gp.v_grace))
           AND NOT EXISTS (
            SELECT 1 FROM public.aml_screening_requests r
             WHERE r.tenant_id = p_tenant AND r.person_id = b.person_id AND r.status = 'done' AND r.outcome = 'clear'
               AND r.completed_at >= gp.v_first AND (b.v_last_match IS NULL OR r.completed_at > b.v_last_match)
               AND public.aml_screening_covers(LEAST(r.completed_at, r.screened_at), r.screened_interval_seconds, r.screened_lists, gp.v_interval, gp.v_lists,
                     public.capa_since_g(p_tenant, gp.jur, LEAST(r.claimed_at, r.completed_at, r.screened_at), gp.v_lists, r.claim_policy_id, gp.last_started, gp.last_id))
            UNION ALL
            SELECT 1 FROM public.aml_screening_requests r
             WHERE r.tenant_id = p_tenant AND r.person_id = b.person_id AND r.status = 'needs_officer' AND r.reviewed_from_request_id IS NOT NULL
               AND r.completed_at >= gp.v_first AND (b.v_last_match IS NULL OR r.completed_at > b.v_last_match)
               AND public.aml_screening_covers(LEAST(r.completed_at, r.screened_at), r.screened_interval_seconds, r.screened_lists, gp.v_interval, gp.v_lists,
                     public.capa_since_g(p_tenant, gp.jur, LEAST(r.claimed_at, r.completed_at, r.screened_at), gp.v_lists, r.claim_policy_id, gp.last_started, gp.last_id))
            UNION ALL
            SELECT 1 FROM public.aml_screening_results x
             WHERE x.tenant_id = p_tenant AND x.person_id = b.person_id AND x.outcome = 'clear' AND NOT b.v_sanc
               AND x.recorded_at >= gp.v_first AND (b.v_last_match IS NULL OR x.recorded_at > b.v_last_match)
               AND public.aml_screening_covers(LEAST(x.recorded_at, x.screened_at), x.screened_interval_seconds, x.lists, gp.v_interval, gp.v_lists,
                     public.capa_since_g(p_tenant, gp.jur, LEAST(x.recorded_at, x.screened_at), gp.v_lists, NULL::uuid, gp.last_started, gp.last_id))
            UNION ALL
            SELECT 1 FROM public.aml_screening_results x
              JOIN public.aml_hold_lifts l ON l.tenant_id = x.tenant_id AND l.hold_id = x.hold_id
             WHERE x.tenant_id = p_tenant AND x.person_id = b.person_id AND x.outcome IN ('potential_match', 'confirmed')
               AND (NOT b.v_sanc OR x.hold_reason_class IN ('confirmed_sanctions', 'sanctions_potential'))
               AND l.lifted_at >= gp.v_first AND (b.v_last_match IS NULL OR l.lifted_at > b.v_last_match)
               AND public.aml_screening_covers(LEAST(x.recorded_at, x.screened_at), x.screened_interval_seconds, x.lists, gp.v_interval, gp.v_lists,
                     public.capa_since_g(p_tenant, gp.jur, LEAST(x.recorded_at, x.screened_at), gp.v_lists, NULL::uuid, gp.last_started, gp.last_id))
           ))
$$;
