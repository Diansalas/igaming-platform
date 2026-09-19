-- Reverses migration 0072: restores migration 0071's original single
-- FOR ALL policy on jurisdiction_resolution_active. Note that doing so
-- re-opens SEC-4I-F4 (a tenant-scoped connection regains DELETE on its
-- own resolution-active facts) - this down migration exists for chain
-- reversibility only.

DROP POLICY IF EXISTS jurisdiction_resolution_active_tenant_isolation_update ON jurisdiction_resolution_active;
DROP POLICY IF EXISTS jurisdiction_resolution_active_tenant_isolation_insert ON jurisdiction_resolution_active;
DROP POLICY IF EXISTS jurisdiction_resolution_active_tenant_isolation_read ON jurisdiction_resolution_active;

CREATE POLICY jurisdiction_resolution_active_tenant_isolation ON jurisdiction_resolution_active
    FOR ALL
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
