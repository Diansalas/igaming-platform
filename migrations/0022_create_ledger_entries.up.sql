-- LedgerEntry: explicit debit/credit direction with a strictly positive
-- amount (never a signed column - see
-- docs/architecture/ledger-accounting-model.md §1.3). tenant_id,
-- wallet_id, player_account_id and asset_code are all denormalized from
-- the owning ledger_account by the trigger below - never trusted from
-- the caller - so RLS policies can check them directly on this row
-- without a subquery into another RLS-protected table.

CREATE TABLE ledger_entries (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ledger_transaction_id  UUID NOT NULL,
    ledger_account_id      UUID NOT NULL,
    tenant_id              UUID NOT NULL,
    wallet_id              UUID,
    player_account_id      UUID,
    asset_code             TEXT NOT NULL,
    direction              TEXT NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount                 NUMERIC(38, 0) NOT NULL CHECK (amount > 0),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Pins tenant_id to the TRANSACTION side (a transaction never spans
    -- tenants) - the account-side FK below alone does not achieve this.
    FOREIGN KEY (ledger_transaction_id, tenant_id)
        REFERENCES ledger_transactions (id, tenant_id),
    -- Pins tenant_id and asset_code to the ACCOUNT side. wallet_id is
    -- deliberately NOT part of this FK: it is NULL for house-level
    -- entries, and a composite FK containing a NULL column is satisfied
    -- trivially under MATCH SIMPLE, which would disable the check for
    -- exactly the house-level rows it must still constrain. wallet_id/
    -- player_account_id agreement with the account is enforced by the
    -- populate-from-account trigger below instead, which is also what
    -- sets these columns in the first place (never trusted from the
    -- caller) - see ledger-accounting-model.md §1.3.
    FOREIGN KEY (ledger_account_id, tenant_id, asset_code)
        REFERENCES ledger_accounts (id, tenant_id, asset_code)
);

CREATE INDEX idx_ledger_entries_transaction ON ledger_entries (ledger_transaction_id);
CREATE INDEX idx_ledger_entries_account ON ledger_entries (ledger_account_id, created_at);
CREATE INDEX idx_ledger_entries_tenant ON ledger_entries (tenant_id);
CREATE INDEX idx_ledger_entries_player_account ON ledger_entries (player_account_id) WHERE player_account_id IS NOT NULL;

-- Populates tenant_id/wallet_id/player_account_id/asset_code from the
-- ledger_account, and rejects a caller-supplied value that disagrees -
-- the same trigger-based denormalization pattern as migration 0020, for
-- the same nullable-composite-FK reason.
CREATE FUNCTION ledger_entries_populate_from_account() RETURNS TRIGGER AS $$
DECLARE
    a_tenant_id UUID;
    a_wallet_id UUID;
    a_player_account_id UUID;
    a_asset_code TEXT;
BEGIN
    SELECT tenant_id, wallet_id, player_account_id, asset_code
        INTO a_tenant_id, a_wallet_id, a_player_account_id, a_asset_code
        FROM ledger_accounts WHERE id = NEW.ledger_account_id;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger_entries: ledger_account % does not exist', NEW.ledger_account_id;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM a_tenant_id THEN
        RAISE EXCEPTION 'ledger_entries: tenant_id % does not match ledger_account %''s tenant %', NEW.tenant_id, NEW.ledger_account_id, a_tenant_id;
    END IF;
    IF NEW.asset_code IS DISTINCT FROM a_asset_code THEN
        RAISE EXCEPTION 'ledger_entries: asset_code % does not match ledger_account %''s asset %', NEW.asset_code, NEW.ledger_account_id, a_asset_code;
    END IF;

    NEW.wallet_id := a_wallet_id;
    NEW.player_account_id := a_player_account_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_entries_populate_from_account_trigger
    BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_populate_from_account();

-- Mandatory Financial Invariant #1 (ledger-accounting-model.md §6):
-- debits = credits, per transaction, per asset. A deferred constraint
-- trigger, checked once at commit (or at an explicit SET CONSTRAINTS ALL
-- IMMEDIATE), so it sees the transaction's complete, final set of
-- entries rather than rejecting a still-in-progress multi-entry post.
CREATE FUNCTION ledger_entries_check_balance() RETURNS TRIGGER AS $$
DECLARE
    imbalance NUMERIC;
BEGIN
    SELECT COALESCE(SUM(CASE WHEN direction = 'debit' THEN amount ELSE -amount END), 0)
        INTO imbalance
        FROM ledger_entries
        WHERE ledger_transaction_id = NEW.ledger_transaction_id AND asset_code = NEW.asset_code;

    IF imbalance <> 0 THEN
        RAISE EXCEPTION 'ledger_entries: unbalanced transaction % for asset % (debits - credits = %)',
            NEW.ledger_transaction_id, NEW.asset_code, imbalance;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER ledger_entries_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_check_balance();

-- The balance-projection trigger (ledger_entries_project_balance) is
-- attached in migration 0023, once wallet_balance_projection exists to
-- write into - see that migration for why it is a trigger rather than
-- application code.

ALTER TABLE ledger_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_entries FORCE ROW LEVEL SECURITY;

-- Same two-policy shape as wallets/ledger_accounts (docs/decisions/0019
-- "The ADR 0016 gotcha applies here, and bites hardest on the
-- projection" - the same reasoning applies to this table, which every
-- posting writes to under tenant-only scope with no player GUC set).
CREATE POLICY tenant_staff_scope ON ledger_entries
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY player_self_scope ON ledger_entries
    FOR SELECT
    USING (
        player_account_id IS NOT NULL
        AND player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
    );

CREATE TRIGGER ledger_entries_immutable
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER ledger_entries_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();
