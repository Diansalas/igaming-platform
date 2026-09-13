-- ProviderCapability: docs/decisions/0022 §2. Data read by RouteProvider,
-- never compiled into the orchestrator. Carries NO credential
-- field/secret material (§2.2) - credentials are configured through the
-- platform's existing secret-handle mechanism, tracked separately from
-- this table.
--
-- provider_kind is 'fiat' | 'crypto_payment' ONLY - a Crypto Custodian is
-- deliberately not representable here (§2 of that ADR): it implements
-- CryptoCustodyProvider, has no Capabilities() method, and must never be
-- a RouteProvider candidate. Stage 3B implements a single mock 'fiat'
-- adapter; the Go-level provider registry (which provider_id maps to
-- which PaymentProvider implementation) is the authority on what a
-- provider_id actually is, per §2.1 - this table is a filter over that
-- registry, not a second registry of its own.
--
-- "Two layers, one shape" (§2): adapter-declared facts (provider_kind,
-- the supported_* lists, the support_* flags, settlement_behavior,
-- callback_capabilities) and operator-configured facts (brand_id,
-- priority, status) live on the same row; Stage 3B does not yet split
-- them into separate tables/write-paths since only one adapter exists,
-- but application code must not let ordinary tenant configuration writes
-- change provider_kind or the declared lists beyond what the adapter
-- itself actually supports (§2.1).

CREATE TABLE provider_capabilities (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL,
    brand_id                  UUID,
    provider_id               TEXT NOT NULL,
    provider_kind             TEXT NOT NULL CHECK (provider_kind IN ('fiat', 'crypto_payment')),
    supported_fiat_currencies TEXT[] NOT NULL DEFAULT '{}',
    supported_crypto_assets   TEXT[] NOT NULL DEFAULT '{}',
    supported_payment_methods TEXT[] NOT NULL DEFAULT '{}',
    supported_countries       TEXT[] NOT NULL DEFAULT '{}',
    supports_deposit          BOOLEAN NOT NULL DEFAULT true,
    supports_withdrawal       BOOLEAN NOT NULL DEFAULT true,
    supports_refund_reversal  BOOLEAN NOT NULL DEFAULT false,
    settlement_behavior       TEXT NOT NULL DEFAULT 'instant',
    callback_capabilities     TEXT NOT NULL DEFAULT 'webhook'
                                  CHECK (callback_capabilities IN ('webhook', 'polling_only', 'both')),
    priority                  INT NOT NULL DEFAULT 100,
    status                    TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- ADR 0012's own pattern: a capability row can never name a brand
    -- belonging to a different tenant. NULL brand_id skips this FK
    -- (MATCH SIMPLE) - tenant_id's own NOT NULL is what binds a
    -- tenant-wide row (docs/decisions/0022 §3).
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

-- Whole-row-replacement resolution (docs/decisions/0022 §3): a
-- brand-specific row replaces the tenant-wide row for that provider and
-- brand entirely, so at most one row can exist per (tenant, provider) or
-- per (tenant, brand, provider).
CREATE UNIQUE INDEX idx_provider_capabilities_tenant_provider
    ON provider_capabilities (tenant_id, provider_id)
    WHERE brand_id IS NULL;
CREATE UNIQUE INDEX idx_provider_capabilities_tenant_brand_provider
    ON provider_capabilities (tenant_id, brand_id, provider_id)
    WHERE brand_id IS NOT NULL;

CREATE INDEX idx_provider_capabilities_tenant ON provider_capabilities (tenant_id);

-- Per-asset limits (docs/decisions/0022 §2: "a child set of (asset_code,
-- min_amount, max_amount) rows, NOT two columns on this row" - a provider
-- declaring several assets carries one limit pair per asset).
CREATE TABLE provider_capability_amount_limits (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_capability_id UUID NOT NULL REFERENCES provider_capabilities (id) ON DELETE CASCADE,
    asset_code             TEXT NOT NULL REFERENCES assets (code),
    min_amount             NUMERIC(38, 0) NOT NULL CHECK (min_amount >= 0),
    max_amount             NUMERIC(38, 0) NOT NULL CHECK (max_amount > 0),
    CHECK (max_amount >= min_amount),
    UNIQUE (provider_capability_id, asset_code)
);

-- Tenant-owned configuration, confidential (which providers/limits/
-- priorities a tenant runs is commercially sensitive - docs/decisions/
-- 0022 §2.2) but not secret material. No player access, no dual-scope:
-- there is no platform-global capability row (§2.2 "no dual-scope
-- policy here - a WithoutTenant connection must not become a cross-
-- tenant read path for provider configuration").
ALTER TABLE provider_capabilities ENABLE ROW LEVEL SECURITY;
ALTER TABLE provider_capabilities FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON provider_capabilities
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE provider_capability_amount_limits ENABLE ROW LEVEL SECURITY;
ALTER TABLE provider_capability_amount_limits FORCE ROW LEVEL SECURITY;

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
