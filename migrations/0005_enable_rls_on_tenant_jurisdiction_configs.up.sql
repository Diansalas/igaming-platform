-- Fix: tenant_jurisdiction_configs (created in migration 0002) carries a
-- tenant_id column but was not RLS-protected, in violation of CLAUDE.md's
-- absolute multi-tenancy rule ("every tenant-owned table carries
-- tenant_id, enforced by PostgreSQL row-level security") and in
-- contradiction of migration 0004's own comment calling itself the
-- pattern every future tenant-owned table follows. Caught in Stage 1
-- specialist review before this schema was used by any real feature.
--
-- Added as a new migration rather than editing 0002 in place, so the
-- historical record of what changed and why stays intact - the same
-- discipline the platform expects of itself once real data exists.

ALTER TABLE tenant_jurisdiction_configs ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_jurisdiction_configs FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON tenant_jurisdiction_configs
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
