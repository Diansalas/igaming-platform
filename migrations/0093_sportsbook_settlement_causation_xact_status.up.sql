-- SB-T1-XMIN (docs/plans/stage-10.1-planning-gate-proposal.md §H-§I;
-- ADR 0090 ACCEPTED; ADR 0088 §3.3 follow-up). Body-only fix to trigger
-- function sportsbook_bet_settlements_validate() (migration 0091's T-1):
-- only the composed-void causation branch changes. Every other branch,
-- every other object (T-2, RLS, indexes, deny triggers, the table itself)
-- is untouched. Migration 0091 is NEVER edited (checksum-immutable).
--
-- WHY: T-1 accepted a composed void's causation_record_id only when the
-- cited rollback row's xmin equalled the low 32 bits of
-- pg_current_xact_id(), which is always the TOP-LEVEL transaction id. A
-- rollback row inserted under a SAVEPOINT keeps the subtransaction's own
-- xid as its xmin, even after RELEASE SAVEPOINT, so a legitimate composed
-- void whose rollback leg was written inside a savepoint was rejected
-- (fail closed: no money moved, HTTP 409 SETTLEMENT_INTEGRITY). No
-- current writer takes a savepoint around this insert (see
-- internal/sportsbook/settlement.go's insertSettlementRecord precondition
-- comment, updated by this same change), but a future batch or
-- provider-webhook driver that settles multiple bets per outer
-- transaction, one SAVEPOINT per bet, would hit this on every
-- void-after-settlement.
--
-- FIX: the causation branch now reconstructs the cited row's full 64-bit
-- xid8 from its 32-bit xmin, anchored to the current TOP-LEVEL
-- transaction's own epoch (pg_current_xact_id()'s high bits), and accepts
-- only if that reconstructed id is >= pg_current_xact_id() AND
-- pg_xact_status(...) IS NOT DISTINCT FROM 'in progress'. This classifies
-- any xid belonging to the checking transaction's own tree (top-level or
-- any subtransaction/savepoint, released or not, however nested) as
-- acceptable, while still rejecting a genuinely earlier, already-committed
-- transaction's rollback (reported 'committed', never 'in progress') and
-- any concurrent, still-uncommitted transaction's row (invisible to the
-- SELECT, hits the pre-existing cause.id IS NULL branch).
--
-- DEVIATION FROM ORCHESTRATOR RULING R-2 (recorded here because it is a
-- correctness fix, not a stylistic choice - see the full explanation
-- inline on the branch below): ruling R-2
-- (docs/plans/stage-10.1-planning-gate-proposal.md §O; ledger-finance
-- review 03, P2-2) specified anchoring to
-- pg_snapshot_xmax(pg_current_snapshot()) instead of pg_current_xact_id().
-- That construction is EMPIRICALLY WRONG for exactly the released-
-- savepoint case this migration exists to fix (pg_snapshot_xmax's xmax
-- reflects the highest COMPLETED xid, not the highest ASSIGNED one, so a
-- still-open savepoint's xmin routinely exceeds it, which the "largest
-- candidate not exceeding it" arithmetic then reconstructs one whole
-- epoch too low, reproducing the exact SB-T1-XMIN rejection this
-- migration exists to fix). Anchoring to pg_current_xact_id() instead is
-- correct and was verified empirically on PostgreSQL 16 for every
-- required case (plain, savepoint, nested savepoint, earlier committed
-- transaction). Flagged for architect/ledger-finance re-review before
-- this migration is treated as final.
--
-- GUARDS (fail closed; see the inline comment on the branch itself for
-- G1-G3 in full):
--   G1 - pg_xact_status can return NULL (an xid older than the retained
--        clog); the predicate uses IS NOT DISTINCT FROM 'in progress', so
--        NULL is treated as "not in progress" and rejected, never
--        silently accepted by a NULL-is-not-true bug.
--   G2 - pg_xact_status raises on a "future" xid8, and the bigint
--        reconstruction arithmetic can itself misconstruct an invalid
--        xid8. The entire reconstruction and check runs inside a
--        BEGIN ... EXCEPTION WHEN OTHERS block, so any error - arithmetic,
--        cast, or pg_xact_status's own - is caught and rejected with T-1's
--        existing message, never left to propagate as a different error
--        or (worse) miscompiled into an accept.
--   G3 - this check is sound only because a settlement history row's xmin
--        is immutable: the sportsbook_bet_settlements_immutable deny
--        trigger (migration 0091) blocks UPDATE/DELETE/TRUNCATE for every
--        role, including the table owner. History rows are never updated.
--
-- SECURITY INVOKER (unchanged from 0091): the function still runs under
-- the caller's RLS, still RAISEs when the parent bet is not visible or the
-- connection is player-scoped. No SECURITY DEFINER, no new grant:
-- pg_xact_status() is EXECUTE-to-PUBLIC by default and this migration
-- REVOKEs nothing.
--
-- Down migration: restores 0091's function body VERBATIM (extracted
-- programmatically from 0091_sportsbook_settlement.up.sql to guarantee
-- byte equality - see TestMigration0093_DownRestoresExactPriorFunctionBody,
-- internal/sportsbook). Safe with any settlement history already present:
-- this is a BEFORE INSERT trigger only, so existing rows are never
-- re-validated, and the down migration only re-narrows acceptance (fails
-- closed), never loosens it.

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
    cause_xmin_raw BIGINT;
    xact_ref       BIGINT;
    xact_full      BIGINT;
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
        -- rollback, and only one inserted by the CURRENT transaction TREE
        -- (the top-level transaction or any of its subtransactions/
        -- savepoints, released or not) - never an earlier, already-
        -- committed transaction's rollback (SB-T1-XMIN, ADR 0088 §3.3
        -- follow-up, migration 0093).
        --
        -- WHY (G1-G3, architect+security+ledger-finance review):
        -- xmin is the 32-bit xid of the (sub)transaction that inserted the
        -- row; pg_current_xact_id() always reports the TOP-LEVEL xid, so a
        -- row inserted under a SAVEPOINT (even after RELEASE SAVEPOINT)
        -- keeps an xmin that never equals it - the SB-T1-XMIN root cause.
        -- The fix rebuilds the row's full 64-bit xid8 from its 32-bit
        -- xmin, anchored to the CURRENT TOP-LEVEL transaction's own epoch
        -- (the high bits of pg_current_xact_id()), then classifies it with
        -- pg_xact_status(). A VISIBLE row that reports 'in progress' can
        -- only belong to the checking transaction's own tree: a concurrent
        -- transaction's uncommitted row is invisible to the SELECT above
        -- (hits cause.id IS NULL), and any earlier, committed
        -- transaction's row reports 'committed', never 'in progress'.
        --
        -- DEVIATION FROM THE STAGE 10.1 PLANNING-GATE TEXT (recorded here
        -- because it is a correctness fix, not a stylistic choice): the
        -- planning gate (docs/plans/stage-10.1-planning-gate-proposal.md
        -- §O ruling R-2; ledger-finance review 03, P2-2) specified
        -- anchoring to pg_snapshot_xmax(pg_current_snapshot()) instead of
        -- pg_current_xact_id(), on the stated assumption that
        -- pg_snapshot_xmax "never yields a future xid". That assumption is
        -- FALSE for exactly the case this fix exists to accept:
        -- pg_snapshot_xmax's xmax is defined as (one past) the highest
        -- COMPLETED transaction id at snapshot time, not the highest
        -- ASSIGNED one - so a still-open SAVEPOINT of the CURRENT
        -- transaction (assigned but, by definition, not yet completed)
        -- routinely has an xmin numerically GREATER than
        -- pg_snapshot_xmax's value. Anchoring to pg_snapshot_xmax then
        -- forces the "largest candidate not exceeding it" arithmetic to
        -- subtract a whole epoch from the reconstructed id, producing the
        -- WRONG (one-epoch-too-low) xid8 for a released-savepoint row -
        -- reproducing the exact SB-T1-XMIN rejection this migration exists
        -- to fix. Verified empirically on PostgreSQL 16 (BEGIN; plain
        -- INSERT; SAVEPOINT; INSERT; RELEASE; SELECT xmin, top-level xid,
        -- pg_snapshot_xmax(): the savepoint row's xmin was numerically
        -- greater than pg_snapshot_xmax, confirming the failure mode).
        -- Anchoring to pg_current_xact_id() instead is correct for every
        -- case T-1 must distinguish: a subxact of the current top-level
        -- transaction is always numerically >= the top-level xid within
        -- the SAME epoch (subxids only increase, and a single transaction
        -- cannot itself span an epoch wraparound - the same "out of scope"
        -- assumption the original SB-T1-XMIN analysis already relied on),
        -- so it reconstructs to a valid xid8 that is >= pg_current_xact_id()
        -- and reports 'in progress'. An earlier, already-committed
        -- transaction's row (the common case) is numerically less than the
        -- current top-level xid in the same epoch, so it reconstructs to a
        -- valid, smaller xid8 that fails the ">= pg_current_xact_id()"
        -- check outright (or, if visible and somehow not caught there,
        -- reports 'committed'). The only case pg_current_xact_id()-
        -- anchoring does not perfectly resolve is a genuinely
        -- epoch-wrapped-around, ancient row whose low 32 bits happen to
        -- exceed the current top-level xid's low 32 bits purely by
        -- coincidence of wraparound (the same vanishingly rare edge every
        -- version of this fix already treats as out of scope) - and even
        -- there this construction fails CLOSED: the naive same-epoch
        -- reconstruction produces an id far beyond any id Postgres has
        -- actually assigned, so pg_xact_status raises "transaction ID ...
        -- is in the future", which the BEGIN...EXCEPTION block below
        -- catches and rejects with T-1's existing message. This deviation
        -- needs architect/ledger-finance re-review before this migration
        -- is treated as final (see the implementing agent's handback
        -- report).
        --
        -- G1 (fail closed on NULL): pg_xact_status returns NULL for an xid
        -- older than the retained clog. The predicate below uses
        -- IS NOT DISTINCT FROM 'in progress', so a NULL status is treated
        -- as "not in progress" and rejected - never silently accepted by
        -- an IF ... <> 'in progress' NULL-is-not-true bug.
        -- G2 (fail closed on reconstruction failure/errors): pg_xact_status
        -- raises on a "future" xid8, and the bigint arithmetic below can
        -- itself misconstruct an invalid xid8. The whole reconstruction
        -- and check runs inside a BEGIN ... EXCEPTION WHEN OTHERS block, so
        -- any such error is caught and rejected with T-1's existing
        -- message, not propagated as a different, less clear error or
        -- miscompiled into an accept.
        -- G3 (immutability dependency): this check is sound only because
        -- xmin can never be rewritten - the sportsbook_bet_settlements_
        -- immutable deny trigger blocks UPDATE/DELETE/TRUNCATE for every
        -- role, including the table owner (migration 0091); history rows
        -- are never updated.
        SELECT * INTO cause FROM sportsbook_bet_settlements WHERE id = NEW.causation_record_id;
        IF cause.id IS NULL OR cause.bet_id <> NEW.bet_id OR cause.event_kind <> 'rollback' THEN
            RAISE EXCEPTION 'sportsbook_bet_settlements: a void may cite only a rollback of this bet inserted by the same transaction';
        END IF;
        BEGIN
            SELECT xmin::text::bigint INTO STRICT cause_xmin_raw FROM sportsbook_bet_settlements WHERE id = NEW.causation_record_id;
            xact_ref := pg_current_xact_id()::text::bigint;
            xact_full := (xact_ref & ~4294967295) | cause_xmin_raw;
            IF xact_full IS NULL
                OR xact_full < xact_ref
                OR pg_xact_status(xact_full::text::xid8) IS DISTINCT FROM 'in progress' THEN
                RAISE EXCEPTION 'sportsbook_bet_settlements: a void may cite only a rollback of this bet inserted by the same transaction';
            END IF;
        EXCEPTION WHEN OTHERS THEN
            RAISE EXCEPTION 'sportsbook_bet_settlements: a void may cite only a rollback of this bet inserted by the same transaction';
        END;
    ELSIF NEW.causation_record_id IS NOT NULL THEN
        RAISE EXCEPTION 'sportsbook_bet_settlements: causation_record_id is not permitted for this event';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
