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

-- Backfill BEFORE enabling RLS on brands, and with tenant_config's own
-- RLS temporarily disabled (it's dropped at the end of this migration
-- anyway). The migration runner connects without app.tenant_id set (by
-- design - it is not, and must never be, a superuser/BYPASSRLS role), so
-- with tenant_config's FORCE ROW LEVEL SECURITY still active, its own
-- SELECT policy (migration 0004, single-scope) would silently filter
-- this backfill's source rows to zero - the INSERT would then report
-- success having copied nothing, silently discarding every tenant's
-- pre-Stage-2 config on any real upgrade. Caught (and reproduced against
-- seeded data) in Stage 2 code review; same failure class as the fix
-- already applied to this migration's down script.
ALTER TABLE tenant_config DISABLE ROW LEVEL SECURITY;

INSERT INTO brands (tenant_id, name, slug, default_locale, theme)
SELECT tc.tenant_id, tc.display_name, t.slug, tc.default_locale, tc.theme
FROM tenant_config tc
JOIN tenants t ON t.id = tc.tenant_id;

DROP TABLE tenant_config;

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
