-- 0182: governed per-tenant WITHDRAWAL RESTRICTION POLICY (lic11; owner instruction "do not hard-code that withdrawals continue
-- under suspension without an approved policy decision"). MONEY-PATH + GOVERNED CONFIGURATION: FOR `ledger-finance`,
-- `security`, `architect`, `code-reviewer`, `qa` REVIEW BEFORE MERGE. Analysis: docs/plans/lic11-withdrawal-restriction-analysis.md;
-- record and proposed ADR / decision-register text: docs/plans/lic11-withdrawal-restriction-record.md.
--
-- NUMBERING: 0182 (assigned by the orchestrator). 0180 (operating bases) and 0181 (AML / residence / KYC) are parallel slices.
-- MERGE NOTE: this migration REPLACES actor_proof_require (CREATE OR REPLACE) with the 0177 body plus one marked block and one
-- allow-list term. If 0180 or 0181 also replaces it, the migration that lands LAST must carry every added block (the 0176 /
-- 0177 convention). This migration does NOT touch licensing_change_kinds, licensing_change_classify,
-- licensing_change_row_matches, operating-market objects or any launchgov object.
--
-- WHAT THIS ADDS
--   1. withdrawal_restriction_causes: the CLOSED cause registry (migration-written, read-only). Configurable causes carry the
--      INTERIM ENGINEERING DEFAULT per stage, which is EXACTLY the behaviour before this migration (see section 1). Causes
--      governed by an approved owner decision (tenant / brand status: ADR 0095 section 44 decisions 13-18) or not evaluable on
--      a withdrawal (a market / country closed for the tenant: Q19, no per-withdrawal market determination) are listed with
--      configurable = false: no policy row can be written for them.
--   2. withdrawal_restriction_policies: append-only, versioned (per-tenant set_version, assigned here under the tenant status
--      gate key taken EXCLUSIVE), FORCE RLS; a tenant reads its own rows, only a platform session writes, and a non-owner
--      write needs an EXECUTED change request of THIS transaction with the same content (WR040).
--   3. withdrawal_restriction_policy_changes / _approvals: the maker-checker (mirrors the 0177 licensing primitive: the
--      session actor function licensing_change_actor() is REUSED unchanged; widening = 2 approvals by distinct Persons other
--      than the requester's, at most 24 h old at execution, 72 h expiry; narrowing = single platform_admin with a reason code,
--      executed in the request's own transaction; every move proof-bound, ADR 0110, operation family
--      withdrawal_restriction_policy:{request,approve,reject,cancel,execute}, platform scope only).
--      WIDENING = the requested action is LESS strict (allow < hold_for_review < block) than the effective action at ANY stage
--      (re-classified at execution under the tenant status gate key: WR024).
--   4. withdrawal_restriction_effective(tenant, cause, stage): the single resolution of the effective action (configured row,
--      else the registry default) with its provenance (source, policy id, set version).
--   5. withdrawal_gate_decisions: gate code WD056 (restriction policy refusal at a dispatch point) admitted by the outcome
--      CHECK, and four provenance columns (restriction_policy_source / _set_version / _cause / _action) written by the
--      application for every new decision. The F4 triggers, WD052 / WD053 and the 0171 signed allow are UNCHANGED.
--   6. actor_proof_require: the 0177 body plus the withdrawal_restriction_policy block.
--
-- WHAT THIS DOES NOT DO: it ships NO policy row and no legal value; it changes no existing guard (0131 WD010/WD011, 0124 HR050,
-- 0169 WD052/WD053, 0171) and no tenant / brand behaviour; it adds no ledger object and moves no money. The licence-cause
-- evaluation is application-level (single licence-validity implementation, INV-M-5): no SQL backstop (record residual R-1).
--
-- SQLSTATEs (class WR, unused before): WR001 session / actor; WR002 cause unknown or not configurable; WR010 invalid input;
-- WR012 content immutable; WR013 illegal state move or expired; WR020 approvals not satisfied; WR021 single-actor change not
-- executed by its requester in its own transaction; WR022 the requester's Person may not decide; WR023 only the requester
-- cancels; WR024 re-classified as widening at execution; WR030 approver not eligible; WR031 request not open; WR032 four-eyes;
-- WR040 policy written outside an executed change of this transaction; WR041 provenance shape; WR055 append-only;
-- WR095 down refused; WR098 pre-flight. The open-request unique index raises 23505.
-- Every function pins search_path (ADR 0108); none but the replaced verifier is SECURITY DEFINER. No role, password or
-- attribute changes; grants to igaming_runtime only (mirrored in deploy/init-app-role.sql).

SET LOCAL lock_timeout = '5s';

-- =========================================================================
-- 0. Pre-flight
-- =========================================================================
DO $$
BEGIN
    IF to_regclass('public.withdrawal_restriction_causes') IS NOT NULL OR to_regclass('public.withdrawal_restriction_policies') IS NOT NULL
       OR to_regclass('public.withdrawal_restriction_policy_changes') IS NOT NULL
       OR to_regclass('public.withdrawal_restriction_policy_approvals') IS NOT NULL THEN
        RAISE EXCEPTION 'migration 0182 pre-flight: a withdrawal_restriction table already exists' USING ERRCODE = 'WR098';
    END IF;
    IF to_regclass('public.withdrawal_gate_decisions') IS NULL OR to_regprocedure('public.licensing_change_actor()') IS NULL
       OR to_regprocedure('public.tenant_status_gate_key(uuid)') IS NULL THEN
        RAISE EXCEPTION 'migration 0182 pre-flight: 0169 / 0177 / 0118 objects are required' USING ERRCODE = 'WR098';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'withdrawal_gate_decisions_outcome_shape'
                    AND conrelid = 'public.withdrawal_gate_decisions'::regclass) THEN
        RAISE EXCEPTION 'migration 0182 pre-flight: withdrawal_gate_decisions_outcome_shape (0169) is missing' USING ERRCODE = 'WR098';
    END IF;
END
$$;

-- =========================================================================
-- 1. The cause registry (closed; migration-written)
-- =========================================================================
CREATE TABLE withdrawal_restriction_causes (
    cause                TEXT PRIMARY KEY CHECK (cause ~ '^[a-z][a-z_]{2,63}$'),
    cause_class          TEXT NOT NULL CHECK (cause_class IN ('licence', 'subject_status', 'market')),
    configurable         BOOLEAN NOT NULL,
    evaluated            BOOLEAN NOT NULL,
    default_request      TEXT NULL CHECK (default_request IS NULL OR default_request IN ('allow', 'hold_for_review', 'block')),
    default_dispatch     TEXT NULL CHECK (default_dispatch IS NULL OR default_dispatch IN ('allow', 'hold_for_review', 'block')),
    default_redispatch   TEXT NULL CHECK (default_redispatch IS NULL OR default_redispatch IN ('allow', 'hold_for_review', 'block')),
    governing_reference  TEXT NOT NULL CHECK (btrim(governing_reference) <> ''),
    current_behaviour    TEXT NOT NULL CHECK (btrim(current_behaviour) <> ''),
    CONSTRAINT withdrawal_restriction_causes_defaults CHECK (
        configurable = (default_request IS NOT NULL AND default_dispatch IS NOT NULL AND default_redispatch IS NOT NULL)),
    CONSTRAINT withdrawal_restriction_causes_configurable_evaluated CHECK (NOT configurable OR evaluated)
);
COMMENT ON TABLE withdrawal_restriction_causes IS '0182: CLOSED registry of withdrawal restriction causes. Written by migrations only. default_* of a configurable cause is the INTERIM ENGINEERING DEFAULT and equals the behaviour before 0182 (pending owner decision Q12); it is not a legal value. A non-configurable cause is governed elsewhere (governing_reference) and accepts no policy row.';

INSERT INTO withdrawal_restriction_causes (cause, cause_class, configurable, evaluated, default_request, default_dispatch, default_redispatch, governing_reference, current_behaviour) VALUES
    ('licence_suspended', 'licence', true, true, 'allow', 'allow', 'allow',
     'INTERIM ENGINEERING DEFAULT (owner decision Q12 pending): behaviour before migration 0182',
     'The tenant''s bound licence has status suspended. Default: withdrawal request, payout claim and re-dispatch proceed.'),
    ('licence_expired', 'licence', true, true, 'allow', 'allow', 'allow',
     'INTERIM ENGINEERING DEFAULT (owner decision Q12 pending): behaviour before migration 0182',
     'The bound licence has status expired or its expiry date has passed (jurisdiction.EvaluateLicenceValidity). Default: proceed.'),
    ('licence_not_yet_issued', 'licence', true, true, 'allow', 'allow', 'allow',
     'INTERIM ENGINEERING DEFAULT (owner decision Q12 pending): behaviour before migration 0182',
     'The bound licence''s issue date is in the future. Default: proceed.'),
    ('licence_missing', 'licence', true, true, 'allow', 'allow', 'allow',
     'INTERIM ENGINEERING DEFAULT (owner decision Q12 pending): behaviour before migration 0182',
     'No licence is bound to the tenant, or the bound licence row is not readable. Default: proceed.'),
    ('tenant_suspended', 'subject_status', false, false, NULL, NULL, NULL,
     'ADR 0095 section 44 decisions 13-18 (owner, 2026-10-08); H-SEC-11; migrations 0131 (WD010), 0124 (HR050)',
     'Request refused; payout claim refused (request stays approved, hold frozen; release only by the four-eyes governed hold resolution); re-dispatch deferred until reactivation.'),
    ('tenant_closed', 'subject_status', false, false, NULL, NULL, NULL,
     'ADR 0095 section 44 decisions 13-18 (owner, 2026-10-08); ADR 0107 HD-CTF-4 open; migrations 0131, 0124, 0130/0133',
     'As tenant_suspended; closure is refused while hold-bearing withdrawals exist (LA023 / LA025).'),
    ('brand_suspended', 'subject_status', false, false, NULL, NULL, NULL,
     'ADR 0095 section 44 decisions 13-18 and 19-23 (owner, 2026-10-08); H-SEC-11; migrations 0131 (WD011), 0124 (HR050)',
     'Request refused; payout claim refused (request stays approved, hold frozen); payout re-dispatch is not gated.'),
    ('brand_closed', 'subject_status', false, false, NULL, NULL, NULL,
     'ADR 0095 section 44 decisions 13-18 (owner, 2026-10-08); migrations 0131, 0124, 0130/0133',
     'As brand_suspended.'),
    ('market_closed', 'market', false, false, NULL, NULL, NULL,
     'Q19 (L-11) open; no per-withdrawal market determination exists (Q18 / L-4)',
     'Not evaluated on any withdrawal path: a withdrawal proceeds whatever the player''s market state.');

ALTER TABLE withdrawal_restriction_causes ENABLE ROW LEVEL SECURITY;
ALTER TABLE withdrawal_restriction_causes FORCE ROW LEVEL SECURITY;
-- Readable by a platform session or any tenant session (the money paths, staff); never by a player or an acting session.
CREATE POLICY withdrawal_restriction_causes_read ON withdrawal_restriction_causes FOR SELECT USING (
    (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
     OR NULLIF(current_setting('app.tenant_id', true), '') IS NOT NULL)
    AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    AND NOT financial_acting_gucs_present()
);
-- ADR 0099 acting fence (the 0177 licensing_change_kinds pattern): an acting session never reads the registry.
CREATE POLICY acting_fence_select ON withdrawal_restriction_causes AS RESTRICTIVE FOR SELECT USING (NOT financial_acting_gucs_present());

CREATE FUNCTION withdrawal_restriction_action_rank(p_action text) RETURNS int
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT CASE p_action WHEN 'allow' THEN 0 WHEN 'hold_for_review' THEN 1 WHEN 'block' THEN 2 ELSE NULL END
$$;

-- =========================================================================
-- 2. withdrawal_restriction_policy_changes / _approvals (maker-checker)
-- =========================================================================
CREATE FUNCTION withdrawal_restriction_payload_hash(p_tenant uuid, p_cause text, p_action text, p_reason_code text) RETURNS text
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT public.k2_sha256_hex(public.k2_canonical('withdrawal_restriction_policy:v1', p_tenant::text, p_cause, p_action, p_reason_code))
$$;

CREATE TABLE withdrawal_restriction_policy_changes (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    cause                  TEXT NOT NULL REFERENCES withdrawal_restriction_causes (cause) ON DELETE RESTRICT,
    action                 TEXT NOT NULL CHECK (action IN ('allow', 'hold_for_review', 'block')),
    reason_code            TEXT NOT NULL CHECK (reason_code ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$'),
    note                   TEXT NULL CHECK (note IS NULL OR (btrim(note) <> '' AND octet_length(note) <= 1000 AND note !~ '[[:cntrl:]]')),
    payload_hash           TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    widening               BOOLEAN NOT NULL,
    required_approvals     INT NOT NULL CHECK (required_approvals BETWEEN 0 AND 5),
    base_set_version       BIGINT NOT NULL CHECK (base_set_version >= 0),
    requested_by           UUID NOT NULL,
    requested_by_person_id UUID NOT NULL,
    status                 TEXT NOT NULL CHECK (status IN ('requested', 'approved', 'executed', 'rejected', 'cancelled', 'expired')),
    created_at             TIMESTAMPTZ NOT NULL,
    created_txid           BIGINT NOT NULL,
    expires_at             TIMESTAMPTZ NOT NULL,
    decided_by             UUID NULL,
    decided_by_person_id   UUID NULL,
    decided_at             TIMESTAMPTZ NULL,
    decision_reason_code   TEXT NULL CHECK (decision_reason_code IS NULL OR decision_reason_code ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$'),
    executed_by            UUID NULL,
    executed_at            TIMESTAMPTZ NULL,
    executed_txid          BIGINT NULL,
    CONSTRAINT withdrawal_restriction_policy_changes_executed
        CHECK ((status = 'executed') = (executed_by IS NOT NULL AND executed_at IS NOT NULL AND executed_txid IS NOT NULL)),
    CONSTRAINT withdrawal_restriction_policy_changes_narrowing_single CHECK (widening OR required_approvals = 0),
    CONSTRAINT withdrawal_restriction_policy_changes_widening_two CHECK (NOT widening OR required_approvals >= 2)
);
CREATE UNIQUE INDEX uq_withdrawal_restriction_policy_changes_open
    ON withdrawal_restriction_policy_changes (tenant_id, cause) WHERE status = 'requested';
CREATE INDEX idx_withdrawal_restriction_policy_changes_tenant
    ON withdrawal_restriction_policy_changes (tenant_id, created_at DESC);
COMMENT ON TABLE withdrawal_restriction_policy_changes IS '0182: append-only change requests of withdrawal restriction policies (platform scope only). Content immutable; the state columns move through the guard''s closed, proof-bound state machine. An executed change admits the policy row of its own transaction (WR040).';

CREATE TABLE withdrawal_restriction_policy_approvals (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    change_id          UUID NOT NULL REFERENCES withdrawal_restriction_policy_changes (id) ON DELETE RESTRICT,
    approver_id        UUID NOT NULL,
    approver_person_id UUID NOT NULL,
    payload_hash       TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    reason_code        TEXT NOT NULL CHECK (reason_code ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$'),
    decided_at         TIMESTAMPTZ NOT NULL,
    CONSTRAINT uq_withdrawal_restriction_policy_approvals_person UNIQUE (change_id, approver_person_id),
    CONSTRAINT uq_withdrawal_restriction_policy_approvals_staff UNIQUE (change_id, approver_id)
);
COMMENT ON TABLE withdrawal_restriction_policy_approvals IS '0182: append-only approvals (one per Person per change), each proof-bound (withdrawal_restriction_policy:approve).';

-- =========================================================================
-- 3. withdrawal_restriction_policies (append-only versions)
-- =========================================================================
CREATE TABLE withdrawal_restriction_policies (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    cause           TEXT NOT NULL REFERENCES withdrawal_restriction_causes (cause) ON DELETE RESTRICT,
    -- Forward compatibility only: there is no per-withdrawal jurisdiction determination (withdrawal/policy.go, Q18), so a
    -- jurisdiction-scoped row could never be matched; it is refused rather than silently ignored.
    jurisdiction_id UUID NULL CHECK (jurisdiction_id IS NULL),
    action          TEXT NOT NULL CHECK (action IN ('allow', 'hold_for_review', 'block')),
    set_version     BIGINT NOT NULL CHECK (set_version >= 1),
    change_id       UUID NULL REFERENCES withdrawal_restriction_policy_changes (id) ON DELETE RESTRICT,
    reason_code     TEXT NOT NULL CHECK (reason_code ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$'),
    created_by      UUID NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT uq_withdrawal_restriction_policies_set_version UNIQUE (tenant_id, set_version),
    CONSTRAINT uq_withdrawal_restriction_policies_change UNIQUE (change_id)
);
CREATE INDEX idx_withdrawal_restriction_policies_lookup ON withdrawal_restriction_policies (tenant_id, cause, set_version DESC);
COMMENT ON TABLE withdrawal_restriction_policies IS '0182: append-only, versioned per-tenant withdrawal restriction policy. The effective action of (tenant, cause) is the row with the highest set_version, else the registry default (withdrawal_restriction_effective). set_version is assigned by the guard under the tenant status gate key taken EXCLUSIVE. Ships with NO rows.';

-- The effective action of (tenant, cause) at a stage, and its provenance. A configured row wins; otherwise the registry
-- default of a configurable cause. An unknown / non-configurable cause or an unknown stage raises WR002 / WR010 (callers fail
-- closed). Reads under the session's RLS: a session that cannot see the tenant's rows resolves to the default.
CREATE FUNCTION withdrawal_restriction_effective(p_tenant uuid, p_cause text, p_stage text,
    OUT action text, OUT source text, OUT policy_id uuid, OUT set_version bigint)
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_c public.withdrawal_restriction_causes%ROWTYPE;
BEGIN
    IF p_stage IS NULL OR p_stage NOT IN ('request', 'dispatch', 'redispatch') THEN
        RAISE EXCEPTION 'withdrawal restriction: unknown stage %', p_stage USING ERRCODE = 'WR010';
    END IF;
    SELECT * INTO v_c FROM public.withdrawal_restriction_causes c WHERE c.cause = p_cause;
    IF NOT FOUND OR NOT v_c.configurable THEN
        RAISE EXCEPTION 'withdrawal restriction: cause % is unknown or not configurable', p_cause USING ERRCODE = 'WR002';
    END IF;
    SELECT p.action, p.id, p.set_version INTO action, policy_id, set_version
      FROM public.withdrawal_restriction_policies p
     WHERE p.tenant_id = p_tenant AND p.cause = p_cause
     ORDER BY p.set_version DESC
     LIMIT 1;
    IF FOUND THEN
        source := 'configured';
        RETURN;
    END IF;
    action := CASE p_stage WHEN 'request' THEN v_c.default_request WHEN 'dispatch' THEN v_c.default_dispatch ELSE v_c.default_redispatch END;
    source := 'default_unconfigured';
    policy_id := NULL;
    set_version := NULL;
END
$$;

-- The tenant's policy set version (0 = nothing configured). Same RLS caveat.
CREATE FUNCTION withdrawal_restriction_set_version(p_tenant uuid) RETURNS bigint
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT COALESCE(max(p.set_version), 0)::bigint FROM public.withdrawal_restriction_policies p WHERE p.tenant_id = p_tenant
$$;

-- Widening iff the action is LESS strict than the effective action at ANY stage.
CREATE FUNCTION withdrawal_restriction_is_widening(p_tenant uuid, p_cause text, p_action text) RETURNS boolean
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_stage text;
    v_cur   text;
BEGIN
    IF public.withdrawal_restriction_action_rank(p_action) IS NULL THEN
        RAISE EXCEPTION 'withdrawal restriction: unknown action %', p_action USING ERRCODE = 'WR010';
    END IF;
    FOREACH v_stage IN ARRAY ARRAY['request', 'dispatch', 'redispatch'] LOOP
        SELECT e.action INTO v_cur FROM public.withdrawal_restriction_effective(p_tenant, p_cause, v_stage) e;
        IF public.withdrawal_restriction_action_rank(p_action) < public.withdrawal_restriction_action_rank(v_cur) THEN
            RETURN true;
        END IF;
    END LOOP;
    RETURN false;
END
$$;

-- Approvals that count: on the change's own payload hash, at most 24 h old, distinct Persons that are neither the requester's
-- Person nor the requester's account, each an ACTIVE platform_admin platform staff row whose Person is still the recorded one.
CREATE FUNCTION withdrawal_restriction_fresh_approvals(p_change uuid) RETURNS int
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT count(DISTINCT a.approver_person_id)::int
      FROM public.withdrawal_restriction_policy_approvals a
      JOIN public.withdrawal_restriction_policy_changes c ON c.id = a.change_id
      JOIN public.staff_users s ON s.id = a.approver_id
     WHERE a.change_id = p_change
       AND a.payload_hash = c.payload_hash
       AND a.decided_at > pg_catalog.clock_timestamp() - interval '24 hours'
       AND a.approver_person_id <> c.requested_by_person_id
       AND a.approver_id <> c.requested_by
       AND s.person_id = a.approver_person_id AND s.status = 'active' AND s.tenant_id IS NULL
       AND s.role = 'platform_admin'
$$;

-- The change guard: INSERT derives everything that matters (actor, classification, approvals, hash, base version, times) and
-- consumes the requester's proof; UPDATE is the closed state machine.
CREATE FUNCTION withdrawal_restriction_policy_changes_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor RECORD;
    v_cfg   boolean;
    v_wide  boolean;
    v_now   timestamptz := pg_catalog.clock_timestamp();
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'withdrawal_restriction_policy_changes is append-only: DELETE is not permitted' USING ERRCODE = 'WR055';
    END IF;

    IF TG_OP = 'INSERT' THEN
        SELECT * INTO v_actor FROM public.licensing_change_actor();
        IF v_actor.scope <> 'platform' OR v_actor.role <> 'platform_admin' THEN
            RAISE EXCEPTION 'withdrawal restriction: only a platform_admin platform session requests a policy change' USING ERRCODE = 'WR001';
        END IF;
        SELECT c.configurable INTO v_cfg FROM public.withdrawal_restriction_causes c WHERE c.cause = NEW.cause;
        IF NOT FOUND OR NOT v_cfg THEN
            RAISE EXCEPTION 'withdrawal restriction: cause % is not configurable', NEW.cause USING ERRCODE = 'WR002';
        END IF;
        v_wide := public.withdrawal_restriction_is_widening(NEW.tenant_id, NEW.cause, NEW.action);
        NEW.widening := v_wide;
        NEW.required_approvals := CASE WHEN v_wide THEN 2 ELSE 0 END;
        NEW.payload_hash := public.withdrawal_restriction_payload_hash(NEW.tenant_id, NEW.cause, NEW.action, NEW.reason_code);
        NEW.base_set_version := public.withdrawal_restriction_set_version(NEW.tenant_id);
        NEW.requested_by := v_actor.actor;
        NEW.requested_by_person_id := v_actor.person_id;
        NEW.status := 'requested';
        NEW.created_at := v_now;
        NEW.created_txid := pg_catalog.txid_current();
        NEW.expires_at := v_now + interval '72 hours';
        NEW.decided_by := NULL; NEW.decided_by_person_id := NULL; NEW.decided_at := NULL; NEW.decision_reason_code := NULL;
        NEW.executed_by := NULL; NEW.executed_at := NULL; NEW.executed_txid := NULL;
        PERFORM public.actor_proof_require(v_actor.actor, 'platform', NULL, 'withdrawal_restriction_policy:request', 'new', NEW.payload_hash);
        RETURN NEW;
    END IF;

    -- UPDATE: the content never changes.
    IF (pg_catalog.to_jsonb(NEW) - ARRAY['status', 'decided_by', 'decided_by_person_id', 'decided_at', 'decision_reason_code',
                                          'executed_by', 'executed_at', 'executed_txid'])
       IS DISTINCT FROM
       (pg_catalog.to_jsonb(OLD) - ARRAY['status', 'decided_by', 'decided_by_person_id', 'decided_at', 'decision_reason_code',
                                          'executed_by', 'executed_at', 'executed_txid']) THEN
        RAISE EXCEPTION 'withdrawal_restriction_policy_changes: the request content is immutable' USING ERRCODE = 'WR012';
    END IF;
    NEW.decided_by := OLD.decided_by; NEW.decided_by_person_id := OLD.decided_by_person_id; NEW.decided_at := OLD.decided_at;
    NEW.decision_reason_code := CASE WHEN NEW.status = 'rejected' THEN NEW.decision_reason_code ELSE OLD.decision_reason_code END;
    NEW.executed_by := OLD.executed_by; NEW.executed_at := OLD.executed_at; NEW.executed_txid := OLD.executed_txid;

    IF OLD.status = 'requested' AND NEW.status = 'expired' THEN
        IF v_now < OLD.expires_at THEN
            RAISE EXCEPTION 'withdrawal_restriction_policy_changes: the request has not expired' USING ERRCODE = 'WR013';
        END IF;
        RETURN NEW;
    END IF;

    SELECT * INTO v_actor FROM public.licensing_change_actor();
    IF v_actor.scope <> 'platform' THEN
        RAISE EXCEPTION 'withdrawal restriction: only a platform session acts on a policy change' USING ERRCODE = 'WR001';
    END IF;

    IF NEW.status = 'cancelled' THEN
        IF OLD.status <> 'requested' THEN
            RAISE EXCEPTION 'withdrawal_restriction_policy_changes: % -> cancelled is not a legal move', OLD.status USING ERRCODE = 'WR013';
        END IF;
        IF v_actor.actor <> OLD.requested_by THEN
            RAISE EXCEPTION 'withdrawal_restriction_policy_changes: only the requester may cancel' USING ERRCODE = 'WR023';
        END IF;
        NEW.decided_by := v_actor.actor; NEW.decided_by_person_id := v_actor.person_id; NEW.decided_at := v_now;
        PERFORM public.actor_proof_require(v_actor.actor, 'platform', NULL, 'withdrawal_restriction_policy:cancel', OLD.id::text, OLD.payload_hash);
        RETURN NEW;
    END IF;

    IF NEW.status = 'rejected' THEN
        IF OLD.status <> 'requested' OR v_now >= OLD.expires_at THEN
            RAISE EXCEPTION 'withdrawal_restriction_policy_changes: % -> rejected is not a legal move (or the request expired)', OLD.status USING ERRCODE = 'WR013';
        END IF;
        IF v_actor.role <> 'platform_admin' THEN
            RAISE EXCEPTION 'withdrawal_restriction_policy_changes: only a platform_admin rejects' USING ERRCODE = 'WR030';
        END IF;
        IF v_actor.actor = OLD.requested_by OR v_actor.person_id = OLD.requested_by_person_id THEN
            RAISE EXCEPTION 'withdrawal_restriction_policy_changes: the requester''s Person may not decide the request' USING ERRCODE = 'WR022';
        END IF;
        IF NEW.decision_reason_code IS NULL THEN
            RAISE EXCEPTION 'withdrawal_restriction_policy_changes: a rejection carries a reason code' USING ERRCODE = 'WR010';
        END IF;
        NEW.decided_by := v_actor.actor; NEW.decided_by_person_id := v_actor.person_id; NEW.decided_at := v_now;
        PERFORM public.actor_proof_require(v_actor.actor, 'platform', NULL, 'withdrawal_restriction_policy:reject', OLD.id::text, OLD.payload_hash);
        RETURN NEW;
    END IF;

    IF NEW.status = 'approved' THEN
        IF OLD.status <> 'requested' OR v_now >= OLD.expires_at OR OLD.required_approvals = 0 THEN
            RAISE EXCEPTION 'withdrawal_restriction_policy_changes: % -> approved is not a legal move (or the request expired)', OLD.status USING ERRCODE = 'WR013';
        END IF;
        IF public.withdrawal_restriction_fresh_approvals(OLD.id) < OLD.required_approvals THEN
            RAISE EXCEPTION 'withdrawal_restriction_policy_changes: the request does not hold % fresh approvals by distinct eligible Persons', OLD.required_approvals
                USING ERRCODE = 'WR020';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.status = 'executed' THEN
        IF v_now >= OLD.expires_at THEN
            RAISE EXCEPTION 'withdrawal_restriction_policy_changes: the request has expired' USING ERRCODE = 'WR013';
        END IF;
        IF v_actor.role <> 'platform_admin' THEN
            RAISE EXCEPTION 'withdrawal_restriction_policy_changes: only a platform_admin session executes' USING ERRCODE = 'WR030';
        END IF;
        IF OLD.required_approvals = 0 THEN
            IF OLD.status <> 'requested' OR v_actor.actor <> OLD.requested_by OR OLD.created_txid <> pg_catalog.txid_current() THEN
                RAISE EXCEPTION 'withdrawal_restriction_policy_changes: a single-actor change executes only by its requester, in its own transaction' USING ERRCODE = 'WR021';
            END IF;
        ELSE
            IF OLD.status <> 'approved' THEN
                RAISE EXCEPTION 'withdrawal_restriction_policy_changes: % -> executed is not a legal move', OLD.status USING ERRCODE = 'WR013';
            END IF;
            IF public.withdrawal_restriction_fresh_approvals(OLD.id) < OLD.required_approvals THEN
                RAISE EXCEPTION 'withdrawal_restriction_policy_changes: the request does not hold % fresh approvals by distinct eligible Persons', OLD.required_approvals
                    USING ERRCODE = 'WR020';
            END IF;
        END IF;
        -- Serialise with the money paths (they read the policy under the same key, SHARED) and with any other change of the
        -- tenant, then re-classify against the state NOW: a change that was narrowing when requested but would widen now
        -- needs the approvals it does not hold.
        PERFORM pg_advisory_xact_lock(public.tenant_status_gate_key(OLD.tenant_id));
        IF public.withdrawal_restriction_is_widening(OLD.tenant_id, OLD.cause, OLD.action) AND NOT OLD.widening THEN
            RAISE EXCEPTION 'withdrawal_restriction_policy_changes: the change now widens; file a new request' USING ERRCODE = 'WR024';
        END IF;
        PERFORM public.actor_proof_require(v_actor.actor, 'platform', NULL, 'withdrawal_restriction_policy:execute', OLD.id::text, OLD.payload_hash);
        NEW.executed_by := v_actor.actor; NEW.executed_at := v_now; NEW.executed_txid := pg_catalog.txid_current();
        RETURN NEW;
    END IF;

    RAISE EXCEPTION 'withdrawal_restriction_policy_changes: % -> % is not a legal move', OLD.status, NEW.status USING ERRCODE = 'WR013';
END
$$;
CREATE TRIGGER withdrawal_restriction_policy_changes_guard
    BEFORE INSERT OR UPDATE OR DELETE ON withdrawal_restriction_policy_changes
    FOR EACH ROW EXECUTE FUNCTION withdrawal_restriction_policy_changes_guard();

CREATE FUNCTION withdrawal_restriction_deny_mutation() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted', TG_TABLE_NAME, TG_OP USING ERRCODE = 'WR055';
END
$$;
CREATE TRIGGER withdrawal_restriction_policy_changes_no_truncate
    BEFORE TRUNCATE ON withdrawal_restriction_policy_changes
    FOR EACH STATEMENT EXECUTE FUNCTION withdrawal_restriction_deny_mutation();

CREATE FUNCTION withdrawal_restriction_policy_approvals_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor RECORD;
    v_chg   public.withdrawal_restriction_policy_changes%ROWTYPE;
    v_now   timestamptz := pg_catalog.clock_timestamp();
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'withdrawal_restriction_policy_approvals is append-only: % is not permitted', TG_OP USING ERRCODE = 'WR055';
    END IF;
    SELECT * INTO v_actor FROM public.licensing_change_actor();
    IF v_actor.scope <> 'platform' OR v_actor.role <> 'platform_admin' THEN
        RAISE EXCEPTION 'withdrawal restriction: only a platform_admin platform session approves' USING ERRCODE = 'WR030';
    END IF;
    SELECT * INTO v_chg FROM public.withdrawal_restriction_policy_changes c WHERE c.id = NEW.change_id FOR UPDATE;
    IF NOT FOUND OR v_chg.status <> 'requested' OR v_now >= v_chg.expires_at OR v_chg.required_approvals = 0 THEN
        RAISE EXCEPTION 'withdrawal restriction: the change is not open for approval' USING ERRCODE = 'WR031';
    END IF;
    IF v_actor.actor = v_chg.requested_by OR v_actor.person_id = v_chg.requested_by_person_id THEN
        RAISE EXCEPTION 'withdrawal restriction: the requester''s Person may not approve the change (four-eyes)' USING ERRCODE = 'WR032';
    END IF;
    NEW.approver_id := v_actor.actor;
    NEW.approver_person_id := v_actor.person_id;
    NEW.payload_hash := v_chg.payload_hash;
    NEW.decided_at := v_now;
    PERFORM public.actor_proof_require(v_actor.actor, 'platform', NULL, 'withdrawal_restriction_policy:approve', v_chg.id::text, v_chg.payload_hash);
    RETURN NEW;
END
$$;
CREATE TRIGGER withdrawal_restriction_policy_approvals_guard
    BEFORE INSERT OR UPDATE OR DELETE ON withdrawal_restriction_policy_approvals
    FOR EACH ROW EXECUTE FUNCTION withdrawal_restriction_policy_approvals_guard();
CREATE TRIGGER withdrawal_restriction_policy_approvals_no_truncate
    BEFORE TRUNCATE ON withdrawal_restriction_policy_approvals
    FOR EACH STATEMENT EXECUTE FUNCTION withdrawal_restriction_deny_mutation();

-- The policy guard: append-only; the cause must be configurable; the version is assigned HERE under the tenant status gate
-- key EXCLUSIVE (the money paths read the policy under the same key SHARED, so a decision and a change serialise); a write by
-- any role but the table owner needs the executed change of THIS transaction with the same content (WR040). The owner path is
-- the documented fixture / provisioning path (the 0177 / ADR 0112 LA021 convention: an owner can disable triggers anyway).
CREATE FUNCTION withdrawal_restriction_policies_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_cfg boolean;
    v_chg public.withdrawal_restriction_policy_changes%ROWTYPE;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'withdrawal_restriction_policies is append-only: % is not permitted', TG_OP USING ERRCODE = 'WR055';
    END IF;
    SELECT c.configurable INTO v_cfg FROM public.withdrawal_restriction_causes c WHERE c.cause = NEW.cause;
    IF NOT FOUND OR NOT v_cfg THEN
        RAISE EXCEPTION 'withdrawal restriction: cause % is not configurable', NEW.cause USING ERRCODE = 'WR002';
    END IF;
    PERFORM pg_advisory_xact_lock(public.tenant_status_gate_key(NEW.tenant_id));
    IF public.launch_is_table_owner() THEN
        NEW.created_by := NULL;
    ELSE
        SELECT * INTO v_chg FROM public.withdrawal_restriction_policy_changes c WHERE c.id = NEW.change_id;
        IF NOT FOUND OR v_chg.status <> 'executed' OR v_chg.executed_txid IS DISTINCT FROM pg_catalog.txid_current()
           OR v_chg.tenant_id <> NEW.tenant_id OR v_chg.cause <> NEW.cause OR v_chg.action <> NEW.action
           OR v_chg.reason_code <> NEW.reason_code THEN
            RAISE EXCEPTION 'withdrawal_restriction_policies: a policy row needs the matching change executed in this transaction (migration 0182)'
                USING ERRCODE = 'WR040';
        END IF;
        NEW.created_by := v_chg.executed_by;
    END IF;
    NEW.id := pg_catalog.gen_random_uuid();
    NEW.created_at := pg_catalog.clock_timestamp();
    SELECT COALESCE(max(p.set_version), 0) + 1 INTO NEW.set_version
      FROM public.withdrawal_restriction_policies p WHERE p.tenant_id = NEW.tenant_id;
    RETURN NEW;
END
$$;
CREATE TRIGGER withdrawal_restriction_policies_guard
    BEFORE INSERT OR UPDATE OR DELETE ON withdrawal_restriction_policies
    FOR EACH ROW EXECUTE FUNCTION withdrawal_restriction_policies_guard();
CREATE TRIGGER withdrawal_restriction_policies_no_truncate
    BEFORE TRUNCATE ON withdrawal_restriction_policies
    FOR EACH STATEMENT EXECUTE FUNCTION withdrawal_restriction_deny_mutation();

-- =========================================================================
-- 4. RLS
-- =========================================================================
ALTER TABLE withdrawal_restriction_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE withdrawal_restriction_policies FORCE ROW LEVEL SECURITY;
ALTER TABLE withdrawal_restriction_policy_changes ENABLE ROW LEVEL SECURITY;
ALTER TABLE withdrawal_restriction_policy_changes FORCE ROW LEVEL SECURITY;
ALTER TABLE withdrawal_restriction_policy_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE withdrawal_restriction_policy_approvals FORCE ROW LEVEL SECURITY;

-- Policies: a platform session reads all and inserts (the guard binds the insert to an executed change); a tenant session
-- (staff, system or the money paths; never a player session, never an acting session) reads ITS OWN rows only.
CREATE POLICY platform_scope_select ON withdrawal_restriction_policies FOR SELECT
    USING (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY platform_scope_insert ON withdrawal_restriction_policies FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
                AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_scope_select ON withdrawal_restriction_policies FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());

-- Changes and approvals: platform sessions only (requests, decisions and the approver identities are platform records).
CREATE POLICY platform_scope_select ON withdrawal_restriction_policy_changes FOR SELECT
    USING (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY platform_scope_insert ON withdrawal_restriction_policy_changes FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
                AND NOT financial_acting_gucs_present());
CREATE POLICY platform_scope_update ON withdrawal_restriction_policy_changes FOR UPDATE
    USING (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present())
    WITH CHECK (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
                AND NOT financial_acting_gucs_present());
CREATE POLICY platform_scope_select ON withdrawal_restriction_policy_approvals FOR SELECT
    USING (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY platform_scope_insert ON withdrawal_restriction_policy_approvals FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
                AND NOT financial_acting_gucs_present());

-- ADR 0099 acting fence on the three tables (the 0177 pattern).
CREATE POLICY acting_fence_select ON withdrawal_restriction_policies AS RESTRICTIVE FOR SELECT USING (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_insert ON withdrawal_restriction_policies AS RESTRICTIVE FOR INSERT WITH CHECK (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_select ON withdrawal_restriction_policy_changes AS RESTRICTIVE FOR SELECT USING (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_insert ON withdrawal_restriction_policy_changes AS RESTRICTIVE FOR INSERT WITH CHECK (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_update ON withdrawal_restriction_policy_changes AS RESTRICTIVE FOR UPDATE USING (NOT financial_acting_gucs_present()) WITH CHECK (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_select ON withdrawal_restriction_policy_approvals AS RESTRICTIVE FOR SELECT USING (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_insert ON withdrawal_restriction_policy_approvals AS RESTRICTIVE FOR INSERT WITH CHECK (NOT financial_acting_gucs_present());

-- =========================================================================
-- 5. withdrawal_gate_decisions: WD056 and the policy provenance (F4 semantics unchanged)
-- =========================================================================
ALTER TABLE withdrawal_gate_decisions
    ADD COLUMN restriction_policy_source TEXT NULL
        CHECK (restriction_policy_source IS NULL OR restriction_policy_source IN ('default_unconfigured', 'configured', 'none')),
    ADD COLUMN restriction_policy_set_version BIGINT NULL
        CHECK (restriction_policy_set_version IS NULL OR restriction_policy_set_version >= 0),
    ADD COLUMN restriction_cause TEXT NULL REFERENCES withdrawal_restriction_causes (cause) ON DELETE RESTRICT,
    ADD COLUMN restriction_action TEXT NULL
        CHECK (restriction_action IS NULL OR restriction_action IN ('allow', 'hold_for_review', 'block'));
ALTER TABLE withdrawal_gate_decisions ADD CONSTRAINT withdrawal_gate_decisions_restriction_provenance CHECK (
    (restriction_policy_source IS NULL) = (restriction_policy_set_version IS NULL)
    AND (restriction_policy_source IS NOT NULL OR (restriction_cause IS NULL AND restriction_action IS NULL))
    AND ((restriction_policy_source = 'none') IS NOT TRUE OR restriction_cause IS NULL));
ALTER TABLE withdrawal_gate_decisions DROP CONSTRAINT withdrawal_gate_decisions_outcome_shape;
ALTER TABLE withdrawal_gate_decisions ADD CONSTRAINT withdrawal_gate_decisions_outcome_shape CHECK (
    (allowed AND gate_code IS NULL)
    OR (NOT allowed AND gate_code IN ('WD050', 'WD051', 'WD056') AND reason_code IS NOT NULL));
-- A WD056 refusal is a CONFIGURED hold_for_review / block of the cause named in reason_code (a default never refuses).
ALTER TABLE withdrawal_gate_decisions ADD CONSTRAINT withdrawal_gate_decisions_wd056_shape CHECK (
    gate_code IS DISTINCT FROM 'WD056'
    OR COALESCE(restriction_policy_source = 'configured' AND restriction_cause IS NOT NULL AND reason_code = restriction_cause
                AND restriction_action IN ('hold_for_review', 'block'), false));
COMMENT ON COLUMN withdrawal_gate_decisions.restriction_policy_source IS '0182: provenance of the withdrawal restriction policy evaluated with this decision: configured, default_unconfigured (interim engineering default) or none (no restriction cause in force). NULL on rows written before 0182 or by a raw fixture.';

-- =========================================================================
-- 6. actor_proof_require: the 0177 body plus the marked 0182 block and allow-list term.
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
                                         OR p_operation LIKE 'withdrawal\_restriction\_policy:%') THEN
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
-- 7. Grants to igaming_runtime (mirrored in deploy/init-app-role.sql). RLS + guards bind; grants are defence in depth.
-- =========================================================================
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON withdrawal_restriction_causes FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON withdrawal_restriction_causes TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON withdrawal_restriction_policies FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON withdrawal_restriction_policies TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON withdrawal_restriction_policy_changes FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON withdrawal_restriction_policy_changes TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (status, decided_by, decided_by_person_id, decided_at, decision_reason_code, executed_by, executed_at, executed_txid) ON withdrawal_restriction_policy_changes TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON withdrawal_restriction_policy_approvals FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON withdrawal_restriction_policy_approvals TO igaming_runtime';
    END IF;
END
$$;
