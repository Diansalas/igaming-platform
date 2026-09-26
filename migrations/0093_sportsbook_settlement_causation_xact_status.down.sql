-- Down migration for 0093 (SB-T1-XMIN). Restores
-- sportsbook_bet_settlements_validate()'s function body to EXACTLY
-- migration 0091's original text (the xmin-equality composed-void
-- causation rule), verbatim byte-for-byte - extracted programmatically
-- from migrations/0091_sportsbook_settlement.up.sql, never retyped, to
-- guarantee equality (see
-- TestMigration0093_DownRestoresExactPriorFunctionBody,
-- internal/sportsbook). Migration 0091 itself is never edited or touched.
--
-- Safe at any time, with any settlement history already present: T-1 is a
-- BEFORE INSERT trigger, so existing rows are never re-validated, and
-- reverting only re-narrows acceptance (rejects the savepoint case again),
-- which fails closed - it never loosens or changes any accept/reject
-- outcome that was already relied upon before 0093 was applied.

CREATE OR REPLACE FUNCTION sportsbook_bet_settlements_validate() RETURNS TRIGGER AS $$
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
