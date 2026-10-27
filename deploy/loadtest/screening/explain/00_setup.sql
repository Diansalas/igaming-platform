-- MANUAL RECONSTRUCTIONS of the inner statements of the plpgsql functions (pg_stat_statements / auto_explain are not available to a
-- non-superuser and this harness does not escalate). Each statement below is copied from the function body in
-- migrations/0192-0194 (aml_screening_gate_denies, aml_screening_since, aml_screening_enqueue_due, aml_screening_gate_overdue,
-- aml_screening_parked_overdue, aml_screening_parked_summary, aml_screening_request_claim) with the plpgsql variables replaced by
-- literals taken from the generated data. Run as the table owner on a PRIVATE scratch database with the tenant setting applied.
\set ON_ERROR_STOP on
\pset pager off
\timing off
SELECT id AS tid FROM tenants WHERE status = 'active' ORDER BY created_at, slug LIMIT 1 \gset
SELECT set_config('app.tenant_id', :'tid', false) \gset
SELECT person_id AS xp FROM aml_screening_requests WHERE tenant_id = :'tid' AND status = 'done' AND completed_at < now() - interval '15 days' ORDER BY person_id LIMIT 1 \gset
SELECT person_id AS cp FROM aml_screening_requests WHERE tenant_id = :'tid' AND status = 'done' AND completed_at > now() - interval '2 days' ORDER BY person_id LIMIT 1 \gset
SELECT licensing_jurisdiction_id AS jur FROM aml_screening_policies WHERE tenant_id = :'tid' ORDER BY created_at DESC LIMIT 1 \gset
SELECT min(effective_from) AS vfirst FROM aml_screening_trigger_policies WHERE tenant_id = :'tid' AND gate_withdrawal \gset
SELECT lists::text AS vlists, rescreen_interval_seconds AS vint, id AS vpol FROM aml_screening_policies WHERE tenant_id = :'tid' AND licensing_jurisdiction_id = :'jur' ORDER BY created_at DESC, id DESC LIMIT 1 \gset
SELECT count(*) AS npersons FROM (SELECT DISTINCT person_id FROM player_accounts WHERE tenant_id = :'tid') q \gset
SELECT count(*) AS nversions FROM aml_screening_policies WHERE tenant_id = :'tid' AND licensing_jurisdiction_id = :'jur' \gset
SELECT count(*) AS nrequests FROM aml_screening_requests WHERE tenant_id = :'tid' \gset
\echo 'persons=' :npersons ' screening_policy_versions=' :nversions ' requests=' :nrequests
SHOW work_mem;
SHOW shared_buffers;
