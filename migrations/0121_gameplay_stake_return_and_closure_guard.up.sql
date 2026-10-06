-- PRH-2 R5 (owner decisions 2026-10-06, task registry row
-- DECISIONS-PRH2-FINAL-GAMEPLAY-SECURITY-2026-10-06; ADR 0095 section 40.6).
-- Builds ON TOP of migration 0118 (never edits it): CREATE OR REPLACE of two
-- existing functions, one new deferred constraint trigger.
--
-- NUMBERING: 0121. 0120 is reserved for the signed-actor-proof workstream;
-- until it merges, a migrate-verify of 0001..0121 has a gap at 0120.
--
-- ITEM A (Q-GP-5): TERMINAL STAKE RETURNS REMAIN ALLOWED on a suspended or
-- closed tenant. A terminal stake return is not a new wager: it returns the
-- stake of a round the tenant already accepted. Exactly these postings now
-- pass the 0118 gameplay backstop (ledger_gameplay_tenant_active_guard) on a
-- non-active tenant, all inside the same shared advisory lock as before:
--   1. casino_rollback whose reverses_transaction_id is a casino_bet of the
--      SAME tenant, and whose round (the ORIGINAL bet's own correlation_id)
--      has NO unreversed casino_win: a bet rollback with the win still
--      standing would leave stake AND win both paid, because the rollback of
--      the win stays refused on a non-active tenant (ledger-finance C1). A
--      rollback of a casino_win stays refused;
--   2. sportsbook_void for a bet that was really placed (a sportsbook_bet
--      ledger transaction of the same tenant under the same correlation id =
--      the bet id) and has NO outstanding settlement: every sportsbook_settlement
--      of that bet already has a reversing transaction (so a void after
--      settlement passes only because its rollback leg is posted first, in the
--      same transaction);
--   3. sportsbook_rollback that reverses a sportsbook_settlement of the same
--      bet, but ONLY as the rollback leg of a void after settlement. A plain
--      rollback that reopens the bet is NOT a stake return and stays refused.
--      The BEFORE INSERT guard cannot see the void that follows, so a
--      DEFERRED constraint trigger (ledger_sportsbook_rollback_requires_void)
--      checks at COMMIT that, on a non-active tenant, a sportsbook_void whose
--      causation_id is that rollback was posted in the same transaction, else
--      GP010 and the whole transaction aborts. (The application chains the
--      void's causation_id to the rollback: settlement.go voidBet/lockAndPost.)
-- Everything else for a non-active tenant is still GP010: casino_bet,
-- casino_win, sportsbook_bet, sportsbook_settlement (payout), any rollback
-- that is not listed. The replay pass-through, the shared lock and the
-- fail-closed behaviour for an unreadable tenant row are unchanged. The
-- trigger WHEN list is unchanged, so the trigger itself is not replaced.
-- Amount bounding (a reversal never exceeds its original) is enforced by the
-- application (exact inversion under FOR UPDATE; the void is checked against
-- the bet's placed stake); entries do not exist yet when a BEFORE INSERT
-- trigger runs, so the database cannot compare them here.
--
-- ITEM B (Q-GP-1): REFUSE tenant CLOSURE while unresolved gaming rounds
-- exist. tenants_status_change_gate (0118; BEFORE UPDATE OF status) already
-- takes the per-tenant advisory lock EXCLUSIVELY. It now also, AFTER taking
-- that lock and only for a transition INTO 'closed', counts the tenant's open
-- sportsbook bets (sportsbook_bets.status = 'open': stake still locked in
-- player_locked_cash) and refuses with SQLSTATE GP020 when any exist, with
-- the counts (only) in DETAIL. Because it runs inside the same trigger under
-- the same lock pair the gameplay gate uses, there is no check-then-act
-- window: a bet placement holds the lock SHARED to its commit, so either the
-- closure waits for it and then counts the new bet (refused), or the closure
-- holds the lock first and the placement then reads the new status. ANY writer
-- of tenants.status (HTTP handler, SQL) is covered.
-- Suspension and reactivation are NOT affected (only a transition into
-- 'closed' is checked).
-- The count must read sportsbook_bets under RLS (tenant_staff_scope needs
-- app.tenant_id = the tenant and no player scope), while the status change
-- itself runs platform-scoped (tenants_platform_admin_update needs app.tenant_id
-- UNSET). The function therefore sets the two GUCs transaction-locally for the
-- count ONLY, for the tenant being closed (OLD.id, never a caller value), and
-- restores both before returning. Same technique as the 0115 pre-flight.
-- It is not SECURITY DEFINER (FORCE RLS would bind the owner anyway).
-- CASINO: a casino round has no representable "open" state in the data model
-- (a cash bet moves the stake to house_gaming at bet time, there is no loss or
-- round-close callback, a bet without a win is also a lost round), so no
-- casino predicate exists and none is invented here. Recorded as an owner
-- question in ADR 0095 section 40.6 (Q-GP-6).
--
-- ONCE-ONLY (ledger-finance C3): a partial unique index makes "a casino bet or
-- win is reversed at most once" database-enforced, not application-only
-- (CLAUDE.md), following migration 0092's pattern for deposit reversals. The
-- application primary control stays the FOR UPDATE + existing-reversal check in
-- casino.postRollback; ledger.Post maps the constraint to
-- ErrCasinoReversalAlreadyExists, which postRollback reports as
-- ErrAlreadyRolledBack. Compatible with the held-win rollback path
-- (bonus_settlement.go, one rollback per held win, guarded by the same check).
-- If this refuses: STOP, never delete ledger rows, escalate (see 0092).
-- PRODUCTION ROLLOUT: the index build is NOT CONCURRENTLY (the runner applies
-- each migration in one transaction), so it holds a SHARE lock on
-- ledger_transactions while it builds, exactly as for 0092; a launch-stage
-- rollout plan at scale is out of scope here.
--
-- Every function pins search_path (ADR 0108 / security H-1).

-- ---------------------------------------------------------------------------
-- A. The gameplay backstop, narrowed for terminal stake returns.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION ledger_gameplay_tenant_active_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
    v_ok     boolean := false;
BEGIN
    -- A replay of an already-posted transaction writes nothing (the INSERT
    -- conflicts on the unique key after this BEFORE trigger): let it through.
    IF EXISTS (SELECT 1 FROM public.ledger_transactions x
                WHERE x.tenant_id = NEW.tenant_id AND x.idempotency_key = NEW.idempotency_key) THEN
        RETURN NEW;
    END IF;
    PERFORM pg_advisory_xact_lock_shared(public.tenant_status_gate_key(NEW.tenant_id));
    SELECT t.status INTO v_status FROM public.tenants t WHERE t.id = NEW.tenant_id;
    IF v_status = 'active' THEN
        RETURN NEW;
    END IF;
    -- Not active (or the row is not visible: fail closed). Only a terminal
    -- stake return may pass, and only for a readable non-active status.
    IF v_status IS NOT NULL THEN
        IF NEW.transaction_type = 'casino_rollback' THEN
            v_ok := NEW.reverses_transaction_id IS NOT NULL
                AND EXISTS (SELECT 1 FROM public.ledger_transactions o
                             WHERE o.id = NEW.reverses_transaction_id
                               AND o.tenant_id = NEW.tenant_id
                               AND o.transaction_type = 'casino_bet'
                               AND NOT EXISTS (SELECT 1 FROM public.ledger_transactions w
                                                WHERE w.tenant_id = NEW.tenant_id
                                                  AND w.correlation_id = o.correlation_id
                                                  AND w.transaction_type = 'casino_win'
                                                  AND NOT EXISTS (SELECT 1 FROM public.ledger_transactions r
                                                                   WHERE r.reverses_transaction_id = w.id)));
        ELSIF NEW.transaction_type = 'sportsbook_void' THEN
            v_ok := EXISTS (SELECT 1 FROM public.ledger_transactions b
                             WHERE b.tenant_id = NEW.tenant_id
                               AND b.correlation_id = NEW.correlation_id
                               AND b.transaction_type = 'sportsbook_bet')
                AND NOT EXISTS (SELECT 1 FROM public.ledger_transactions s
                                 WHERE s.tenant_id = NEW.tenant_id
                                   AND s.correlation_id = NEW.correlation_id
                                   AND s.transaction_type = 'sportsbook_settlement'
                                   AND NOT EXISTS (SELECT 1 FROM public.ledger_transactions r
                                                    WHERE r.reverses_transaction_id = s.id));
        ELSIF NEW.transaction_type = 'sportsbook_rollback' THEN
            -- Necessary condition only; the deferred constraint trigger below
            -- requires the paired void at commit.
            v_ok := NEW.reverses_transaction_id IS NOT NULL
                AND EXISTS (SELECT 1 FROM public.ledger_transactions s
                             WHERE s.id = NEW.reverses_transaction_id
                               AND s.tenant_id = NEW.tenant_id
                               AND s.correlation_id = NEW.correlation_id
                               AND s.transaction_type = 'sportsbook_settlement');
        END IF;
    END IF;
    IF NOT v_ok THEN
        RAISE EXCEPTION 'gameplay posting refused: tenant is not active'
            USING ERRCODE = 'GP010';
    END IF;
    RETURN NEW;
END
$$;

-- The commit-time half of the sportsbook rollback exemption: on a non-active
-- tenant a sportsbook_rollback is only valid as the first leg of a void after
-- settlement, i.e. a sportsbook_void for the same bet whose causation_id is
-- this rollback was posted in the same transaction. Fires only for inserted
-- rows (a replay inserts nothing), re-takes the (re-entrant) shared lock, and
-- is a no-op for an active tenant.
CREATE FUNCTION ledger_sportsbook_rollback_requires_void() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
BEGIN
    PERFORM pg_advisory_xact_lock_shared(public.tenant_status_gate_key(NEW.tenant_id));
    SELECT t.status INTO v_status FROM public.tenants t WHERE t.id = NEW.tenant_id;
    IF v_status = 'active' THEN
        RETURN NULL;
    END IF;
    IF EXISTS (SELECT 1 FROM public.ledger_transactions v
                WHERE v.tenant_id = NEW.tenant_id
                  AND v.transaction_type = 'sportsbook_void'
                  AND v.causation_id = NEW.id
                  AND v.correlation_id = NEW.correlation_id) THEN
        RETURN NULL;
    END IF;
    RAISE EXCEPTION 'gameplay posting refused: tenant is not active and the rollback is not part of a void'
        USING ERRCODE = 'GP010';
END
$$;

CREATE CONSTRAINT TRIGGER ledger_sportsbook_rollback_requires_void
    AFTER INSERT ON ledger_transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    WHEN (NEW.transaction_type = 'sportsbook_rollback')
    EXECUTE FUNCTION ledger_sportsbook_rollback_requires_void();

-- ---------------------------------------------------------------------------
-- B. Refuse closing a tenant while open gaming rounds exist.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION tenants_status_change_gate() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_open_bets    bigint;
    v_prev_tenant  text;
    v_prev_player  text;
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        PERFORM pg_advisory_xact_lock(public.tenant_status_gate_key(OLD.id));
        IF NEW.status = 'closed' THEN
            v_prev_tenant := current_setting('app.tenant_id', true);
            v_prev_player := current_setting('app.player_account_id', true);
            PERFORM set_config('app.tenant_id', OLD.id::text, true);
            PERFORM set_config('app.player_account_id', '', true);
            -- NOTE: this count depends on RLS visibility (app.tenant_id was set
            -- above); without it every bet is hidden and the guard would fail
            -- open. TestTenantClosure_RefusedWithOpenBetAllowedWhenResolved
            -- is the guard for that.
            SELECT count(*) INTO v_open_bets
              FROM public.sportsbook_bets b
             WHERE b.tenant_id = OLD.id AND b.status = 'open';
            PERFORM set_config('app.tenant_id', COALESCE(v_prev_tenant, ''), true);
            PERFORM set_config('app.player_account_id', COALESCE(v_prev_player, ''), true);
            IF v_open_bets > 0 THEN
                RAISE EXCEPTION 'tenant closure refused: open gaming rounds exist'
                    USING ERRCODE = 'GP020',
                          DETAIL = format('sportsbook_open_bets=%s', v_open_bets);
            END IF;
        END IF;
    END IF;
    RETURN NEW;
END
$$;

-- ---------------------------------------------------------------------------
-- C. At most one casino_rollback per original (ledger-finance C3).
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    v_detail text;
BEGIN
    CREATE UNIQUE INDEX ledger_transactions_one_casino_rollback
        ON ledger_transactions (tenant_id, reverses_transaction_id)
        WHERE transaction_type = 'casino_rollback';
EXCEPTION WHEN unique_violation THEN
    GET STACKED DIAGNOSTICS v_detail = PG_EXCEPTION_DETAIL;
    RAISE EXCEPTION 'migration 0121: duplicate casino_rollback rows exist for at least one (tenant_id, reverses_transaction_id) pair; this migration cannot run until they are escalated and resolved; never delete ledger rows (%: %)', SQLERRM, v_detail;
END $$;
