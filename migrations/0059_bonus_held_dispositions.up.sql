-- bonus_held_dispositions - the HeldDispositionRecord. THIS TABLE'S
-- SCHEMA IS FROZEN, ledger-finance's own binding contract
-- (ledger-accounting-model.md §7.7.2.5, verbatim-mirrored at doc
-- 10-bonus-engine-architecture.md §N1.9): "build bonus_held_dispositions
-- to exactly §7.7.2.5's contract... this document invents no field names
-- of its own." Every column, CHECK, and constraint below is copied
-- field-for-field from that contract - do not improvise, per this
-- dispatch's own explicit instruction.
--
-- Ownership: bonus-engine-owned (this migration, per §7.7.2.5: "the same
-- split as WageringProgress/§6.6.4: ledger-finance specifies the
-- contract, bonus-engine builds and owns the migration, in its own
-- 0054+ range" - landing here as 0059 rather than the design document's
-- own speculative earlier numbering, per this codebase's established
-- "trust the filesystem over a speculative number in a design document"
-- precedent, migration 0052's own comment).
--
-- Schema and plumbing only (Stage 4H-B1 Wave 2 Phase 2). This migration
-- does NOT implement: the hold-capture posting (§7.7.2.2, casino/
-- ledger's job), the resolution transaction/HR-25 lock order (§7.7.2.9),
-- the rollback-of-a-held-win transition (§7.7.2.7), or the WP-R/held-
-- disposition reconciliation stream (§7.7.2.8). Those are Phase 3.
CREATE TABLE bonus_held_dispositions (
    id                                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                         UUID NOT NULL,   -- RLS
    brand_id                          UUID NOT NULL,   -- denormalized from Wallet
    wallet_id                         UUID NOT NULL,   -- FK wallets.id
    player_account_id                 UUID NOT NULL,   -- denormalized from Wallet; RLS-key parity with LedgerAccount
    asset_code                        TEXT NOT NULL REFERENCES assets (code),
    grant_id                          UUID NOT NULL,   -- FK Grant; the G this occurrence is attributed to
    correlation_id                    UUID NOT NULL,   -- underlying round's correlation_id - AUDIT TRAIL ONLY, never the idempotency key (LF-22)
    settlement_ledger_transaction_id  UUID NOT NULL,   -- FK ledger_transactions.id; the ONE balanced transaction that posted this occurrence's hold-capture
    payout_amount                     NUMERIC(38, 0) NOT NULL CHECK (payout_amount >= 0),             -- W
    released_lock_amount              NUMERIC(38, 0) NOT NULL DEFAULT 0 CHECK (released_lock_amount >= 0),  -- X; 0 if never locked
    status                            TEXT NOT NULL DEFAULT 'held'
                                          CHECK (status IN ('held', 'resolved_reforfeit',
                                                             'resolved_route_to_cash', 'voided_by_rollback')),
    created_at                        TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    resolved_at                       TIMESTAMPTZ,
    resolved_by_actor_id              UUID,   -- staff actor applying the G-2 answer; feeds REQ-SEP-BONUS-4, does not itself enforce it
    resolution_reason_code            TEXT,   -- CLAUDE.md reason-code requirement for the manual/administrative act
    resolution_ledger_transaction_id  UUID,   -- FK ledger_transactions.id; the transaction that moved the held value out. NULL while 'held';
                                              -- for 'voided_by_rollback' this is the REVERSAL's transaction id

    CHECK (payout_amount + released_lock_amount > 0),               -- no vacuous hold
    CHECK ((status = 'held') = (resolved_at IS NULL)),
    CHECK (status <> 'held' OR resolution_ledger_transaction_id IS NULL),

    FOREIGN KEY (wallet_id, tenant_id) REFERENCES wallets (id, tenant_id),
    FOREIGN KEY (wallet_id, player_account_id) REFERENCES wallets (id, player_account_id),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id),
    FOREIGN KEY (grant_id, tenant_id) REFERENCES bonus_grants (id, tenant_id),
    FOREIGN KEY (settlement_ledger_transaction_id, tenant_id) REFERENCES ledger_transactions (id, tenant_id),
    FOREIGN KEY (resolution_ledger_transaction_id, tenant_id) REFERENCES ledger_transactions (id, tenant_id)
);

-- THE per-occurrence idempotency key (§7.7.2.6, LF-22's fix) - NOT
-- correlation_id, NOT grant_id.
ALTER TABLE bonus_held_dispositions
    ADD CONSTRAINT bonus_held_dispositions_settlement_tx_key
        UNIQUE (tenant_id, settlement_ledger_transaction_id);

CREATE INDEX idx_bonus_held_dispositions_tenant ON bonus_held_dispositions (tenant_id);
CREATE INDEX idx_bonus_held_dispositions_grant ON bonus_held_dispositions (tenant_id, grant_id);
CREATE INDEX idx_bonus_held_dispositions_correlation ON bonus_held_dispositions (tenant_id, correlation_id);
-- "held-disposition lookup by status for the aging/reconciliation sweep"
-- (this dispatch's own named requirement; §7.7.2.8's aging stream).
CREATE INDEX idx_bonus_held_dispositions_status ON bonus_held_dispositions (tenant_id, status);

-- Immutable-after-insert guard for the occurrence-defining fields;
-- status/resolved_*/resolution_* are exactly the columns a real
-- resolution (Phase 3) is expected to change, mirroring
-- withdrawal_requests_enforce_immutable_fields's shape.
CREATE FUNCTION bonus_held_dispositions_enforce_immutable_fields() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
        OR NEW.wallet_id IS DISTINCT FROM OLD.wallet_id
        OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
        OR NEW.grant_id IS DISTINCT FROM OLD.grant_id
        OR NEW.correlation_id IS DISTINCT FROM OLD.correlation_id
        OR NEW.settlement_ledger_transaction_id IS DISTINCT FROM OLD.settlement_ledger_transaction_id
        OR NEW.payout_amount IS DISTINCT FROM OLD.payout_amount
        OR NEW.released_lock_amount IS DISTINCT FROM OLD.released_lock_amount
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'bonus_held_dispositions: occurrence-defining fields are immutable after insert (ledger-accounting-model.md §7.7.2.5)';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER bonus_held_dispositions_immutable_fields
    BEFORE UPDATE ON bonus_held_dispositions
    FOR EACH ROW EXECUTE FUNCTION bonus_held_dispositions_enforce_immutable_fields();

CREATE TRIGGER bonus_held_dispositions_no_truncate
    BEFORE TRUNCATE ON bonus_held_dispositions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

ALTER TABLE bonus_held_dispositions ENABLE ROW LEVEL SECURITY;
ALTER TABLE bonus_held_dispositions FORCE ROW LEVEL SECURITY;

-- RLS: tenant_id, mirroring every other tenant-owned table (CLAUDE.md;
-- §7.7.2.5's own instruction). No player-facing policy: a held,
-- undisposed G-2 occurrence is an internal financial-control record, not
-- player-facing state (mirrors bonus_suggestions'/bonus_change_requests'
-- staff-only precedent, not bonus_grants'/bonus_wagering_progress' dual
-- scope).
CREATE POLICY tenant_staff_read ON bonus_held_dispositions
    FOR SELECT
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
CREATE POLICY tenant_staff_insert ON bonus_held_dispositions
    FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
CREATE POLICY tenant_staff_update ON bonus_held_dispositions
    FOR UPDATE
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
