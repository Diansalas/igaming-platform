-- Staff users are distinct from player accounts: they operate the
-- platform/back office, not the brand as a customer. tenant_id is
-- nullable - NULL means a platform-wide staff member (role
-- 'platform_admin'); non-NULL means staff scoped to that one tenant.
-- See docs/decisions/0011-platform-scoped-identity-tokens.md.

CREATE TABLE staff_users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID REFERENCES tenants(id) ON DELETE CASCADE,
    email         TEXT NOT NULL CHECK (email = lower(email)),
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('platform_admin', 'tenant_admin', 'support', 'compliance', 'finance')),
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- NULLS NOT DISTINCT so two platform_admin rows (tenant_id IS NULL)
    -- can't reuse the same email either - plain UNIQUE treats every NULL
    -- as distinct from every other NULL, which would defeat this here.
    UNIQUE NULLS NOT DISTINCT (tenant_id, email),
    -- platform_admin must be platform-wide; every other role must belong
    -- to a tenant. Prevents a nonsensical "tenant-scoped platform_admin"
    -- or "platform-wide support user" row from ever being created.
    CHECK (
        (role = 'platform_admin' AND tenant_id IS NULL)
        OR (role != 'platform_admin' AND tenant_id IS NOT NULL)
    )
);

CREATE INDEX idx_staff_users_tenant ON staff_users (tenant_id);

ALTER TABLE staff_users ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_users FORCE ROW LEVEL SECURITY;

-- Dual-scope pattern (docs/decisions/0013): a tenant-scoped connection
-- sees only that tenant's staff; a connection with no tenant context
-- (WithoutTenant) sees only platform-wide staff (tenant_id IS NULL) -
-- needed so a platform_admin can be looked up by email at login, before
-- any tenant context exists.
CREATE POLICY dual_scope_isolation ON staff_users
    FOR ALL
    USING (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    )
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    );
