-- Stage 6 (B2C Player/Brand MVP + First Sportsbook Vertical Slice).
-- internal/sportsbook's canonical domain schema: a platform-wide,
-- read-open, platform-admin-write-only catalogue (sb_sports ->
-- sb_competitions -> sb_events -> sb_markets -> sb_selections, mirroring
-- migration 0035's casino_games precedent - game/event CONTENT is shared
-- across every tenant, never tenant-owned) plus the tenant-owned,
-- RLS-protected sportsbook_bets table (mirroring migration 0035's
-- casino_launch_sessions precedent exactly).
--
-- Scope: singles bets only, "open" status only (no settlement/void/
-- cashout is ever written this stage - see internal/sportsbook's own
-- package doc comment). No Season/Participant/MarketType-vs-Market split
-- (docs/architecture/09-sportsbook-architecture.md's fuller design is
-- explicitly NOT built this stage, per this stage's own directive).

-- 1. Ledger transaction type for the cash-funded bet-placement posting
-- (docs/decisions/0038 §3). Postgres has no ALTER CHECK - the constraint
-- is dropped and recreated with the additive value, exactly like
-- migrations 0035/0048/0050/0051 before it. Purely additive: sixteen
-- accepted transaction_type values become seventeen, none removed.
ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_transaction_type_check;
DO $$
BEGIN
    ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_transaction_type_check CHECK (transaction_type IN (
        'deposit', 'deposit_reversal',
        'withdrawal_requested', 'withdrawal_completed', 'withdrawal_rejected',
        'withdrawal_failed', 'withdrawal_reversed',
        'manual_adjustment', 'tombstone',
        'casino_bet', 'casino_win', 'casino_rollback',
        'bonus_grant', 'bonus_conversion', 'bonus_forfeiture', 'bonus_reversal',
        'sportsbook_bet'
    ));
EXCEPTION WHEN check_violation THEN
    -- Defense in depth, believed UNREACHABLE for the identical reason
    -- migrations 0048/0051's own guards give: the widened list is a
    -- strict superset, ledger_transactions carries FORCE ROW LEVEL
    -- SECURITY (migration 0021), and constraint validation - unlike a
    -- SELECT - cannot be blinded by it. No RLS setting is toggled here,
    -- and none ever should be (security finding S-1, Stage 4H-B0-R7).
    RAISE EXCEPTION 'migration 0078: ledger_transactions holds a transaction_type outside the seventeen admitted values (detected at constraint validation, which row-level security cannot filter). This widening is additive, so this should be unreachable: verify migrations 0035/0048/0050/0051 applied and that no row was written while the constraint was absent';
END $$;

-- 2. Platform-wide catalogue (no tenant_id, no RLS - read-open,
-- platform-admin-write-only, mirroring casino_games/the assets registry).

CREATE TABLE sb_sports (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- external_ref is the mock/future-real provider's own identifier,
    -- retained ONLY as a non-authoritative external reference for
    -- upsert-keying (docs/architecture/09-sportsbook-architecture.md
    -- §2.5) - the platform's own `id` is the row's actual identity.
    external_ref  TEXT NOT NULL UNIQUE,
    code          TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sb_competitions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sport_id      UUID NOT NULL REFERENCES sb_sports (id),
    external_ref  TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_sb_competitions_sport ON sb_competitions (sport_id);

CREATE TABLE sb_events (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    competition_id UUID NOT NULL REFERENCES sb_competitions (id),
    external_ref   TEXT NOT NULL UNIQUE,
    name           TEXT NOT NULL,
    start_time     TIMESTAMPTZ NOT NULL,
    status         TEXT NOT NULL DEFAULT 'scheduled' CHECK (status IN ('scheduled', 'live', 'finished', 'cancelled')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_sb_events_competition ON sb_events (competition_id);
CREATE INDEX idx_sb_events_start_time ON sb_events (start_time);

CREATE TABLE sb_markets (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id     UUID NOT NULL REFERENCES sb_events (id),
    external_ref TEXT NOT NULL UNIQUE,
    name         TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'suspended', 'closed')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_sb_markets_event ON sb_markets (event_id);

CREATE TABLE sb_selections (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    market_id        UUID NOT NULL REFERENCES sb_markets (id),
    external_ref     TEXT NOT NULL UNIQUE,
    name             TEXT NOT NULL,
    -- Fixed decimal odds as an integer numerator/denominator pair - NEVER
    -- a float (CLAUDE.md). E.g. numerator=250, denominator=100 means
    -- decimal odds of 2.50.
    odds_numerator   BIGINT NOT NULL CHECK (odds_numerator > 0),
    odds_denominator BIGINT NOT NULL CHECK (odds_denominator > 0),
    status           TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_sb_selections_market ON sb_selections (market_id);

-- 3. sportsbook_bets: tenant-owned, RLS-protected open-bet record
-- (docs/decisions/0038 §2/§3). Mirrors casino_launch_sessions' (migration
-- 0035) two-policy RLS shape and FK-composite-key discipline exactly.
CREATE TABLE sportsbook_bets (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL,
    brand_id              UUID NOT NULL,
    player_account_id     UUID NOT NULL,
    wallet_id             UUID NOT NULL,
    selection_id          UUID NOT NULL REFERENCES sb_selections (id),
    asset_code            TEXT NOT NULL REFERENCES assets (code),
    stake_amount          BIGINT NOT NULL CHECK (stake_amount > 0),
    -- Odds FROZEN AT ACCEPTANCE (doc 09 §1.4) - copied from the
    -- selection's live odds at placement time, never a live reference to
    -- sb_selections' current odds, so a bet's potential_return remains
    -- computable and auditable even after the selection's price changes.
    odds_numerator        BIGINT NOT NULL CHECK (odds_numerator > 0),
    odds_denominator      BIGINT NOT NULL CHECK (odds_denominator > 0),
    -- potential_return is a DOMAIN PROJECTION (ADR 0038 §2), never a
    -- ledger-visible fact - the only ledger-visible fact for an open bet
    -- is the stake sitting in player_locked_cash.
    potential_return      BIGINT NOT NULL CHECK (potential_return >= 0),
    -- Closed enum for the full future settlement lifecycle (ADR 0038 §5/
    -- §8) - only 'open' is ever WRITTEN this stage; the other three exist
    -- so the column is future-ready without a widening migration.
    status                TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'settled_won', 'settled_lost', 'void')),
    idempotency_key       TEXT NOT NULL,
    ledger_transaction_id UUID NOT NULL REFERENCES ledger_transactions (id),
    placed_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (wallet_id, tenant_id) REFERENCES wallets (id, tenant_id),
    FOREIGN KEY (wallet_id, player_account_id) REFERENCES wallets (id, player_account_id),
    -- Stage 6.1 hardening (DB/RLS review finding): the original three FKs
    -- above pin wallet->tenant and wallet->player, but left brand_id
    -- pinned only to "some brand in this tenant", not specifically the
    -- player's own brand - a direct INSERT (bypassing the application,
    -- which always derives brand_id correctly via
    -- identity.GetPlayerAccountByID) could misattribute a bet to a
    -- different brand within the same tenant. This composite FK reuses
    -- the exact (id, tenant_id, brand_id) unique key migration 0019
    -- already added to player_accounts for wallets' own identical
    -- pinning, closing the same class of gap here at the DB level rather
    -- than relying on application discipline alone (CLAUDE.md's RLS/
    -- integrity-by-database rule, applied to ownership FKs generally).
    FOREIGN KEY (player_account_id, tenant_id, brand_id) REFERENCES player_accounts (id, tenant_id, brand_id),
    UNIQUE (tenant_id, player_account_id, idempotency_key)
);

CREATE INDEX idx_sportsbook_bets_tenant ON sportsbook_bets (tenant_id);
CREATE INDEX idx_sportsbook_bets_player ON sportsbook_bets (player_account_id);

ALTER TABLE sportsbook_bets ENABLE ROW LEVEL SECURITY;
ALTER TABLE sportsbook_bets FORCE ROW LEVEL SECURITY;

-- Same two-policy shape as withdrawal_requests/casino_launch_sessions:
-- staff/system get full access under tenant-only scope (placing a bet,
-- listing every player's bets for the Back Office); a player gets
-- SELECT-only visibility into their OWN bets (bet placement itself
-- happens server-side under WithTenant, never a player-writable row,
-- exactly like casino_launch_sessions' own insert path).
CREATE POLICY tenant_staff_scope ON sportsbook_bets
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY player_self_scope ON sportsbook_bets
    FOR SELECT
    USING (
        player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
    );
