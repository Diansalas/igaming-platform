-- PRH-2 R5 (SIGNED-ACTOR-PROOF; ADR 0110; owner decisions
-- THREAT-MODEL-ARBITRARY-SQL-1 = YES and SIGNED-ACTOR-PROOF = AUTHORIZED,
-- docs/governance/task-registry.md DECISIONS-PRH2-CLEARANCE-2026-10-05 and
-- DECISIONS-PRH2-FINAL-GAMEPLAY-SECURITY-2026-10-06).
--
-- THREAT. A compromised or stolen igaming_runtime credential (arbitrary SQL).
-- The four-eyes actor identity (financial_actor_session(), 0112) is derived
-- ONLY from transaction-local GUCs that any runtime session can set_config()
-- (app.tenant_id, app.principal_id, app.platform_admin_principal_id,
-- app.acting_*). The functions check that the named principal EXISTS as an
-- active staff_users row, never that the CALLER IS that person, so a session
-- with arbitrary SQL could approve a K2 ledger adjustment / K3 manual payment
-- resolution as two different real admins.
--
-- MECHANISM (candidate A of the security design; nothing wider). The
-- application server, AFTER it has authenticated the principal (verified JWT)
-- and authorised the governed action, signs a short-lived proof binding
--     kid | actor | scope | tenant | operation | target | payload_hash |
--     iat | exp | nonce
-- with HMAC-SHA256 under a key that is NEVER readable by igaming_runtime. The
-- proof travels in the transaction-local GUC app.actor_proof. An OWNER-owned
-- SECURITY DEFINER verifier (actor_proof_require) recomputes the MAC from an
-- OWNER-ONLY key table, checks the time window (exp - iat <= 60 s), checks the
-- binding against the actor the session GUCs resolve to and against the
-- operation/target/payload of the row being written, and consumes the nonce in
-- an OWNER-ONLY table under a UNIQUE constraint (replay protection). The
-- governed tables then carry a LAST-firing BEFORE trigger that calls it; no
-- valid proof -> the write fails closed.
--
-- WHAT IS PROTECTED (exactly): INSERT into ledger_adjustment_requests,
-- ledger_adjustment_approvals, payment_manual_resolutions and
-- payment_manual_resolution_approvals (the six actor-identity guards of
-- 0113/0115), plus the initiator's/requester's CANCEL of a pending
-- request/resolution; and, extending the SAME mechanism (owner decision, review
-- finding H2), the K1 capability-grant and financial-policy-change flows:
-- staff_capability_grant_requests (INSERT, cancel; and a request may be marked
-- expired only when it actually has), staff_capability_grant_approvals (INSERT),
-- staff_capability_grants (the revoke UPDATE; the INSERT is bound through its
-- same-transaction approval), financial_approval_policy_changes (INSERT, cancel)
-- and financial_approval_policy_change_approvals (INSERT). These flows run in the
-- plain 'platform' scope with a NULL tenant, encoded as an EMPTY tenant field,
-- valid only for scope 'platform' and only for the capability_grant: and
-- financial_policy_change: operations. Every zz function also asserts that the
-- actor column the earlier guard forced equals the proven actor. Ordinary
-- non-governed posting paths and every other GUC-derived check are NOT
-- protected by this migration (ADR 0110 residuals).
--
-- KEYS. actor_proof_keys holds >= 1 ACTIVE keys (kid, secret). Rotation =
-- insert the new key, roll the app over to the new kid, retire the old kid
-- after the proof lifetime has passed (runbook, ADR 0110). The key material is
-- never committed: this migration creates the EMPTY table; keys are
-- provisioned by the owner/migration role out of band.
--
-- ACCESS. Both new tables are owned by the migration role, have ALL privileges
-- revoked from PUBLIC and from igaming_runtime and ENABLE ROW LEVEL SECURITY
-- with NO policy (a second, independent denial should a default privilege ever
-- re-grant them). They are deliberately NOT FORCE RLS: the SECURITY DEFINER
-- verifier runs as the owner and must read/write them. igaming_runtime
-- receives EXECUTE on actor_proof_require / actor_proof_key_active only.
--
-- SECURITY DEFINER hardening (the repo's FIRST definer functions): search_path
-- is pinned to pg_catalog, public, pg_temp; igaming_runtime cannot CREATE in
-- public (PG15+ default) and cannot create TEMP objects (0116); every table
-- reference is schema-qualified; pgcrypto's hmac is referenced as public.hmac.
--
-- ERROR CODES (class AP): AP001 proof missing/malformed, AP002 unknown/inactive
-- key or bad MAC, AP003 expired / not yet valid / lifetime too long, AP004
-- binding mismatch (actor/scope/tenant/operation/target/payload), AP005 nonce
-- replayed.

DO $$
BEGIN
    IF to_regprocedure('public.hmac(bytea, bytea, text)') IS NULL THEN
        RAISE EXCEPTION 'migration 0120 requires pgcrypto (public.hmac(bytea, bytea, text)); it is created by migration 0001';
    END IF;
END;
$$;

CREATE TABLE actor_proof_keys (
    kid        TEXT PRIMARY KEY CHECK (kid ~ '^[A-Za-z0-9._-]{1,32}$'),
    secret     BYTEA NOT NULL CHECK (octet_length(secret) >= 32),
    status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'retired')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    retired_at TIMESTAMPTZ NULL,
    CHECK ((status = 'retired') = (retired_at IS NOT NULL))
);

CREATE TABLE actor_proof_nonces (
    nonce         TEXT PRIMARY KEY CHECK (nonce ~ '^[A-Za-z0-9_-]{16,64}$'),
    kid           TEXT NOT NULL,
    actor         UUID NOT NULL,
    scope         TEXT NOT NULL,
    tenant        UUID NULL,
    operation     TEXT NOT NULL,
    target        TEXT NOT NULL,
    payload_hash  TEXT NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL,
    consumed_txid BIGINT NOT NULL,
    consumed_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX actor_proof_nonces_expires ON actor_proof_nonces (expires_at);

REVOKE ALL ON actor_proof_keys, actor_proof_nonces FROM PUBLIC;
ALTER TABLE actor_proof_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE actor_proof_nonces ENABLE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        REVOKE ALL ON actor_proof_keys, actor_proof_nonces FROM igaming_runtime;
    END IF;
END;
$$;

-- -------------------------------------------------------------------------
-- The verifier. SECURITY DEFINER (runs as the table owner). Returns void; it
-- mints nothing and discloses nothing beyond the error class.
-- -------------------------------------------------------------------------
CREATE FUNCTION actor_proof_require(
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

-- Startup/health probe: is this kid an ACTIVE key? Discloses no key material.
CREATE FUNCTION actor_proof_key_active(p_kid text) RETURNS boolean AS $$
    SELECT EXISTS (SELECT 1 FROM public.actor_proof_keys k WHERE k.kid = p_kid AND k.status = 'active');
$$ LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp;

REVOKE ALL ON FUNCTION actor_proof_require(uuid, text, uuid, text, text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION actor_proof_key_active(text) FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        GRANT EXECUTE ON FUNCTION actor_proof_require(uuid, text, uuid, text, text, text) TO igaming_runtime;
        GRANT EXECUTE ON FUNCTION actor_proof_key_active(text) TO igaming_runtime;
    END IF;
END;
$$;

-- -------------------------------------------------------------------------
-- The governed-table triggers. They fire LAST (the 'zz_' prefix sorts after
-- every existing BEFORE trigger on these tables; a catalog test pins that), so
-- every existing guard's own refusal and error code is unchanged and the proof
-- is the final gate. These functions are plain invoker functions: the actor is
-- resolved exactly as the existing guards resolve it (financial_actor_session()),
-- and only the proof check itself crosses into the definer. Every function also
-- asserts that the actor column the earlier guard FORCED (initiated_by /
-- decided_by / requested_by / revoked_by) equals the PROVEN actor.
--
-- Digests. A create's payload hash is SHA-256(k2_canonical(caller-supplied
-- columns)); the Go issuer (internal/actorproof.Digest) computes the same value
-- from the same inputs. Timestamps are canonical UTC text with microseconds.
-- -------------------------------------------------------------------------

CREATE FUNCTION actor_proof_ts(p timestamptz) RETURNS text AS $$
    SELECT pg_catalog.to_char(p AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"');
$$ LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp;

-- The K1 request's content, as an approver reviews it (target of an approval's
-- payload binding). Read by the Go issuer through actor_proof_k1_request_digest
-- so both sides always agree.
CREATE FUNCTION actor_proof_k1_request_digest(p_request uuid) RETURNS text AS $$
    SELECT k2_sha256_hex(k2_canonical(r.id::text, r.tenant_id::text, r.grantee_staff_id::text, r.capability,
               actor_proof_ts(r.valid_from), actor_proof_ts(r.valid_until), r.reason_code))
      FROM staff_capability_grant_requests r WHERE r.id = p_request;
$$ LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp;

-- ---- K2 -----------------------------------------------------------------

CREATE FUNCTION actor_proof_ledger_adjustment_requests_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor RECORD;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO v_actor FROM financial_actor_session();
        IF NEW.initiated_by IS DISTINCT FROM v_actor.actor THEN
            RAISE EXCEPTION 'actor_proof: the initiator column is not the proven actor' USING ERRCODE = 'AP004';
        END IF;
        PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
            'ledger_adjustment:initiate', NEW.id::text,
            k2_sha256_hex(k2_canonical(NEW.tenant_id::text, NEW.wallet_id::text, NEW.asset_code, NEW.direction,
                NEW.amount::text, NEW.reason_code, NEW.causation_transaction_id::text, NEW.evidence_ref_hash, NEW.note_hash)));
    ELSIF OLD.state = 'pending' AND NEW.state = 'cancelled' THEN
        SELECT * INTO v_actor FROM financial_actor_session();
        IF v_actor.actor IS DISTINCT FROM OLD.initiated_by THEN
            RAISE EXCEPTION 'actor_proof: only the initiator may cancel' USING ERRCODE = 'AP004';
        END IF;
        PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
            'ledger_adjustment:cancel', OLD.id::text, OLD.payload_hash);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE FUNCTION actor_proof_ledger_adjustment_approvals_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor RECORD;
BEGIN
    SELECT * INTO v_actor FROM financial_actor_session();
    IF NEW.decided_by IS DISTINCT FROM v_actor.actor THEN
        RAISE EXCEPTION 'actor_proof: the decided_by column is not the proven actor' USING ERRCODE = 'AP004';
    END IF;
    PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
        'ledger_adjustment:' || NEW.decision, NEW.request_id::text, NEW.payload_hash);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

-- ---- K3 -----------------------------------------------------------------

CREATE FUNCTION actor_proof_payment_manual_resolutions_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor RECORD;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO v_actor FROM financial_actor_session();
        IF NEW.requested_by IS DISTINCT FROM v_actor.actor THEN
            RAISE EXCEPTION 'actor_proof: the requested_by column is not the proven actor' USING ERRCODE = 'AP004';
        END IF;
        -- The id is server-forced by the 0115 guard, so the target is the
        -- literal 'new'; the digest binds the caller-supplied payload columns.
        PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
            'payment_force_resolve:request', 'new',
            k2_sha256_hex(k2_canonical(NEW.tenant_id::text, NEW.attempt_id::text, NEW.kind, NEW.finding_code,
                NEW.basis_code, NEW.context_code, NEW.evidence_ref_hash, NEW.reason_code)));
    ELSIF OLD.state = 'pending' AND NEW.state = 'cancelled' THEN
        SELECT * INTO v_actor FROM financial_actor_session();
        IF v_actor.actor IS DISTINCT FROM OLD.requested_by THEN
            RAISE EXCEPTION 'actor_proof: only the requester may cancel' USING ERRCODE = 'AP004';
        END IF;
        PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
            'payment_force_resolve:cancel', OLD.id::text, OLD.payload_hash);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE FUNCTION actor_proof_payment_manual_resolution_approvals_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor RECORD;
BEGIN
    SELECT * INTO v_actor FROM financial_actor_session();
    IF NEW.decided_by IS DISTINCT FROM v_actor.actor THEN
        RAISE EXCEPTION 'actor_proof: the decided_by column is not the proven actor' USING ERRCODE = 'AP004';
    END IF;
    PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
        'payment_force_resolve:' || NEW.decision, NEW.resolution_id::text, NEW.payload_hash);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

-- ---- K1 capability grants (scope 'tenant' or 'platform'; NULL tenant for platform) ----

CREATE FUNCTION actor_proof_staff_capability_grant_requests_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor RECORD;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO v_actor FROM financial_actor_session();
        IF NEW.requested_by IS DISTINCT FROM v_actor.actor THEN
            RAISE EXCEPTION 'actor_proof: the requested_by column is not the proven actor' USING ERRCODE = 'AP004';
        END IF;
        PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
            'capability_grant:request', 'new',
            k2_sha256_hex(k2_canonical(NEW.tenant_id::text, NEW.grantee_staff_id::text, NEW.capability,
                actor_proof_ts(NEW.valid_from), actor_proof_ts(NEW.valid_until), NEW.reason_code)));
    ELSIF NEW.status = 'cancelled' THEN
        SELECT * INTO v_actor FROM financial_actor_session();
        -- Closes the 0112 branch that let ANY actor in scope cancel: initiator only.
        IF v_actor.actor IS DISTINCT FROM OLD.requested_by THEN
            RAISE EXCEPTION 'staff_capability_grant_requests: only the requester may cancel a request' USING ERRCODE = 'CG010';
        END IF;
        PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
            'capability_grant:cancel', OLD.id::text,
            k2_sha256_hex(k2_canonical(OLD.id::text, OLD.tenant_id::text, OLD.grantee_staff_id::text, OLD.capability,
                actor_proof_ts(OLD.valid_from), actor_proof_ts(OLD.valid_until), OLD.reason_code)));
    ELSIF NEW.status = 'expired' THEN
        -- Closes the 0112 branch that let a request be marked expired before it
        -- actually expired.
        IF pg_catalog.now() < OLD.expires_at THEN
            RAISE EXCEPTION 'staff_capability_grant_requests: the request has not expired' USING ERRCODE = 'CG010';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE FUNCTION actor_proof_staff_capability_grant_approvals_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor RECORD;
BEGIN
    SELECT * INTO v_actor FROM financial_actor_session();
    IF NEW.decided_by IS DISTINCT FROM v_actor.actor THEN
        RAISE EXCEPTION 'actor_proof: the decided_by column is not the proven actor' USING ERRCODE = 'AP004';
    END IF;
    -- The approver's payload binding is the request's own content, read here
    -- and by the Go issuer through the same function.
    PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
        'capability_grant:' || NEW.decision, NEW.request_id::text,
        COALESCE(actor_proof_k1_request_digest(NEW.request_id), repeat('0', 64)));
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE FUNCTION actor_proof_staff_capability_grants_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor RECORD;
BEGIN
    -- INSERT is bound through the same-transaction approval (which carries a
    -- proof); only the revoke UPDATE is actor-bound here.
    IF TG_OP = 'UPDATE' AND OLD.revoked_at IS NULL AND NEW.revoked_at IS NOT NULL THEN
        SELECT * INTO v_actor FROM financial_actor_session();
        IF NEW.revoked_by IS DISTINCT FROM v_actor.actor THEN
            RAISE EXCEPTION 'actor_proof: the revoked_by column is not the proven actor' USING ERRCODE = 'AP004';
        END IF;
        PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
            'capability_grant:revoke', OLD.id::text,
            k2_sha256_hex(k2_canonical(OLD.id::text, OLD.tenant_id::text, NEW.revoke_reason_code)));
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

-- ---- financial policy changes (scope 'tenant' or 'platform') ----

CREATE FUNCTION actor_proof_financial_approval_policy_changes_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor RECORD;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO v_actor FROM financial_actor_session();
        IF NEW.requested_by IS DISTINCT FROM v_actor.actor THEN
            RAISE EXCEPTION 'actor_proof: the requested_by column is not the proven actor' USING ERRCODE = 'AP004';
        END IF;
        PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
            'financial_policy_change:propose', 'new',
            k2_sha256_hex(k2_canonical(NEW.change_kind, NEW.operation_kind, NEW.level, NEW.tenant_id::text, NEW.brand_id::text,
                NEW.jurisdiction_id::text, NEW.profile_code, NEW.asset_code, NEW.base_required_approvals::text,
                NEW.threshold_minor_units::text, NEW.required_approvals_above_threshold::text,
                actor_proof_ts(NEW.effective_from), NEW.legal_review_reference)));
    ELSIF OLD.status = 'pending' AND NEW.status = 'cancelled' THEN
        SELECT * INTO v_actor FROM financial_actor_session();
        IF v_actor.actor IS DISTINCT FROM OLD.requested_by THEN
            RAISE EXCEPTION 'actor_proof: only the requester may cancel' USING ERRCODE = 'AP004';
        END IF;
        PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
            'financial_policy_change:cancel', OLD.id::text, OLD.content_hash);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE FUNCTION actor_proof_financial_approval_policy_change_approvals_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor RECORD;
BEGIN
    SELECT * INTO v_actor FROM financial_actor_session();
    IF NEW.decided_by IS DISTINCT FROM v_actor.actor THEN
        RAISE EXCEPTION 'actor_proof: the decided_by column is not the proven actor' USING ERRCODE = 'AP004';
    END IF;
    PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
        'financial_policy_change:' || NEW.decision, NEW.change_id::text, NEW.content_hash);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER zz_actor_proof_guard
    BEFORE INSERT OR UPDATE ON ledger_adjustment_requests
    FOR EACH ROW EXECUTE FUNCTION actor_proof_ledger_adjustment_requests_guard();
CREATE TRIGGER zz_actor_proof_guard
    BEFORE INSERT ON ledger_adjustment_approvals
    FOR EACH ROW EXECUTE FUNCTION actor_proof_ledger_adjustment_approvals_guard();
CREATE TRIGGER zz_actor_proof_guard
    BEFORE INSERT OR UPDATE ON payment_manual_resolutions
    FOR EACH ROW EXECUTE FUNCTION actor_proof_payment_manual_resolutions_guard();
CREATE TRIGGER zz_actor_proof_guard
    BEFORE INSERT ON payment_manual_resolution_approvals
    FOR EACH ROW EXECUTE FUNCTION actor_proof_payment_manual_resolution_approvals_guard();
CREATE TRIGGER zz_actor_proof_guard
    BEFORE INSERT OR UPDATE ON staff_capability_grant_requests
    FOR EACH ROW EXECUTE FUNCTION actor_proof_staff_capability_grant_requests_guard();
CREATE TRIGGER zz_actor_proof_guard
    BEFORE INSERT ON staff_capability_grant_approvals
    FOR EACH ROW EXECUTE FUNCTION actor_proof_staff_capability_grant_approvals_guard();
CREATE TRIGGER zz_actor_proof_guard
    BEFORE UPDATE ON staff_capability_grants
    FOR EACH ROW EXECUTE FUNCTION actor_proof_staff_capability_grants_guard();
CREATE TRIGGER zz_actor_proof_guard
    BEFORE INSERT OR UPDATE ON financial_approval_policy_changes
    FOR EACH ROW EXECUTE FUNCTION actor_proof_financial_approval_policy_changes_guard();
CREATE TRIGGER zz_actor_proof_guard
    BEFORE INSERT ON financial_approval_policy_change_approvals
    FOR EACH ROW EXECUTE FUNCTION actor_proof_financial_approval_policy_change_approvals_guard();
