-- GrantLedgerAttribution (docs/architecture/10-bonus-engine-architecture.md
-- "doc 10" §W2.5) - the mechanism that makes "this Grant's remaining
-- bonus balance" answerable WITHOUT a maintained counter, per CLAUDE.md's
-- "balances are projections recomputed from ledger entries" rule and
-- doc 10 §W2.5's own binding text:
--
--   "To make 'this Grant's remaining bonus balance' answerable without a
--   maintained counter, Bonus Engine owns an append-only
--   GrantLedgerAttribution record: one row per (Grant, ledger
--   transaction), written in the same database transaction as the
--   posting it attributes, DB-unique on (tenant_id, grant_id,
--   ledger_transaction_id)... A Grant's remaining bonus balance is
--   always a derived read (Sum over this table's attributed entries),
--   never a stored figure."
--
-- Deferred by Phase 2 (Stage 4H-B1 Wave 2), built here by bonus-engine's
-- own Phase 3 implementation stage. This table carries NO monetary field
-- of its own beyond the sign/amount already implied by the referenced
-- ledger_transaction's own entries - it is a pure attribution index, not
-- a second ledger. bonus-engine reads the referenced transaction's own
-- entries (via ledger_entries) to derive amount/direction rather than
-- duplicating them here, which is what keeps this table from ever being
-- able to disagree with the ledger it attributes.
CREATE TABLE grant_ledger_attributions (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NOT NULL,
    grant_id                 UUID NOT NULL,
    ledger_transaction_id    UUID NOT NULL,
    -- Denormalized purely for cheap filtering/observability (e.g. "every
    -- attribution of kind X for this Grant") - never consulted for the
    -- balance derivation itself, which always reads the real ledger
    -- entries via ledger_transaction_id (this table's own doc comment
    -- above: "carrying no field the ledger does not already have except
    -- the Grant it belongs to").
    transaction_type         TEXT NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),

    FOREIGN KEY (grant_id, tenant_id) REFERENCES bonus_grants (id, tenant_id),
    FOREIGN KEY (ledger_transaction_id, tenant_id) REFERENCES ledger_transactions (id, tenant_id)
);

-- The load-bearing uniqueness (§W2.5): one attribution row per (Grant,
-- ledger transaction) - a retried posting attempt that resolves to the
-- SAME already-posted transaction (ledger.Post's own idempotency) must
-- never attribute twice.
ALTER TABLE grant_ledger_attributions
    ADD CONSTRAINT grant_ledger_attributions_grant_tx_key
        UNIQUE (tenant_id, grant_id, ledger_transaction_id);

CREATE INDEX idx_grant_ledger_attributions_tenant ON grant_ledger_attributions (tenant_id);
CREATE INDEX idx_grant_ledger_attributions_grant ON grant_ledger_attributions (tenant_id, grant_id);
-- Supports "every Grant a given ledger transaction is attributed to"
-- (a reversal/rollback needing to find which Grant(s) a transaction it is
-- correcting belonged to).
CREATE INDEX idx_grant_ledger_attributions_tx ON grant_ledger_attributions (tenant_id, ledger_transaction_id);

-- Append-only (§W1's common object contract: "no mutable financial
-- history... never an UPDATE of a historical fact").
CREATE FUNCTION grant_ledger_attributions_deny_update() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'grant_ledger_attributions: rows are immutable and append-only (doc 10 §W2.5)';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER grant_ledger_attributions_deny_update
    BEFORE UPDATE ON grant_ledger_attributions
    FOR EACH ROW EXECUTE FUNCTION grant_ledger_attributions_deny_update();

CREATE TRIGGER grant_ledger_attributions_deny_delete
    BEFORE DELETE ON grant_ledger_attributions
    FOR EACH ROW EXECUTE FUNCTION grant_ledger_attributions_deny_update();

CREATE TRIGGER grant_ledger_attributions_no_truncate
    BEFORE TRUNCATE ON grant_ledger_attributions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

ALTER TABLE grant_ledger_attributions ENABLE ROW LEVEL SECURITY;
ALTER TABLE grant_ledger_attributions FORCE ROW LEVEL SECURITY;

-- Staff-only (mirrors bonus_held_dispositions' precedent: an internal
-- financial-control/reconciliation record, not player-facing state in
-- its own right - a player's OWN remaining balance is exposed through
-- the Grant read model, not by exposing this attribution index directly).
CREATE POLICY tenant_staff_read ON grant_ledger_attributions
    FOR SELECT
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
CREATE POLICY tenant_staff_insert ON grant_ledger_attributions
    FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
