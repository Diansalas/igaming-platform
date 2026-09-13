-- LedgerTransaction: the atomicity/idempotency boundary. Deliberately has
-- NO stored status column - 'posted' is the only state a completed row
-- has (append-only, per ADR 0001), and 'reversed' is a derived label
-- (queried via reverses_transaction_id), never a mutated field. See
-- docs/architecture/ledger-accounting-model.md §1.2 and the Stage 3A
-- `security` review correction recorded there.
--
-- transaction_type is deliberately limited to the types Stage 3B actually
-- posts (deposit/withdrawal/manual_adjustment/tombstone) rather than the
-- full Blueprint flow catalogue - casino/sportsbook/bonus/crypto types
-- are explicitly blocked this stage (Stage 3B scope gate) and are added
-- by an additive migration when their owning stage implements them.

CREATE TABLE ledger_transactions (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID NOT NULL,
    transaction_type        TEXT NOT NULL CHECK (transaction_type IN (
        'deposit', 'deposit_reversal',
        'withdrawal_requested', 'withdrawal_completed', 'withdrawal_rejected',
        'withdrawal_failed', 'withdrawal_reversed',
        'manual_adjustment', 'tombstone'
    )),
    idempotency_key         TEXT NOT NULL,
    provider_id             TEXT,
    provider_tx_id          TEXT,
    correlation_id          UUID NOT NULL,
    causation_id            UUID,
    reverses_transaction_id UUID REFERENCES ledger_transactions (id),
    reason_code             TEXT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    posted_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((provider_id IS NULL) = (provider_tx_id IS NULL)),
    -- CLAUDE.md's four-eyes rule: a manual adjustment must always carry a
    -- reason code.
    CHECK ((transaction_type = 'manual_adjustment') = (reason_code IS NOT NULL))
);

-- Both idempotency mechanisms are tenant-scoped (docs/decisions/0019 §3,
-- ADR 0020 "Idempotency key scope") - a platform-global unique key on a
-- tenant-partitioned, RLS-protected table is a cross-tenant collision and
-- existence-oracle risk, not merely a style preference.
ALTER TABLE ledger_transactions
    ADD CONSTRAINT ledger_transactions_tenant_idempotency_key_key UNIQUE (tenant_id, idempotency_key);
CREATE UNIQUE INDEX idx_ledger_transactions_tenant_provider_tx
    ON ledger_transactions (tenant_id, provider_id, provider_tx_id)
    WHERE provider_id IS NOT NULL;

-- Target for ledger_entries' composite FK (migration 0022) - the
-- database-level guarantee that an entry's tenant_id always matches its
-- transaction's tenant_id, independent of RLS.
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_id_tenant_key UNIQUE (id, tenant_id);

CREATE INDEX idx_ledger_transactions_tenant ON ledger_transactions (tenant_id);
CREATE INDEX idx_ledger_transactions_correlation ON ledger_transactions (correlation_id);
CREATE INDEX idx_ledger_transactions_reverses ON ledger_transactions (reverses_transaction_id) WHERE reverses_transaction_id IS NOT NULL;

ALTER TABLE ledger_transactions ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_transactions FORCE ROW LEVEL SECURITY;

-- No player-scope policy: a player never reads raw ledger_transactions
-- directly in Stage 3B (their own view is served through wallet balance
-- and withdrawal-request status endpoints, which read wallet_balance_
-- projection / withdrawal_requests instead - both of which carry their
-- own player-scoped policy). Tenant-scoped only, for staff/system.
CREATE POLICY tenant_isolation ON ledger_transactions
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Append-only enforcement: a trigger, not REVOKE, because the
-- application's own runtime role OWNS this table and a table owner can
-- re-GRANT itself any privilege (docs/decisions/0019 "Append-only
-- enforcement", ADR 0013's own Stage 2 correction for audit_log). Both a
-- row-level trigger (UPDATE/DELETE) and a statement-level trigger
-- (TRUNCATE, which row-level triggers never fire on) are required.
CREATE FUNCTION ledger_deny_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_transactions_immutable
    BEFORE UPDATE OR DELETE ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER ledger_transactions_no_truncate
    BEFORE TRUNCATE ON ledger_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();
