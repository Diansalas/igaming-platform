-- Brand: the consumer-facing product, distinct from Tenant (the
-- commercial/legal/licensing relationship). See
-- docs/decisions/0012-brand-distinct-from-tenant.md. Replaces Stage 1's
-- tenant_config, which incorrectly assumed one brand per tenant.

CREATE TABLE brands (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    slug            TEXT NOT NULL UNIQUE,
    status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'closed')),
    default_locale  TEXT NOT NULL DEFAULT 'en',
    theme           JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Lets child tables (player_accounts) enforce "my brand_id really
    -- belongs to my tenant_id" via a composite foreign key instead of
    -- trusting application code to keep the two consistent.
    UNIQUE (id, tenant_id)
);

CREATE INDEX idx_brands_tenant ON brands (tenant_id);

ALTER TABLE brands ENABLE ROW LEVEL SECURITY;
ALTER TABLE brands FORCE ROW LEVEL SECURITY;

-- Brand identity/theme is public data by nature - it's what an
-- unauthenticated visitor's browser needs to render the site, the same
-- class of data as a public website's own HTML. See ADR 0012 for why
-- this differs from the single tenant-scoped policy used elsewhere.
CREATE POLICY brand_public_read ON brands
    FOR SELECT
    USING (true);

-- Mutations remain tenant-scoped.
CREATE POLICY brand_tenant_insert ON brands
    FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY brand_tenant_update ON brands
    FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE POLICY brand_tenant_delete ON brands
    FOR DELETE
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Migrate any existing tenant_config rows into a brand per tenant (a
-- no-op on today's empty table, written correctly for the general case).
INSERT INTO brands (tenant_id, name, slug, default_locale, theme)
SELECT tc.tenant_id, tc.display_name, t.slug, tc.default_locale, tc.theme
FROM tenant_config tc
JOIN tenants t ON t.id = tc.tenant_id;

DROP TABLE tenant_config;
