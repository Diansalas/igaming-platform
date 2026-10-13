-- ADR 0118 (BRAND-REHOME-HOLD-1): the player brand binding is immutable.
-- IDENTITY / MONEY-ADJACENT INTEGRITY CONTROL: FOR `security`, `ledger-finance`, identity owner, `code-reviewer`,
-- `qa` REVIEW BEFORE MERGE (ADR 0118 s9).
--
-- NUMBERING: 0136 (gap-free after 0135_rg_limits_schema_and_index; no ref carried a 0136 file at authoring time).
-- Migration numbers are allocated by the orchestrator; confirm before merge.
--
-- The problem (ADR 0118 s1, security-confirmed): igaming_runtime could re-home a player into another brand of the
-- same tenant with ONE statement (a CTE updating player_accounts.brand_id and wallets.brand_id together; the composite
-- FK wallets (player_account_id, tenant_id, brand_id) -> player_accounts is NO ACTION, checked at end of statement,
-- when both sides already agree; RLS checks tenant_id only). It also could re-point wallets.player_account_id /
-- wallets.asset_code and kyc_verifications (player_account_id, brand_id) (ADR 0118 s1.1).
--
-- SQLSTATE class "BR" (brand binding, new; no BR code existed before this file):
--   BR001 player_accounts identity column change refused (id, tenant_id, brand_id, created_at)
--   BR002 wallets identity column change refused (id, tenant_id, brand_id, player_account_id, asset_code, created_at)
--   BR003 kyc_verifications identity column change refused (tenant_id, brand_id, player_account_id)
--   BR098 up pre-flight refused: evidence of an earlier re-home / re-point (counts per class only)
-- No BR code is mapped to an HTTP code: no legitimate path can raise one, so a BR raise is a security signal.
--
-- What it adds, and only this (ADR 0118 s4):
--   0. SET LOCAL lock_timeout = '5s' (0133 precedent), for the whole file.
--   1. Pre-flight (ADR 0118 s5): refuses BR098 on evidence of an earlier re-point (classes 1-3); NEVER repairs a
--      row. Class 4 (non-zero player_withdrawal_hold / player_cash balance, rebuilt from ledger_entries, on a wallet
--      of a closed brand) is a NOTICE with counts only and does not refuse. FORCE ROW LEVEL SECURITY is lifted for
--      the owner's counts and restored inside this migration transaction (the 0124 / 0132 class 9b pattern); a
--      non-owner migration role cannot lift it and fails closed.
--   2. Three plain plpgsql functions (NOT SECURITY DEFINER, search_path pinned, schema-qualified): the message names
--      the table and the changed column set, never a row value.
--   3. Three BEFORE UPDATE ... FOR EACH ROW triggers with a WHEN clause listing exactly the pinned columns, so an
--      ordinary update (status, password_hash, verified_at, declared residence, KYC status / reason / provider
--      reference / verified residence) never calls the function. No other BEFORE UPDATE trigger exists on these three
--      tables at authoring time; a later one must keep this guard first by name ordering (record it there).
--   4. COMMENT ON TRIGGER citing ADR 0118.
--
-- NOT pinned here, deliberately (ADR 0118 s2): player_accounts.person_id (PERSON-RELINK-PIN-1), player_accounts.email,
-- kyc_verifications.person_id.
--
-- No existing function, policy, constraint or grant is changed (0128-0135 bodies are not replaced). No role is exempt
-- while the triggers are enabled (owner included; the owner's DISABLE TRIGGER is outside the threat model, ADR 0110
-- s9.4 / ADR 0118 s7 item 1). The down drops exactly what this file creates (whole-schema snapshot test:
-- internal/tenant/migration_0136_integration_test.go).
--
-- Literal pin (docs/security/launch-status-readers.md): the class 4 NOTICE reads brands.status = 'closed'
-- (informational only; allow-listed there and in internal/tenant/status_readers_static_test.go).

SET LOCAL lock_timeout = '5s';

-- =========================================================================
-- 1. Pre-flight (ADR 0118 s5).
-- =========================================================================
DO $$
DECLARE
    c_wallet_repointed  BIGINT := 0;  -- class 1
    c_wallet_asset      BIGINT := 0;  -- class 2
    c_adjustment_stale  BIGINT := 0;  -- class 3
    c_closed_hold       BIGINT := 0;  -- class 4 (NOTICE only)
    c_closed_cash       BIGINT := 0;  -- class 4 (NOTICE only)
BEGIN
    -- Writers of the three guarded tables wait from here until this migration commits, so no re-point can slip in
    -- between the scan and the creation of the triggers below (the same lock CREATE TRIGGER takes; reads admitted).
    LOCK TABLE public.player_accounts, public.wallets, public.kyc_verifications IN SHARE ROW EXCLUSIVE MODE;

    -- RLS-independent owner counts: FORCE is lifted for the owner and restored inside this transaction.
    ALTER TABLE public.wallets NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE public.ledger_accounts NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE public.ledger_adjustment_requests NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE public.ledger_entries NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE public.brands NO FORCE ROW LEVEL SECURITY;

    -- (1) a wallet re-pointed to another player: its ledger accounts keep the (immutable since 0082) player copy.
    SELECT count(*) INTO c_wallet_repointed
      FROM public.ledger_accounts la
      JOIN public.wallets w ON w.id = la.wallet_id
     WHERE la.wallet_id IS NOT NULL
       AND la.player_account_id IS DISTINCT FROM w.player_account_id;

    -- (2) a wallet whose asset changed: its ledger accounts keep the (immutable) asset.
    SELECT count(*) INTO c_wallet_asset
      FROM public.ledger_accounts la
      JOIN public.wallets w ON w.id = la.wallet_id
     WHERE la.wallet_id IS NOT NULL
       AND la.asset_code IS DISTINCT FROM w.asset_code;

    -- (3) a ledger adjustment request whose insert-time brand / player snapshot no longer matches its wallet.
    SELECT count(*) INTO c_adjustment_stale
      FROM public.ledger_adjustment_requests r
      JOIN public.wallets w ON w.id = r.wallet_id
     WHERE r.brand_id IS DISTINCT FROM w.brand_id
        OR r.player_account_id IS DISTINCT FROM w.player_account_id;

    -- (4) informational: non-zero balances rebuilt from ledger_entries on wallets of a closed brand.
    SELECT count(*) FILTER (WHERE s.account_type = 'player_withdrawal_hold'),
           count(*) FILTER (WHERE s.account_type = 'player_cash')
      INTO c_closed_hold, c_closed_cash
      FROM (
        SELECT la.account_type
          FROM public.ledger_accounts la
          JOIN public.wallets w ON w.id = la.wallet_id
          JOIN public.brands b ON b.id = w.brand_id
         WHERE la.account_type IN ('player_withdrawal_hold', 'player_cash')
           AND b.status = 'closed'
           AND (SELECT COALESCE(sum(CASE e.direction WHEN 'credit' THEN e.amount ELSE -e.amount END), 0)
                  FROM public.ledger_entries e WHERE e.ledger_account_id = la.id) <> 0
      ) s;

    ALTER TABLE public.wallets FORCE ROW LEVEL SECURITY;
    ALTER TABLE public.ledger_accounts FORCE ROW LEVEL SECURITY;
    ALTER TABLE public.ledger_adjustment_requests FORCE ROW LEVEL SECURITY;
    ALTER TABLE public.ledger_entries FORCE ROW LEVEL SECURITY;
    ALTER TABLE public.brands FORCE ROW LEVEL SECURITY;

    IF c_wallet_repointed + c_wallet_asset + c_adjustment_stale > 0 THEN
        RAISE EXCEPTION 'migration 0136 pre-flight: evidence of an earlier re-home / re-point: wallet_repointed=% wallet_asset_changed=% adjustment_snapshot_stale=%. Nothing was changed. Rows are never edited to make this pass: STOP, open a P1 and escalate to ledger-finance and security (ADR 0118 s5, s8); on a local / dev database rebuild from migrations.',
            c_wallet_repointed, c_wallet_asset, c_adjustment_stale
            USING ERRCODE = 'BR098';
    END IF;

    IF c_closed_hold + c_closed_cash > 0 THEN
        RAISE NOTICE 'migration 0136 pre-flight (informational, ADR 0118 s5 class 4): non-zero balances on wallets of a closed brand: player_withdrawal_hold=% player_cash=%. Investigate with ledger-finance as a potential P1 before any closure is treated as clean (ADR 0118 s8).',
            c_closed_hold, c_closed_cash;
    END IF;
END
$$;

-- =========================================================================
-- 2. The guard functions (ADR 0118 D2).
-- =========================================================================

CREATE FUNCTION player_accounts_enforce_identity_immutable() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_changed text[] := ARRAY[]::text[];
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id THEN v_changed := v_changed || 'id'::text; END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id THEN v_changed := v_changed || 'tenant_id'::text; END IF;
    IF NEW.brand_id IS DISTINCT FROM OLD.brand_id THEN v_changed := v_changed || 'brand_id'::text; END IF;
    IF NEW.created_at IS DISTINCT FROM OLD.created_at THEN v_changed := v_changed || 'created_at'::text; END IF;
    IF pg_catalog.cardinality(v_changed) > 0 THEN
        RAISE EXCEPTION 'player_accounts: the brand binding is immutable (ADR 0118); refused change of column(s) %',
            pg_catalog.array_to_string(v_changed, ', ')
            USING ERRCODE = 'BR001';
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION wallets_enforce_identity_immutable() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_changed text[] := ARRAY[]::text[];
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id THEN v_changed := v_changed || 'id'::text; END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id THEN v_changed := v_changed || 'tenant_id'::text; END IF;
    IF NEW.brand_id IS DISTINCT FROM OLD.brand_id THEN v_changed := v_changed || 'brand_id'::text; END IF;
    IF NEW.player_account_id IS DISTINCT FROM OLD.player_account_id THEN v_changed := v_changed || 'player_account_id'::text; END IF;
    IF NEW.asset_code IS DISTINCT FROM OLD.asset_code THEN v_changed := v_changed || 'asset_code'::text; END IF;
    IF NEW.created_at IS DISTINCT FROM OLD.created_at THEN v_changed := v_changed || 'created_at'::text; END IF;
    IF pg_catalog.cardinality(v_changed) > 0 THEN
        RAISE EXCEPTION 'wallets: the wallet identity and brand binding are immutable (ADR 0118); refused change of column(s) %',
            pg_catalog.array_to_string(v_changed, ', ')
            USING ERRCODE = 'BR002';
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION kyc_verifications_enforce_identity_immutable() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_changed text[] := ARRAY[]::text[];
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id THEN v_changed := v_changed || 'tenant_id'::text; END IF;
    IF NEW.brand_id IS DISTINCT FROM OLD.brand_id THEN v_changed := v_changed || 'brand_id'::text; END IF;
    IF NEW.player_account_id IS DISTINCT FROM OLD.player_account_id THEN v_changed := v_changed || 'player_account_id'::text; END IF;
    IF pg_catalog.cardinality(v_changed) > 0 THEN
        RAISE EXCEPTION 'kyc_verifications: the player and brand binding are immutable (ADR 0118); refused change of column(s) %',
            pg_catalog.array_to_string(v_changed, ', ')
            USING ERRCODE = 'BR003';
    END IF;
    RETURN NEW;
END
$$;

-- =========================================================================
-- 3. The triggers.
-- =========================================================================

CREATE TRIGGER player_accounts_identity_immutable
    BEFORE UPDATE ON player_accounts
    FOR EACH ROW
    WHEN (NEW.id IS DISTINCT FROM OLD.id
       OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at)
    EXECUTE FUNCTION player_accounts_enforce_identity_immutable();

CREATE TRIGGER wallets_identity_immutable
    BEFORE UPDATE ON wallets
    FOR EACH ROW
    WHEN (NEW.id IS DISTINCT FROM OLD.id
       OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
       OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
       OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
       OR NEW.created_at IS DISTINCT FROM OLD.created_at)
    EXECUTE FUNCTION wallets_enforce_identity_immutable();

CREATE TRIGGER kyc_verifications_identity_immutable
    BEFORE UPDATE ON kyc_verifications
    FOR EACH ROW
    WHEN (NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
       OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id)
    EXECUTE FUNCTION kyc_verifications_enforce_identity_immutable();

-- =========================================================================
-- 4. Comments.
-- =========================================================================

COMMENT ON TRIGGER player_accounts_identity_immutable ON player_accounts IS
    'ADR 0118 (BRAND-REHOME-HOLD-1): id, tenant_id, brand_id, created_at are immutable (BR001). Re-home is not a supported operation; a governed brand transfer needs its own ADR.';
COMMENT ON TRIGGER wallets_identity_immutable ON wallets IS
    'ADR 0118 (BRAND-REHOME-HOLD-1): id, tenant_id, brand_id, player_account_id, asset_code, created_at are immutable (BR002).';
COMMENT ON TRIGGER kyc_verifications_identity_immutable ON kyc_verifications IS
    'ADR 0118 (BRAND-REHOME-HOLD-1): tenant_id, brand_id, player_account_id are immutable (BR003).';
