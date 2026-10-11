-- ADR 0112 SLICE 3 (brand and tenant launch authorisation; sections 6.4, 12 items 9-12,
-- 13 slice 3): the database four-eyes control of migrations 0128/0129 stops trusting
-- session settings. Four things, and only these:
--
--   1. SIGNED ACTOR PROOF (ADR 0110 extension; closes residual S-3 / ADR 0112 12.9).
--      actor_proof_require() gains the operation family launch_authorisation:*
--      (request, suspend, cancel, approve, reject): provable only for scope 'platform'
--      (NULL tenant, the K1 encoding) or 'tenant'; approve / reject only for scope
--      'platform'; never platform_acting. Every other line of the verifier is 0124's.
--      zz_actor_proof_guard (the last BEFORE trigger, as on every governed table) is
--      installed on launch_authorisation_requests (INSERT, cancel; and a suspension is
--      executed only by its own requester) and launch_authorisation_approvals (INSERT).
--      Transitions and the subject status UPDATE are bound through their same-transaction
--      executing request (ADR 0112 6.4). The request digest binds tenant_id,
--      subject_kind, brand_id, action, reason_code and every other caller-supplied
--      content column including readiness_snapshot_hash; the operation is derived from
--      the row (':suspend' iff action = 'suspend'), so a proof signed for the other
--      operation is refused (AP004). The digest function is
--      launch_request_content_digest(); internal/actorproof computes the same value.
--   2. REJECT CARRY-OVER (ADR 0112 12.12 (i), slice-3 policy decision). A request that
--      repeats the (subject, action, payload_hash) of a request rejected within the
--      request TTL (72 h) is refused with LA014, so an approver cannot be "shopped" by
--      re-requesting identical content. Any change of content (a different
--      conditions_note, reason_code, readiness) is a different payload_hash and a
--      different request: the carry-over is deliberately exact, not fuzzy.
--   3. C-3(b) (ADR 0112 12.10): a deferred commit check refuses (LA030) a transaction
--      that executed a suspension and left a stale 'pending' request of the same
--      subject behind (the executor supersedes them in the same transaction).
--   4. SECURITY C2 (ADR 0112 13, inherited by slice 3): launch_subject_status_guard
--      (CREATE OR REPLACE of 0129's body; the one addition is marked C2) refuses a
--      TENANT status change in a transaction that already executed a governed status
--      change of one of that tenant's BRANDS (brand key X then tenant key X inverts the
--      tenant-then-brand order of gameplay placements; ADR 0082 / 0129 lock order).
--   5. CLOSURE BACKSTOP (security C1 / ledger-finance condition 2): the same function
--      refuses a closure (LA023) while a hold-bearing withdrawal request (requested,
--      pending_review, approved, submitted) exists for the tenant / brand, and (LA024) a
--      BRAND closure while an open sportsbook bet of that brand exists. A ledger-side arm
--      (non-zero player_withdrawal_hold balances) is NOT implemented: it needs a full
--      per-wallet ledger aggregate under the gate lock (ADR 0112 residual; invariant 12).
--
-- NUMBERING: 0130 (next free after 0129; confirmed against migrations/ and every local
-- branch at branch time). Numbers are allocated by the orchestrator.
--
-- SQLSTATE: LA014 (new) reject carry-over; AP004 for every proof binding mismatch (the
-- class of 0120). Every function pins search_path (ADR 0108); none is SECURITY DEFINER
-- (the verifier, 0120's, is the only definer on this path).

-- ---------------------------------------------------------------------------
-- 1a. The verifier (0124's body + the launch_authorisation:* family).
-- ---------------------------------------------------------------------------
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
    -- The NULL-tenant encoding (an EMPTY tenant field) exists only for scope
    -- 'platform' and only for the K1 capability-grant, financial-policy-change and
    -- (ADR 0112, migration 0130) launch_authorisation operations; every other scope
    -- and operation must carry a tenant.
    IF p_scope = 'platform' THEN
        IF p_tenant IS NOT NULL OR NOT (p_operation LIKE 'capability\_grant:%' OR p_operation LIKE 'financial\_policy\_change:%'
                                         OR p_operation LIKE 'launch\_authorisation:%') THEN
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

-- ---------------------------------------------------------------------------
-- 1b. The request digest (what the requester's proof binds; what the Go issuer
--     internal/actorproof computes). Caller-supplied content columns only: from_status,
--     to_status, required_approvals, payload_hash and the actor columns are forced by the
--     0128 guard and are bound through payload_hash on every later step. Arrays are
--     comma-joined in a locale-independent order (COLLATE "C"; uuid order is byte order).
-- ---------------------------------------------------------------------------
CREATE FUNCTION launch_request_content_digest(r launch_authorisation_requests) RETURNS text
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT public.k2_sha256_hex(public.k2_canonical(
        r.tenant_id::text, r.subject_kind, r.brand_id::text, r.action,
        r.licensing_model, r.licensing_status, r.licence_id::text,
        r.determination_reference, r.determination_by_role,
        r.responsible_operator_name, r.responsible_operator_registration,
        (SELECT string_agg(j::text, ',' ORDER BY j) FROM unnest(r.jurisdiction_ids) AS j),
        r.conditions_note, r.readiness_snapshot_hash,
        (SELECT string_agg(c, ',' ORDER BY c COLLATE "C") FROM unnest(r.acknowledged_check_codes) AS c),
        r.reason_code))
$$;

-- ---------------------------------------------------------------------------
-- 1c. zz_actor_proof_guard on the governed launch tables.
-- ---------------------------------------------------------------------------
CREATE FUNCTION actor_proof_launch_requests_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor RECORD;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO v_actor FROM public.launch_actor_session();
        IF NEW.requested_by IS DISTINCT FROM v_actor.actor OR NEW.requested_by_scope IS DISTINCT FROM v_actor.scope THEN
            RAISE EXCEPTION 'actor_proof: the requester columns are not the proven actor' USING ERRCODE = 'AP004';
        END IF;
        -- ':suspend' iff action = 'suspend' (the operation is derived from the row, so a proof
        -- signed for the other operation fails the binding). The id is server-forced, so the
        -- target is the literal 'new'; the digest binds the caller-supplied content columns.
        PERFORM public.actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
            CASE WHEN NEW.action = 'suspend' THEN 'launch_authorisation:suspend' ELSE 'launch_authorisation:request' END,
            'new', public.launch_request_content_digest(NEW));
    ELSIF OLD.status = 'pending' AND NEW.status = 'cancelled' THEN
        SELECT * INTO v_actor FROM public.launch_actor_session();
        IF v_actor.actor IS DISTINCT FROM OLD.requested_by OR v_actor.scope IS DISTINCT FROM OLD.requested_by_scope THEN
            RAISE EXCEPTION 'actor_proof: only the requester may cancel' USING ERRCODE = 'AP004';
        END IF;
        PERFORM public.actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
            'launch_authorisation:cancel', OLD.id::text, OLD.payload_hash);
    ELSIF OLD.status = 'pending' AND NEW.status = 'executing' AND OLD.action = 'suspend' THEN
        -- A suspension needs no approval, so its execution is bound to its (proven) requester:
        -- another session cannot drive a suspension it did not request.
        SELECT * INTO v_actor FROM public.launch_actor_session();
        IF v_actor.actor IS DISTINCT FROM OLD.requested_by OR v_actor.scope IS DISTINCT FROM OLD.requested_by_scope THEN
            RAISE EXCEPTION 'actor_proof: a suspension is executed only by its requester' USING ERRCODE = 'AP004';
        END IF;
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION actor_proof_launch_approvals_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor RECORD;
BEGIN
    SELECT * INTO v_actor FROM public.launch_actor_session();
    IF NEW.decided_by IS DISTINCT FROM v_actor.actor OR v_actor.scope IS DISTINCT FROM 'platform' THEN
        RAISE EXCEPTION 'actor_proof: the decided_by column is not the proven platform actor' USING ERRCODE = 'AP004';
    END IF;
    -- target = the request id; payload = the request's payload_hash (the 0128 guard already
    -- forced NEW.payload_hash to equal it, LA012).
    PERFORM public.actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
        'launch_authorisation:' || NEW.decision, NEW.request_id::text, NEW.payload_hash);
    RETURN NEW;
END
$$;

-- The 'zz_' prefix sorts after every other BEFORE trigger on these tables (a catalog test
-- pins it), so every 0128 guard's own refusal and error code is unchanged and the proof is
-- the final gate.
CREATE TRIGGER zz_actor_proof_guard
    BEFORE INSERT OR UPDATE ON launch_authorisation_requests
    FOR EACH ROW EXECUTE FUNCTION actor_proof_launch_requests_guard();
CREATE TRIGGER zz_actor_proof_guard
    BEFORE INSERT ON launch_authorisation_approvals
    FOR EACH ROW EXECUTE FUNCTION actor_proof_launch_approvals_guard();

-- ---------------------------------------------------------------------------
-- 2. Reject carry-over (LA014). Fires after launch_requests_guard (which forces
--    payload_hash) and before zz_actor_proof_guard (alphabetical order).
-- ---------------------------------------------------------------------------
CREATE FUNCTION launch_requests_reject_carryover() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.launch_authorisation_requests r
                WHERE r.tenant_id = NEW.tenant_id AND r.subject_kind = NEW.subject_kind
                  AND r.brand_id IS NOT DISTINCT FROM NEW.brand_id
                  AND r.action = NEW.action AND r.payload_hash = NEW.payload_hash
                  AND r.status = 'rejected' AND r.decided_at > now() - interval '72 hours') THEN
        RAISE EXCEPTION 'launch request: an identical request was rejected within the request TTL; it cannot be re-requested unchanged'
            USING ERRCODE = 'LA014';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER launch_requests_reject_carryover
    BEFORE INSERT ON launch_authorisation_requests
    FOR EACH ROW EXECUTE FUNCTION launch_requests_reject_carryover();

-- ---------------------------------------------------------------------------
-- 3. C-3(b): a suspension that reached 'executed' in this transaction leaves no stale
--    'pending' request of the same subject. Deferred (the executor supersedes after the
--    status UPDATE). Fail closed: an invisible own row refuses the commit.
-- ---------------------------------------------------------------------------
CREATE FUNCTION launch_suspension_no_stale_pending_at_commit() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_self  public.launch_authorisation_requests%ROWTYPE;
    v_stale bigint;
BEGIN
    SELECT * INTO v_self FROM public.launch_authorisation_requests r WHERE r.id = NEW.id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'launch suspension % is not visible at commit', NEW.id USING ERRCODE = 'LA030';
    END IF;
    IF v_self.status <> 'executed' THEN
        RETURN NULL;
    END IF;
    SELECT count(*) INTO v_stale
      FROM public.launch_authorisation_requests r
     WHERE r.id <> v_self.id AND r.tenant_id = v_self.tenant_id AND r.subject_kind = v_self.subject_kind
       AND r.brand_id IS NOT DISTINCT FROM v_self.brand_id
       AND r.status = 'pending' AND r.action <> 'suspend';
    IF v_stale > 0 THEN
        RAISE EXCEPTION 'launch suspension % left % stale pending request(s) of the same subject', v_self.id, v_stale
            USING ERRCODE = 'LA030';
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER launch_suspension_no_stale_pending_at_commit
    AFTER UPDATE ON launch_authorisation_requests
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    WHEN (NEW.action = 'suspend' AND NEW.status = 'executed' AND OLD.status IS DISTINCT FROM 'executed')
    EXECUTE FUNCTION launch_suspension_no_stale_pending_at_commit();

-- ---------------------------------------------------------------------------
-- 4. launch_subject_status_guard: 0129's body with one addition (C2).
-- ---------------------------------------------------------------------------
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
    v_mixed       bigint := 0;
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
    -- C2 (security, ADR 0112 13): one subject per executing transaction; a tenant status
    -- change after a governed status change of one of its brands in this transaction is
    -- refused (brand key then tenant key inverts the placement lock order).
    IF v_kind = 'tenant' THEN
        SELECT count(*) INTO v_mixed
          FROM public.launch_status_transitions t
         WHERE t.tenant_id = v_tenant AND t.subject_kind = 'brand'
           AND t.kind = 'governed' AND t.txid = txid_current() AND t.from_status <> t.to_status;
    END IF;
    IF v_prev_tenant IS NOT NULL THEN
        PERFORM set_config('app.tenant_id', v_prev_tenant, true);
    END IF;
    IF v_n <> 1 THEN
        RAISE EXCEPTION '% status change refused: no same-transaction governed transition of an executing request (ADR 0112)', TG_TABLE_NAME
            USING ERRCODE = 'LA020';
    END IF;
    IF v_mixed > 0 THEN
        RAISE EXCEPTION 'tenants status change refused: a brand status change of this tenant was already executed in this transaction (one subject per executing transaction)'
            USING ERRCODE = 'LA020';
    END IF;
    -- CLOSURE BACKSTOP (security C1 / ledger-finance condition 2; ADR 0112 invariant 11): a
    -- closure is refused while withdrawal requests that still hold the players' funds exist
    -- (LA023) and, for a brand, while open sportsbook bets of that brand exist (LA024; a
    -- tenant's own open bets are refused by tenants_status_change_gate, GP020). Both reads
    -- are RLS-bound, so app.tenant_id is bound to the subject's tenant for the read and
    -- restored (the 0121 technique; TestClosureBackstop_* fail if the read goes blind). The
    -- reads run under the gate lock this trigger already holds (brand key above; the tenant
    -- key is taken by tenants_status_change_gate in the same statement). A request inserted
    -- by a concurrent transaction that has not yet committed is not visible: the residual is
    -- recorded in ADR 0112 (withdrawal creation does not take the gate lock).
    IF NEW.status = 'closed' THEN
        DECLARE
            v_prev_t text := current_setting('app.tenant_id', true);
            v_prev_p text := current_setting('app.player_account_id', true);
            v_wd     bigint;
            v_bets   bigint := 0;
        BEGIN
            PERFORM set_config('app.tenant_id', v_tenant::text, true);
            PERFORM set_config('app.player_account_id', '', true);
            SELECT count(*) INTO v_wd FROM public.withdrawal_requests w
             WHERE w.tenant_id = v_tenant AND (v_brand IS NULL OR w.brand_id = v_brand)
               AND w.state IN ('requested', 'pending_review', 'approved', 'submitted');
            IF v_kind = 'brand' THEN
                SELECT count(*) INTO v_bets FROM public.sportsbook_bets b
                 WHERE b.tenant_id = v_tenant AND b.brand_id = v_brand AND b.status = 'open';
            END IF;
            PERFORM set_config('app.tenant_id', COALESCE(v_prev_t, ''), true);
            PERFORM set_config('app.player_account_id', COALESCE(v_prev_p, ''), true);
            IF v_wd > 0 THEN
                RAISE EXCEPTION '% closure refused: withdrawal requests still hold player funds', TG_TABLE_NAME
                    USING ERRCODE = 'LA023', DETAIL = format('hold_bearing_withdrawals=%s', v_wd);
            END IF;
            IF v_bets > 0 THEN
                RAISE EXCEPTION 'brand closure refused: open sportsbook bets exist'
                    USING ERRCODE = 'LA024', DETAIL = format('open_sportsbook_bets=%s', v_bets);
            END IF;
        END;
    END IF;
    RETURN NEW;
END
$$;
