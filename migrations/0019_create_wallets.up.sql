-- Wallet identity: one per (player_account_id, asset_code). See
-- docs/architecture/financial-domain-model.md "Wallet identity" and ADR
-- 0007/0012. Wallet belongs to PlayerAccount, not Person - a Person with
-- accounts at two brands gets independent wallet sets per brand.

-- Prerequisite composite unique constraint so the FK below can verify
-- brand_id really belongs to tenant_id via the player_account, mirroring
-- the existing player_accounts -> brands composite FK pattern
-- (docs/decisions/0012). player_accounts had no (id, tenant_id, brand_id)
-- unique constraint before Stage 3B because nothing needed to FK against
-- it at that granularity yet.
ALTER TABLE player_accounts
    ADD CONSTRAINT player_accounts_id_tenant_brand_key UNIQUE (id, tenant_id, brand_id);

CREATE TABLE wallets (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL,
    brand_id           UUID NOT NULL,
    player_account_id UUID NOT NULL,
    asset_code         TEXT NOT NULL REFERENCES assets (code),
    status             TEXT NOT NULL DEFAULT 'active'
                           CHECK (status IN ('active', 'frozen', 'closed')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (player_account_id, asset_code),
    -- Guarantees tenant_id/brand_id can never drift from the player
    -- account they claim to belong to - a database-level fact, not an
    -- application-trusted one (financial-domain-model.md "Wallet
    -- identity").
    FOREIGN KEY (player_account_id, tenant_id, brand_id)
        REFERENCES player_accounts (id, tenant_id, brand_id)
);

-- Target for composite FKs from ledger_accounts (via a trigger, not a
-- nullable composite FK - see migration 0020's comment on why).
ALTER TABLE wallets ADD CONSTRAINT wallets_id_tenant_key UNIQUE (id, tenant_id);
ALTER TABLE wallets ADD CONSTRAINT wallets_id_player_account_key UNIQUE (id, player_account_id);

CREATE INDEX idx_wallets_tenant ON wallets (tenant_id);
CREATE INDEX idx_wallets_player_account ON wallets (player_account_id);

ALTER TABLE wallets ENABLE ROW LEVEL SECURITY;
ALTER TABLE wallets FORCE ROW LEVEL SECURITY;

-- Two permissive policies, deliberately not merged into one, per ADR 0019
-- ("The ADR 0016 gotcha applies here") and docs/decisions/0016's own
-- discovery: a single policy that ANDs the player scope in would make
-- every tenant-only-scoped (staff/system) write silently affect zero
-- rows. tenant_staff_scope is therefore explicitly restricted to
-- connections that have NOT set a player scope, so it can never be
-- satisfied by a player's own narrowly-scoped connection and leak other
-- players' wallets to them; player_self_scope is the separate, narrower
-- grant for a player's own self-service reads.
CREATE POLICY tenant_staff_scope ON wallets
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY player_self_scope ON wallets
    FOR SELECT
    USING (
        player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
    );
