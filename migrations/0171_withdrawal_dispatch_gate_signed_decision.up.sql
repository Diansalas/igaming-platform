-- F4 residual round 1 (docs/plans/f4r1-record.md): SF4-1 / R1 "a runtime credential can insert a forged ALLOW row into
-- withdrawal_gate_decisions", F4-R2-NEGBAL-SQL (the negative-balance rule in the SQL backstop) and SF4-5 (a silent fail-closed gate).
-- MONEY-PATH TRIGGERS: FOR `ledger-finance`, `security`, `code-reviewer`, `qa` REVIEW BEFORE MERGE.
--
-- NUMBERING: 0171 (0159 and 0161 stay reserved and unused; 0170 is F5(b)).
--
-- PROBLEM (0169 residual R1). WD053 proves that an allowing decision row was RECORDED in the transaction of the transition, not
-- that the dispatch code evaluated the gate: igaming_runtime holds INSERT on withdrawal_gate_decisions, so arbitrary SQL with a
-- stolen runtime credential could insert `allowed = true` and then perform E7 / T2 / T12 in the same transaction.
--
-- MECHANISM (ADR 0110, reused; no new mechanism). The ALLOW row is now an ATTESTATION of the application server, signed with the
-- actor-proof key the runtime role cannot read and verified in SQL by the existing SECURITY DEFINER verifier actor_proof_require:
--   * a new proof SCOPE 'system' (provable ONLY for the one operation withdrawal_dispatch_gate:allow, only with a tenant, only for
--     the fixed system actor 00000000-0000-0000-0000-00000f4d1500; that operation is provable for no other scope). Nothing else in
--     the verifier changes (0166's body verbatim plus one marked block and the scope list);
--   * a BEFORE INSERT trigger withdrawal_gate_decisions_zz_actor_proof_guard on withdrawal_gate_decisions (the LAST BEFORE ROW trigger
--     of the table, catalog-pinned) that, for an ALLOWED row, calls actor_proof_require(system actor, 'system', tenant,
--     'withdrawal_dispatch_gate:allow', <withdrawal request id>, <digest>) where digest = SHA-256(k2_canonical(tenant, request,
--     attempt | ~, stage, risk_outcome | ~, restriction_in_force | ~, account, wallet, asset, amount)) recomputed from the INSERTED
--     row and the immutable request columns. The proof therefore binds the tenant, the request, the attempt (re-dispatch), the stage,
--     the evaluated outcome and the very figures the gate evaluated; freshness (60 s cap, 5 s skew), replay (nonce UNIQUE, consumed
--     in the SAME transaction) and key-id rules are the verifier's (AP001..AP005);
--   * the consumed nonce is recorded in withdrawal_gate_decisions.actor_proof_nonce, written ONLY by that trigger (any caller value is
--     overwritten), UNIQUE when present; the guards WD053 (withdrawal_dispatch_decision_ok) count ONLY allowed rows that carry it.
--     A CHECK (NOT VALID: history is append-only and is not rewritten) forbids a new allowed row without one.
-- DENYING rows are not proof-bound: a denial only restricts, so a forged one gains nothing (and a denying row of the transaction
-- already vetoes WD053).
--
-- NOT changed: the table's grants (SELECT + INSERT), RLS, WD054/WD055, WD052/WD053 codes, the zz guards on withdrawal_requests and
-- payment_attempts (they call withdrawal_dispatch_decision_ok (replaced IN PLACE below) / withdrawal_dispatch_rg_violation (unchanged)).
-- Residual (documented, ADR 0110 T6): an attacker who holds the application's signing key can sign an allow.
--
-- F4-R2-NEGBAL-SQL is NOT folded in (docs/plans/f4r1-record.md section 5): the SQL clause (the read of rg.walletBalanceNegative in
-- withdrawal_dispatch_rg_violation) was built and proven, then withdrawn because it refuses every wdpath.Plant fixture (unfunded
-- holds make player_cash negative by design) and so cascades into the reconciliation / launch-closure / payments fixtures of other
-- workstreams. With the signed allow, the application gate's negative-balance refusal can no longer be bypassed by a forged row.
--
-- SF4-5. One alert Kind, withdrawal.payout_gate_unavailable (detached, p2 ENGINEERING DEFAULT, ALERT-DELIVERY-1 OPEN): raised by
-- internal/payments when the gate fails closed at T1p (evaluation error, no gate, no signing key) or repeatedly at T2 / T12.
--
-- SQLSTATEs: AP001-AP005 (the verifier); WD095 down refused while proof-bound decision rows (or alerts of the new Kind) exist.
-- No lock is taken by the new trigger; every function pins search_path; none but the (replaced) verifier is SECURITY DEFINER.

SET LOCAL lock_timeout = '5s';

-- =========================================================================
-- 1. The proof nonce column on withdrawal_gate_decisions.
-- =========================================================================

ALTER TABLE withdrawal_gate_decisions ADD COLUMN actor_proof_nonce TEXT NULL;
ALTER TABLE withdrawal_gate_decisions ADD CONSTRAINT withdrawal_gate_decisions_actor_proof_nonce_shape
    CHECK (actor_proof_nonce IS NULL OR actor_proof_nonce ~ '^[A-Za-z0-9_-]{16,64}$');
-- NOT VALID: rows recorded by 0169 (no proof) stay as unrewritten history; every row inserted from now on is checked.
ALTER TABLE withdrawal_gate_decisions ADD CONSTRAINT withdrawal_gate_decisions_allow_is_proof_bound
    CHECK (NOT allowed OR actor_proof_nonce IS NOT NULL) NOT VALID;
CREATE UNIQUE INDEX withdrawal_gate_decisions_actor_proof_nonce_key
    ON withdrawal_gate_decisions (actor_proof_nonce) WHERE actor_proof_nonce IS NOT NULL;

-- =========================================================================
-- 2. The verifier: 0166's body + the 'system' scope and the closed withdrawal_dispatch_gate:allow operation.
--    CREATE OR REPLACE keeps the owner, SECURITY DEFINER, the pinned search_path and the EXECUTE grants.
-- =========================================================================

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
    IF p_scope NOT IN ('tenant', 'platform_acting', 'platform', 'system') THEN
        RAISE EXCEPTION 'actor_proof: scope is not provable' USING ERRCODE = 'AP004';
    END IF;
    -- F4-R1 (migration 0171): the WD-RG-1 dispatch gate ALLOW is a SYSTEM attestation, not a staff action. The scope
    -- 'system' is provable ONLY for the single operation withdrawal_dispatch_gate:allow, with a tenant (checked below
    -- like every non-platform scope) and ONLY for the fixed system actor of the dispatch gate; that operation is
    -- provable for no other scope. The operation set is closed.
    IF p_scope = 'system' OR p_operation LIKE 'withdrawal\_dispatch\_gate:%' THEN
        IF p_operation IS DISTINCT FROM 'withdrawal_dispatch_gate:allow' OR p_scope IS DISTINCT FROM 'system'
           OR p_actor IS DISTINCT FROM '00000000-0000-0000-0000-00000f4d1500'::uuid THEN
            RAISE EXCEPTION 'actor_proof: this operation is not provable for this scope or actor' USING ERRCODE = 'AP004';
        END IF;
    END IF;
    -- ADR 0110 operation table (ADR 0111 M-10): withdrawal_hold_resolution:* is
    -- provable ONLY for scope platform_acting with a tenant.
    IF p_operation LIKE 'withdrawal\_hold\_resolution:%' AND (p_scope <> 'platform_acting' OR p_tenant IS NULL) THEN
        RAISE EXCEPTION 'actor_proof: this operation is provable only for a platform_acting scope with a tenant' USING ERRCODE = 'AP004';
    END IF;
    -- ADR 0112 6.4 (migration 0130): launch_authorisation:* is provable only for scope
    -- 'tenant' or 'platform' (never platform_acting), and a decision (approve / reject)
    -- only for scope 'platform'. The operation set is closed.
    IF p_operation LIKE 'launch\_authorisation:%' THEN
        IF p_operation NOT IN ('launch_authorisation:request', 'launch_authorisation:suspend', 'launch_authorisation:cancel',
                               'launch_authorisation:approve', 'launch_authorisation:reject')
           OR p_scope NOT IN ('platform', 'tenant')
           OR (p_operation IN ('launch_authorisation:approve', 'launch_authorisation:reject') AND p_scope <> 'platform') THEN
            RAISE EXCEPTION 'actor_proof: this launch_authorisation operation is not provable for this scope' USING ERRCODE = 'AP004';
        END IF;
    END IF;
    -- ADR 0115 S1 (migration 0135): rg_limit_config:* (the jurisdiction-floor four-eyes flow,
    -- section 2.1) is provable only for scope 'platform' (NULL tenant). The operation set is closed.
    IF p_operation LIKE 'rg\_limit\_config:%' THEN
        IF p_operation NOT IN ('rg_limit_config:request', 'rg_limit_config:approve', 'rg_limit_config:reject',
                               'rg_limit_config:cancel', 'rg_limit_config:apply')
           OR p_scope <> 'platform' THEN
            RAISE EXCEPTION 'actor_proof: this rg_limit_config operation is not provable for this scope' USING ERRCODE = 'AP004';
        END IF;
    END IF;
    -- ADR 0119 S5 + S6 (migration 0139): staff_lifecycle:* (the platform-staff four-eyes flow, section 10.4) is
    -- provable only for scope 'platform' (NULL tenant). The operation set is closed.
    IF p_operation LIKE 'staff\_lifecycle:%' THEN
        IF p_operation NOT IN ('staff_lifecycle:request', 'staff_lifecycle:approve', 'staff_lifecycle:reject',
                               'staff_lifecycle:cancel')
           OR p_scope <> 'platform' THEN
            RAISE EXCEPTION 'actor_proof: this staff_lifecycle operation is not provable for this scope' USING ERRCODE = 'AP004';
        END IF;
    END IF;
    -- The NULL-tenant encoding (an EMPTY tenant field) exists only for scope
    -- 'platform' and only for the K1 capability-grant, financial-policy-change,
    -- (ADR 0112, migration 0130) launch_authorisation, (ADR 0115) rg_limit_config and
    -- (ADR 0119, migration 0139) staff_lifecycle operations; every other scope and
    -- operation must carry a tenant.
    IF p_scope = 'platform' THEN
        IF p_tenant IS NOT NULL OR NOT (p_operation LIKE 'capability\_grant:%' OR p_operation LIKE 'financial\_policy\_change:%'
                                         OR p_operation LIKE 'launch\_authorisation:%'
                                         OR p_operation LIKE 'rg\_limit\_config:%'
                                         OR p_operation LIKE 'staff\_lifecycle:%') THEN
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
    SELECT k.secret INTO v_secret FROM actorproof_private.actor_proof_keys k WHERE k.kid = v_parts[2] AND k.status = 'active';
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

-- =========================================================================
-- 3. The proof guard (LAST BEFORE ROW trigger: 'zz' sorts after withdrawal_gate_decisions_before_insert).
-- =========================================================================

CREATE FUNCTION withdrawal_gate_decisions_actor_proof_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_account uuid;
    v_wallet  uuid;
    v_asset   text;
    v_amount  numeric;
BEGIN
    -- Never a caller value: the nonce is written only below, after the verifier consumed it.
    NEW.actor_proof_nonce := NULL;
    IF NOT NEW.allowed THEN
        RETURN NEW;   -- a denial only restricts: not proof-bound
    END IF;
    SELECT r.player_account_id, r.wallet_id, r.asset_code, r.amount INTO v_account, v_wallet, v_asset, v_amount
      FROM public.withdrawal_requests r WHERE r.id = NEW.withdrawal_request_id AND r.tenant_id = NEW.tenant_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'withdrawal_gate_decisions: the withdrawal request is not visible in this tenant' USING ERRCODE = 'WD054';
    END IF;
    PERFORM public.actor_proof_require('00000000-0000-0000-0000-00000f4d1500'::uuid, 'system', NEW.tenant_id,
        'withdrawal_dispatch_gate:allow', NEW.withdrawal_request_id::text,
        public.k2_sha256_hex(public.k2_canonical(NEW.tenant_id::text, NEW.withdrawal_request_id::text, NEW.payment_attempt_id::text,
            NEW.stage, NEW.risk_outcome, NEW.restriction_in_force, v_account::text, v_wallet::text, v_asset, v_amount::text)));
    -- The proof just verified and consumed is the one in the GUC (same statement): record its nonce.
    NEW.actor_proof_nonce := pg_catalog.split_part(pg_catalog.current_setting('app.actor_proof', true), '|', 11);
    RETURN NEW;
END
$$;

CREATE TRIGGER withdrawal_gate_decisions_zz_actor_proof_guard BEFORE INSERT ON withdrawal_gate_decisions
    FOR EACH ROW EXECUTE FUNCTION withdrawal_gate_decisions_actor_proof_guard();

-- =========================================================================
-- 4. WD053: an allowing row counts only when it is proof-bound (0169 body + the marked line).
-- =========================================================================

CREATE OR REPLACE FUNCTION withdrawal_dispatch_decision_ok(p_tenant uuid, p_request uuid, p_stage text, p_attempt uuid) RETURNS boolean
    LANGUAGE sql
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT EXISTS (SELECT 1 FROM public.withdrawal_gate_decisions d
                    WHERE d.tenant_id = p_tenant AND d.withdrawal_request_id = p_request AND d.stage = p_stage
                      AND d.payment_attempt_id IS NOT DISTINCT FROM p_attempt
                      AND d.decided_xact = pg_catalog.pg_current_xact_id() AND d.allowed
                      AND d.actor_proof_nonce IS NOT NULL) -- F4-R1 (0171): proof-bound allows only
       AND NOT EXISTS (SELECT 1 FROM public.withdrawal_gate_decisions d
                        WHERE d.tenant_id = p_tenant AND d.withdrawal_request_id = p_request AND d.stage = p_stage
                          AND d.payment_attempt_id IS NOT DISTINCT FROM p_attempt
                          AND d.decided_xact = pg_catalog.pg_current_xact_id() AND NOT d.allowed)
$$;

-- =========================================================================
-- 5. The alert Kind withdrawal.payout_gate_unavailable (the 0141 / 0169 / 0170 pattern).
-- =========================================================================

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.alert_kinds WHERE kind = 'withdrawal.payout_gate_unavailable') THEN
        RAISE EXCEPTION 'migration 0171 pre-flight: alert kind withdrawal.payout_gate_unavailable already exists' USING ERRCODE = 'WD098';
    END IF;
    CREATE POLICY alert_kinds_seed_0171 ON alert_kinds
        FOR INSERT TO CURRENT_USER
        WITH CHECK (kind = 'withdrawal.payout_gate_unavailable');
    INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject,
                             in_tx_raisable_by_tenant, allowed_keys, raise_mode)
    VALUES ('withdrawal.payout_gate_unavailable', 'p2', 'platform', false, true, true,
            ARRAY['stage', 'cause'], 'detached');
    DROP POLICY alert_kinds_seed_0171 ON alert_kinds;
END
$$;

COMMENT ON COLUMN withdrawal_gate_decisions.actor_proof_nonce IS 'F4-R1: nonce of the system actor proof (withdrawal_dispatch_gate:allow) an ALLOWED decision consumed; written only by the zz proof guard.';
