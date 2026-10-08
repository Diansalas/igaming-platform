-- MUST RUN INSIDE A SINGLE TRANSACTION (db.MigrateDown does this). Run by hand
-- under psql autocommit, an HR099 refusal below would leave FORCE ROW LEVEL SECURITY
-- lifted for the owner on the checked tables. By hand, use
-- `psql --single-transaction -v ON_ERROR_STOP=1 -f <this file>`.
--
-- Reverses 0124 (ADR 0111 section 6). Refuses (HR099) while any hold resolution
-- (or approval) row exists, or any financial policy / policy change / capability
-- grant row of the withdrawal_hold_resolution operation exists - once such a row
-- exists 0124 is effectively irreversible. Otherwise it restores byte-for-byte the
-- 0113 financial_policy_required_approvals, the 0115 ledger_governed_fence_allows /
-- ledger_entries_governed_fence / acting ledger_accounts INSERT policy / acting
-- withdrawal_requests UPDATE policy, the 0120 actor_proof_require and the 0112/0113
-- CHECK constraints, and drops everything else 0124 added. The whole-schema snapshot
-- test verifies the restoration.

ALTER TABLE withdrawal_hold_resolutions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE withdrawal_hold_resolution_approvals NO FORCE ROW LEVEL SECURITY;
ALTER TABLE financial_approval_policies NO FORCE ROW LEVEL SECURITY;
ALTER TABLE financial_approval_policy_changes NO FORCE ROW LEVEL SECURITY;
ALTER TABLE staff_capability_grant_requests NO FORCE ROW LEVEL SECURITY;
ALTER TABLE staff_capability_grants NO FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM withdrawal_hold_resolutions)
       OR EXISTS (SELECT 1 FROM withdrawal_hold_resolution_approvals)
       OR EXISTS (SELECT 1 FROM financial_approval_policies WHERE operation_kind = 'withdrawal_hold_resolution')
       OR EXISTS (SELECT 1 FROM financial_approval_policy_changes WHERE operation_kind = 'withdrawal_hold_resolution')
       OR EXISTS (SELECT 1 FROM staff_capability_grant_requests WHERE capability IN ('withdrawal_hold_resolution:request', 'withdrawal_hold_resolution:approve'))
       OR EXISTS (SELECT 1 FROM staff_capability_grants WHERE capability IN ('withdrawal_hold_resolution:request', 'withdrawal_hold_resolution:approve')) THEN
        RAISE EXCEPTION '0124 down refused: withdrawal hold resolution / policy / grant rows exist' USING ERRCODE = 'HR099';
    END IF;
END $$;

ALTER TABLE financial_approval_policies FORCE ROW LEVEL SECURITY;
ALTER TABLE financial_approval_policy_changes FORCE ROW LEVEL SECURITY;
ALTER TABLE staff_capability_grant_requests FORCE ROW LEVEL SECURITY;
ALTER TABLE staff_capability_grants FORCE ROW LEVEL SECURITY;

-- Triggers first (they reference the functions below).
DROP TRIGGER zz_actor_proof_guard ON withdrawal_hold_resolution_approvals;
DROP TRIGGER zz_actor_proof_guard ON withdrawal_hold_resolutions;
DROP TRIGGER withdrawal_requests_approved_hold_freeze ON withdrawal_requests;
DROP TRIGGER withdrawal_requests_governed_release_guard ON withdrawal_requests;
DROP TRIGGER ledger_transactions_hold_release_key_guard ON ledger_transactions;

-- The acting policies of the two shared policy objects, back to 0115.
DROP POLICY acting_update ON withdrawal_requests;
CREATE POLICY acting_update ON withdrawal_requests FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
                AND (SELECT financial_acting_session_valid())
                AND EXISTS (SELECT 1 FROM payment_manual_resolutions m
                             WHERE m.withdrawal_request_id = withdrawal_requests.id AND m.tenant_id = withdrawal_requests.tenant_id
                               AND m.state = 'executing' AND m.executed_txid = txid_current()
                               AND m.kind IN ('m2_declare_paid', 'm2_declare_not_paid')));

DROP POLICY acting_insert ON ledger_accounts;
CREATE POLICY acting_insert ON ledger_accounts FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
                AND (SELECT financial_acting_session_valid())
                AND (account_type IN ('player_cash', 'manual_adjustment')
                     OR (account_type = 'player_withdrawal_hold'
                         AND EXISTS (SELECT 1 FROM payment_manual_resolutions m
                                       JOIN withdrawal_requests w ON w.id = m.withdrawal_request_id AND w.tenant_id = m.tenant_id
                                      WHERE m.tenant_id = ledger_accounts.tenant_id
                                        AND m.kind IN ('m2_declare_paid', 'm2_declare_not_paid')
                                        AND m.state = 'executing' AND m.executed_txid = txid_current()
                                        AND w.wallet_id = ledger_accounts.wallet_id
                                        AND w.asset_code = ledger_accounts.asset_code))
                     OR (account_type = 'psp_clearing' AND wallet_id IS NULL
                         AND EXISTS (SELECT 1 FROM payment_manual_resolutions m
                                       JOIN withdrawal_requests w ON w.id = m.withdrawal_request_id AND w.tenant_id = m.tenant_id
                                      WHERE m.tenant_id = ledger_accounts.tenant_id
                                        AND m.kind = 'm2_declare_paid'
                                        AND m.state = 'executing' AND m.executed_txid = txid_current()
                                        AND w.asset_code = ledger_accounts.asset_code))));

-- The shared function bodies, back to 0115 / 0120 / 0113.
CREATE OR REPLACE FUNCTION ledger_entries_governed_fence() RETURNS TRIGGER AS $$
DECLARE
    v_tx          RECORD;
    v_req         RECORD;
    v_m           RECORD;
    v_acct        RECORD;
    v_player_dir  text;
    v_house_dir   text;
BEGIN
    IF financial_acting_gucs_present() THEN
        SELECT t.tenant_id, t.transaction_type, t.idempotency_key, t.correlation_id, t.provider_id, t.provider_tx_id INTO v_tx
          FROM ledger_transactions t WHERE t.id = NEW.ledger_transaction_id AND t.tenant_id = NEW.tenant_id;
        IF NOT FOUND OR NOT ledger_governed_fence_allows(v_tx.tenant_id, v_tx.transaction_type, v_tx.idempotency_key, v_tx.correlation_id,
                                                         v_tx.provider_id, v_tx.provider_tx_id) THEN
            RAISE EXCEPTION 'ledger_entries_governed_fence: an acting session may add entries only to a governed, executing request''s transaction' USING ERRCODE = 'CG030';
        END IF;
        IF v_tx.transaction_type = 'manual_adjustment' THEN
        SELECT r.wallet_id, r.asset_code, r.amount, r.direction INTO v_req
          FROM ledger_adjustment_requests r
         WHERE r.id = v_tx.correlation_id AND r.tenant_id = v_tx.tenant_id
           AND v_tx.idempotency_key = 'manual_adjustment:' || r.id::text;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'ledger_entries_governed_fence: governed request not found' USING ERRCODE = 'CG030';
        END IF;
        v_player_dir := CASE WHEN v_req.direction = 'credit_player' THEN 'credit' ELSE 'debit' END;
        v_house_dir  := CASE WHEN v_req.direction = 'credit_player' THEN 'debit' ELSE 'credit' END;
        SELECT la.account_type, la.wallet_id INTO v_acct
          FROM ledger_accounts la WHERE la.id = NEW.ledger_account_id AND la.tenant_id = NEW.tenant_id;
        IF NOT FOUND
           OR NEW.asset_code IS DISTINCT FROM v_req.asset_code
           OR NEW.amount IS DISTINCT FROM v_req.amount
           OR NOT ((v_acct.account_type = 'player_cash' AND v_acct.wallet_id = v_req.wallet_id AND NEW.direction = v_player_dir)
                OR (v_acct.account_type = 'manual_adjustment' AND v_acct.wallet_id IS NULL AND NEW.direction = v_house_dir)) THEN
            RAISE EXCEPTION 'ledger_entries_governed_fence: the entry is not a leg of the approved §4 shape (account, asset, amount or direction)' USING ERRCODE = 'CG030';
        END IF;
        ELSIF v_tx.transaction_type IN ('withdrawal_completed', 'withdrawal_failed') THEN
            -- (b) declare paid / (c) declare not paid: the resolution, the
            -- withdrawal and the entry, all in this transaction.
            SELECT m.amount AS m_amount, w.wallet_id, w.asset_code, w.amount AS w_amount INTO v_m
              FROM payment_manual_resolutions m
              JOIN withdrawal_requests w ON w.id = m.withdrawal_request_id AND w.tenant_id = m.tenant_id
             WHERE m.tenant_id = v_tx.tenant_id
               AND m.kind = CASE v_tx.transaction_type WHEN 'withdrawal_completed' THEN 'm2_declare_paid' ELSE 'm2_declare_not_paid' END
               AND m.state = 'executing' AND m.executed_txid = txid_current()
               AND m.withdrawal_request_id = v_tx.correlation_id
               AND v_tx.idempotency_key = CASE v_tx.transaction_type
                       WHEN 'withdrawal_completed' THEN m.provider_id || ':' || m.reserved_provider_tx_id
                       ELSE m.withdrawal_request_id::text || ':failed' END;
            IF NOT FOUND THEN
                RAISE EXCEPTION 'ledger_entries_governed_fence: governed M2 resolution not found' USING ERRCODE = 'CG030';
            END IF;
            SELECT la.account_type, la.wallet_id INTO v_acct
              FROM ledger_accounts la WHERE la.id = NEW.ledger_account_id AND la.tenant_id = NEW.tenant_id;
            IF NOT FOUND
               OR NEW.asset_code IS DISTINCT FROM v_m.asset_code
               OR NEW.amount IS DISTINCT FROM v_m.w_amount
               OR NEW.amount IS DISTINCT FROM v_m.m_amount
               OR NOT COALESCE((v_acct.account_type = 'player_withdrawal_hold' AND v_acct.wallet_id = v_m.wallet_id AND NEW.direction = 'debit')
                    OR (v_tx.transaction_type = 'withdrawal_completed' AND v_acct.account_type = 'psp_clearing'
                        AND v_acct.wallet_id IS NULL AND NEW.direction = 'credit')
                    OR (v_tx.transaction_type = 'withdrawal_failed' AND v_acct.account_type = 'player_cash'
                        AND v_acct.wallet_id = v_m.wallet_id AND NEW.direction = 'credit'), false) THEN
                RAISE EXCEPTION 'ledger_entries_governed_fence: the entry is not a leg of the approved M2 shape (account, asset, amount or direction)' USING ERRCODE = 'CG030';
            END IF;
        ELSE
            RAISE EXCEPTION 'ledger_entries_governed_fence: no governed shape for transaction type %', v_tx.transaction_type USING ERRCODE = 'CG030';
        END IF;
        IF EXISTS (SELECT 1 FROM ledger_entries e
                    WHERE e.ledger_transaction_id = NEW.ledger_transaction_id
                      AND (e.direction = NEW.direction
                           OR (SELECT count(*) FROM ledger_entries e2 WHERE e2.ledger_transaction_id = NEW.ledger_transaction_id) >= 2)) THEN
            RAISE EXCEPTION 'ledger_entries_governed_fence: the linked transaction already holds this leg (at most two entries, one per direction)' USING ERRCODE = 'CG030';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE OR REPLACE FUNCTION ledger_governed_fence_allows(
    p_tenant uuid, p_type text, p_idempotency_key text, p_correlation uuid, p_provider_id text, p_provider_tx_id text
) RETURNS boolean AS $$
    SELECT (p_type = 'manual_adjustment' AND EXISTS (
        SELECT 1 FROM ledger_adjustment_requests r
         WHERE r.tenant_id = p_tenant
           AND p_idempotency_key = 'manual_adjustment:' || r.id::text
           AND p_correlation = r.id
           AND r.state = 'executing' AND r.executed_txid = txid_current()))
        -- (b) M2 declare paid
        OR (p_type = 'withdrawal_completed' AND EXISTS (
        SELECT 1 FROM payment_manual_resolutions m
         WHERE m.tenant_id = p_tenant AND m.kind = 'm2_declare_paid'
           AND m.state = 'executing' AND m.executed_txid = txid_current()
           AND p_correlation = m.withdrawal_request_id
           AND p_provider_id = m.provider_id
           AND p_provider_tx_id = m.reserved_provider_tx_id
           AND p_idempotency_key = m.provider_id || ':' || m.reserved_provider_tx_id))
        -- (c) M2 declare not paid
        OR (p_type = 'withdrawal_failed' AND EXISTS (
        SELECT 1 FROM payment_manual_resolutions m
         WHERE m.tenant_id = p_tenant AND m.kind = 'm2_declare_not_paid'
           AND m.state = 'executing' AND m.executed_txid = txid_current()
           AND p_correlation = m.withdrawal_request_id
           AND p_idempotency_key = m.withdrawal_request_id::text || ':failed'));
$$ LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp;

CREATE OR REPLACE FUNCTION actor_proof_require(
    p_actor        uuid,
    p_scope        text,
    p_tenant       uuid,
    p_operation    text,
    p_target       text,
    p_payload_hash text
) RETURNS void AS $$
DECLARE
    v_token   text := NULLIF(pg_catalog.current_setting('app.actor_proof', true), '');
    v_parts   text[];
    v_secret  bytea;
    v_signed  text;
    v_mac     text;
    v_iat     bigint;
    v_exp     bigint;
    v_now     bigint := pg_catalog.floor(pg_catalog.date_part('epoch', pg_catalog.clock_timestamp()))::bigint;
    v_actor   uuid;
    v_tenant  uuid;
    v_n       int;
BEGIN
    IF v_token IS NULL OR p_actor IS NULL OR p_scope IS NULL OR p_operation IS NULL OR p_target IS NULL OR p_payload_hash IS NULL THEN
        RAISE EXCEPTION 'actor_proof: no proof presented' USING ERRCODE = 'AP001';
    END IF;
    IF p_scope NOT IN ('tenant', 'platform_acting', 'platform') THEN
        RAISE EXCEPTION 'actor_proof: scope is not provable' USING ERRCODE = 'AP004';
    END IF;
    -- The NULL-tenant encoding (an EMPTY tenant field) exists only for scope
    -- 'platform' and only for the K1 capability-grant and financial-policy-change
    -- operations; every other scope and operation must carry a tenant.
    IF p_scope = 'platform' THEN
        IF p_tenant IS NOT NULL OR NOT (p_operation LIKE 'capability\_grant:%' OR p_operation LIKE 'financial\_policy\_change:%') THEN
            RAISE EXCEPTION 'actor_proof: platform scope is not provable for this operation' USING ERRCODE = 'AP004';
        END IF;
    ELSIF p_tenant IS NULL THEN
        RAISE EXCEPTION 'actor_proof: no proof presented' USING ERRCODE = 'AP001';
    END IF;

    v_parts := pg_catalog.string_to_array(v_token, '|');
    IF pg_catalog.array_length(v_parts, 1) IS DISTINCT FROM 12 OR v_parts[1] <> 'v1' THEN
        RAISE EXCEPTION 'actor_proof: malformed proof' USING ERRCODE = 'AP001';
    END IF;
    IF v_parts[9] !~ '^[0-9]{1,12}$' OR v_parts[10] !~ '^[0-9]{1,12}$'
       OR v_parts[11] !~ '^[A-Za-z0-9_-]{16,64}$' OR v_parts[12] !~ '^[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'actor_proof: malformed proof' USING ERRCODE = 'AP001';
    END IF;
    BEGIN
        v_actor  := v_parts[3]::uuid;
        v_tenant := NULLIF(v_parts[5], '')::uuid;
    EXCEPTION WHEN OTHERS THEN
        RAISE EXCEPTION 'actor_proof: malformed proof' USING ERRCODE = 'AP001';
    END;
    v_iat := v_parts[9]::bigint;
    v_exp := v_parts[10]::bigint;

    -- Key lookup (active keys only) and MAC.
    SELECT k.secret INTO v_secret FROM public.actor_proof_keys k WHERE k.kid = v_parts[2] AND k.status = 'active';
    IF NOT FOUND THEN
        RAISE EXCEPTION 'actor_proof: proof rejected' USING ERRCODE = 'AP002';
    END IF;
    v_signed := pg_catalog.array_to_string(v_parts[1:11], '|');
    v_mac := pg_catalog.encode(public.hmac(pg_catalog.convert_to(v_signed, 'UTF8'), v_secret, 'sha256'), 'hex');
    -- Compare HMACs of both values under the same key, so the comparison is
    -- not a byte-wise early-exit on attacker-controlled input.
    IF public.hmac(pg_catalog.convert_to(v_mac, 'UTF8'), v_secret, 'sha256')
       IS DISTINCT FROM public.hmac(pg_catalog.convert_to(v_parts[12], 'UTF8'), v_secret, 'sha256') THEN
        RAISE EXCEPTION 'actor_proof: proof rejected' USING ERRCODE = 'AP002';
    END IF;

    -- Time window: not expired, not issued in the future (5 s skew), and the
    -- lifetime is at most 60 s.
    IF v_exp <= v_now OR v_iat > v_now + 5 OR v_exp <= v_iat OR v_exp - v_iat > 60 THEN
        RAISE EXCEPTION 'actor_proof: proof expired or not yet valid' USING ERRCODE = 'AP003';
    END IF;

    -- Binding: the proof must be for exactly this actor/scope/tenant and this
    -- operation/target/payload.
    IF v_actor IS DISTINCT FROM p_actor OR v_parts[4] <> p_scope OR v_tenant IS DISTINCT FROM p_tenant
       OR v_parts[6] <> p_operation OR v_parts[7] <> p_target OR v_parts[8] <> p_payload_hash THEN
        RAISE EXCEPTION 'actor_proof: proof does not match the actor/operation/target' USING ERRCODE = 'AP004';
    END IF;

    -- Replay protection: one proof, one consumption. The row commits or rolls
    -- back with the governed write, so a failed attempt does not burn it and a
    -- committed one can never be replayed (the UNIQUE nonce key serialises two
    -- concurrent consumers).
    INSERT INTO public.actor_proof_nonces
        (nonce, kid, actor, scope, tenant, operation, target, payload_hash, expires_at, consumed_txid)
    VALUES (v_parts[11], v_parts[2], v_actor, p_scope, v_tenant, p_operation, p_target, p_payload_hash,
            pg_catalog.to_timestamp(v_exp), pg_catalog.txid_current())
    ON CONFLICT (nonce) DO NOTHING;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    IF v_n = 0 THEN
        RAISE EXCEPTION 'actor_proof: proof already used' USING ERRCODE = 'AP005';
    END IF;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp;

CREATE OR REPLACE FUNCTION financial_policy_required_approvals(
    p_operation text, p_tenant uuid, p_brand uuid, p_asset text, p_amount numeric, p_as_of timestamptz,
    OUT enabled boolean, OUT required int, OUT contributing_policy_ids uuid[], OUT tenant_status text
) AS $$
DECLARE
    v_licence       uuid;
    v_jurisdiction  uuid;
    v_profile       text;
    v_max           int;
    v_has_platform  boolean;
    v_tenant_rows   boolean;
BEGIN
    SELECT t.status, t.licence_id INTO tenant_status, v_licence FROM tenants t WHERE t.id = p_tenant;
    IF NOT FOUND THEN
        enabled := false; required := 1; contributing_policy_ids := '{}'; tenant_status := NULL;
        RETURN;
    END IF;
    IF v_licence IS NOT NULL THEN
        SELECT l.jurisdiction_id INTO v_jurisdiction FROM licences l WHERE l.id = v_licence;
    END IF;
    SELECT pp.profile_code INTO v_profile FROM tenant_financial_policy_profiles pp
     WHERE pp.tenant_id = p_tenant AND pp.effective_from <= p_as_of
     ORDER BY pp.effective_from DESC, pp.created_at DESC LIMIT 1;

    v_tenant_rows := NOT (p_operation = 'payment_force_resolve' AND tenant_status <> 'active');

    WITH candidates AS (
        SELECT p.* FROM financial_approval_policies p
         WHERE p.operation_kind = p_operation
           AND p.effective_from <= p_as_of
           AND (p.asset_code IS NULL OR p.asset_code = p_asset)
           AND (p.level = 'platform'
                OR (p.level = 'jurisdiction' AND p.jurisdiction_id = v_jurisdiction)
                OR (p.level = 'profile' AND p.profile_code = v_profile)
                OR (v_tenant_rows AND p.level = 'tenant' AND p.tenant_id = p_tenant)
                OR (v_tenant_rows AND p.level = 'brand' AND p.tenant_id = p_tenant AND p.brand_id = p_brand))
    ), in_force AS (
        SELECT DISTINCT ON (c.level, c.tenant_id, c.brand_id, c.jurisdiction_id, c.profile_code, c.asset_code) c.*
          FROM candidates c
         ORDER BY c.level, c.tenant_id, c.brand_id, c.jurisdiction_id, c.profile_code, c.asset_code,
                  c.effective_from DESC, c.created_at DESC, c.id
    )
    SELECT max(CASE WHEN f.threshold_minor_units IS NULL OR p_amount IS NULL OR p_amount <= f.threshold_minor_units
                    THEN f.base_required_approvals ELSE f.required_approvals_above_threshold END),
           COALESCE(array_agg(f.id ORDER BY f.id), '{}'),
           COALESCE(bool_or(f.level = 'platform'), false)
      INTO v_max, contributing_policy_ids, v_has_platform
      FROM in_force f;

    required := GREATEST(1, COALESCE(v_max, 0));
    enabled := v_has_platform;
    IF v_jurisdiction IS NULL AND EXISTS (
        SELECT 1 FROM financial_approval_policies p
         WHERE p.operation_kind = p_operation AND p.level = 'jurisdiction' AND p.effective_from <= p_as_of
           AND (p.asset_code IS NULL OR p.asset_code = p_asset)) THEN
        enabled := false;
    END IF;
END;
$$ LANGUAGE plpgsql STABLE;

DROP FUNCTION actor_proof_withdrawal_hold_resolution_approvals_guard();
DROP FUNCTION actor_proof_withdrawal_hold_resolutions_guard();
DROP FUNCTION withdrawal_requests_approved_hold_freeze();
DROP FUNCTION withdrawal_requests_governed_release_guard();
DROP FUNCTION ledger_transactions_hold_release_key_guard();

DROP POLICY acting_lock ON brands;

-- The tables (dropping a table drops its triggers, policies and indexes).
DROP FUNCTION withdrawal_hold_resolution_payload_hash(withdrawal_hold_resolutions);
DROP TABLE withdrawal_hold_resolution_approvals;
DROP TABLE withdrawal_hold_resolutions;
DROP FUNCTION withdrawal_hold_resolution_approvals_apply_reject();
DROP FUNCTION withdrawal_hold_resolution_approvals_guard();
DROP FUNCTION withdrawal_hold_resolutions_no_executing_commit();
DROP FUNCTION withdrawal_hold_resolutions_beneficiary_guard();
DROP FUNCTION withdrawal_hold_resolutions_guard();
DROP FUNCTION withdrawal_hold_resolution_execution_status(uuid);
DROP FUNCTION withdrawal_hold_resolution_lock_scope(uuid, uuid);

-- The reference rows and the widened CHECKs, back to 0112/0113. The classification's
-- delete trigger is lifted for this one statement only (same transaction).
ALTER TABLE financial_control_classifications NO FORCE ROW LEVEL SECURITY;
ALTER TABLE financial_capability_catalogue NO FORCE ROW LEVEL SECURITY;
ALTER TABLE financial_control_classifications DISABLE TRIGGER financial_control_classifications_no_delete;
DELETE FROM financial_capability_catalogue WHERE operation_kind = 'withdrawal_hold_resolution';
DELETE FROM financial_control_classifications WHERE operation_kind = 'withdrawal_hold_resolution';
ALTER TABLE financial_control_classifications ENABLE TRIGGER financial_control_classifications_no_delete;
ALTER TABLE financial_control_classifications FORCE ROW LEVEL SECURITY;
ALTER TABLE financial_capability_catalogue FORCE ROW LEVEL SECURITY;

ALTER TABLE financial_capability_catalogue DROP CONSTRAINT financial_capability_catalogue_operation_kind_check;
ALTER TABLE financial_capability_catalogue ADD CONSTRAINT financial_capability_catalogue_operation_kind_check
    CHECK (operation_kind IN ('ledger_adjustment', 'payment_force_resolve'));
ALTER TABLE financial_control_classifications DROP CONSTRAINT financial_control_classifications_check;
ALTER TABLE financial_control_classifications ADD CONSTRAINT financial_control_classifications_check
    CHECK (operation_kind NOT IN ('ledger_adjustment', 'payment_force_resolve') OR class = 'mandatory_four_eyes');
