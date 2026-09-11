-- Stage 1 foundation: tenant registry. No business/financial tables yet -
-- see docs/architecture/03-database-architecture.md and
-- docs/decisions/0002-multi-tenancy-isolation-strategy.md.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE tenants (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL,
    slug            TEXT NOT NULL UNIQUE,
    -- Hybrid licensing model (docs/decisions/0006): a tenant operates
    -- either under our platform licence or brings its own.
    licensing_model TEXT NOT NULL CHECK (licensing_model IN ('under_platform_licence', 'own_licence')),
    status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'closed')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE tenants IS 'Platform tenants (brands). tenant_id from this table is the isolation key enforced by RLS on every tenant-owned table.';
