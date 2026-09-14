-- Stage 4A (Casino Integration Foundation) - ADR 0025 and
-- docs/architecture/08-casino-integration-architecture.md. Adds the
-- three casino ledger transaction types migration 0021 anticipated
-- ("added by an additive migration when their owning stage implements
-- them"), plus the casino catalogue/capability/launch-session schema.
-- No real casino provider, no production credentials - foundation only.

-- 1. Ledger transaction types for Flows 5-7 (financial-transaction-
-- flows.md, already BLUEPRINT). Postgres has no ALTER CHECK - the
-- constraint is dropped and recreated with the additive values, which is
-- safe for THIS (up) direction because it only ADDS accepted values,
-- never removes one an existing row could already hold. The corresponding
-- down migration's narrower recreation is NOT safe once a row holds one of
-- these new values - see this migration's own down.sql for why that is
-- correct, expected behavior for an append-only ledger rather than a bug.
ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_transaction_type_check;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_transaction_type_check CHECK (transaction_type IN (
    'deposit', 'deposit_reversal',
    'withdrawal_requested', 'withdrawal_completed', 'withdrawal_rejected',
    'withdrawal_failed', 'withdrawal_reversed',
    'manual_adjustment', 'tombstone',
    'casino_bet', 'casino_win', 'casino_rollback'
));

-- 2. casino_games: platform-wide game catalogue (ADR 0025 §2) - no
-- tenant_id, no RLS, same shape as the `assets` registry (migration
-- 0003): game CONTENT is a fact about the provider relationship, shared
-- across every tenant that can reach that provider, never tenant-owned
-- configuration. provider_game_id is the provider's own identifier,
-- deliberately never promoted to the platform's own primary key.
CREATE TABLE casino_games (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id             TEXT NOT NULL,
    provider_game_id        TEXT NOT NULL,
    name                    TEXT NOT NULL,
    game_type               TEXT NOT NULL, -- 'slot' | 'live_dealer' | 'table' | ... open-ended, never a fixed enum (ADR 0025 §2)
    rtp_variant             TEXT,
    volatility              TEXT,
    feature_flags           TEXT[] NOT NULL DEFAULT '{}',
    supported_assets        TEXT[] NOT NULL DEFAULT '{}',
    mobile_supported        BOOLEAN NOT NULL DEFAULT false,
    demo_supported          BOOLEAN NOT NULL DEFAULT false,
    -- Jurisdiction codes (jurisdictions.code) this title may NEVER be
    -- launched into - a licence problem, not a bug (08-casino-
    -- integration-architecture.md). Free TEXT[], not FK'd to
    -- jurisdictions.code, matching docs/decisions/0022 §2's own
    -- supported_countries precedent (ISO/jurisdiction codes as data, not
    -- a referential constraint) - the set of valid codes is enforced at
    -- the point jurisdictions are configured, not re-validated here.
    jurisdiction_blocklist  TEXT[] NOT NULL DEFAULT '{}',
    status                  TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_id, provider_game_id)
);

-- 3. casino_game_availability: tenant/brand opt-in layer (ADR 0025 §2) -
-- a tenant may enable a subset of the platform catalogue, never
-- register a new game or widen a platform-level block. brand_id NULL
-- means "every brand under this tenant", the same nullable-brand
-- fallback docs/decisions/0022 §3 established for payment capabilities.
CREATE TABLE casino_game_availability (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants (id),
    brand_id    UUID REFERENCES brands (id),
    game_id     UUID NOT NULL REFERENCES casino_games (id),
    enabled     BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

CREATE UNIQUE INDEX idx_casino_game_availability_brand
    ON casino_game_availability (tenant_id, brand_id, game_id) WHERE brand_id IS NOT NULL;
CREATE UNIQUE INDEX idx_casino_game_availability_tenant_wide
    ON casino_game_availability (tenant_id, game_id) WHERE brand_id IS NULL;

ALTER TABLE casino_game_availability ENABLE ROW LEVEL SECURITY;
ALTER TABLE casino_game_availability FORCE ROW LEVEL SECURITY;

-- Tenant-owned configuration, staff/system only - a player never reads
-- this table directly (their own view is the resolved catalogue, served
-- through the launch/list-games handler, which applies this join
-- server-side). Includes the player-scope exclusion guard migration
-- 0028 established for every sibling staff-only financial-configuration
-- table, so a WithPlayerScope-scoped transaction can never read or write
-- it.
CREATE POLICY tenant_isolation ON casino_game_availability
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- 4. casino_provider_capabilities: tenant-owned capability model (ADR
-- 0025 §4), mirroring provider_capabilities' own shape and RLS pattern
-- (docs/decisions/0022 §2, migrations 0024/0033).
CREATE TABLE casino_provider_capabilities (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL REFERENCES tenants (id),
    brand_id               UUID REFERENCES brands (id),
    provider_id            TEXT NOT NULL,
    supports_catalogue     BOOLEAN NOT NULL DEFAULT false,
    supports_launch        BOOLEAN NOT NULL DEFAULT false,
    supports_balance       BOOLEAN NOT NULL DEFAULT false,
    supports_bet           BOOLEAN NOT NULL DEFAULT false,
    supports_win           BOOLEAN NOT NULL DEFAULT false,
    supports_rollback      BOOLEAN NOT NULL DEFAULT false,
    supported_assets       TEXT[] NOT NULL DEFAULT '{}',
    supported_game_types   TEXT[] NOT NULL DEFAULT '{}',
    callback_capabilities  TEXT NOT NULL DEFAULT 'webhook' CHECK (callback_capabilities IN ('webhook', 'polling_only', 'both')),
    priority               INT NOT NULL DEFAULT 0,
    status                 TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

CREATE UNIQUE INDEX idx_casino_provider_capabilities_brand
    ON casino_provider_capabilities (tenant_id, brand_id, provider_id) WHERE brand_id IS NOT NULL;
CREATE UNIQUE INDEX idx_casino_provider_capabilities_tenant_wide
    ON casino_provider_capabilities (tenant_id, provider_id) WHERE brand_id IS NULL;

ALTER TABLE casino_provider_capabilities ENABLE ROW LEVEL SECURITY;
ALTER TABLE casino_provider_capabilities FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON casino_provider_capabilities
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- 5. casino_launch_sessions: the single-use, opaque game-launch
-- credential (ADR 0025 §3) - never the player's own JWT/session token.
-- token_hash mirrors sessions.refresh_token_hash's pattern exactly: the
-- raw token is never persisted, only its SHA-256.
CREATE TABLE casino_launch_sessions (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL,
    brand_id           UUID NOT NULL,
    player_account_id  UUID NOT NULL,
    wallet_id          UUID NOT NULL,
    game_id            UUID NOT NULL REFERENCES casino_games (id),
    provider_id        TEXT NOT NULL,
    -- Denormalized from casino_games at launch time, per ADR 0025 §3, so
    -- a later catalogue edit never changes what an in-flight session
    -- resolves to.
    provider_game_id   TEXT NOT NULL,
    asset_code         TEXT NOT NULL,
    mode               TEXT NOT NULL CHECK (mode IN ('real', 'demo')),
    token_hash         TEXT NOT NULL UNIQUE,
    status             TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'consumed', 'expired', 'revoked')),
    expires_at         TIMESTAMPTZ NOT NULL,
    consumed_at        TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (wallet_id, tenant_id) REFERENCES wallets (id, tenant_id),
    FOREIGN KEY (wallet_id, player_account_id) REFERENCES wallets (id, player_account_id),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

CREATE INDEX idx_casino_launch_sessions_tenant ON casino_launch_sessions (tenant_id);
CREATE INDEX idx_casino_launch_sessions_player ON casino_launch_sessions (player_account_id);

ALTER TABLE casino_launch_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE casino_launch_sessions FORCE ROW LEVEL SECURITY;

-- Same two-policy shape as withdrawal_requests (migration 0026):
-- staff/system get full access under tenant-only scope (resolving a
-- launch token, marking it consumed); a player gets SELECT-only visibility
-- into their OWN sessions (never able to forge or consume one directly -
-- consumption happens server-side, resolving the opaque token, never a
-- player-writable row).
CREATE POLICY tenant_staff_scope ON casino_launch_sessions
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY player_self_scope ON casino_launch_sessions
    FOR SELECT
    USING (
        player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
    );
