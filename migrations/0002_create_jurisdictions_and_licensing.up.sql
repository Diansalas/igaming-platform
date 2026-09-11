-- Jurisdiction/licensing model per docs/architecture/15-jurisdiction-and-
-- licensing-model.md and docs/decisions/0006-hybrid-licensing-and-
-- jurisdiction-model.md. Jurisdiction is first-class and pluggable -
-- never a hardcoded region/ruleset in application code.

CREATE TABLE jurisdictions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code            TEXT NOT NULL UNIQUE, -- e.g. 'KM-ANJ', 'MT', 'CO'
    name            TEXT NOT NULL,
    regulatory_body TEXT,
    notes           TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE licences (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    jurisdiction_id     UUID NOT NULL REFERENCES jurisdictions(id),
    licensee            TEXT NOT NULL CHECK (licensee IN ('platform', 'tenant')),
    licence_number      TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'expired')),
    issued_at           DATE,
    expires_at          DATE,
    permitted_products  JSONB NOT NULL DEFAULT '[]'::jsonb,
    permitted_markets   JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE tenants
    ADD COLUMN licence_id UUID REFERENCES licences(id);

COMMENT ON COLUMN tenants.licence_id IS 'The licence this tenant actually operates under - our platform licence (shared across platform-licensed tenants) or a licence the tenant brought.';

-- Per-tenant, per-jurisdiction regulatory configuration. A tenant can
-- serve players from multiple jurisdictions at once, each with its own
-- row here - this is what makes "Europe + LATAM" representable without
-- hardcoding either region's rules (docs/decisions/0006).
CREATE TABLE tenant_jurisdiction_configs (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    jurisdiction_id         UUID NOT NULL REFERENCES jurisdictions(id),
    kyc_ruleset_id          TEXT,
    aml_ruleset_id          TEXT,
    rg_ruleset_id           TEXT,
    reporting_ruleset_id    TEXT,
    allowed_currencies      JSONB NOT NULL DEFAULT '[]'::jsonb,
    allowed_payment_methods JSONB NOT NULL DEFAULT '[]'::jsonb,
    geo_block_list          JSONB NOT NULL DEFAULT '[]'::jsonb,
    effective_from          TIMESTAMPTZ NOT NULL DEFAULT now(),
    effective_to            TIMESTAMPTZ,
    UNIQUE (tenant_id, jurisdiction_id, effective_from)
);

CREATE INDEX idx_tenant_jurisdiction_configs_tenant ON tenant_jurisdiction_configs (tenant_id);
