-- Full-tenant comparison on a seeded scenario DB (./run.sh explain fn-10k experiments/05_compare_fn10k.sql): the denied SET and the time of
-- (a) the production per-person plpgsql path, (b) the set-based candidate, for the first 500 / 5000 / all persons in person_id order.
\set ON_ERROR_STOP on
\i 03_gate_set.sql
SELECT id AS tid FROM tenants WHERE status = 'active' ORDER BY created_at, slug LIMIT 1 \gset
SELECT set_config('app.tenant_id', :'tid', false) \gset
\timing on
-- (a) production path
CREATE TEMP TABLE old_500 AS SELECT pa.person_id FROM (SELECT DISTINCT person_id FROM player_accounts WHERE tenant_id = :'tid' ORDER BY person_id LIMIT 500) pa WHERE aml_screening_gate_denies(:'tid', pa.person_id, 'withdrawal');
CREATE TEMP TABLE old_5000 AS SELECT pa.person_id FROM (SELECT DISTINCT person_id FROM player_accounts WHERE tenant_id = :'tid' ORDER BY person_id LIMIT 5000) pa WHERE aml_screening_gate_denies(:'tid', pa.person_id, 'withdrawal');
-- (b) candidate
CREATE TEMP TABLE new_500 AS SELECT * FROM capa_gate_denied_set(:'tid', ARRAY(SELECT DISTINCT person_id FROM player_accounts WHERE tenant_id = :'tid' ORDER BY person_id LIMIT 500));
CREATE TEMP TABLE new_5000 AS SELECT * FROM capa_gate_denied_set(:'tid', ARRAY(SELECT DISTINCT person_id FROM player_accounts WHERE tenant_id = :'tid' ORDER BY person_id LIMIT 5000));
-- repeat (b) once more for a warm timing
CREATE TEMP TABLE new_5000b AS SELECT * FROM capa_gate_denied_set(:'tid', ARRAY(SELECT DISTINCT person_id FROM player_accounts WHERE tenant_id = :'tid' ORDER BY person_id LIMIT 5000));
\timing off
SELECT (SELECT count(*) FROM old_500) AS old_500, (SELECT count(*) FROM new_500) AS new_500,
       (SELECT count(*) FROM (SELECT * FROM old_500 EXCEPT SELECT * FROM new_500) q) + (SELECT count(*) FROM (SELECT * FROM new_500 EXCEPT SELECT * FROM old_500) q) AS diff_500,
       (SELECT count(*) FROM old_5000) AS old_5000, (SELECT count(*) FROM new_5000) AS new_5000,
       (SELECT count(*) FROM (SELECT * FROM old_5000 EXCEPT SELECT * FROM new_5000) q) + (SELECT count(*) FROM (SELECT * FROM new_5000 EXCEPT SELECT * FROM old_5000) q) AS diff_5000;
\timing on
CREATE TEMP TABLE old_all AS SELECT pa.person_id FROM (SELECT DISTINCT person_id FROM player_accounts WHERE tenant_id = :'tid') pa WHERE aml_screening_gate_denies(:'tid', pa.person_id, 'withdrawal');
CREATE TEMP TABLE new_all AS SELECT * FROM capa_gate_denied_set(:'tid', ARRAY(SELECT DISTINCT person_id FROM player_accounts WHERE tenant_id = :'tid'));
\timing off
SELECT (SELECT count(*) FROM old_all) AS old_all, (SELECT count(*) FROM new_all) AS new_all,
       (SELECT count(*) FROM (SELECT * FROM old_all EXCEPT SELECT * FROM new_all) q) + (SELECT count(*) FROM (SELECT * FROM new_all EXCEPT SELECT * FROM old_all) q) AS diff_all;
