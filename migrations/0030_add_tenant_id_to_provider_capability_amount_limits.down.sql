DROP POLICY tenant_isolation ON provider_capability_amount_limits;
CREATE POLICY tenant_isolation ON provider_capability_amount_limits
    FOR ALL
    USING (
        provider_capability_id IN (
            SELECT id FROM provider_capabilities
            WHERE tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        )
    )
    WITH CHECK (
        provider_capability_id IN (
            SELECT id FROM provider_capabilities
            WHERE tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        )
    );

DROP INDEX idx_provider_capability_amount_limits_tenant;

ALTER TABLE provider_capability_amount_limits
    DROP CONSTRAINT provider_capability_amount_limits_capability_tenant_fkey;
ALTER TABLE provider_capability_amount_limits
    ADD CONSTRAINT provider_capability_amount_limits_provider_capability_id_fkey
    FOREIGN KEY (provider_capability_id) REFERENCES provider_capabilities (id) ON DELETE CASCADE;

ALTER TABLE provider_capabilities DROP CONSTRAINT provider_capabilities_id_tenant_key;

ALTER TABLE provider_capability_amount_limits DROP COLUMN tenant_id;
