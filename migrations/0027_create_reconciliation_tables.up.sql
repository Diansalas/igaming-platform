-- Reconciliation framework (docs/architecture/reconciliation-model.md
-- §5). Tenant-scoped, staff/system only - no player access, and
-- deliberately NO dual-scope (platform-wide) policy: a cross-tenant
-- drift dashboard reads through the reporting layer, never a
-- WithoutTenant connection against these operational tables
-- (docs/decisions/0019 "RLS and tenancy shape").

CREATE TABLE reconciliation_runs (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL,
    stream     TEXT NOT NULL, -- e.g. 'ledger_vs_projection', 'wallet_vs_psp'
    period_start TIMESTAMPTZ NOT NULL,
    period_end   TIMESTAMPTZ NOT NULL,
    run_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    status     TEXT NOT NULL CHECK (status IN ('clean', 'mismatches_found')),
    CHECK (period_end >= period_start)
);

CREATE INDEX idx_reconciliation_runs_tenant ON reconciliation_runs (tenant_id, stream, run_at DESC);
ALTER TABLE reconciliation_runs ADD CONSTRAINT reconciliation_runs_id_tenant_key UNIQUE (id, tenant_id);

ALTER TABLE reconciliation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_runs FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON reconciliation_runs
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE TABLE reconciliation_mismatches (
    id                            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                     UUID NOT NULL,
    reconciliation_run_id         UUID NOT NULL,
    reconciliation_key            TEXT NOT NULL, -- e.g. a ledger_account_id or (provider_id, provider_tx_id) rendered as text
    expected_value                TEXT NOT NULL,
    actual_value                  TEXT NOT NULL,
    investigation_status          TEXT NOT NULL DEFAULT 'open'
                                      CHECK (investigation_status IN ('open', 'investigating', 'resolved')),
    resolution_note               TEXT,
    resolved_by                   UUID,
    resolved_at                   TIMESTAMPTZ,
    -- A resolution that changes the ledger does so exclusively via a
    -- compensating LedgerTransaction (reconciliation-model.md §5) - this
    -- row is never itself a mechanism for mutating historical data.
    correction_ledger_transaction_id UUID REFERENCES ledger_transactions (id),
    created_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((investigation_status = 'resolved') = (resolved_at IS NOT NULL)),
    FOREIGN KEY (reconciliation_run_id, tenant_id) REFERENCES reconciliation_runs (id, tenant_id)
);

CREATE INDEX idx_reconciliation_mismatches_run ON reconciliation_mismatches (reconciliation_run_id);
CREATE INDEX idx_reconciliation_mismatches_tenant_status ON reconciliation_mismatches (tenant_id, investigation_status);

ALTER TABLE reconciliation_mismatches ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_mismatches FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON reconciliation_mismatches
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
