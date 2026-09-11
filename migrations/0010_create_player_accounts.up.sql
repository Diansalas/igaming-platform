-- Player account: a Person's relationship with one specific tenant/brand.
-- Never the same entity as Person (see docs/architecture/05-identity-
-- architecture.md and Stage 2 instructions: person, player account,
-- tenant, brand, wallet are kept distinct).
--
-- Privacy note: only what's needed for authentication and brand
-- relationship is collected here. No name/address/document data - that
-- belongs to the Stage 4 KYC subsystem, behind its own vendor-agnostic
-- interface, not this table. See docs/architecture/16-privacy.md.

CREATE TABLE player_accounts (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL,
    brand_id         UUID NOT NULL,
    person_id        UUID NOT NULL REFERENCES persons(id),
    email            TEXT NOT NULL CHECK (email = lower(email)),
    password_hash    TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending_verification'
                         CHECK (status IN ('pending_verification', 'active', 'suspended', 'self_excluded', 'closed')),
    -- Hooks for the Stage 4 KYC/AML subsystem - not enforced or
    -- interpreted by anything in Stage 2.
    kyc_tier         INT NOT NULL DEFAULT 0,
    verified_at      TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (brand_id, email),
    -- Guarantees brand_id really belongs to tenant_id - a database-level
    -- fact, not an application-trusted one (docs/decisions/0012).
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

CREATE INDEX idx_player_accounts_tenant ON player_accounts (tenant_id);
CREATE INDEX idx_player_accounts_person ON player_accounts (person_id);

ALTER TABLE player_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE player_accounts FORCE ROW LEVEL SECURITY;

-- Player PII is never publicly readable - unlike brands, this is the
-- ordinary single-scope tenant-isolation pattern from Stage 1.
CREATE POLICY tenant_isolation ON player_accounts
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
