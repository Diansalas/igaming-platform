-- Two small, independent, additive fixes flagged (not silently dropped)
-- by Phase 2 and this dispatch's own reading of the frozen design:

-- 1. Widen staff_users.role's CHECK to accept 'promotions_manager' and
--    'bonus_operations' (docs/security/security-architecture.md §B1.1's
--    "Role wiring", already real Go constants in internal/auth/
--    permission.go's RoleMap - RolePromotionsManager/RoleBonusOperations
--    - since migration 0041/Stage 4H-B1 Wave 2 Phase 2, but never
--    admitted by this CHECK constraint or by identity.StaffRole/
--    admin_routes.go's allowlist until now). Mirrors migration 0041's own
--    additive-widening shape for 'risk_manager'.
ALTER TABLE staff_users DROP CONSTRAINT staff_users_role_check;
ALTER TABLE staff_users
    ADD CONSTRAINT staff_users_role_check
        CHECK (role IN ('platform_admin', 'tenant_admin', 'support', 'compliance', 'finance', 'risk_manager', 'promotions_manager', 'bonus_operations'));

-- 2. bulk_grant_job_items.parent_operation_id (doc 34 §3.4/doc 10
--    §N2.4a): "doc 34's own worked example, confirmed to fit this
--    document's existing W5 schema with one additive column
--    (BulkGrantJobItem.parent_operation_id)" - the per-item denormalized
--    copy of the owning BulkGrantJob's own parent_operation_id, which is
--    what lets the EOI consumption join (doc 34 §3.4's
--    "c.parent_operation_id = eoi.operation_id") read this table
--    directly rather than joining back through bulk_grant_jobs on every
--    budget check.
ALTER TABLE bulk_grant_job_items ADD COLUMN parent_operation_id UUID;
ALTER TABLE bulk_grant_job_items
    ADD CONSTRAINT bulk_grant_job_items_parent_operation_tenant_fk
        FOREIGN KEY (parent_operation_id, tenant_id) REFERENCES economic_operations (operation_id, tenant_id);
CREATE INDEX idx_bulk_grant_job_items_parent_operation ON bulk_grant_job_items (parent_operation_id) WHERE parent_operation_id IS NOT NULL;
