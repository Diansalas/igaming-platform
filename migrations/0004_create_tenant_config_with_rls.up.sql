-- Tenant configuration table AND the reference implementation of the
-- platform's row-level-security tenant-isolation pattern
-- (docs/decisions/0002-multi-tenancy-isolation-strategy.md). Every future
-- tenant-owned table follows this same pattern: a tenant_id column, RLS
-- enabled AND forced, and a policy comparing tenant_id to
-- current_setting('app.tenant_id', true)::uuid, which is set exclusively
-- by internal/db.Pool.WithTenant - never by application-level filtering.

CREATE TABLE tenant_config (
    tenant_id       UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
    display_name    TEXT NOT NULL,
    theme           JSONB NOT NULL DEFAULT '{}'::jsonb,
    default_locale  TEXT NOT NULL DEFAULT 'en',
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE tenant_config ENABLE ROW LEVEL SECURITY;
-- FORCE, not just ENABLE: RLS applies even to the table owner. Without
-- FORCE, whichever role owns this table bypasses RLS by default, which
-- would make the isolation guarantee dependent on which role runs a
-- given query - the exact "discipline fails exactly once" failure mode
-- CLAUDE.md and the Blueprint warn about. Note that FORCE still does NOT
-- apply to an actual superuser or a role with BYPASSRLS - see
-- internal/db/db.go's connection-time check, which refuses to connect at
-- all as such a role, since no in-database mechanism can constrain one.
ALTER TABLE tenant_config FORCE ROW LEVEL SECURITY;

-- NULLIF(...,'') matters here, not just current_setting(...,true) alone:
-- once a session has run ANY transaction that called
-- set_config('app.tenant_id', <value>, true) (a "SET LOCAL"-style,
-- transaction-scoped set), Postgres creates a placeholder for that custom
-- GUC on the backend; after that transaction ends, the setting reverts to
-- '' (empty string), not to "undefined". A later query on the SAME pooled
-- connection that runs outside any WithTenant scope (e.g. via
-- WithoutTenant) would otherwise see current_setting(...)::uuid attempt
-- to cast '' to uuid and raise a Postgres error instead of cleanly
-- denying access. NULLIF converts that '' to a real NULL first, so the
-- comparison evaluates to UNKNOWN (denied) instead of erroring.
CREATE POLICY tenant_isolation ON tenant_config
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
