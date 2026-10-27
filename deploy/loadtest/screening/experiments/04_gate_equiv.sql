-- Randomised differential test: aml_screening_gate_denies (production, per person) vs capa_gate_denied_set (candidate, 03_gate_set.sql).
-- Run on a clone of capa_base (nothing seeded):  ./run.sh explain-src capa_base experiments/04_gate_equiv.sql
-- Fixture (SYNTHETIC, setseed fixed): 3 tenants x 3 jurisdictions; per (tenant, jurisdiction) 1-4 trigger-policy versions (gate on/off, effective_from
-- before/after created_at, some not yet effective) and 0-6 screening-policy versions (random lists / intervals / grace); 400 persons per tenant
-- (10% with a second account), account ages 0-200 d; per person 0-4 requests (done clear with random provenance / claim data incl. NULLs, open
-- needs_officer matches incl. confirmed/sanctions, auto-reviewed re-flags, pending, failed_terminal), 0-3 results (clear with/without provenance,
-- errors, matches with holds, half of them lifted), officer resolutions of open matches under the request's idempotency key.
\set ON_ERROR_STOP on
\i 03_gate_set.sql
\if :{?seed}\else\set seed 0.2718\endif
SELECT setseed(:seed);
ALTER TABLE jurisdictions NO FORCE ROW LEVEL SECURITY; ALTER TABLE tenants NO FORCE ROW LEVEL SECURITY; ALTER TABLE brands NO FORCE ROW LEVEL SECURITY;
ALTER TABLE persons NO FORCE ROW LEVEL SECURITY; ALTER TABLE player_accounts NO FORCE ROW LEVEL SECURITY; ALTER TABLE aml_screening_policies NO FORCE ROW LEVEL SECURITY;
ALTER TABLE aml_screening_trigger_policies NO FORCE ROW LEVEL SECURITY; ALTER TABLE aml_screening_requests NO FORCE ROW LEVEL SECURITY;
ALTER TABLE aml_screening_results NO FORCE ROW LEVEL SECURITY; ALTER TABLE aml_holds NO FORCE ROW LEVEL SECURITY; ALTER TABLE aml_hold_lifts NO FORCE ROW LEVEL SECURITY;
ALTER TABLE aml_screening_policies DISABLE TRIGGER USER; ALTER TABLE aml_screening_trigger_policies DISABLE TRIGGER USER; ALTER TABLE aml_screening_requests DISABLE TRIGGER USER;
ALTER TABLE aml_screening_results DISABLE TRIGGER USER; ALTER TABLE aml_holds DISABLE TRIGGER USER; ALTER TABLE aml_hold_lifts DISABLE TRIGGER USER;
ALTER TABLE tenants DISABLE TRIGGER USER; ALTER TABLE brands DISABLE TRIGGER USER; ALTER TABLE player_accounts DISABLE TRIGGER USER;

CREATE TEMP TABLE eq_t AS SELECT md5('eqt'||g)::uuid AS tid, md5('eqb'||g)::uuid AS bid, g FROM generate_series(1,3) g;
CREATE TEMP TABLE eq_j AS SELECT md5('eqj'||g)::uuid AS jid, g FROM generate_series(1,3) g;
INSERT INTO jurisdictions (id, code, name) SELECT jid, 'EQG-'||g, 'equiv' FROM eq_j;
INSERT INTO tenants (id,name,slug,licensing_model,status) SELECT tid,'eq','eqg-'||g,'under_platform_licence','active' FROM eq_t;
INSERT INTO brands (id,tenant_id,name,slug,status) SELECT bid,tid,'eq','eqg-b-'||g,'active' FROM eq_t;
-- trigger policy versions
INSERT INTO aml_screening_trigger_policies (id,tenant_id,licensing_jurisdiction_id,events,gate_withdrawal,gate_alert_after_seconds,effective_from,legal_review_reference,reason_code,created_by,created_by_person_id,created_by_scope,created_at)
SELECT md5('eqtp'||t.tid||j.jid||k)::uuid, t.tid, j.jid, ARRAY['scheduled'], gate, CASE WHEN gate THEN 3600 END,
       ca + (random()*10 - 5) * interval '1 day', 'EQ','eq',md5('s')::uuid,md5('p')::uuid,'platform_acting', ca
  FROM eq_t t CROSS JOIN eq_j j CROSS JOIN LATERAL generate_series(0, floor(random()*4)::int) k
  CROSS JOIN LATERAL (SELECT now() - (180 - 40*k) * interval '1 day' AS ca, random() < 0.6 AS gate) r;
-- screening policy versions (jurisdiction 1 of each tenant always has at least one)
INSERT INTO aml_screening_policies (id,tenant_id,licensing_jurisdiction_id,lists,rescreen_interval_seconds,backfill_grace_seconds,effective_from,legal_review_reference,reason_code,created_by,created_by_person_id,created_by_scope,created_at)
SELECT md5('eqsp'||t.tid||j.jid||k)::uuid, t.tid, j.jid, string_to_array((ARRAY['sanctions','pep','sanctions,pep','sanctions,pep','sanctions,pep'])[1+floor(random()*5)::int], ','),
       (20+floor(random()*80)::int)*86400::bigint, floor(random()*30)::bigint*86400, ca + (random()*6 - 3) * interval '1 day','EQ','eq',md5('s')::uuid,md5('p')::uuid,'platform_acting', ca
  FROM eq_t t CROSS JOIN eq_j j CROSS JOIN LATERAL generate_series(1, CASE WHEN j.g = 1 THEN 1 + floor(random()*6)::int ELSE floor(random()*6)::int END) k
  CROSS JOIN LATERAL (SELECT now() - (190 - 30*k) * interval '1 day' + random()*5 * interval '1 day' AS ca) r;
-- persons and accounts
CREATE TEMP TABLE eq_p AS SELECT t.tid, t.bid, md5('eqp'||t.g||'-'||i)::uuid AS pid, md5('eqa'||t.g||'-'||i)::uuid AS aid, i,
       now() - (random()*200) * interval '1 day' - interval '1 hour' AS reg FROM eq_t t CROSS JOIN generate_series(1,400) i;
INSERT INTO persons (id) SELECT pid FROM eq_p;
INSERT INTO player_accounts (id,tenant_id,brand_id,person_id,email,password_hash,status,created_at,updated_at) SELECT aid,tid,bid,pid,'e'||i||'-'||tid||'@eq.invalid','!x','active',reg,reg FROM eq_p;
INSERT INTO player_accounts (id,tenant_id,brand_id,person_id,email,password_hash,status,created_at,updated_at)
SELECT md5('eqa2'||pid)::uuid,tid,bid,pid,'e2-'||i||'-'||tid||'@eq.invalid','!x','active',reg + random()*30*interval '1 day',reg FROM eq_p WHERE i % 10 = 0;
-- a helper: random policy id / lists for a tenant
CREATE TEMP TABLE eq_pol AS SELECT id, tenant_id, rescreen_interval_seconds, lists FROM aml_screening_policies;
CREATE TEMP TABLE eq_req AS
SELECT p.tid, p.pid, n, md5('eqr'||p.pid||n)::uuid AS rid, random() AS r1, random() AS r2, random() AS r3, random() AS r4,
       (SELECT jid FROM eq_j ORDER BY md5(p.pid::text||n::text||'j') LIMIT 1) AS jid,
       now() - (random()*60) * interval '1 day' AS ca,
       string_to_array((ARRAY['sanctions','pep','sanctions,pep','sanctions,pep','sanctions,pep'])[1+floor(random()*5)::int], ',') AS lists,
       (30+floor(random()*90)::int)*86400::bigint AS iv,
       (SELECT id FROM eq_pol q WHERE q.tenant_id = p.tid ORDER BY md5(p.pid::text||n::text||'pp') LIMIT 1) AS pol,
       (SELECT id FROM eq_pol q WHERE q.tenant_id = p.tid ORDER BY md5(p.pid::text||n::text||'cp') LIMIT 1) AS cpol
  FROM eq_p p CROSS JOIN LATERAL generate_series(1, floor(random()*5)::int) n;
-- done clear (r1 < .55), open needs_officer (< .70), pending (< .80), failed_terminal (< .88), done clear (rest)
INSERT INTO aml_screening_requests (id,tenant_id,person_id,licensing_jurisdiction_id,trigger,idempotency_key,status,attempts,next_attempt_at,outcome,provider_id,provider_reference,subject_ref,screened_at,completed_at,created_at,updated_at,screened_policy_id,screened_lists,screened_interval_seconds,claimed_at,claim_policy_id)
SELECT rid,tid,pid,jid,'scheduled','eq:'||rid,'done',1,ca,'clear','mock-screening','x',md5(pid::text)||md5(rid::text),ca - interval '1 second',ca,ca - interval '10 seconds',ca,
       pol, lists, iv, CASE WHEN r3 < 0.85 THEN ca - (1 + r4*10) * interval '1 second' END, CASE WHEN r2 < 0.75 THEN cpol END
  FROM eq_req WHERE r1 < 0.55 OR r1 >= 0.88;
INSERT INTO aml_screening_requests (id,tenant_id,person_id,licensing_jurisdiction_id,trigger,idempotency_key,status,attempts,next_attempt_at,outcome,match_lists,provider_id,provider_reference,subject_ref,screened_at,completed_at,match_fingerprint,created_at,updated_at,screened_policy_id,screened_lists,screened_interval_seconds,claimed_at,claim_policy_id)
SELECT rid,tid,pid,jid,'scheduled','eq:'||rid,'needs_officer',1,ca,
       CASE WHEN r2 < 0.25 THEN 'confirmed' ELSE 'potential_match' END,
       CASE WHEN r2 < 0.25 THEN ARRAY['sanctions'] WHEN r3 < 0.4 THEN ARRAY['pep'] WHEN r3 < 0.7 THEN ARRAY['sanctions'] ELSE ARRAY['sanctions','pep'] END,
       'mock-screening','x',md5(pid::text)||md5(rid::text),ca - interval '1 second',ca, md5('f'||rid)||md5('g'||rid), ca - interval '10 seconds',ca,
       pol, lists, iv, ca - interval '3 seconds', cpol
  FROM eq_req WHERE r1 >= 0.55 AND r1 < 0.70;
INSERT INTO aml_screening_requests (id,tenant_id,person_id,licensing_jurisdiction_id,trigger,idempotency_key,status,attempts,next_attempt_at,created_at)
SELECT rid,tid,pid,jid,'scheduled','eq:'||rid,'pending',0,ca,ca FROM eq_req WHERE r1 >= 0.70 AND r1 < 0.80;
INSERT INTO aml_screening_requests (id,tenant_id,person_id,licensing_jurisdiction_id,trigger,idempotency_key,status,attempts,next_attempt_at,last_error_code,completed_at,created_at)
SELECT rid,tid,pid,jid,'scheduled','eq:'||rid,'failed_terminal',5,ca,'timeout',ca,ca FROM eq_req WHERE r1 >= 0.80 AND r1 < 0.88;
-- auto-reviewed re-flags (reviewed_from = an open match of the same person), later than that match
INSERT INTO aml_screening_requests (id,tenant_id,person_id,licensing_jurisdiction_id,trigger,idempotency_key,status,attempts,next_attempt_at,outcome,match_lists,provider_id,provider_reference,subject_ref,screened_at,completed_at,match_fingerprint,reviewed_from_request_id,created_at,updated_at,screened_policy_id,screened_lists,screened_interval_seconds,claimed_at,claim_policy_id)
SELECT md5('eqrv'||b.id)::uuid,b.tenant_id,b.person_id,b.licensing_jurisdiction_id,'scheduled','eqv:'||b.id,'needs_officer',1,b.completed_at,b.outcome,b.match_lists,'mock-screening','x',b.subject_ref,
       LEAST(now() - interval '1 minute', b.completed_at + interval '10 days'), LEAST(now() - interval '1 minute', b.completed_at + interval '10 days'), b.match_fingerprint, b.id, b.created_at, b.updated_at,
       b.screened_policy_id, b.screened_lists, b.screened_interval_seconds, b.claimed_at, b.claim_policy_id
  FROM aml_screening_requests b WHERE b.status = 'needs_officer' AND b.reviewed_from_request_id IS NULL AND random() < 0.4;
-- results: clear (with / without provenance), error
INSERT INTO aml_screening_results (id,tenant_id,owner_scope,person_id,idempotency_key,trigger,provider_id,subject_ref,lists,outcome,screened_at,decision,payload_hash,recorded_by,recorded_by_person_id,recorded_by_scope,recorded_at,screened_policy_id,screened_interval_seconds)
SELECT md5('eqx'||pid||n)::uuid,tid,'platform',pid,'eqx:'||pid||':'||n,'manual','mock-screening',md5(pid::text)||md5(n::text),lists,'clear',ca - interval '5 seconds','none',md5('h'||pid||n)||md5('i'||pid||n),
       md5('s')::uuid,md5('p')::uuid,'platform_acting',ca, CASE WHEN r2 < 0.6 THEN pol END, CASE WHEN r2 < 0.6 THEN iv END
  FROM eq_req WHERE r3 < 0.35;
INSERT INTO aml_screening_results (id,tenant_id,owner_scope,person_id,idempotency_key,trigger,provider_id,subject_ref,lists,outcome,error_code,screened_at,decision,payload_hash,recorded_by,recorded_by_person_id,recorded_by_scope,recorded_at)
SELECT md5('eqe'||pid||n)::uuid,tid,'platform',pid,'eqe:'||pid||':'||n,'manual','mock-screening',md5(pid::text)||md5(n::text),lists,'error','timeout',ca,'none',md5('j'||pid||n)||md5('k'||pid||n),
       md5('s')::uuid,md5('p')::uuid,'platform_acting',ca FROM eq_req WHERE r4 < 0.12;
-- matches with holds, half lifted
CREATE TEMP TABLE eq_h AS
SELECT md5('eqh'||pid||n)::uuid AS hid, tid, pid, n, ca, lists, iv, pol, random() < 0.5 AS lifted, random() < 0.7 AS prov,
       string_to_array((ARRAY['sanctions','pep','sanctions,pep','sanctions,pep','sanctions,pep'])[1+floor(random()*5)::int], ',') AS ml, random() < 0.3 AS confirmed
  FROM eq_req WHERE r4 > 0.88;
INSERT INTO aml_holds (id,tenant_id,owner_scope,person_id,hold_scope,reason_class,reason_code,placed_by,placed_by_person_id,placed_by_scope,placed_at)
SELECT hid,tid,'platform',pid,CASE WHEN 'sanctions' = ANY(ml) THEN 'all_value_movement' ELSE 'withdrawal_and_payout' END,
       CASE WHEN 'sanctions' = ANY(ml) THEN 'sanctions_potential' ELSE 'manual' END,'eq',md5('s')::uuid,md5('p')::uuid,'platform_acting', ca FROM eq_h;
INSERT INTO aml_screening_results (id,tenant_id,owner_scope,person_id,idempotency_key,trigger,provider_id,subject_ref,lists,outcome,match_lists,screened_at,decision,hold_id,hold_reason_class,payload_hash,recorded_by,recorded_by_person_id,recorded_by_scope,recorded_at,screened_policy_id,screened_interval_seconds)
SELECT md5('eqm'||pid||n)::uuid,tid,'platform',pid,'eqm:'||pid||':'||n,'manual','mock-screening',md5(pid::text)||md5(n::text),lists,'potential_match',ml,ca,
       CASE WHEN 'sanctions' = ANY(ml) THEN 'hold_all_value_movement' ELSE 'hold_withdrawal_and_payout' END, hid,
       CASE WHEN 'sanctions' = ANY(ml) THEN 'sanctions_potential' ELSE 'manual' END, md5('m'||pid||n)||md5('n'||pid||n),md5('s')::uuid,md5('p')::uuid,'platform_acting',ca,
       CASE WHEN prov THEN pol END, CASE WHEN prov THEN iv END FROM eq_h;
INSERT INTO aml_hold_lifts (id,hold_id,tenant_id,owner_scope,lifted_by,lifted_by_person_id,lifted_by_scope,lifted_at,reason_code)
SELECT md5('eql'||hid)::uuid,hid,tid,'platform',md5('s')::uuid,md5('p')::uuid,'platform_acting', LEAST(now() - interval '1 minute', ca + random()*60*interval '1 day'),'eq' FROM eq_h WHERE lifted;
-- officer resolutions of open matches: a non-error result under the request's idempotency key, at or after the request's completion
INSERT INTO aml_screening_results (id,tenant_id,owner_scope,person_id,idempotency_key,trigger,provider_id,subject_ref,lists,outcome,screened_at,decision,payload_hash,recorded_by,recorded_by_person_id,recorded_by_scope,recorded_at)
SELECT md5('eqres'||r.id)::uuid,r.tenant_id,'platform',r.person_id,r.idempotency_key,'manual','mock-screening',md5(r.id::text)||md5('res'),ARRAY['sanctions','pep'],'clear',r.completed_at,'none',
       md5('p'||r.id)||md5('q'||r.id),md5('s')::uuid,md5('p')::uuid,'platform_acting', LEAST(now() - interval '1 minute', r.completed_at + random()*20*interval '1 day')
  FROM aml_screening_requests r WHERE r.status = 'needs_officer' AND r.reviewed_from_request_id IS NULL AND random() < 0.35 AND r.idempotency_key !~ '^eqv:';
ALTER TABLE aml_screening_policies ENABLE TRIGGER USER; ALTER TABLE aml_screening_trigger_policies ENABLE TRIGGER USER; ALTER TABLE aml_screening_requests ENABLE TRIGGER USER;
ALTER TABLE aml_screening_results ENABLE TRIGGER USER; ALTER TABLE aml_holds ENABLE TRIGGER USER; ALTER TABLE aml_hold_lifts ENABLE TRIGGER USER;
ALTER TABLE tenants ENABLE TRIGGER USER; ALTER TABLE brands ENABLE TRIGGER USER; ALTER TABLE player_accounts ENABLE TRIGGER USER;
ALTER TABLE jurisdictions FORCE ROW LEVEL SECURITY; ALTER TABLE tenants FORCE ROW LEVEL SECURITY; ALTER TABLE brands FORCE ROW LEVEL SECURITY; ALTER TABLE persons FORCE ROW LEVEL SECURITY;
ALTER TABLE player_accounts FORCE ROW LEVEL SECURITY; ALTER TABLE aml_screening_policies FORCE ROW LEVEL SECURITY; ALTER TABLE aml_screening_trigger_policies FORCE ROW LEVEL SECURITY;
ALTER TABLE aml_screening_requests FORCE ROW LEVEL SECURITY; ALTER TABLE aml_screening_results FORCE ROW LEVEL SECURITY; ALTER TABLE aml_holds FORCE ROW LEVEL SECURITY; ALTER TABLE aml_hold_lifts FORCE ROW LEVEL SECURITY;

CREATE TEMP TABLE eq_old (tid uuid, pid uuid);
CREATE TEMP TABLE eq_new (tid uuid, pid uuid);
DO $$
DECLARE t record; b_set uuid[];
BEGIN
  FOR t IN SELECT tid FROM eq_t ORDER BY tid LOOP
    PERFORM set_config('app.tenant_id', t.tid::text, true);
    SELECT array_agg(pid) INTO b_set FROM eq_p WHERE tid = t.tid;
    INSERT INTO eq_old SELECT t.tid, x.pid FROM eq_p x WHERE x.tid = t.tid AND public.aml_screening_gate_denies(t.tid, x.pid, 'withdrawal');
    INSERT INTO eq_new SELECT t.tid, person_id FROM public.capa_gate_denied_set(t.tid, b_set);
  END LOOP;
END $$;
CREATE TEMP TABLE eq_res AS SELECT p.tid, p.pid, EXISTS (SELECT 1 FROM eq_old o WHERE o.pid = p.pid AND o.tid = p.tid) AS a,
       EXISTS (SELECT 1 FROM eq_new o WHERE o.pid = p.pid AND o.tid = p.tid) AS b FROM eq_p p;
SELECT count(*) AS persons, count(*) FILTER (WHERE a) AS denied_old, count(*) FILTER (WHERE b) AS denied_new, count(*) FILTER (WHERE a IS DISTINCT FROM b) AS mismatches FROM eq_res;
SELECT (SELECT count(*) FROM aml_screening_requests) AS requests, (SELECT count(*) FROM aml_screening_results) AS results, (SELECT count(*) FROM aml_holds) AS holds, (SELECT count(*) FROM aml_hold_lifts) AS lifts;
SELECT t.g AS tenant, count(*) FILTER (WHERE a) AS denied, count(*) FILTER (WHERE NOT a) AS allowed FROM eq_res r JOIN eq_t t ON t.tid = r.tid GROUP BY 1 ORDER BY 1;
