-- EXPERIMENT (private scratch database). Same 4M-decision fixture as 08. HYPOTHESES tested:
--  H1 the spill (Sort Method: external merge) of aml_screening_parked_summary is a work_mem effect: SET LOCAL work_mem only in this session;
--  H2 a per-person LATERAL "latest decision" read + a partial index makes the persons count independent of decision volume (equivalent output).
\set ON_ERROR_STOP on
\pset pager off
SELECT id AS tid FROM tenants WHERE status = 'active' ORDER BY created_at, slug LIMIT 1 \gset
SELECT set_config('app.tenant_id', :'tid', false) \gset
SELECT aml_screening_gate_overdue(:'tid', 5000) AS warm \gset
CREATE TEMP TABLE accs AS SELECT row_number() OVER (ORDER BY id) AS rn, id AS aid, person_id AS pid FROM player_accounts WHERE tenant_id = :'tid';
SELECT count(*) AS n_accounts FROM accs \gset
INSERT INTO aml_enforcement_decisions (tenant_id, player_account_id, person_id, operation, kyc_outcome, hold_outcome, public_outcome, allowed, correlation_id, decided_at)
SELECT :'tid', a.aid, a.pid, 'withdrawal_payout', 'passed', CASE WHEN g % 10 < 7 THEN 'held' ELSE 'none' END, CASE WHEN g % 10 < 7 THEN 'review' ELSE 'passed' END, g % 10 >= 7,
       gen_random_uuid(), now() - (random() * 7) * interval '1 day'
  FROM generate_series(1, 4000000) g JOIN accs a ON a.rn = 1 + (g % :n_accounts);
VACUUM (ANALYZE) aml_enforcement_decisions;
\echo '--- 4M decisions, production function (twice):'
\timing on
SELECT * FROM aml_screening_parked_summary(:'tid', clock_timestamp() - interval '7 days');
SELECT * FROM aml_screening_parked_summary(:'tid', clock_timestamp() - interval '7 days');
\echo '--- H1: the same statement text of the function, session work_mem 4MB (default) vs 256MB (SET, hypothesis only):'
SHOW work_mem;
EXPLAIN (ANALYZE, BUFFERS) WITH latest AS (SELECT DISTINCT ON (d.person_id) d.person_id, d.hold_outcome, d.hold_id, d.kyc_outcome, d.allowed FROM aml_enforcement_decisions d
 WHERE d.tenant_id = :'tid' AND d.operation = 'withdrawal_payout' AND d.decided_at >= clock_timestamp() - interval '7 days' ORDER BY d.person_id, d.decided_at DESC, d.id DESC)
SELECT (SELECT count(*)::integer FROM latest l WHERE l.hold_outcome = 'held' AND l.hold_id IS NULL AND NOT l.allowed AND l.kyc_outcome IN ('passed', 'not_required'));
SET work_mem = '256MB';
EXPLAIN (ANALYZE, BUFFERS) WITH latest AS (SELECT DISTINCT ON (d.person_id) d.person_id, d.hold_outcome, d.hold_id, d.kyc_outcome, d.allowed FROM aml_enforcement_decisions d
 WHERE d.tenant_id = :'tid' AND d.operation = 'withdrawal_payout' AND d.decided_at >= clock_timestamp() - interval '7 days' ORDER BY d.person_id, d.decided_at DESC, d.id DESC)
SELECT (SELECT count(*)::integer FROM latest l WHERE l.hold_outcome = 'held' AND l.hold_id IS NULL AND NOT l.allowed AND l.kyc_outcome IN ('passed', 'not_required'));
RESET work_mem;
\echo '--- H2: partial index + LATERAL latest-per-person (persons count) and index-only count (evaluations)'
CREATE INDEX capa_decisions_person ON aml_enforcement_decisions (tenant_id, person_id, decided_at DESC, id DESC) INCLUDE (hold_outcome, hold_id, kyc_outcome, allowed) WHERE operation = 'withdrawal_payout';
VACUUM (ANALYZE) aml_enforcement_decisions;
CREATE FUNCTION capa_parked_summary_v2(p_tenant uuid, p_since timestamptz) RETURNS TABLE (o_persons integer, o_evaluations integer) LANGUAGE sql STABLE AS $$
    SELECT (SELECT count(*)::integer FROM (SELECT DISTINCT a.person_id FROM public.player_accounts a WHERE a.tenant_id = p_tenant) p
             CROSS JOIN LATERAL (SELECT d.hold_outcome, d.hold_id, d.kyc_outcome, d.allowed FROM public.aml_enforcement_decisions d
                                  WHERE d.tenant_id = p_tenant AND d.person_id = p.person_id AND d.operation = 'withdrawal_payout' AND d.decided_at >= p_since
                                  ORDER BY d.decided_at DESC, d.id DESC LIMIT 1) l
             WHERE l.hold_outcome = 'held' AND l.hold_id IS NULL AND NOT l.allowed AND l.kyc_outcome IN ('passed', 'not_required')),
           (SELECT count(*)::integer FROM public.aml_enforcement_decisions d
             WHERE d.tenant_id = p_tenant AND d.operation = 'withdrawal_payout' AND d.decided_at >= p_since
               AND d.hold_outcome = 'held' AND d.hold_id IS NULL AND NOT d.allowed AND d.kyc_outcome IN ('passed', 'not_required'))
$$;
\echo 'NOTE: capa_parked_summary_v2 reads persons from player_accounts: it differs from the original only for a decision whose person has no account in the tenant (impossible: FK + tenant).'
SELECT * FROM aml_screening_parked_summary(:'tid', now() - interval '7 days');
SELECT * FROM capa_parked_summary_v2(:'tid', now() - interval '7 days');
SELECT * FROM capa_parked_summary_v2(:'tid', now() - interval '7 days');
\timing off
\echo '--- equivalence of the two functions for several windows (same p_since passed to both):'
SELECT w, (SELECT row(o_persons, o_evaluations)::text FROM aml_screening_parked_summary(:'tid', s)) AS original, (SELECT row(o_persons, o_evaluations)::text FROM capa_parked_summary_v2(:'tid', s)) AS candidate,
       (SELECT row(o_persons, o_evaluations)::text FROM aml_screening_parked_summary(:'tid', s)) = (SELECT row(o_persons, o_evaluations)::text FROM capa_parked_summary_v2(:'tid', s)) AS equal
  FROM (SELECT d AS w, date_trunc('minute', now()) - make_interval(days => d) AS s FROM generate_series(1,7) d) q;
