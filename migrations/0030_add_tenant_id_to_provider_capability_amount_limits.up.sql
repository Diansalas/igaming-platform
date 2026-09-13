-- Stage 3C hardening: resolves the documented ownership ambiguity on
-- provider_capability_amount_limits (flagged in the Stage 3B completion
-- report and by the architecture review). docs/decisions/0022 §2 frames
-- these rows as "a child set of (asset_code, min_amount, max_amount)
-- rows" belonging to one ProviderCapability - and a ProviderCapability
-- IS tenant-owned (provider_capabilities.tenant_id NOT NULL). There is
-- no platform-wide amount-limit concept anywhere in the approved
-- architecture, so the correct ownership model is unambiguous:
-- tenant-owned, inheriting the same tenant as the parent capability row.
--
-- Before this migration, the table had no tenant_id of its own and its
-- RLS policy was a subquery into provider_capabilities - which
-- happened to still be correct (a tenant-scoped connection could only
-- ever find its OWN capability ids in the subquery), but ADR 0019
-- explicitly forbids exactly this policy shape ("never make a table's
-- isolation depend on a runtime subquery into a different table - a
-- future migration to that other table's RLS could silently change this
-- table's effective isolation without anyone touching this file"), and
-- CLAUDE.md's own multi-tenancy rule requires tenant_id "enforced by
-- PostgreSQL row-level security bound to a connection-level setting" on
-- every tenant-owned table directly, not transitively.

ALTER TABLE provider_capability_amount_limits ADD COLUMN tenant_id UUID;

-- Migrations run as the ordinary `igaming` role, deliberately
-- NOBYPASSRLS even for tables it owns (deploy/init-app-role.sql) - the
-- whole point being that migrations cannot accidentally read across
-- tenants through a superuser connection. Both tables involved in the
-- backfill below carry FORCE ROW LEVEL SECURITY, and this migration runs
-- with no app.tenant_id set (there is no single "current tenant" for a
-- schema migration), so under normal RLS enforcement this UPDATE would
-- see zero rows in EITHER table and silently backfill nothing. Disabling
-- RLS is DDL, not DML - it requires only table ownership, not a
-- privileged role - and is scoped to this migration's own transaction
-- (internal/db/migrate.go runs every migration inside one), so it is
-- never observable from a concurrent connection. Both tables are
-- restored to ENABLE (their FORCE flag is untouched throughout, since
-- DISABLE/ENABLE toggle a separate flag) before this transaction commits.
ALTER TABLE provider_capability_amount_limits DISABLE ROW LEVEL SECURITY;
ALTER TABLE provider_capabilities DISABLE ROW LEVEL SECURITY;

UPDATE provider_capability_amount_limits pcal
SET tenant_id = pc.tenant_id
FROM provider_capabilities pc
WHERE pc.id = pcal.provider_capability_id;

ALTER TABLE provider_capability_amount_limits ALTER COLUMN tenant_id SET NOT NULL;

-- Composite FK against provider_capabilities' own (id, tenant_id) pair -
-- the same "carries both, not only the composite into brands" pattern
-- ADR 0022 §3 already established for provider_capabilities itself. This
-- makes it a DATABASE fact, not merely an application-trusted one, that
-- an amount-limit row's tenant_id always agrees with its parent
-- capability's tenant_id - the exact guarantee a plain
-- REFERENCES provider_capabilities (id) alone does not provide.
ALTER TABLE provider_capabilities
    ADD CONSTRAINT provider_capabilities_id_tenant_key UNIQUE (id, tenant_id);

ALTER TABLE provider_capability_amount_limits
    DROP CONSTRAINT provider_capability_amount_limits_provider_capability_id_fkey;
ALTER TABLE provider_capability_amount_limits
    ADD CONSTRAINT provider_capability_amount_limits_capability_tenant_fkey
    FOREIGN KEY (provider_capability_id, tenant_id) REFERENCES provider_capabilities (id, tenant_id) ON DELETE CASCADE;

CREATE INDEX idx_provider_capability_amount_limits_tenant ON provider_capability_amount_limits (tenant_id);

-- Replace the subquery-based policy with the same direct, single-column
-- tenant_isolation pattern every other Stage 3B tenant-owned table uses.
DROP POLICY tenant_isolation ON provider_capability_amount_limits;
CREATE POLICY tenant_isolation ON provider_capability_amount_limits
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Restore enforcement before this transaction commits - see the DISABLE
-- comment above. FORCE ROW LEVEL SECURITY was never touched, so this
-- restores exactly the same enforcement (including against the owning
-- role) both tables had before this migration ran.
ALTER TABLE provider_capability_amount_limits ENABLE ROW LEVEL SECURITY;
ALTER TABLE provider_capabilities ENABLE ROW LEVEL SECURITY;
