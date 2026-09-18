-- WageringProgress - the P_net/P_firm dual-measure contribution record
-- (doc 10 §W2.8; ledger-accounting-model.md §6.6.3/§6.6.4, "Model C").
-- Exact shape as specified there: "shape constrained by ledger-finance;
-- the table's name, package and migration belong to bonus-engine's own
-- stage." One append-only row per (Grant, lock ledger transaction).
--
-- This migration builds ONLY the append-only contribution record. It
-- does NOT implement the P_net/P_firm derivation itself (a read-only
-- computation over this table plus ledger_transactions/ledger_entries,
-- §6.6.5/§6.6.6), the nullification predicate, or the completion/
-- conversion gate - all Phase 3 business logic, per this dispatch's own
-- scope.
--
-- player_account_id is NOT in ledger-accounting-model.md §6.6.4's own
-- field table, but IS required by that same section's own RLS
-- instruction ("the player-self-scope read policy pattern ledger_accounts
-- uses, so a player can read their own progress trail") - ledger_accounts
-- denormalizes player_account_id onto its own row for exactly this
-- reason (that migration's own "Why wallet_id is on this row" rationale)
-- rather than requiring RLS to subquery through another RLS-protected
-- table. Added here as the same, necessary denormalization, not a field
-- §6.6.4 forgot to name but a structural precondition for the RLS shape
-- it explicitly requires.
CREATE TABLE bonus_wagering_progress (
    id                          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                   UUID NOT NULL,
    grant_id                    UUID NOT NULL,
    player_account_id           UUID NOT NULL,
    offer_version_id            UUID NOT NULL,
    lock_ledger_transaction_id  UUID NOT NULL,
    -- Denormalized from the posting (§6.6.4: "lets the netting query hit
    -- idx_ledger_transactions_correlation without a second hop").
    correlation_id              UUID NOT NULL,
    asset_code                  TEXT NOT NULL REFERENCES assets (code),
    -- Read from the posted ledger entry, NEVER from the caller (§6.6.4:
    -- "prevents a caller claiming a contribution larger than the debit
    -- that justified it").
    staked_bonus_amount         NUMERIC(38, 0) NOT NULL CHECK (staked_bonus_amount > 0),
    -- Basis points, exact integer, copied immutably from the Offer
    -- version at contribution time - never a float, never a live
    -- reference (§6.6.4).
    contribution_weight_bp      INTEGER NOT NULL CHECK (contribution_weight_bp BETWEEN 0 AND 10000),
    -- b x contribution_weight_bp, an EXACT scaled integer - no rounding,
    -- since this quantity is never posted (§6.6.10).
    qualifying_scaled           NUMERIC(38, 0) NOT NULL CHECK (qualifying_scaled >= 0),
    -- Recorded for HR-13 even though this exact-integer form needs no
    -- rounding today (§6.6.4) - nullable, since the rounding_rules
    -- reference table itself is not yet built (ledger-accounting-model.md
    -- §7.16: "still not built... unclaimed migration"; internal/money's
    -- own doc comment discloses the same gap).
    rounding_rule_id            UUID,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),

    -- HR-10's DB-enforced idempotency - not "check then insert", not a
    -- per-attempt UUID.
    UNIQUE (tenant_id, grant_id, lock_ledger_transaction_id),
    FOREIGN KEY (grant_id, tenant_id) REFERENCES bonus_grants (id, tenant_id),
    FOREIGN KEY (offer_version_id, tenant_id) REFERENCES bonus_offer_versions (id, tenant_id),
    FOREIGN KEY (lock_ledger_transaction_id, tenant_id) REFERENCES ledger_transactions (id, tenant_id)
);

CREATE INDEX idx_bonus_wagering_progress_tenant ON bonus_wagering_progress (tenant_id);
-- "grant lookup by ... " family - the P_net/P_firm derivation's own read
-- pattern (every contribution for a Grant).
CREATE INDEX idx_bonus_wagering_progress_grant ON bonus_wagering_progress (tenant_id, grant_id);
CREATE INDEX idx_bonus_wagering_progress_player ON bonus_wagering_progress (tenant_id, player_account_id);
CREATE INDEX idx_bonus_wagering_progress_correlation ON bonus_wagering_progress (tenant_id, correlation_id);

-- Append-only (§6.6.4): "no UPDATE/DELETE policy plus the BEFORE UPDATE
-- OR DELETE / BEFORE TRUNCATE trigger pair invariant #2 already uses. A
-- contribution is a historical fact."
CREATE TRIGGER bonus_wagering_progress_immutable
    BEFORE UPDATE OR DELETE ON bonus_wagering_progress
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER bonus_wagering_progress_no_truncate
    BEFORE TRUNCATE ON bonus_wagering_progress
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

ALTER TABLE bonus_wagering_progress ENABLE ROW LEVEL SECURITY;
ALTER TABLE bonus_wagering_progress FORCE ROW LEVEL SECURITY;

-- ledger_accounts' dual scope (§6.6.4's own explicit instruction), staff
-- half split per command (migration 0047 Fix 3's "no FOR ALL" lesson),
-- no UPDATE policy at all (the immutability trigger above already
-- rejects every UPDATE unconditionally).
CREATE POLICY tenant_staff_read ON bonus_wagering_progress
    FOR SELECT
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
CREATE POLICY tenant_staff_insert ON bonus_wagering_progress
    FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
CREATE POLICY player_self_scope ON bonus_wagering_progress
    FOR SELECT
    USING (
        player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
    );
