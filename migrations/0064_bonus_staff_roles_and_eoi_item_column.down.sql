ALTER TABLE bulk_grant_job_items DROP CONSTRAINT IF EXISTS bulk_grant_job_items_parent_operation_tenant_fk;
ALTER TABLE bulk_grant_job_items DROP COLUMN IF EXISTS parent_operation_id;

ALTER TABLE staff_users DROP CONSTRAINT staff_users_role_check;
ALTER TABLE staff_users ADD CONSTRAINT staff_users_role_check
    CHECK (role IN ('platform_admin', 'tenant_admin', 'support', 'compliance', 'finance', 'risk_manager'));
