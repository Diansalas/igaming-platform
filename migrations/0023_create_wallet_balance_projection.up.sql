-- Balance projection: a rebuildable, subordinate cache over
-- ledger_entries - never a second source of truth (docs/decisions/0019
-- "Balance serving", ledger-accounting-model.md §5). Grain is one row per
-- ledger_account_id, not per wallet, so house-level accounts (which have
-- no wallet_id) get a projection row too, per docs/decisions/0019
-- "Projection grain".
--
-- balance = credit_total - debit_total (credit-positive for every
-- account type, without exception - ledger-accounting-model.md §5's
-- signed_balance formula). Display/sign convention for a debit-normal
-- account (e.g. psp_clearing) is a presentation-layer concern, not stored
-- here.

CREATE TABLE wallet_balance_projection (
    ledger_account_id UUID PRIMARY KEY REFERENCES ledger_accounts (id),
    tenant_id          UUID NOT NULL,
    wallet_id          UUID,
    player_account_id UUID,
    asset_code         TEXT NOT NULL,
    account_type       TEXT NOT NULL,
    debit_total        NUMERIC(38, 0) NOT NULL DEFAULT 0 CHECK (debit_total >= 0),
    credit_total        NUMERIC(38, 0) NOT NULL DEFAULT 0 CHECK (credit_total >= 0),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_wallet_balance_projection_tenant ON wallet_balance_projection (tenant_id);
CREATE INDEX idx_wallet_balance_projection_wallet ON wallet_balance_projection (wallet_id) WHERE wallet_id IS NOT NULL;
CREATE INDEX idx_wallet_balance_projection_player_account ON wallet_balance_projection (player_account_id) WHERE player_account_id IS NOT NULL;

-- Maintained EXCLUSIVELY by this trigger, fired by every ledger_entries
-- insert, so it is structurally impossible for a future posting code
-- path to forget the update: it always runs in the same database
-- transaction as the entry it projects, because that is what an AFTER
-- trigger is (docs/decisions/0019 "Balance serving" / "Consequences").
-- Application code never writes to this table directly.
CREATE FUNCTION ledger_entries_update_projection() RETURNS TRIGGER AS $$
DECLARE
    a_account_type TEXT;
BEGIN
    SELECT account_type INTO a_account_type FROM ledger_accounts WHERE id = NEW.ledger_account_id;

    INSERT INTO wallet_balance_projection
        (ledger_account_id, tenant_id, wallet_id, player_account_id, asset_code, account_type, debit_total, credit_total, updated_at)
    VALUES (
        NEW.ledger_account_id, NEW.tenant_id, NEW.wallet_id, NEW.player_account_id, NEW.asset_code, a_account_type,
        CASE WHEN NEW.direction = 'debit' THEN NEW.amount ELSE 0 END,
        CASE WHEN NEW.direction = 'credit' THEN NEW.amount ELSE 0 END,
        now()
    )
    ON CONFLICT (ledger_account_id) DO UPDATE SET
        debit_total = wallet_balance_projection.debit_total + EXCLUDED.debit_total,
        credit_total = wallet_balance_projection.credit_total + EXCLUDED.credit_total,
        updated_at = now();

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_entries_project_balance
    AFTER INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_update_projection();

ALTER TABLE wallet_balance_projection ENABLE ROW LEVEL SECURITY;
ALTER TABLE wallet_balance_projection FORCE ROW LEVEL SECURITY;

-- Same two-policy shape as ledger_entries - see docs/decisions/0019 "The
-- ADR 0016 gotcha applies here, and bites hardest on the projection": the
-- ON CONFLICT DO UPDATE above runs under whatever scope the posting
-- transaction used (tenant-only for provider callbacks/system code), so
-- tenant_staff_scope alone must be sufficient for it to succeed - it must
-- never depend on a player-scope policy also matching.
CREATE POLICY tenant_staff_scope ON wallet_balance_projection
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY player_self_scope ON wallet_balance_projection
    FOR SELECT
    USING (
        player_account_id IS NOT NULL
        AND player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
    );
