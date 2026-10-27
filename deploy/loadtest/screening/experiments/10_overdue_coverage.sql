-- MEASUREMENT: aml_screening_gate_overdue(tenant, 5000) takes "SELECT DISTINCT person_id ... LIMIT 5000" (no ORDER BY). On a tenant with more than 5000
-- persons it evaluates only an arbitrary 5000 of them. Compare with the limit 20000 (the function maximum) on a 10,000-person tenant.
\set ON_ERROR_STOP on
SELECT id AS tid FROM tenants WHERE status = 'active' ORDER BY created_at, slug LIMIT 1 \gset
SELECT set_config('app.tenant_id', :'tid', false) \gset
\timing on
SELECT aml_screening_gate_overdue(:'tid', 5000) AS overdue_limit_5000;
SELECT aml_screening_gate_overdue(:'tid', 5000) AS overdue_limit_5000;
SELECT aml_screening_gate_overdue(:'tid', 20000) AS overdue_limit_20000;
