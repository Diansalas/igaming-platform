-- ADR 0112 SLICE 2 (decision LF1; security S5 first alternative): brand status
-- gates NEW wagering, race-free, mirroring the tenant R3 gate of migrations
-- 0118/0121. Amends ADR 0095 section 40.5 (R3) and the 0118 backstop design;
-- builds ON TOP of 0118, 0121 and 0128 (never edits them).
--
-- NUMBERING: 0129 (next free after 0128; confirmed against migrations/ and every
-- local branch at branch time). Migration numbers are allocated by the orchestrator.
--
-- What this does (and ONLY this):
--   1. brand_status_gate_key(uuid): the one per-brand lock-key derivation (the Go
--      helper internal/tenant.RequireBrandActiveForGameplay calls it too). A
--      different namespace prefix from tenant_status_gate_key, so a tenant key and
--      a brand key never collide by construction of the input text.
--   2. launch_subject_status_guard (0128's zz_launch_status_governed function,
--      CREATE OR REPLACE): on brands, a real status change now FIRST takes the
--      per-brand gate lock EXCLUSIVE and holds it to the end of the changing
--      transaction. Every other line is 0128's, unchanged (the tenants branch takes
--      no new lock: tenants_status_change_gate of 0118/0121 already serialises it).
--      A gameplay placement takes the lock SHARED, then reads brands.status in a
--      fresh statement, so either the brand change committed first and the
--      placement reads it, or the placement holds the lock and the change waits
--      until the placement's transaction ends. Every writer of brands.status is
--      covered (governed executor, SQL, owner), because the trigger has no column
--      list and acts on any value change.
--   3. ledger_wager_brand_active_guard: a DEFERRED constraint trigger on
--      ledger_transactions for the two NEW-STAKE types only (casino_bet,
--      sportsbook_bet). Defence in depth behind the Go gate for any future entry
--      point. At COMMIT it resolves the brand of every player wallet the posting
--      touches (ledger_entries.wallet_id -> wallets.brand_id, the FK-pinned brand of
--      the player), takes each brand's gate lock SHARED (ascending brand id) and
--      refuses with SQLSTATE GP011 unless the brand is 'active' (a brand row that is
--      not visible fails closed). A row with no player-wallet entry moves no player
--      money, so it has no brand to gate and passes (ledger.Post never produces an
--      entry-less non-tombstone posting; raw entry-less rows exist in test fixtures).
--      The resolution needs the entries to be visible to the posting session (tenant
--      scope, as every gameplay path runs; ADR 0095 40.5 residual). Deferred because the brand is only derivable from the entries, which do not
--      exist yet when a BEFORE INSERT trigger on ledger_transactions runs (same
--      reason as 0121's ledger_sportsbook_rollback_requires_void). A replay inserts
--      no ledger_transactions row (ledger.Post's savepoint discards it), so the
--      trigger never fires for a replay.
--
-- NOT covered by the brand gate (owner decision Q-GP-5 analogue, ADR 0112 2 item
-- 3): casino_win, casino_rollback, sportsbook_settlement, sportsbook_void,
-- sportsbook_rollback and tombstones. Terminal stake returns and the settlement of
-- rounds a brand already accepted stay exactly as the TENANT gate (0118/0121)
-- leaves them; a brand closure never strands a stake. Deposits, withdrawals,
-- adjustments and bonus types are untouched. No ledger row, balance or projection
-- is touched; nothing is backfilled.
--
-- LOCK ORDER (ADR 0082; recorded in ADR 0112 section 13 amendment note): the
-- status-gate advisory keys are taken tenant key first, then brand key, before the
-- L0.2.. classes. A brand status change holds the brands row (tuple lock of its
-- UPDATE) and then the brand key; no gameplay path locks a brands row, so the pair
-- cannot invert. A single transaction must not change a brand's status and then
-- its tenant's status (brand X -> tenant X inverts tenant -> brand); ADR 0112 3.1
-- keeps tenant and brand as separate decisions.
--
-- SQLSTATE: GP011 (new, class GP of 0118): "gameplay posting refused: brand is not
-- active". Callers branch on the code only.
--
-- Every function pins search_path (ADR 0108 / security H-1); none is SECURITY DEFINER.

CREATE FUNCTION brand_status_gate_key(p_brand_id uuid) RETURNS bigint
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT hashtextextended('brand_status_gate:' || p_brand_id::text, 0)
$$;

-- 0128's guard with exactly one addition (marked LF1). Kept textually identical
-- otherwise so the 0128 contract (S10, S2, closed terminal, S6 executor shape) is
-- unchanged.
CREATE OR REPLACE FUNCTION launch_subject_status_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_kind        text;
    v_tenant      uuid;
    v_brand       uuid;
    v_prev_tenant text;
    v_n           bigint;
BEGIN
    IF NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
    END IF;
    -- LF1 (ADR 0112 slice 2): a brand status change takes the per-brand gameplay
    -- gate lock EXCLUSIVE first, so it waits for in-flight new-stake placements of
    -- this brand (which hold it SHARED) and every later placement waits for it.
    IF TG_TABLE_NAME = 'brands' THEN
        PERFORM pg_advisory_xact_lock(public.brand_status_gate_key(OLD.id));
    END IF;
    IF OLD.status = 'closed' THEN
        RAISE EXCEPTION '% status: closed is terminal', TG_TABLE_NAME USING ERRCODE = 'LA020';
    END IF;
    IF NOT public.launch_status_move_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION '% status: % -> % is not a legal move', TG_TABLE_NAME, OLD.status, NEW.status USING ERRCODE = 'LA020';
    END IF;
    IF TG_TABLE_NAME = 'tenants' THEN
        v_kind := 'tenant'; v_tenant := OLD.id; v_brand := NULL;
    ELSE
        v_kind := 'brand'; v_tenant := OLD.tenant_id; v_brand := OLD.id;
    END IF;

    -- S6 executor shape: a platform principal with app.tenant_id bound to the subject's
    -- own tenant (the brand_tenant_update technique). Reads of the decision records use
    -- the platform policies, so clear the tenant GUC for the reads and restore it.
    v_prev_tenant := NULLIF(current_setting('app.tenant_id', true), '');
    IF v_prev_tenant IS NOT NULL
       AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL THEN
        PERFORM set_config('app.tenant_id', '', true);
    ELSE
        v_prev_tenant := NULL;
    END IF;
    SELECT count(*) INTO v_n
      FROM public.launch_status_transitions t
      JOIN public.launch_authorisation_requests r ON r.id = t.request_id AND r.tenant_id = t.tenant_id
     WHERE t.tenant_id = v_tenant AND t.subject_kind = v_kind AND t.brand_id IS NOT DISTINCT FROM v_brand
       AND t.kind = 'governed' AND t.txid = txid_current()
       AND t.from_status = OLD.status AND t.to_status = NEW.status
       AND r.status = 'executing' AND r.executing_txid = txid_current()
       AND r.subject_kind = v_kind AND r.brand_id IS NOT DISTINCT FROM v_brand
       AND r.from_status = OLD.status AND r.to_status = NEW.status AND r.action <> 'ratify';
    IF v_prev_tenant IS NOT NULL THEN
        PERFORM set_config('app.tenant_id', v_prev_tenant, true);
    END IF;
    IF v_n <> 1 THEN
        RAISE EXCEPTION '% status change refused: no same-transaction governed transition of an executing request (ADR 0112)', TG_TABLE_NAME
            USING ERRCODE = 'LA020';
    END IF;
    RETURN NEW;
END
$$;

-- The new-stake brand backstop (deferred: the entries carry the brand).
CREATE FUNCTION ledger_wager_brand_active_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_brand  uuid;
    v_status text;
BEGIN
    -- The brands of the player wallets this posting moves money on. A wallet's
    -- brand_id is FK-pinned to its player account (0019), so it is the player's
    -- brand, never a caller value. A row with no player-wallet entry takes no
    -- player stake and has no brand to gate (ledger.Post refuses an entry-less
    -- non-tombstone posting; raw entry-less rows exist only in test fixtures).
    FOR v_brand IN
        SELECT DISTINCT w.brand_id
          FROM public.ledger_entries e
          JOIN public.wallets w ON w.id = e.wallet_id AND w.tenant_id = e.tenant_id
         WHERE e.ledger_transaction_id = NEW.id AND e.tenant_id = NEW.tenant_id
           AND e.wallet_id IS NOT NULL
         ORDER BY 1
    LOOP
        PERFORM pg_advisory_xact_lock_shared(public.brand_status_gate_key(v_brand));
        SELECT b.status INTO v_status FROM public.brands b WHERE b.id = v_brand AND b.tenant_id = NEW.tenant_id;
        IF v_status IS DISTINCT FROM 'active' THEN
            -- Also fails closed when the brand row is not visible.
            RAISE EXCEPTION 'gameplay posting refused: brand is not active'
                USING ERRCODE = 'GP011';
        END IF;
    END LOOP;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER ledger_wager_brand_active_guard
    AFTER INSERT ON ledger_transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    WHEN (NEW.transaction_type IN ('casino_bet', 'sportsbook_bet'))
    EXECUTE FUNCTION ledger_wager_brand_active_guard();
