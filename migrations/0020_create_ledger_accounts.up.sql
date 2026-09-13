-- LedgerAccount: a named bucket ledger entries post to. Never carries a
-- balance column itself (balance is always SELECT SUM(...) over
-- ledger_entries - see docs/architecture/ledger-accounting-model.md §5).
-- Player-owned account types (wallet_id NOT NULL) belong to exactly one
-- Wallet; house-level account types (wallet_id NULL) belong to the
-- Tenant directly. See ledger-accounting-model.md §1.1/§2.

CREATE TABLE ledger_accounts (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL,
    wallet_id          UUID REFERENCES wallets (id),
    -- Denormalized from wallets.player_account_id by the trigger below,
    -- NULL for house-level accounts. Exists so RLS policies (and
    -- ledger_entries, via a further trigger) can check player ownership
    -- directly on the row rather than via a subquery into another
    -- RLS-protected table - see docs/decisions/0019 "Why wallet_id is on
    -- this row" (the same reasoning applies here).
    player_account_id UUID,
    account_type       TEXT NOT NULL CHECK (account_type IN (
        'player_cash', 'player_bonus', 'player_locked', 'player_withdrawal_hold',
        'house_gaming', 'provider_payable', 'psp_clearing', 'psp_reserve',
        'jackpot_contribution', 'promo_liability', 'manual_adjustment'
    )),
    asset_code         TEXT NOT NULL REFERENCES assets (code),
    -- NULL for player-owned accounts: their effective status is always
    -- read from the owning Wallet, never stored a second time (a stored
    -- copy could drift from a frozen wallet - see
    -- ledger-accounting-model.md §1.1). Only house-level accounts
    -- (wallet_id NULL) carry their own status.
    status             TEXT CHECK (status IN ('active', 'frozen', 'closed')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((wallet_id IS NULL) = (player_account_id IS NULL)),
    CHECK ((wallet_id IS NOT NULL) OR (status IS NOT NULL)),
    CHECK ((wallet_id IS NULL) OR (status IS NULL))
);

-- Exactly one account per (wallet, type, asset) or (tenant, type, asset) -
-- accounts are not created per-transaction (ledger-accounting-model.md
-- §1.1). Partial unique indexes since Postgres UNIQUE constraints cannot
-- carry a WHERE clause inline.
CREATE UNIQUE INDEX idx_ledger_accounts_wallet_type_asset
    ON ledger_accounts (wallet_id, account_type, asset_code)
    WHERE wallet_id IS NOT NULL;
CREATE UNIQUE INDEX idx_ledger_accounts_tenant_type_asset
    ON ledger_accounts (tenant_id, account_type, asset_code)
    WHERE wallet_id IS NULL;

CREATE INDEX idx_ledger_accounts_tenant ON ledger_accounts (tenant_id);
CREATE INDEX idx_ledger_accounts_player_account ON ledger_accounts (player_account_id);

-- Target for ledger_entries' composite FK (see migration 0022).
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_id_tenant_asset_key UNIQUE (id, tenant_id, asset_code);

-- Populates tenant_id/player_account_id/asset_code from the owning
-- Wallet automatically, rather than trusting the caller to pass
-- consistent values. A composite FK cannot do this job here: with
-- wallet_id nullable, a MATCH SIMPLE composite FK is satisfied trivially
-- the moment ANY column in it is NULL, which would silently disable the
-- check for exactly the house-level rows it must still constrain
-- correctly (the same reasoning the Stage 3A specialist review recorded
-- for ledger_entries - see ledger-accounting-model.md §1.3). A
-- BEFORE INSERT trigger is both simpler and stricter than fighting
-- nullable composite FKs.
CREATE FUNCTION ledger_accounts_populate_from_wallet() RETURNS TRIGGER AS $$
DECLARE
    w_tenant_id UUID;
    w_player_account_id UUID;
    w_asset_code TEXT;
BEGIN
    IF NEW.wallet_id IS NULL THEN
        RETURN NEW;
    END IF;

    SELECT tenant_id, player_account_id, asset_code
        INTO w_tenant_id, w_player_account_id, w_asset_code
        FROM wallets WHERE id = NEW.wallet_id;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger_accounts: wallet % does not exist', NEW.wallet_id;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM w_tenant_id THEN
        RAISE EXCEPTION 'ledger_accounts: tenant_id % does not match wallet %''s tenant %', NEW.tenant_id, NEW.wallet_id, w_tenant_id;
    END IF;
    IF NEW.asset_code IS DISTINCT FROM w_asset_code THEN
        RAISE EXCEPTION 'ledger_accounts: asset_code % does not match wallet %''s asset %', NEW.asset_code, NEW.wallet_id, w_asset_code;
    END IF;

    NEW.player_account_id := w_player_account_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_accounts_populate_from_wallet_trigger
    BEFORE INSERT ON ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION ledger_accounts_populate_from_wallet();

ALTER TABLE ledger_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_accounts FORCE ROW LEVEL SECURITY;

-- Same two-policy shape as wallets - see that migration's comment.
CREATE POLICY tenant_staff_scope ON ledger_accounts
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY player_self_scope ON ledger_accounts
    FOR SELECT
    USING (
        player_account_id IS NOT NULL
        AND player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
    );
