-- Differential test for 01_since_v2.sql: random policy histories (random lists, intervals, effective_from before AND after created_at,
-- ties in created_at, versions not yet started), random inputs (p_at, current lists, after-policy: a version of the jurisdiction, of another
-- jurisdiction, a random uuid, or NULL). Run on a clone of capa_base: ./run.sh explain-src capa_base experiments/02_since_equiv.sql
\set ON_ERROR_STOP on
\i 01_since_v2.sql
\if :{?seed}\else\set seed 0.4242\endif
SELECT setseed(:seed);
CREATE TEMP TABLE eq_t AS SELECT md5('eqt'||g)::uuid AS tid, g FROM generate_series(1,4) g;
CREATE TEMP TABLE eq_j AS SELECT md5('eqj'||g)::uuid AS jid, g FROM generate_series(1,3) g;
ALTER TABLE jurisdictions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE tenants NO FORCE ROW LEVEL SECURITY;
ALTER TABLE aml_screening_policies NO FORCE ROW LEVEL SECURITY;
INSERT INTO jurisdictions (id, code, name) SELECT jid, 'EQ-'||g, 'equiv' FROM eq_j;
INSERT INTO tenants (id,name,slug,licensing_model,status) SELECT tid,'eq','eq-'||g,'under_platform_licence','active' FROM eq_t;
ALTER TABLE aml_screening_policies DISABLE TRIGGER USER;
ALTER TABLE tenants DISABLE TRIGGER USER;
CREATE TEMP TABLE eq_v AS
SELECT t.tid, j.jid, k,
       -- creation instants: whole seconds (ties are common), spread over 200 days
       (date_trunc('second', now() - (random()*200) * interval '1 day')) AS created_at,
       string_to_array((ARRAY['sanctions','pep','sanctions,pep'])[1+floor(random()*3)::int], ',') AS lists,
       (1+floor(random()*100)::int) * 86400::bigint AS iv,
       (random()*20 - 10) * interval '1 day' AS eff_off
  FROM eq_t t CROSS JOIN eq_j j CROSS JOIN LATERAL generate_series(1, 5 + (t.g * j.g * 7) % 55) k;
INSERT INTO aml_screening_policies (id,tenant_id,licensing_jurisdiction_id,lists,rescreen_interval_seconds,backfill_grace_seconds,effective_from,legal_review_reference,reason_code,created_by,created_by_person_id,created_by_scope,created_at)
SELECT md5('eqv'||tid||jid||k)::uuid, tid, jid, lists, iv, 0, created_at + eff_off, 'EQ','eq', md5('s')::uuid, md5('p')::uuid, 'platform_acting', created_at FROM eq_v;
ALTER TABLE aml_screening_policies ENABLE TRIGGER USER;
ALTER TABLE tenants ENABLE TRIGGER USER;
ALTER TABLE jurisdictions FORCE ROW LEVEL SECURITY;
ALTER TABLE tenants FORCE ROW LEVEL SECURITY;
ALTER TABLE aml_screening_policies FORCE ROW LEVEL SECURITY;
SELECT count(*) AS versions, count(DISTINCT created_at) AS distinct_created_at FROM aml_screening_policies;
-- inputs
CREATE TEMP TABLE eq_in AS
SELECT i, t.tid, j.jid,
       now() - (random()*230) * interval '1 day' + (random()*10) * interval '1 day' AS p_at,
       (CASE floor(random()*5)::int WHEN 0 THEN ARRAY['sanctions'] WHEN 1 THEN ARRAY['pep'] WHEN 2 THEN ARRAY['sanctions','pep'] WHEN 3 THEN NULL::text[] ELSE ARRAY[]::text[] END) AS cur,
       floor(random()*4)::int AS after_kind
  FROM generate_series(1,6000) i
  JOIN LATERAL (SELECT tid FROM eq_t ORDER BY md5(i::text||'t') LIMIT 1) t ON true
  JOIN LATERAL (SELECT jid FROM eq_j ORDER BY md5(i::text||'j') LIMIT 1) j ON true;
CREATE TEMP TABLE eq_run AS
SELECT x.i, x.tid, x.jid, x.p_at, x.cur,
       CASE x.after_kind WHEN 0 THEN NULL
            WHEN 1 THEN (SELECT id FROM aml_screening_policies p WHERE p.tenant_id=x.tid AND p.licensing_jurisdiction_id=x.jid ORDER BY md5(p.id::text||x.i::text) LIMIT 1)
            WHEN 2 THEN (SELECT id FROM aml_screening_policies p WHERE p.tenant_id=x.tid AND p.licensing_jurisdiction_id<>x.jid ORDER BY md5(p.id::text||x.i::text) LIMIT 1)
            ELSE md5('nope'||x.i)::uuid END AS after_policy
  FROM eq_in x;
SELECT set_config('app.tenant_id','',false);
CREATE TEMP TABLE eq_res (i int, a bigint, b bigint);
DO $$
DECLARE r record; a bigint; b bigint;
BEGIN
  FOR r IN SELECT * FROM eq_run LOOP
    PERFORM set_config('app.tenant_id', r.tid::text, true);
    a := public.aml_screening_since(r.tid, r.jid, r.p_at, r.cur, r.after_policy);
    b := public.capa_since_v2(r.tid, r.jid, r.p_at, r.cur, r.after_policy);
    INSERT INTO eq_res VALUES (r.i, a, b);
  END LOOP;
END $$;
SELECT count(*) AS inputs, count(*) FILTER (WHERE a IS DISTINCT FROM b) AS mismatches,
       count(*) FILTER (WHERE a IS NULL) AS null_results, count(*) FILTER (WHERE a = 0) AS zero_results, count(*) FILTER (WHERE a > 0) AS positive_results FROM eq_res;
