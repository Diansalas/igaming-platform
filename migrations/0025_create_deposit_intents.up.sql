-- DepositIntent: PaymentOrchestrator-owned workflow state for a deposit
-- attempt, from InitiateDeposit through to a posted LedgerTransaction
-- (see docs/architecture/payment-orchestration.md §3/§7). Distinct from
-- LedgerTransaction per ledger-accounting-model.md §4: the ledger itself
-- has no "pending" concept, so a not-yet-successful attempt lives here,
-- never as a partial ledger row.

CREATE TABLE deposit_intents (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL,
    brand_id              UUID NOT NULL,
    player_account_id     UUID NOT NULL,
    wallet_id             UUID NOT NULL,
    asset_code            TEXT NOT NULL,
    amount                NUMERIC(38, 0) NOT NULL CHECK (amount > 0),
    payment_method        TEXT NOT NULL,
    -- Client/session-supplied, so namespaced by tenant AND player - never
    -- a globally- or even tenant-only-unique column (docs/decisions/0022
    -- §3's binding rule: "a payload field never asserts the tenant", and
    -- a client-chosen value must not let one player deny another's
    -- deposit by guessing keys).
    idempotency_key       TEXT NOT NULL,
    provider_id           TEXT,
    provider_reference    TEXT,
    status                TEXT NOT NULL DEFAULT 'pending'
                              CHECK (status IN ('pending', 'succeeded', 'declined', 'ambiguous', 'failed')),
    ledger_transaction_id UUID REFERENCES ledger_transactions (id),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (wallet_id, tenant_id) REFERENCES wallets (id, tenant_id),
    FOREIGN KEY (wallet_id, player_account_id) REFERENCES wallets (id, player_account_id),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

ALTER TABLE deposit_intents
    ADD CONSTRAINT deposit_intents_tenant_player_idempotency_key UNIQUE (tenant_id, player_account_id, idempotency_key);
CREATE UNIQUE INDEX idx_deposit_intents_tenant_provider_ref
    ON deposit_intents (tenant_id, provider_id, provider_reference)
    WHERE provider_reference IS NOT NULL;

CREATE INDEX idx_deposit_intents_tenant ON deposit_intents (tenant_id);
CREATE INDEX idx_deposit_intents_player_account ON deposit_intents (player_account_id);

ALTER TABLE deposit_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE deposit_intents FORCE ROW LEVEL SECURITY;

-- Same two-policy shape as the ledger tables: a player may check their
-- own deposit's status (player_self_scope), while the orchestrator's own
-- callback-handling code runs under tenant-only scope
-- (tenant_staff_scope) with no player GUC set.
CREATE POLICY tenant_staff_scope ON deposit_intents
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY player_self_scope ON deposit_intents
    FOR SELECT
    USING (
        player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
    );
