-- 0175: closure security review F-1 (docs/plans/tomb-record.md). A DEPOSIT reversal tombstone is a signed SYSTEM attestation.
-- MONEY-PATH TRIGGERS: FOR `ledger-finance`, `security`, `code-reviewer`, `qa` REVIEW BEFORE MERGE.
--
-- NUMBERING: 0175 (0173 = bonus first-deposit backstop, 0174 = reconciliation negative-balance kinds; 0159 / 0161 stay reserved).
--
-- PROBLEM. igaming_runtime holds INSERT on ledger_transactions. A SQL-only actor holding that credential could insert
--   (tenant, 'tombstone', <key>, provider_id = <PSP>, provider_tx_id = <deposit reference>, <random correlation>)
-- and so (a) make payment_ref_cleared (0172: a tombstone is the ONLY thing that clears) return true, removing the MA020
-- preventive barrier against a manual credit for a captured-unposted deposit, and (b) make a genuine late success of that
-- deposit be rejected (tombstone semantics + the (tenant, provider_id, provider_tx_id) unique index). The only tombstone guard
-- (0162/0165 WD027) covers PAYOUT tombstones (correlation = a withdrawal request).
--
-- MECHANISM (ADR 0110, reused exactly as 0171 did; no new mechanism, no new table, no new SECURITY DEFINER function):
--   * the verifier actor_proof_require (CREATE OR REPLACE: 0171's body + ONE changed marked block; owner, SECURITY DEFINER,
--     pinned search_path and EXECUTE grants are kept) admits the scope 'system' for exactly one more operation,
--     deposit_reversal_tombstone:write, only for its own fixed system actor 00000000-0000-0000-0000-0000000de175;
--   * ledger_transactions.actor_proof_nonce (NULL), written ONLY by the guard below (any caller value is overwritten), shape
--     checked, tombstones only, UNIQUE when present;
--   * ledger_transactions_zz_deposit_tombstone_proof_guard, BEFORE INSERT FOR EACH ROW, the LAST BEFORE ROW trigger of the table
--     (catalog-pinned). For a tombstone that names a provider and is NOT a payout tombstone of the A5 branch, and whose provider
--     is a PAYMENT provider of the tenant (or whose session context could hide that fact: fail closed), it calls
--       actor_proof_require(...de175, 'system', tenant, 'deposit_reversal_tombstone:write', <receipt id>,
--         SHA-256(k2_canonical(tenant, provider_id, provider_tx_id, idempotency_key, correlation_id, <receipt id>)))
--     recomputed from the INSERTED row; the receipt id is the proof's own (MAC-covered) target, and it must name a stored,
--     applied deposit_reversal receipt (payment_provider_events) of the same tenant / provider / original reference (DT001).
--     So the proof binds tenant, provider, reference, ledger key, correlation and the verified callback receipt; freshness
--     (60 s cap, 5 s skew), replay (nonce UNIQUE, consumed in the SAME transaction or savepoint) and key rules are the verifier's;
--   * payment_ref_cleared (MA020) counts ONLY proof-bound tombstones (actor_proof_nonce IS NOT NULL). This makes the MA020
--     closure independent of the classification above: a tombstone that escaped the guard (a casino-classified or a payout
--     tombstone) can never clear MA020.
--
-- CLASSIFICATION (which tombstones need the proof). Ledger tombstones are also written by casino rollbacks (provider = a game
-- provider, same key shape 'tombstone:<provider>:<ref>'), sportsbook rollbacks (no provider: untouched) and the A5 payout
-- reversal branch (correlation = the withdrawal request; WD027 governs it). A tombstone is EXEMPT from this guard only if
--   (payout) its correlation is a withdrawal request of the tenant AND its (provider, reference) is that request's own payout
--            reference (on the request or on its payout attempt) AND no DEPOSIT attempt of the tenant holds that reference; or
--   (non-payment) the session context is the plain tenant context of the row (app.tenant_id = the row's tenant, no player, no
--            platform-admin GUC: exactly the context in which the tenant_staff_scope RLS of payment_attempts and
--            payment_provider_events shows every row of the tenant) AND the provider has neither a payment attempt nor a
--            payment receipt in the tenant (both tables are append-only: no DELETE, no TRUNCATE for any role).
-- Everything else needs the proof. A genuine deposit reversal tombstone always has its receipt row in the same transaction,
-- and the production code always signs, so the legitimate path never depends on the classification.
--
-- Fail closed: no proof -> AP001 (the callback transaction rolls back; the application maps the unsigned case to 503 + Retry-After
-- so the PSP redelivers, never a silent skip). Posting semantics of a legitimate reversal are unchanged (same row, same key, same
-- correlation, same audit; the only addition is the nonce column).
--
-- SQLSTATEs: AP001-AP005 (the verifier); DT001 the proof's receipt is not an applied deposit_reversal receipt of this tenant /
-- provider / reference; DT095 down refused while proof-bound tombstones exist.
-- No lock is taken by the new trigger; every function pins search_path; none but the (replaced) verifier is SECURITY DEFINER.
-- Intentional behaviour change: a tombstone written WITHOUT a proof (history before 0175, or a test fixture that posts one raw)
-- no longer clears MA020 in SQL; there is no production history (pre-launch, synthetic data only).

SET LOCAL lock_timeout = '5s';

-- =========================================================================
-- 1. The proof nonce column on ledger_transactions.
-- =========================================================================

ALTER TABLE ledger_transactions ADD COLUMN actor_proof_nonce TEXT NULL;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_actor_proof_nonce_shape
    CHECK (actor_proof_nonce IS NULL OR (transaction_type = 'tombstone' AND actor_proof_nonce ~ '^[A-Za-z0-9_-]{16,64}$'));
CREATE UNIQUE INDEX ledger_transactions_actor_proof_nonce_key
    ON ledger_transactions (actor_proof_nonce) WHERE actor_proof_nonce IS NOT NULL;

-- =========================================================================
-- 2. The verifier: 0171's body with the system-scope block widened by exactly one (operation, actor) pair.
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
    -- 0175 (closure security review F-1): the scope 'system' is ALSO provable for exactly one more operation,
    -- deposit_reversal_tombstone:write, ONLY for its own fixed system actor (...00de0175, distinct from the dispatch
    -- gate's); that operation is provable for no other scope. Each operation is bound to its own actor.
    IF p_scope = 'system' OR p_operation LIKE 'withdrawal\_dispatch\_gate:%' OR p_operation LIKE 'deposit\_reversal\_tombstone:%' THEN
        IF NOT ((p_operation = 'withdrawal_dispatch_gate:allow' AND p_scope = 'system'
                 AND p_actor = '00000000-0000-0000-0000-00000f4d1500'::uuid)
             OR (p_operation = 'deposit_reversal_tombstone:write' AND p_scope = 'system'
                 AND p_actor = '00000000-0000-0000-0000-0000000de175'::uuid)) THEN
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
-- 3. The deposit tombstone proof guard (LAST BEFORE ROW trigger: 'ledger_transactions_zz_' sorts after every other one).
-- =========================================================================

CREATE FUNCTION ledger_transactions_deposit_tombstone_proof_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_target text;
BEGIN
    -- Never a caller value: the nonce is written only below, after the verifier consumed it.
    NEW.actor_proof_nonce := NULL;
    IF NEW.transaction_type IS DISTINCT FROM 'tombstone' OR NEW.provider_id IS NULL THEN
        RETURN NEW;
    END IF;
    -- (payout) the A5 payout-reversal tombstone: WD027 (0162/0165, AFTER INSERT) governs it.
    IF EXISTS (SELECT 1 FROM public.withdrawal_requests r
                WHERE r.id = NEW.correlation_id AND r.tenant_id = NEW.tenant_id
                  AND ((r.provider_id = NEW.provider_id AND r.provider_reference = NEW.provider_tx_id)
                       OR EXISTS (SELECT 1 FROM public.payment_attempts a
                                   WHERE a.tenant_id = NEW.tenant_id AND a.operation = 'payout' AND a.withdrawal_request_id = r.id
                                     AND a.provider_id = NEW.provider_id AND a.provider_reference = NEW.provider_tx_id)))
       AND NOT EXISTS (SELECT 1 FROM public.payment_attempts d
                        WHERE d.tenant_id = NEW.tenant_id AND d.operation = 'deposit'
                          AND d.provider_id = NEW.provider_id AND d.provider_reference = NEW.provider_tx_id) THEN
        RETURN NEW;
    END IF;
    -- (non-payment) a casino rollback tombstone: only when the lookup below can see every payment row of the tenant.
    IF NULLIF(pg_catalog.current_setting('app.tenant_id', true), '') IS NOT DISTINCT FROM NEW.tenant_id::text
       AND NULLIF(pg_catalog.current_setting('app.player_account_id', true), '') IS NULL
       AND NULLIF(pg_catalog.current_setting('app.platform_admin_principal_id', true), '') IS NULL
       AND NOT EXISTS (SELECT 1 FROM public.payment_attempts a WHERE a.tenant_id = NEW.tenant_id AND a.provider_id = NEW.provider_id)
       AND NOT EXISTS (SELECT 1 FROM public.payment_provider_events e WHERE e.tenant_id = NEW.tenant_id AND e.provider_id = NEW.provider_id) THEN
        RETURN NEW;
    END IF;
    -- A deposit reversal tombstone: a signed system attestation bound to the verified reversal receipt (the proof's target).
    v_target := pg_catalog.split_part(NULLIF(pg_catalog.current_setting('app.actor_proof', true), ''), '|', 7);
    PERFORM public.actor_proof_require('00000000-0000-0000-0000-0000000de175'::uuid, 'system', NEW.tenant_id,
        'deposit_reversal_tombstone:write', v_target,
        public.k2_sha256_hex(public.k2_canonical(NEW.tenant_id::text, NEW.provider_id, NEW.provider_tx_id, NEW.idempotency_key,
            NEW.correlation_id::text, v_target)));
    IF v_target !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
       OR NOT EXISTS (SELECT 1 FROM public.payment_provider_events e
                       WHERE e.id = v_target::uuid AND e.tenant_id = NEW.tenant_id AND e.provider_id = NEW.provider_id
                         AND e.event_type = 'deposit_reversal' AND e.original_provider_reference = NEW.provider_tx_id
                         AND e.disposition_at_receipt = 'applied') THEN
        RAISE EXCEPTION 'ledger_transactions: a deposit reversal tombstone needs an applied deposit_reversal receipt of its tenant, provider and reference'
            USING ERRCODE = 'DT001';
    END IF;
    -- The proof just verified and consumed is the one in the GUC (same statement): record its nonce.
    NEW.actor_proof_nonce := pg_catalog.split_part(pg_catalog.current_setting('app.actor_proof', true), '|', 11);
    RETURN NEW;
END
$$;

CREATE TRIGGER ledger_transactions_zz_deposit_tombstone_proof_guard BEFORE INSERT ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_transactions_deposit_tombstone_proof_guard();

-- =========================================================================
-- 4. MA020: payment_ref_cleared counts proof-bound tombstones only (0172 body + the marked line).
-- =========================================================================

CREATE OR REPLACE FUNCTION payment_ref_cleared(p_tenant uuid, p_provider text, p_ref text) RETURNS boolean AS $$
    SELECT p_ref IS NOT NULL AND p_ref <> '' AND
        EXISTS (SELECT 1 FROM ledger_transactions t
                 WHERE t.tenant_id = p_tenant AND t.provider_id = p_provider
                   AND t.transaction_type = 'tombstone' AND t.provider_tx_id = p_ref
                   AND t.actor_proof_nonce IS NOT NULL); -- 0175: proof-bound deposit tombstones only
$$ LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp;

COMMENT ON COLUMN ledger_transactions.actor_proof_nonce IS '0175 (F-1): nonce of the system actor proof (deposit_reversal_tombstone:write) a deposit reversal tombstone consumed; written only by the zz deposit tombstone proof guard.';
