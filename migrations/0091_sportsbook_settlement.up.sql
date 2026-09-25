-- Stage 10 W1 — sportsbook settlement for cash-funded singles, in-house
-- mock mode (ADR 0088; ADR 0087 approved scope). The settlement driver is
-- a MOCK (a test-support staff route, ADR 0088 §9), never a real provider
-- integration.
--
-- Contents (ADR 0088 §12 — one migration, committed together with the
-- internal/risk ReversalTypes change, INV-SB-CUM-1):
--   1. ledger_transactions.transaction_type: 17 -> 20 values
--      (sportsbook_settlement, sportsbook_void, sportsbook_rollback).
--   2. UNIQUE (id, tenant_id) on sportsbook_bets (composite FK target).
--   3. sportsbook_bet_settlements: append-only settlement history, FORCE
--      RLS, deny triggers, partial unique indexes, trigger T-1.
--   4. Trigger T-2 on sportsbook_bets: status is a cache of the history.
--   5. reconciliation_mismatches.mismatch_kind widened for the sportsbook
--      reconciliation stream (ADR 0088 §8).
--   6. Guarded REVOKE of UPDATE/DELETE/TRUNCATE on the history table from
--      igaming_runtime (defence in depth; the deny triggers are the
--      binding control, ADR 0088 §3.5).

-- 1. Transaction types (same drop/recreate + constraint-validation guard
-- as migration 0078). Purely additive.
ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_transaction_type_check;
DO $$
BEGIN
    ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_transaction_type_check CHECK (transaction_type IN (
        'deposit', 'deposit_reversal',
        'withdrawal_requested', 'withdrawal_completed', 'withdrawal_rejected',
        'withdrawal_failed', 'withdrawal_reversed',
        'manual_adjustment', 'tombstone',
        'casino_bet', 'casino_win', 'casino_rollback',
        'bonus_grant', 'bonus_conversion', 'bonus_forfeiture', 'bonus_reversal',
        'sportsbook_bet',
        'sportsbook_settlement', 'sportsbook_void', 'sportsbook_rollback'
    ));
EXCEPTION WHEN check_violation THEN
    -- Believed unreachable (strict superset); constraint validation cannot
    -- be blinded by FORCE ROW LEVEL SECURITY. No RLS setting is toggled.
    RAISE EXCEPTION 'migration 0091: ledger_transactions holds a transaction_type outside the twenty admitted values (detected at constraint validation, which row-level security cannot filter)';
END $$;

-- 2. Composite key so the history table can pin (bet_id, tenant_id).
ALTER TABLE sportsbook_bets ADD CONSTRAINT sportsbook_bets_id_tenant_key UNIQUE (id, tenant_id);

-- 3. Append-only settlement history (ADR 0088 §3.2).
CREATE TABLE sportsbook_bet_settlements (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL,
    bet_id                 UUID NOT NULL,
    event_kind             TEXT NOT NULL CHECK (event_kind IN ('settlement', 'rollback', 'void', 'tombstone')),
    generation             INTEGER CHECK (generation >= 1),
    outcome                TEXT CHECK (outcome IN ('won', 'lost')),
    -- BIGINT to match sportsbook_bets.stake_amount/potential_return and the
    -- int64 posting path; NUMERIC(38,0) widening is recorded debt (ADR 0088
    -- §3.6).
    payout_amount          BIGINT CHECK (payout_amount >= 0),
    asset_code             TEXT NOT NULL REFERENCES assets (code),
    void_reason            TEXT CHECK (void_reason IN ('market_cancelled', 'push', 'data_error')),
    reverses_settlement_id UUID REFERENCES sportsbook_bet_settlements (id),
    causation_record_id    UUID REFERENCES sportsbook_bet_settlements (id),
    ledger_transaction_id  UUID NOT NULL,
    actor_staff_account_id UUID,
    request_id             TEXT,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (bet_id, tenant_id) REFERENCES sportsbook_bets (id, tenant_id),
    FOREIGN KEY (ledger_transaction_id, tenant_id) REFERENCES ledger_transactions (id, tenant_id),
    CONSTRAINT sportsbook_bet_settlements_ledger_transaction_key UNIQUE (ledger_transaction_id),
    -- Shape per event_kind (the NOT NULL-iff rules of ADR 0088 §3.2).
    CONSTRAINT sportsbook_bet_settlements_settlement_shape CHECK (
        event_kind <> 'settlement'
        OR (generation IS NOT NULL AND outcome IS NOT NULL AND payout_amount IS NOT NULL
            AND void_reason IS NULL AND reverses_settlement_id IS NULL)),
    CONSTRAINT sportsbook_bet_settlements_rollback_shape CHECK (
        event_kind <> 'rollback'
        OR (generation IS NOT NULL AND outcome IS NULL AND payout_amount IS NULL
            AND void_reason IS NULL AND reverses_settlement_id IS NOT NULL)),
    CONSTRAINT sportsbook_bet_settlements_void_shape CHECK (
        event_kind <> 'void'
        OR (generation IS NULL AND outcome IS NULL AND payout_amount IS NULL
            AND void_reason IS NOT NULL AND reverses_settlement_id IS NULL)),
    CONSTRAINT sportsbook_bet_settlements_tombstone_shape CHECK (
        event_kind <> 'tombstone'
        OR (generation IS NOT NULL AND outcome IS NULL AND payout_amount IS NULL
            AND void_reason IS NULL AND reverses_settlement_id IS NULL AND causation_record_id IS NULL))
);

-- State uniqueness (ADR 0088 §3.2): the backstop behind INV-LOCK-E4.
CREATE UNIQUE INDEX sportsbook_bet_settlements_one_per_generation
    ON sportsbook_bet_settlements (bet_id, generation) WHERE event_kind IN ('settlement', 'tombstone');
CREATE UNIQUE INDEX sportsbook_bet_settlements_one_rollback_per_settlement
    ON sportsbook_bet_settlements (reverses_settlement_id) WHERE event_kind = 'rollback';
CREATE UNIQUE INDEX sportsbook_bet_settlements_one_void_per_bet
    ON sportsbook_bet_settlements (bet_id) WHERE event_kind = 'void';
CREATE INDEX idx_sportsbook_bet_settlements_bet ON sportsbook_bet_settlements (tenant_id, bet_id);
CREATE INDEX idx_sportsbook_bet_settlements_created ON sportsbook_bet_settlements (tenant_id, created_at);

ALTER TABLE sportsbook_bet_settlements ENABLE ROW LEVEL SECURITY;
ALTER TABLE sportsbook_bet_settlements FORCE ROW LEVEL SECURITY;

-- Append-only, so no FOR ALL policy (security review S4): staff/system
-- may read and insert under tenant scope; a player may only read rows of
-- their own bets. No UPDATE/DELETE policy exists.
CREATE POLICY tenant_staff_select ON sportsbook_bet_settlements
    FOR SELECT
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY tenant_staff_insert ON sportsbook_bet_settlements
    FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY player_self_scope ON sportsbook_bet_settlements
    FOR SELECT
    USING (
        EXISTS (
            SELECT 1 FROM sportsbook_bets b
             WHERE b.id = sportsbook_bet_settlements.bet_id
               AND b.player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
        )
    );

CREATE TRIGGER sportsbook_bet_settlements_immutable
    BEFORE UPDATE OR DELETE ON sportsbook_bet_settlements
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER sportsbook_bet_settlements_no_truncate
    BEFORE TRUNCATE ON sportsbook_bet_settlements
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- T-1 (ADR 0088 §3.3): validates every history insert against the bet and
-- its existing history. SECURITY INVOKER (the default): it runs under the
-- caller's RLS and RAISEs when the parent bet is not visible or the
-- connection is player-scoped, so an empty read is never "valid".
-- Soundness of this read-then-insert rests on INV-LOCK-E4 (every writer
-- holds the bet's FOR UPDATE row lock); the partial unique indexes above
-- are the backstop.
CREATE FUNCTION sportsbook_bet_settlements_validate() RETURNS TRIGGER AS $$
DECLARE
    bet            sportsbook_bets%ROWTYPE;
    max_generation INTEGER;
    has_void       BOOLEAN;
    has_unreversed BOOLEAN;
    target         sportsbook_bet_settlements%ROWTYPE;
    latest_id      UUID;
    ltx_type       TEXT;
    ltx_corr       UUID;
    expected_type  TEXT;
    cause          sportsbook_bet_settlements%ROWTYPE;
    cause_xmin     TEXT;
BEGIN
    IF NULLIF(current_setting('app.player_account_id', true), '') IS NOT NULL THEN
        RAISE EXCEPTION 'sportsbook_bet_settlements: history rows are never written under a player-scoped connection';
    END IF;

    SELECT * INTO bet FROM sportsbook_bets WHERE id = NEW.bet_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'sportsbook_bet_settlements: parent bet % is not visible (no tenant context or wrong scope)', NEW.bet_id;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM bet.tenant_id OR NEW.asset_code IS DISTINCT FROM bet.asset_code THEN
        RAISE EXCEPTION 'sportsbook_bet_settlements: tenant/asset must equal the bet''s';
    END IF;

    SELECT COALESCE(max(generation), 0) INTO max_generation
      FROM sportsbook_bet_settlements
     WHERE bet_id = NEW.bet_id AND event_kind IN ('settlement', 'tombstone');
    SELECT EXISTS (SELECT 1 FROM sportsbook_bet_settlements WHERE bet_id = NEW.bet_id AND event_kind = 'void')
      INTO has_void;
    SELECT EXISTS (
        SELECT 1 FROM sportsbook_bet_settlements s
         WHERE s.bet_id = NEW.bet_id AND s.event_kind = 'settlement'
           AND NOT EXISTS (SELECT 1 FROM sportsbook_bet_settlements r
                            WHERE r.event_kind = 'rollback' AND r.reverses_settlement_id = s.id))
      INTO has_unreversed;

    IF NEW.event_kind IN ('settlement', 'tombstone') THEN
        IF has_void THEN
            RAISE EXCEPTION 'sportsbook_bet_settlements: bet % is void (terminal)', NEW.bet_id;
        END IF;
        IF has_unreversed THEN
            RAISE EXCEPTION 'sportsbook_bet_settlements: bet % already has an un-reversed settlement', NEW.bet_id;
        END IF;
        IF NEW.generation <> max_generation + 1 THEN
            RAISE EXCEPTION 'sportsbook_bet_settlements: generation % out of sequence (expected %)', NEW.generation, max_generation + 1;
        END IF;
    END IF;

    IF NEW.event_kind = 'settlement' THEN
        IF NEW.outcome = 'won' AND NOT (NEW.payout_amount = bet.potential_return AND NEW.payout_amount > 0) THEN
            RAISE EXCEPTION 'sportsbook_bet_settlements: a won payout must equal the bet''s positive potential_return';
        END IF;
        IF NEW.outcome = 'lost' AND NEW.payout_amount <> 0 THEN
            RAISE EXCEPTION 'sportsbook_bet_settlements: a lost settlement carries no payout';
        END IF;
    END IF;

    IF NEW.event_kind = 'rollback' THEN
        SELECT * INTO target FROM sportsbook_bet_settlements WHERE id = NEW.reverses_settlement_id;
        IF NOT FOUND OR target.bet_id <> NEW.bet_id OR target.event_kind <> 'settlement'
            OR target.generation <> NEW.generation THEN
            RAISE EXCEPTION 'sportsbook_bet_settlements: rollback target must be this bet''s settlement of the same generation';
        END IF;
        SELECT id INTO latest_id FROM sportsbook_bet_settlements
         WHERE bet_id = NEW.bet_id AND event_kind = 'settlement'
         ORDER BY generation DESC LIMIT 1;
        IF latest_id IS DISTINCT FROM target.id THEN
            RAISE EXCEPTION 'sportsbook_bet_settlements: only the latest settlement can be rolled back';
        END IF;
    END IF;

    IF NEW.event_kind = 'void' THEN
        IF has_void THEN
            RAISE EXCEPTION 'sportsbook_bet_settlements: bet % is already void', NEW.bet_id;
        END IF;
        IF has_unreversed THEN
            RAISE EXCEPTION 'sportsbook_bet_settlements: void requires no un-reversed settlement (roll back first)';
        END IF;
    END IF;

    SELECT transaction_type, correlation_id INTO ltx_type, ltx_corr
      FROM ledger_transactions WHERE id = NEW.ledger_transaction_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'sportsbook_bet_settlements: ledger transaction % is not visible', NEW.ledger_transaction_id;
    END IF;
    expected_type := CASE NEW.event_kind
        WHEN 'settlement' THEN 'sportsbook_settlement'
        WHEN 'rollback' THEN 'sportsbook_rollback'
        WHEN 'void' THEN 'sportsbook_void'
        WHEN 'tombstone' THEN 'tombstone' END;
    IF ltx_corr IS DISTINCT FROM NEW.bet_id OR ltx_type IS DISTINCT FROM expected_type THEN
        RAISE EXCEPTION 'sportsbook_bet_settlements: ledger transaction type/correlation does not match the event';
    END IF;

    -- Causation (ADR 0088 §2.1).
    IF NEW.event_kind = 'settlement' AND NEW.generation > 1 THEN
        SELECT * INTO cause FROM sportsbook_bet_settlements WHERE id = NEW.causation_record_id;
        IF NOT FOUND OR cause.bet_id <> NEW.bet_id OR cause.event_kind NOT IN ('rollback', 'tombstone')
            OR cause.generation <> NEW.generation - 1 THEN
            RAISE EXCEPTION 'sportsbook_bet_settlements: a re-settlement must cite this bet''s generation % rollback or tombstone', NEW.generation - 1;
        END IF;
    ELSIF NEW.event_kind = 'void' AND NEW.causation_record_id IS NOT NULL THEN
        -- Only the composed void of a void-after-settlement cites its
        -- rollback, and only one inserted by this same transaction.
        -- xmin is a 32-bit xid; pg_current_xact_id() is the epoch-qualified
        -- xid8 of the top-level transaction (history rows are never
        -- inserted inside a savepoint), so compare its low 32 bits.
        SELECT * INTO cause FROM sportsbook_bet_settlements WHERE id = NEW.causation_record_id;
        SELECT xmin::text INTO cause_xmin FROM sportsbook_bet_settlements WHERE id = NEW.causation_record_id;
        IF cause.id IS NULL OR cause.bet_id <> NEW.bet_id OR cause.event_kind <> 'rollback'
            OR cause_xmin <> (pg_current_xact_id()::text::bigint % 4294967296)::text THEN
            RAISE EXCEPTION 'sportsbook_bet_settlements: a void may cite only a rollback of this bet inserted by the same transaction';
        END IF;
    ELSIF NEW.causation_record_id IS NOT NULL THEN
        RAISE EXCEPTION 'sportsbook_bet_settlements: causation_record_id is not permitted for this event';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER sportsbook_bet_settlements_validate
    BEFORE INSERT ON sportsbook_bet_settlements
    FOR EACH ROW EXECUTE FUNCTION sportsbook_bet_settlements_validate();

-- 4. T-2 (ADR 0088 §3.3): sportsbook_bets.status is a cache of the
-- history. INSERT must be 'open'; an UPDATE must be an allowed transition
-- (§3.1) AND equal the status derived from history, so the history row is
-- always inserted before the status UPDATE.
CREATE FUNCTION sportsbook_bets_status_transition() RETURNS TRIGGER AS $$
DECLARE
    derived TEXT;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'open' THEN
            RAISE EXCEPTION 'sportsbook_bets: a bet is always inserted open';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.status = OLD.status THEN
        RETURN NEW;
    END IF;
    IF NULLIF(current_setting('app.player_account_id', true), '') IS NOT NULL THEN
        RAISE EXCEPTION 'sportsbook_bets: status is never changed under a player-scoped connection';
    END IF;
    IF (OLD.status, NEW.status) NOT IN (
        ('open', 'settled_won'), ('open', 'settled_lost'), ('open', 'void'),
        ('settled_won', 'open'), ('settled_lost', 'open')) THEN
        RAISE EXCEPTION 'sportsbook_bets: status transition % -> % is not permitted', OLD.status, NEW.status;
    END IF;

    IF EXISTS (SELECT 1 FROM sportsbook_bet_settlements WHERE bet_id = NEW.id AND event_kind = 'void') THEN
        derived := 'void';
    ELSE
        SELECT 'settled_' || s.outcome INTO derived
          FROM sportsbook_bet_settlements s
         WHERE s.bet_id = NEW.id AND s.event_kind = 'settlement'
           AND NOT EXISTS (SELECT 1 FROM sportsbook_bet_settlements r
                            WHERE r.event_kind = 'rollback' AND r.reverses_settlement_id = s.id)
         ORDER BY s.generation DESC LIMIT 1;
        IF derived IS NULL THEN
            derived := 'open';
        END IF;
    END IF;
    IF NEW.status <> derived THEN
        RAISE EXCEPTION 'sportsbook_bets: status % does not match the settlement history (derived %)', NEW.status, derived;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER sportsbook_bets_status_transition
    BEFORE INSERT OR UPDATE OF status ON sportsbook_bets
    FOR EACH ROW EXECUTE FUNCTION sportsbook_bets_status_transition();

-- 5. Reconciliation mismatch kinds for the sportsbook stream (ADR 0088 §8).
ALTER TABLE reconciliation_mismatches DROP CONSTRAINT reconciliation_mismatches_mismatch_kind_check;
ALTER TABLE reconciliation_mismatches ADD CONSTRAINT reconciliation_mismatches_mismatch_kind_check
    CHECK (mismatch_kind IN ('missing_projection', 'balance_mismatch',
        'sb_locked_mismatch', 'sb_bet_net_mismatch', 'sb_orphan_ledger',
        'sb_orphan_history', 'sb_status_mismatch', 'sb_mock_statement_mismatch'));

-- 6. Runtime role: defence in depth only (the deny triggers above bind
-- every role, owner included). Guarded because the role may be
-- provisioned after migrations run (docs/security/runtime-role-separation.md
-- §6); deploy/init-app-role.sql carries the matching guarded REVOKE so a
-- re-run of its backfill GRANT cannot silently restore these privileges.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE UPDATE, DELETE, TRUNCATE ON sportsbook_bet_settlements FROM igaming_runtime';
    END IF;
END $$;
