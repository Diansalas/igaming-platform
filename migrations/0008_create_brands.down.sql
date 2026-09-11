-- Lossy in reverse by design: if a tenant ended up with more than one
-- brand, only one (the earliest-created) survives the trip back to
-- tenant_config's one-row-per-tenant shape. This is acceptable because
-- `down` is a development/CI verification tool, never a production
-- rollback path for a change of this shape (see docs/architecture/03-
-- database-architecture.md's migration philosophy) - a real
-- brands-were-introduced rollback in production would be a new forward
-- migration, not this script.

CREATE TABLE tenant_config (
    tenant_id       UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
    display_name    TEXT NOT NULL,
    theme           JSONB NOT NULL DEFAULT '{}'::jsonb,
    default_locale  TEXT NOT NULL DEFAULT 'en',
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Backfill before RLS is enabled: the migration runner connects without
-- app.tenant_id set (it is not, and must never be, a superuser/BYPASSRLS
-- role - see internal/db's verifyNotPrivileged), so a FORCE ROW LEVEL
-- SECURITY policy already in effect would reject every row of this
-- INSERT under its own WITH CHECK clause.
INSERT INTO tenant_config (tenant_id, display_name, theme, default_locale)
SELECT DISTINCT ON (tenant_id) tenant_id, name, theme, default_locale
FROM brands
ORDER BY tenant_id, created_at ASC;

ALTER TABLE tenant_config ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_config FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON tenant_config
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

DROP TABLE brands;
