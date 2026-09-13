-- WithdrawalRequest: the withdrawal workflow's own state, separate from
-- LedgerTransaction (docs/architecture/withdrawal-state-machine.md §2 -
-- the ledger itself has no "pending" concept). provider_id/
-- provider_reference are populated once the request reaches 'submitted',
-- carrying the same role payment-orchestration.md's DepositIntent plays
-- for deposits - Stage 3B does not duplicate that into a separate
-- "WithdrawalIntent" table.

CREATE TABLE withdrawal_requests (
    id                             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                      UUID NOT NULL,
    brand_id                       UUID NOT NULL,
    player_account_id              UUID NOT NULL,
    wallet_id                      UUID NOT NULL,
    asset_code                     TEXT NOT NULL,
    amount                         NUMERIC(38, 0) NOT NULL CHECK (amount > 0),
    state                          TEXT NOT NULL DEFAULT 'requested' CHECK (state IN (
        'requested', 'pending_review', 'approved', 'rejected',
        'submitted', 'completed', 'failed', 'cancelled', 'reversed'
    )),
    -- Client/session-supplied, namespaced by tenant AND player - see
    -- deposit_intents' identical rationale and docs/decisions/0022 §3.
    idempotency_key                TEXT NOT NULL,
    provider_id                    TEXT,
    provider_reference             TEXT,
    hold_ledger_transaction_id     UUID REFERENCES ledger_transactions (id),
    release_ledger_transaction_id  UUID REFERENCES ledger_transactions (id),
    requested_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                     TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (wallet_id, tenant_id) REFERENCES wallets (id, tenant_id),
    FOREIGN KEY (wallet_id, player_account_id) REFERENCES wallets (id, player_account_id),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

ALTER TABLE withdrawal_requests
    ADD CONSTRAINT withdrawal_requests_tenant_player_idempotency_key UNIQUE (tenant_id, player_account_id, idempotency_key);
ALTER TABLE withdrawal_requests ADD CONSTRAINT withdrawal_requests_id_tenant_key UNIQUE (id, tenant_id);
CREATE UNIQUE INDEX idx_withdrawal_requests_tenant_provider_ref
    ON withdrawal_requests (tenant_id, provider_id, provider_reference)
    WHERE provider_reference IS NOT NULL;

CREATE INDEX idx_withdrawal_requests_tenant ON withdrawal_requests (tenant_id);
CREATE INDEX idx_withdrawal_requests_player_account ON withdrawal_requests (player_account_id);

-- Immutable-after-insert guard (docs/architecture/withdrawal-state-
-- machine.md §7): without it, the four-eyes threshold check is trivially
-- bypassable (request a below-threshold amount, collect the single
-- approval it needs, then raise the amount), and the hold posted at
-- `requested` would desynchronize from the request. `state`,
-- `provider_id`, `provider_reference`, the two ledger-transaction
-- columns, and `updated_at` are exactly the columns the workflow itself
-- is expected to change; everything else is frozen at creation.
CREATE FUNCTION withdrawal_requests_enforce_immutable_fields() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
        OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
        OR NEW.wallet_id IS DISTINCT FROM OLD.wallet_id
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
        OR NEW.amount IS DISTINCT FROM OLD.amount
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.requested_at IS DISTINCT FROM OLD.requested_at
    THEN
        RAISE EXCEPTION 'withdrawal_requests: amount/asset/wallet/player/tenant/brand/idempotency_key/requested_at are immutable after insert';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER withdrawal_requests_immutable_fields
    BEFORE UPDATE ON withdrawal_requests
    FOR EACH ROW EXECUTE FUNCTION withdrawal_requests_enforce_immutable_fields();

ALTER TABLE withdrawal_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE withdrawal_requests FORCE ROW LEVEL SECURITY;

-- Same two-policy shape as the ledger tables (docs/architecture/
-- withdrawal-state-machine.md §7's own restatement of the ADR 0016/0019
-- gotcha): every state transition here is a staff/system UPDATE running
-- in tenant-only scope, which must not be filtered out by a player-scope
-- policy also being present.
CREATE POLICY tenant_staff_scope ON withdrawal_requests
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- Players get SELECT only (list/check status of their own requests) -
-- never INSERT/UPDATE, since a player-writable row here would be a
-- complete four-eyes bypass (withdrawal-state-machine.md §7).
CREATE POLICY player_self_scope ON withdrawal_requests
    FOR SELECT
    USING (
        player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
    );

-- WithdrawalApproval: the four-eyes record (docs/architecture/
-- withdrawal-state-machine.md §5, and the bypass-closure subsection the
-- Stage 3A security review added there). Append-only: a reconsideration
-- is a new request, never an edited decision.

CREATE TABLE withdrawal_approvals (
    id                            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                     UUID NOT NULL,
    withdrawal_request_id         UUID NOT NULL,
    approver_principal_id         UUID NOT NULL,
    -- True only for a risk-engine/service-identity auto-approval (ADR
    -- 0014) below the four-eyes threshold - such a row can never count as
    -- one of the two required human approvals for an above-threshold
    -- request (bypass #6 in the withdrawal state machine doc).
    is_automated_approval         BOOLEAN NOT NULL DEFAULT false,
    decision                      TEXT NOT NULL CHECK (decision IN ('approve', 'reject')),
    reason_code                   TEXT,
    threshold_amount_at_decision  NUMERIC(38, 0) NOT NULL,
    request_amount_at_decision    NUMERIC(38, 0) NOT NULL,
    decided_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- reason_code is required on 'reject', optional on 'approve'.
    CHECK (decision = 'approve' OR reason_code IS NOT NULL),
    UNIQUE (withdrawal_request_id, approver_principal_id),
    FOREIGN KEY (withdrawal_request_id, tenant_id) REFERENCES withdrawal_requests (id, tenant_id)
);

CREATE INDEX idx_withdrawal_approvals_request ON withdrawal_approvals (withdrawal_request_id);
CREATE INDEX idx_withdrawal_approvals_tenant ON withdrawal_approvals (tenant_id);

ALTER TABLE withdrawal_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE withdrawal_approvals FORCE ROW LEVEL SECURITY;

-- Staff/system only - a player-writable or player-readable approvals
-- table would itself be a four-eyes bypass vector (who approved what is
-- an internal control detail, not player-facing data).
CREATE POLICY tenant_isolation ON withdrawal_approvals
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE TRIGGER withdrawal_approvals_immutable
    BEFORE UPDATE OR DELETE ON withdrawal_approvals
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER withdrawal_approvals_no_truncate
    BEFORE TRUNCATE ON withdrawal_approvals
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();
