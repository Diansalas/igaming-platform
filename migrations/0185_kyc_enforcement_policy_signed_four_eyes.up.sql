-- lic19 (S4, security review of lic10): the KYC enforcement policy writes become SIGNED, Person-distinct acts, enforced by the
-- DATABASE. Before this migration a platform_admin could create, activate, withdraw or supersede a policy with only the account-
-- level four-eyes of migrations 0100 / 0103 (the activating principal differs from the author's account); two staff accounts of
-- one Person satisfied it and no ADR 0110 proof was required. Software capability only: no policy value is shipped.
--
--   1. actor_proof_require is REPLACED (CREATE OR REPLACE; owner, SECURITY DEFINER, search_path and grants are kept) with the
--      CURRENT body (migration 0182; no later migration replaced it) plus ONE marked block and ONE allow-list term: the closed
--      operation family kyc_enforcement_policy:{create,activate,withdraw,supersede}, provable only for scope 'platform' (NULL
--      tenant). The down restores the 0182 body byte for byte.
--   2. kyc_enforcement_policies_enforce_lifecycle is REPLACED with the 0103 body (no later migration replaced it) plus ONE marked
--      block: the status change needs an active platform_admin session, both the acting administrator and the author linked to a
--      Person, the two Persons different, and a verified, consumed proof bound to (policy id, from, to, successor, reason code).
--      The reason code travels in the transaction-local setting app.kyc_policy_reason_code (single use; it is part of the signed
--      hash, so it cannot be swapped after signing).
--   3. A new AFTER INSERT trigger requires a verified proof bound to the whole row content for the DRAFT creation. It is AFTER so
--      that the existing row-level-security and CHECK refusals keep their own errors.
--
-- Fail closed: an account with no Person link cannot create, activate, withdraw or supersede (SQLSTATE KP002). The documented
-- remediation is the platform-staff person-link route (ADR 0119 S5); no bootstrap bypass exists for these writes. A policy
-- authored by an account that has no Person link can never be activated (author a new one).
--
-- SQLSTATE class KP (kyc policy): KP001 session / role, KP002 no Person link, KP003 same Person, KP004 reason code missing.

SET LOCAL lock_timeout = '5s';

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
    -- 0177 (licensing / jurisdiction maker-checker, MKT-DUAL-1): licensing_change:* is provable only for scope 'platform'
    -- (NULL tenant) or, for request / cancel only, scope 'tenant' (with a tenant). approve / reject / execute are platform
    -- only. The operation set is closed.
    IF p_operation LIKE 'licensing\_change:%' THEN
        IF p_operation NOT IN ('licensing_change:request', 'licensing_change:approve', 'licensing_change:reject',
                               'licensing_change:cancel', 'licensing_change:execute')
           OR p_scope NOT IN ('platform', 'tenant')
           OR (p_operation IN ('licensing_change:approve', 'licensing_change:reject', 'licensing_change:execute') AND p_scope <> 'platform') THEN
            RAISE EXCEPTION 'actor_proof: this licensing_change operation is not provable for this scope' USING ERRCODE = 'AP004';
        END IF;
    END IF;
    -- 0182 (withdrawal restriction policy, lic11): withdrawal_restriction_policy:* is provable only for scope 'platform'
    -- (NULL tenant). The operation set is closed.
    IF p_operation LIKE 'withdrawal\_restriction\_policy:%' THEN
        IF p_operation NOT IN ('withdrawal_restriction_policy:request', 'withdrawal_restriction_policy:approve',
                               'withdrawal_restriction_policy:reject', 'withdrawal_restriction_policy:cancel',
                               'withdrawal_restriction_policy:execute')
           OR p_scope <> 'platform' THEN
            RAISE EXCEPTION 'actor_proof: this withdrawal_restriction_policy operation is not provable for this scope' USING ERRCODE = 'AP004';
        END IF;
    END IF;
    -- 0185 (lic19, S4): kyc_enforcement_policy:* (create / activate / withdraw / supersede of a KYC enforcement policy) is
    -- provable only for scope 'platform' (NULL tenant). The operation set is closed.
    IF p_operation LIKE 'kyc\_enforcement\_policy:%' THEN
        IF p_operation NOT IN ('kyc_enforcement_policy:create', 'kyc_enforcement_policy:activate',
                               'kyc_enforcement_policy:withdraw', 'kyc_enforcement_policy:supersede')
           OR p_scope <> 'platform' THEN
            RAISE EXCEPTION 'actor_proof: this kyc_enforcement_policy operation is not provable for this scope' USING ERRCODE = 'AP004';
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
                                         OR p_operation LIKE 'staff\_lifecycle:%'
                                         OR p_operation LIKE 'licensing\_change:%'
                                         OR p_operation LIKE 'withdrawal\_restriction\_policy:%'
                                         OR p_operation LIKE 'kyc\_enforcement\_policy:%') THEN
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

CREATE OR REPLACE FUNCTION kyc_enforcement_policies_enforce_lifecycle() RETURNS TRIGGER AS $$
DECLARE
    acting_principal UUID;
    -- 0185 (lic19) BEGIN
    v_actor          record;
    v_author_person  uuid;
    v_reason         text;
    v_op             text;
    v_hash           text;
    -- 0185 (lic19) END
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'kyc_enforcement_policies is append-only: TRUNCATE is not permitted';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'kyc_enforcement_policies is append-only: DELETE is not permitted';
    END IF;
    IF (to_jsonb(NEW) - 'status' - 'superseded_by_policy_id') IS DISTINCT FROM (to_jsonb(OLD) - 'status' - 'superseded_by_policy_id') THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: only status and superseded_by_policy_id may change after insert';
    END IF;
    IF NOT (
        (OLD.status = 'draft' AND NEW.status IN ('active', 'withdrawn'))
        OR (OLD.status = 'active' AND NEW.status = 'withdrawn')
    ) THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: illegal status transition % -> %', OLD.status, NEW.status;
    END IF;
    IF NEW.status = 'active' AND NEW.trigger_type NOT IN ('cumulative_deposit', 'play') THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: trigger_type % is not yet evaluator-wired and may not be activated', NEW.trigger_type;
    END IF;
    -- Security F3 (ADR 0096 §17.3): superseded_by_policy_id may be set
    -- (NULL -> non-null) if and only if this is specifically the
    -- active -> withdrawn transition. It is immutable in every other
    -- direction (in particular, a draft row being withdrawn - never
    -- having been enforced - carries no successor requirement).
    IF NEW.superseded_by_policy_id IS DISTINCT FROM OLD.superseded_by_policy_id THEN
        IF NOT (OLD.status = 'active' AND NEW.status = 'withdrawn') THEN
            RAISE EXCEPTION 'kyc_enforcement_policies: superseded_by_policy_id may only be set on an active -> withdrawn transition';
        END IF;
        IF OLD.superseded_by_policy_id IS NOT NULL THEN
            RAISE EXCEPTION 'kyc_enforcement_policies: superseded_by_policy_id is already set and may not change';
        END IF;
    END IF;
    IF OLD.status = 'active' AND NEW.status = 'withdrawn' THEN
        IF NEW.superseded_by_policy_id IS NULL THEN
            RAISE EXCEPTION 'kyc_enforcement_policies: withdrawing an ACTIVE policy requires supersession - set superseded_by_policy_id to an approved, activating replacement in the same transaction (see kyc.SupersedeEnforcementPolicy)';
        END IF;
        IF NEW.superseded_by_policy_id = NEW.id THEN
            RAISE EXCEPTION 'kyc_enforcement_policies: a policy cannot supersede itself';
        END IF;
    END IF;
    acting_principal := NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid;
    IF acting_principal IS NULL THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: a platform-admin principal is required to change status';
    END IF;
    IF acting_principal = OLD.created_by_actor_id THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: four-eyes required - the activating/withdrawing principal must differ from the row''s creator';
    END IF;
    -- 0185 (lic19) BEGIN. S4: every status change is a SIGNED, Person-distinct act. The session must be an active platform_admin
    -- staff member that is linked to a Person (fail closed: an account without a Person link is refused; the documented way to
    -- link one is the platform-staff person-link route, ADR 0119 S5); the author of the row must also have a Person; the two
    -- Persons must differ (accounts compare below, Persons here); and an ADR 0110 proof bound to this exact transition and reason
    -- code must verify and is consumed in this transaction.
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF v_actor.scope IS DISTINCT FROM 'platform' OR v_actor.actor IS DISTINCT FROM acting_principal OR v_actor.role IS DISTINCT FROM 'platform_admin' THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: only an active platform_admin staff member may change the status' USING ERRCODE = 'KP001';
    END IF;
    IF v_actor.person_id IS NULL THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: the acting administrator is not linked to a Person (four-eyes cannot be established)' USING ERRCODE = 'KP002';
    END IF;
    SELECT su.person_id INTO v_author_person FROM public.staff_users su WHERE su.id = OLD.created_by_actor_id;
    IF v_author_person IS NULL THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: the policy author has no Person link (four-eyes cannot be established); author a new policy' USING ERRCODE = 'KP002';
    END IF;
    IF v_author_person = v_actor.person_id THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: four-eyes required - the activating/withdrawing Person must differ from the Person who authored the row' USING ERRCODE = 'KP003';
    END IF;
    v_reason := NULLIF(pg_catalog.current_setting('app.kyc_policy_reason_code', true), '');
    IF v_reason IS NULL OR v_reason !~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$' THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: a reason code is required for a status change' USING ERRCODE = 'KP004';
    END IF;
    PERFORM pg_catalog.set_config('app.kyc_policy_reason_code', '', true);
    v_op := CASE WHEN NEW.status = 'active' THEN 'kyc_enforcement_policy:activate'
                 WHEN OLD.status = 'draft' THEN 'kyc_enforcement_policy:withdraw'
                 ELSE 'kyc_enforcement_policy:supersede' END;
    v_hash := public.k2_sha256_hex(public.k2_canonical(OLD.id::text, OLD.status, NEW.status, NEW.superseded_by_policy_id::text, v_reason));
    PERFORM public.actor_proof_require(v_actor.actor, 'platform', NULL, v_op, OLD.id::text, v_hash);
    -- 0185 (lic19) END
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE FUNCTION kyc_enforcement_policies_require_create_proof() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor record;
    v_hash  text;
BEGIN
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF v_actor.scope IS DISTINCT FROM 'platform' OR v_actor.role IS DISTINCT FROM 'platform_admin'
       OR v_actor.actor IS DISTINCT FROM NEW.created_by_actor_id THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: only an active platform_admin staff member may author a policy' USING ERRCODE = 'KP001';
    END IF;
    IF v_actor.person_id IS NULL THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: the authoring administrator is not linked to a Person (four-eyes cannot be established)' USING ERRCODE = 'KP002';
    END IF;
    v_hash := public.k2_sha256_hex(public.k2_canonical(NEW.licensing_jurisdiction_id::text, NEW.trigger_type, NEW.threshold_minor_units::text,
        NEW.asset_code, NEW.required_tier, NEW.play_operation, NEW.legal_review_reference, NEW.reason_code));
    PERFORM public.actor_proof_require(v_actor.actor, 'platform', NULL, 'kyc_enforcement_policy:create', 'new', v_hash);
    RETURN NEW;
END
$$;

CREATE TRIGGER kyc_enforcement_policies_require_create_proof
    AFTER INSERT ON kyc_enforcement_policies
    FOR EACH ROW EXECUTE FUNCTION kyc_enforcement_policies_require_create_proof();

REVOKE ALL ON FUNCTION kyc_enforcement_policies_require_create_proof() FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        REVOKE ALL ON FUNCTION kyc_enforcement_policies_require_create_proof() FROM igaming_runtime;
    END IF;
END
$$;
