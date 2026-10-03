-- PRH-2 K2 (ADR 0100): governed manual adjustments and financial approval
-- policies. ADR 0100's text calls this migration "0112"; the final
-- allocation (ADR 0100 §19, plan §11) makes it 0113.
--
-- What this adds:
--   1. K2-G1 (security K1 re-check): AS RESTRICTIVE acting SELECT fences on
--      asset_operation_eligibility and open_bet_self_exclusion_policies,
--      whose SELECT NULL-tenant arms the hardened A-18 found reachable by
--      an acting session (ADR 0099 §6.2 had judged them by their WRITE
--      policies only).
--   2. Family-R reference data: financial_control_classifications (the
--      HD-PRH2-1 mandatory four-eyes class) and
--      ledger_adjustment_reason_codes (LF ruling 1, literal - K3 adds the
--      Step-B causation arm, not this migration).
--   3. Append-only, effective-dated approval policies (HD-PRH2-7, S-2),
--      written ONLY through a four-eyes change request + approval, plus
--      the tenant -> profile assignment. No policy row is seeded
--      (HD-PRH2-3): with no in-force platform baseline the operation is
--      DISABLED.
--   4. ledger_adjustment_requests / ledger_adjustment_approvals: the
--      immutable, hash-pinned payload, the closed posting shape
--      (manual_adjustment <-> player_cash only), the independence floor
--      (distinct non-NULL Person, S-12 beneficiary exclusion), the
--      preventive MA020 open-payment-exposure refusal (K2-a), the
--      suspended-asset rule including the 0045 tenant layer (K2-b), the
--      state machine, the link trigger and the deferred "never commit
--      executing" check.
--   5. LF C-K1-2: the ADR 0099 §6.6 ledger fence (branch (a) only) and §6.7
--      projection fence, created in THIS migration, before (and alongside)
--      every acting permissive policy on ledger_transactions,
--      ledger_entries, ledger_accounts and wallet_balance_projection.
--   6. The acting permissive policies of ADR 0099 §6.5 / ADR 0100 §10.7,
--      plus three implementation-record additions (ADR 0100 §20): acting
--      SELECT on licences (own licence only), asset_authorizations and
--      staff_capability_grant_requests (the executor's jurisdiction,
--      K2-b and K2-G4 reads).
--   7. The reconciliation kind ledger_unlinked_manual_adjustment (LF
--      ruling 4) as a strict superset of 0107's CHECK.
--
-- SQLSTATE class 'MA' (manual adjustments). Application code classifies
-- by code only:
--   MA001 session/actor not permitted for this K2 operation
--   MA002 actor has no linked person_id
--   MA003 actor lacks the eligible role or an in-force grant
--   MA010 non-tightening tenant/brand policy row without two platform principals
--   MA011 a contributing policy's author/approver may not initiate or approve (S-2(iii))
--   MA012 policy change / change-approval guard violation
--   MA013 policy row guard violation (not via approval; base < 1 for the mandatory class)
--   MA014 policy evaluation: operation disabled (no in-force platform baseline, or
--         a jurisdiction row with an unresolvable tenant jurisdiction)
--   MA020 open_payment_exposure (credit refused; no override)
--   MA021 asset rule (asset mismatch / suspended asset for goodwill_credit)
--   MA022 reason-code rule (direction, causation, evidence, compensation cap)
--   MA023 non-active tenant (goodwill_credit refused)
--   MA025 note invalid (1-1000 bytes, no C0/C1 controls except \n)
--   MA030 request guard (immutability, state machine)
--   MA031 approval guard (distinct Person, pending, payload hash, grant)
--   MA032 beneficiary exclusion (S-12)
--   MA040 link trigger (executed request does not match its ledger transaction)
--   MA041 a request may never commit in state 'executing'
--   MA099 down-migration refused while rows exist
--   CG030 ledger fence (ADR 0099 §6.6), CG031 projection fence (§6.7)
--
-- No SECURITY DEFINER. FORCE ROW LEVEL SECURITY on every new table. No
-- FOR ALL permissive policy. DELETE and TRUNCATE refused on every new
-- table. No threshold value anywhere (HD-PRH2-3).

-- =========================================================================
-- 1. K2-G1: restrictive acting SELECT fences on two NULL-arm tables
-- =========================================================================

CREATE POLICY acting_fence_select ON asset_operation_eligibility AS RESTRICTIVE FOR SELECT
    USING (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_select ON open_bet_self_exclusion_policies AS RESTRICTIVE FOR SELECT
    USING (NOT financial_acting_gucs_present());

-- =========================================================================
-- 2. Reference tables (family R; §10.1). Rows inserted BEFORE FORCE RLS.
-- =========================================================================

CREATE TABLE financial_control_classifications (
    operation_kind TEXT PRIMARY KEY,
    class          TEXT NOT NULL CHECK (class IN ('mandatory_four_eyes', 'outside_mandatory_class')),
    governed_since TIMESTAMPTZ NOT NULL,
    CHECK (operation_kind NOT IN ('ledger_adjustment', 'payment_force_resolve') OR class = 'mandatory_four_eyes')
);

-- governed_since is the migration time: the cutover for the §12
-- ledger_unlinked_manual_adjustment detector.
INSERT INTO financial_control_classifications (operation_kind, class, governed_since) VALUES
    ('ledger_adjustment',     'mandatory_four_eyes', now()),
    ('payment_force_resolve', 'mandatory_four_eyes', now());

ALTER TABLE financial_control_classifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE financial_control_classifications FORCE ROW LEVEL SECURITY;
CREATE POLICY reference_read ON financial_control_classifications FOR SELECT USING (true);

ALTER TABLE financial_capability_catalogue
    ADD CONSTRAINT financial_capability_catalogue_operation_kind_fkey
    FOREIGN KEY (operation_kind) REFERENCES financial_control_classifications (operation_kind);

CREATE TABLE ledger_adjustment_reason_codes (
    reason_code                   TEXT PRIMARY KEY,
    allowed_directions            TEXT[] NOT NULL,
    causation_rule                TEXT NOT NULL CHECK (causation_rule IN ('forbidden', 'optional_same_wallet', 'required_compensation')),
    evidence_required             BOOLEAN NOT NULL,
    allowed_in_suspended_asset    BOOLEAN NOT NULL,
    allowed_for_non_active_tenant BOOLEAN NOT NULL,
    CHECK (allowed_directions <@ ARRAY['credit_player', 'debit_player']::text[] AND cardinality(allowed_directions) >= 1)
);

-- LF ruling 1, literal. There is NO deposit-allocation code (LF-2).
INSERT INTO ledger_adjustment_reason_codes
    (reason_code, allowed_directions, causation_rule, evidence_required, allowed_in_suspended_asset, allowed_for_non_active_tenant) VALUES
    ('operational_error_correction', '{credit_player,debit_player}', 'optional_same_wallet',  false, true,  true),
    ('compensating_entry',           '{credit_player,debit_player}', 'required_compensation', true,  true,  true),
    ('goodwill_credit',              '{credit_player}',              'forbidden',             false, false, false),
    ('external_instruction',         '{credit_player,debit_player}', 'optional_same_wallet',  true,  true,  true);

ALTER TABLE ledger_adjustment_reason_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_adjustment_reason_codes FORCE ROW LEVEL SECURITY;
CREATE POLICY reference_read ON ledger_adjustment_reason_codes FOR SELECT USING (true);

-- =========================================================================
-- 3. Helpers
-- =========================================================================

-- A collision-free canonical encoding: each field as "<octet length>:<value>",
-- NULL as "~", joined by ",". Used for content_hash and payload_hash, so
-- no field's content can ever be confused with its neighbour's.
CREATE FUNCTION k2_canonical(VARIADIC p_fields text[]) RETURNS text AS $$
    SELECT string_agg(CASE WHEN f IS NULL THEN '~' ELSE octet_length(f)::text || ':' || f END, ',' ORDER BY ord)
      FROM unnest(p_fields) WITH ORDINALITY AS u(f, ord);
$$ LANGUAGE sql IMMUTABLE;

CREATE FUNCTION k2_sha256_hex(p text) RETURNS text AS $$
    SELECT encode(sha256(convert_to(p, 'UTF8')), 'hex');
$$ LANGUAGE sql IMMUTABLE;

-- The note (C-100-5): validated here AND in Go; only its hash is stored on
-- the request, the bounded note itself goes to the audit metadata.
CREATE FUNCTION ledger_adjustment_note_hash(p_note text) RETURNS text AS $$
BEGIN
    IF p_note IS NULL OR octet_length(p_note) < 1 OR octet_length(p_note) > 1000 THEN
        RAISE EXCEPTION 'ledger_adjustment: note must be 1-1000 bytes' USING ERRCODE = 'MA025';
    END IF;
    IF p_note ~ '[\x01-\x09\x0B-\x1F\x7F-\x9F]' THEN
        RAISE EXCEPTION 'ledger_adjustment: note may not contain control characters other than newline' USING ERRCODE = 'MA025';
    END IF;
    RETURN k2_sha256_hex(p_note);
END;
$$ LANGUAGE plpgsql IMMUTABLE;

-- Tightening, defined against the in-force predecessor of the same key
-- (§3.3). With no predecessor any row is a tightening.
CREATE FUNCTION financial_policy_is_tightening(
    p_old_base int, p_old_threshold numeric, p_old_above int,
    p_new_base int, p_new_threshold numeric, p_new_above int
) RETURNS boolean AS $$
    SELECT p_new_base >= p_old_base
       AND (p_old_threshold IS NULL
            OR (p_new_threshold IS NOT NULL AND p_new_threshold <= p_old_threshold AND p_new_above >= p_old_above));
$$ LANGUAGE sql IMMUTABLE;

-- =========================================================================
-- 4. Policy change requests and approvals (§3.3, §10.4; T, P - no A)
-- =========================================================================

CREATE TABLE financial_approval_policy_changes (
    id                                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    change_kind                         TEXT NOT NULL CHECK (change_kind IN ('policy', 'profile_assignment')),
    -- The proposed row, as typed columns (stricter than a JSONB blob: the
    -- same CHECKs as financial_approval_policies apply at request time).
    operation_kind                      TEXT NULL REFERENCES financial_control_classifications (operation_kind),
    level                               TEXT NULL CHECK (level IN ('platform', 'jurisdiction', 'profile', 'tenant', 'brand')),
    tenant_id                           UUID NULL REFERENCES tenants (id),
    brand_id                            UUID NULL,
    jurisdiction_id                     UUID NULL REFERENCES jurisdictions (id),
    profile_code                        TEXT NULL CHECK (profile_code ~ '^[a-z0-9][a-z0-9_.-]{0,63}$'),
    asset_code                          TEXT NULL REFERENCES assets (code),
    base_required_approvals             INT NULL CHECK (base_required_approvals >= 0),
    threshold_minor_units               NUMERIC(38,0) NULL CHECK (threshold_minor_units >= 0),
    required_approvals_above_threshold  INT NULL,
    effective_from                      TIMESTAMPTZ NOT NULL,
    legal_review_reference              TEXT NULL CHECK (octet_length(legal_review_reference) BETWEEN 1 AND 256),
    content_hash                        TEXT NOT NULL,
    requested_by                        UUID NOT NULL,
    requested_by_scope                  TEXT NOT NULL CHECK (requested_by_scope IN ('tenant', 'platform')),
    requested_by_person_id              UUID NOT NULL,
    status                              TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'cancelled', 'expired')),
    created_at                          TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at                          TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id),
    CHECK (
        (change_kind = 'policy'
            AND operation_kind IS NOT NULL AND level IS NOT NULL AND base_required_approvals IS NOT NULL
            AND (level <> 'platform'     OR (tenant_id IS NULL AND brand_id IS NULL AND jurisdiction_id IS NULL AND profile_code IS NULL))
            AND (level <> 'jurisdiction' OR (tenant_id IS NULL AND brand_id IS NULL AND jurisdiction_id IS NOT NULL AND profile_code IS NULL))
            AND (level <> 'profile'      OR (tenant_id IS NULL AND brand_id IS NULL AND jurisdiction_id IS NULL AND profile_code IS NOT NULL))
            AND (level <> 'tenant'       OR (tenant_id IS NOT NULL AND brand_id IS NULL AND jurisdiction_id IS NULL AND profile_code IS NULL))
            AND (level <> 'brand'        OR (tenant_id IS NOT NULL AND brand_id IS NOT NULL AND jurisdiction_id IS NULL AND profile_code IS NULL))
            AND ((threshold_minor_units IS NULL) = (required_approvals_above_threshold IS NULL))
            AND (threshold_minor_units IS NULL OR asset_code IS NOT NULL)
            AND (required_approvals_above_threshold IS NULL OR required_approvals_above_threshold >= base_required_approvals))
        OR
        (change_kind = 'profile_assignment'
            AND operation_kind IS NULL AND level IS NULL AND tenant_id IS NOT NULL AND profile_code IS NOT NULL
            AND brand_id IS NULL AND jurisdiction_id IS NULL AND asset_code IS NULL AND base_required_approvals IS NULL
            AND threshold_minor_units IS NULL AND required_approvals_above_threshold IS NULL)
    )
);

CREATE INDEX financial_approval_policy_changes_tenant ON financial_approval_policy_changes (tenant_id);

CREATE FUNCTION financial_approval_policy_change_content_hash(c financial_approval_policy_changes) RETURNS text AS $$
    SELECT k2_sha256_hex(k2_canonical(
        c.change_kind, c.operation_kind, c.level, c.tenant_id::text, c.brand_id::text, c.jurisdiction_id::text,
        c.profile_code, c.asset_code, c.base_required_approvals::text, c.threshold_minor_units::text,
        c.required_approvals_above_threshold::text,
        to_char(c.effective_from AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
        c.legal_review_reference));
$$ LANGUAGE sql STABLE;

-- financial_policy_actor_has_permission: the static governance
-- permission (financial_governance_permissions, the ADR 0099 §3.2
-- catalogue that A-15 pins against internal/auth) for the actor's role.
CREATE FUNCTION financial_policy_actor_has_permission(p_role text, p_permission text) RETURNS boolean AS $$
    SELECT EXISTS (SELECT 1 FROM financial_governance_permissions g WHERE g.permission = p_permission AND p_role = ANY (g.roles));
$$ LANGUAGE sql STABLE;

-- The in-force predecessor of a policy key at p_at (latest effective_from
-- <= p_at). Keys: (operation_kind, level, scope key, asset_code).
CREATE FUNCTION financial_approval_policy_predecessor(
    p_operation text, p_level text, p_tenant uuid, p_brand uuid, p_jurisdiction uuid, p_profile text, p_asset text, p_at timestamptz
) RETURNS uuid AS $$
BEGIN
    RETURN (
        SELECT p.id FROM financial_approval_policies p
         WHERE p.operation_kind = p_operation AND p.level = p_level
           AND p.tenant_id IS NOT DISTINCT FROM p_tenant
           AND p.brand_id IS NOT DISTINCT FROM p_brand
           AND p.jurisdiction_id IS NOT DISTINCT FROM p_jurisdiction
           AND p.profile_code IS NOT DISTINCT FROM p_profile
           AND p.asset_code IS NOT DISTINCT FROM p_asset
           AND p.effective_from <= p_at
         ORDER BY p.effective_from DESC, p.created_at DESC, p.id
         LIMIT 1);
END;
$$ LANGUAGE plpgsql STABLE;

-- financial_approval_policy_change_tightens: is change c a tightening of
-- its key's in-force predecessor at GREATEST(c.effective_from, now())?
CREATE FUNCTION financial_approval_policy_change_tightens(c financial_approval_policy_changes) RETURNS boolean AS $$
DECLARE
    v_pred_id uuid;
    v_pred    RECORD;
BEGIN
    IF c.change_kind <> 'policy' THEN
        RETURN false;
    END IF;
    v_pred_id := financial_approval_policy_predecessor(c.operation_kind, c.level, c.tenant_id, c.brand_id,
        c.jurisdiction_id, c.profile_code, c.asset_code, GREATEST(c.effective_from, now()));
    IF v_pred_id IS NULL THEN
        RETURN true;
    END IF;
    SELECT * INTO v_pred FROM financial_approval_policies WHERE id = v_pred_id;
    RETURN financial_policy_is_tightening(v_pred.base_required_approvals, v_pred.threshold_minor_units, v_pred.required_approvals_above_threshold,
        c.base_required_approvals, c.threshold_minor_units, c.required_approvals_above_threshold);
END;
$$ LANGUAGE plpgsql STABLE;

CREATE FUNCTION financial_approval_policy_changes_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor RECORD;
BEGIN
    SELECT * INTO v_actor FROM financial_actor_session();

    IF TG_OP = 'INSERT' THEN
        IF v_actor.scope NOT IN ('tenant', 'platform') THEN
            RAISE EXCEPTION 'financial_approval_policy_changes: only a tenant or platform session may propose a policy change' USING ERRCODE = 'MA012';
        END IF;
        IF v_actor.person_id IS NULL THEN
            RAISE EXCEPTION 'financial_approval_policy_changes: requester has no linked person_id' USING ERRCODE = 'MA002';
        END IF;
        NEW.requested_by := v_actor.actor;
        NEW.requested_by_scope := v_actor.scope;
        NEW.requested_by_person_id := v_actor.person_id;
        NEW.status := 'pending';
        NEW.created_at := now();
        NEW.expires_at := now() + interval '24 hours';
        NEW.effective_from := COALESCE(NEW.effective_from, now());

        -- §3.1 authorship matrix, requester side.
        IF NEW.change_kind = 'profile_assignment' OR NEW.level IN ('platform', 'jurisdiction', 'profile') THEN
            IF v_actor.scope <> 'platform' OR NOT financial_policy_actor_has_permission(v_actor.role, 'financial_policy:author') THEN
                RAISE EXCEPTION 'financial_approval_policy_changes: platform/jurisdiction/profile rows and profile assignments are platform-authored only (HD-PRH2-7)' USING ERRCODE = 'MA012';
            END IF;
        ELSE
            IF v_actor.scope = 'platform' THEN
                IF NOT financial_policy_actor_has_permission(v_actor.role, 'financial_policy:author') THEN
                    RAISE EXCEPTION 'financial_approval_policy_changes: platform requester lacks financial_policy:author' USING ERRCODE = 'MA012';
                END IF;
            ELSE
                IF NEW.tenant_id IS DISTINCT FROM v_actor.tenant THEN
                    RAISE EXCEPTION 'financial_approval_policy_changes: a tenant requester may only propose its own tenant/brand rows' USING ERRCODE = 'MA012';
                END IF;
                IF NOT financial_policy_actor_has_permission(v_actor.role, 'financial_policy:tighten') THEN
                    RAISE EXCEPTION 'financial_approval_policy_changes: tenant requester lacks financial_policy:tighten' USING ERRCODE = 'MA012';
                END IF;
                -- Legible pre-check; binding at the policy insert.
                IF NOT financial_approval_policy_change_tightens(NEW) THEN
                    RAISE EXCEPTION 'financial_approval_policy_changes: a tenant principal may only tighten (HD-PRH2-7)' USING ERRCODE = 'MA010';
                END IF;
            END IF;
        END IF;
        NEW.content_hash := financial_approval_policy_change_content_hash(NEW);
        RETURN NEW;
    END IF;

    -- UPDATE: status only, pending -> cancelled|expired (by the requester or
    -- on expiry) or -> approved|rejected (by the same-transaction approval).
    IF OLD.status <> 'pending' THEN
        RAISE EXCEPTION 'financial_approval_policy_changes: change % is terminal', OLD.id USING ERRCODE = 'MA012';
    END IF;
    IF (to_jsonb(NEW) - 'status') IS DISTINCT FROM (to_jsonb(OLD) - 'status') THEN
        RAISE EXCEPTION 'financial_approval_policy_changes: only status may change' USING ERRCODE = 'MA012';
    END IF;
    IF NEW.status IN ('approved', 'rejected') THEN
        IF NOT EXISTS (SELECT 1 FROM financial_approval_policy_change_approvals a
                        WHERE a.change_id = OLD.id AND a.decided_txid = txid_current()
                          AND a.decision = CASE WHEN NEW.status = 'approved' THEN 'approve' ELSE 'reject' END) THEN
            RAISE EXCEPTION 'financial_approval_policy_changes: approved/rejected only via a same-transaction decision' USING ERRCODE = 'MA012';
        END IF;
    ELSIF NEW.status = 'cancelled' THEN
        IF v_actor.actor IS DISTINCT FROM OLD.requested_by THEN
            RAISE EXCEPTION 'financial_approval_policy_changes: only the requester may cancel' USING ERRCODE = 'MA012';
        END IF;
    ELSIF NEW.status = 'expired' THEN
        IF now() < OLD.expires_at THEN
            RAISE EXCEPTION 'financial_approval_policy_changes: change has not expired' USING ERRCODE = 'MA012';
        END IF;
    ELSE
        RAISE EXCEPTION 'financial_approval_policy_changes: invalid status transition' USING ERRCODE = 'MA012';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER financial_approval_policy_changes_guard
    BEFORE INSERT OR UPDATE ON financial_approval_policy_changes
    FOR EACH ROW EXECUTE FUNCTION financial_approval_policy_changes_guard();

CREATE TABLE financial_approval_policy_change_approvals (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    change_id             UUID NOT NULL UNIQUE REFERENCES financial_approval_policy_changes (id),
    tenant_id             UUID NULL,
    decision              TEXT NOT NULL CHECK (decision IN ('approve', 'reject')),
    content_hash          TEXT NOT NULL,
    decided_by            UUID NOT NULL,
    decided_by_scope      TEXT NOT NULL CHECK (decided_by_scope IN ('tenant', 'platform')),
    decided_by_person_id  UUID NOT NULL,
    decided_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_txid          BIGINT NOT NULL,
    reason_code           TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64)
);

CREATE INDEX financial_approval_policy_change_approvals_tenant ON financial_approval_policy_change_approvals (tenant_id);

CREATE FUNCTION financial_approval_policy_change_approvals_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor  RECORD;
    v_change financial_approval_policy_changes%ROWTYPE;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'financial_approval_policy_change_approvals: immutable' USING ERRCODE = 'MA012';
    END IF;
    SELECT * INTO v_actor FROM financial_actor_session();
    IF v_actor.scope NOT IN ('tenant', 'platform') THEN
        RAISE EXCEPTION 'financial_approval_policy_change_approvals: only a tenant or platform session may decide' USING ERRCODE = 'MA012';
    END IF;
    IF v_actor.person_id IS NULL THEN
        RAISE EXCEPTION 'financial_approval_policy_change_approvals: approver has no linked person_id' USING ERRCODE = 'MA002';
    END IF;

    SELECT * INTO v_change FROM financial_approval_policy_changes WHERE id = NEW.change_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'financial_approval_policy_change_approvals: change % not found', NEW.change_id USING ERRCODE = 'MA012';
    END IF;
    IF v_change.status <> 'pending' OR now() >= v_change.expires_at THEN
        RAISE EXCEPTION 'financial_approval_policy_change_approvals: change % is not pending or has expired', NEW.change_id USING ERRCODE = 'MA012';
    END IF;
    IF NEW.content_hash IS DISTINCT FROM v_change.content_hash
       OR v_change.content_hash IS DISTINCT FROM financial_approval_policy_change_content_hash(v_change) THEN
        RAISE EXCEPTION 'financial_approval_policy_change_approvals: content_hash does not match the proposed row' USING ERRCODE = 'MA012';
    END IF;
    IF v_actor.actor = v_change.requested_by OR v_actor.person_id = v_change.requested_by_person_id THEN
        RAISE EXCEPTION 'financial_approval_policy_change_approvals: requester and approver must be distinct principals and Persons' USING ERRCODE = 'MA012';
    END IF;

    -- §3.1 authorship matrix, approver side.
    IF v_change.change_kind = 'profile_assignment' OR v_change.level IN ('platform', 'jurisdiction', 'profile') THEN
        IF v_actor.scope <> 'platform' OR NOT financial_policy_actor_has_permission(v_actor.role, 'financial_policy:author') THEN
            RAISE EXCEPTION 'financial_approval_policy_change_approvals: only a platform financial_policy:author may approve this change' USING ERRCODE = 'MA012';
        END IF;
    ELSIF v_actor.scope = 'platform' THEN
        IF NOT financial_policy_actor_has_permission(v_actor.role, 'financial_policy:author') THEN
            RAISE EXCEPTION 'financial_approval_policy_change_approvals: platform approver lacks financial_policy:author' USING ERRCODE = 'MA012';
        END IF;
    ELSE
        IF v_change.tenant_id IS DISTINCT FROM v_actor.tenant OR NOT financial_policy_actor_has_permission(v_actor.role, 'financial_policy:tighten') THEN
            RAISE EXCEPTION 'financial_approval_policy_change_approvals: tenant approver must hold financial_policy:tighten in the change''s tenant' USING ERRCODE = 'MA012';
        END IF;
    END IF;
    IF NEW.decision = 'approve' AND v_change.change_kind = 'policy'
       AND v_change.level IN ('tenant', 'brand')
       AND NOT (v_change.requested_by_scope = 'platform' AND v_actor.scope = 'platform')
       AND NOT financial_approval_policy_change_tightens(v_change) THEN
        RAISE EXCEPTION 'financial_approval_policy_change_approvals: a non-tightening tenant/brand row needs a platform requester AND a platform approver' USING ERRCODE = 'MA010';
    END IF;

    NEW.tenant_id := v_change.tenant_id;
    NEW.decided_by := v_actor.actor;
    NEW.decided_by_scope := v_actor.scope;
    NEW.decided_by_person_id := v_actor.person_id;
    NEW.decided_at := now();
    NEW.decided_txid := txid_current();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER financial_approval_policy_change_approvals_guard
    BEFORE INSERT OR UPDATE ON financial_approval_policy_change_approvals
    FOR EACH ROW EXECUTE FUNCTION financial_approval_policy_change_approvals_guard();

-- =========================================================================
-- 5. financial_approval_policies (append-only; §10.2) and profiles (§10.3)
-- =========================================================================

CREATE TABLE financial_approval_policies (
    id                                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    operation_kind                      TEXT NOT NULL REFERENCES financial_control_classifications (operation_kind),
    level                               TEXT NOT NULL CHECK (level IN ('platform', 'jurisdiction', 'profile', 'tenant', 'brand')),
    tenant_id                           UUID NULL REFERENCES tenants (id),
    brand_id                            UUID NULL,
    jurisdiction_id                     UUID NULL REFERENCES jurisdictions (id),
    profile_code                        TEXT NULL,
    asset_code                          TEXT NULL REFERENCES assets (code),
    -- >= 0 here; the >= 1 mandatory-class rule is by trigger (the HD-PRH2-8
    -- interim is a single switch).
    base_required_approvals             INT NOT NULL CHECK (base_required_approvals >= 0),
    threshold_minor_units               NUMERIC(38,0) NULL CHECK (threshold_minor_units >= 0),
    required_approvals_above_threshold  INT NULL,
    effective_from                      TIMESTAMPTZ NOT NULL,
    supersedes_id                       UUID NULL REFERENCES financial_approval_policies (id),
    change_id                           UUID NOT NULL UNIQUE REFERENCES financial_approval_policy_changes (id),
    author_person_id                    UUID NOT NULL,
    approver_person_id                  UUID NOT NULL,
    legal_review_reference              TEXT NULL CHECK (octet_length(legal_review_reference) <= 256),
    created_at                          TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id),
    CHECK (level <> 'platform'     OR (tenant_id IS NULL AND brand_id IS NULL AND jurisdiction_id IS NULL AND profile_code IS NULL)),
    CHECK (level <> 'jurisdiction' OR (tenant_id IS NULL AND brand_id IS NULL AND jurisdiction_id IS NOT NULL AND profile_code IS NULL)),
    CHECK (level <> 'profile'      OR (tenant_id IS NULL AND brand_id IS NULL AND jurisdiction_id IS NULL AND profile_code IS NOT NULL)),
    CHECK (level <> 'tenant'       OR (tenant_id IS NOT NULL AND brand_id IS NULL AND jurisdiction_id IS NULL AND profile_code IS NULL)),
    CHECK (level <> 'brand'        OR (tenant_id IS NOT NULL AND brand_id IS NOT NULL AND jurisdiction_id IS NULL AND profile_code IS NULL)),
    CHECK ((threshold_minor_units IS NULL) = (required_approvals_above_threshold IS NULL)),
    -- LF-12: a threshold is per asset.
    CHECK (threshold_minor_units IS NULL OR asset_code IS NOT NULL),
    CHECK (required_approvals_above_threshold IS NULL OR required_approvals_above_threshold >= base_required_approvals)
);

-- One row per key per effective instant (so "the in-force row of a key"
-- is always unambiguous).
CREATE UNIQUE INDEX financial_approval_policies_key_instant ON financial_approval_policies (
    operation_kind, level, COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid),
    COALESCE(brand_id, '00000000-0000-0000-0000-000000000000'::uuid),
    COALESCE(jurisdiction_id, '00000000-0000-0000-0000-000000000000'::uuid),
    COALESCE(profile_code, ''), COALESCE(asset_code, ''), effective_from);
CREATE INDEX financial_approval_policies_tenant ON financial_approval_policies (tenant_id);

CREATE FUNCTION financial_approval_policies_guard() RETURNS TRIGGER AS $$
DECLARE
    v_appr   financial_approval_policy_change_approvals%ROWTYPE;
    v_change financial_approval_policy_changes%ROWTYPE;
    v_class  text;
    v_pred   RECORD;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'financial_approval_policies: append-only' USING ERRCODE = 'MA013';
    END IF;
    SELECT * INTO v_appr FROM financial_approval_policy_change_approvals
     WHERE change_id = NEW.change_id AND decision = 'approve' AND decided_txid = txid_current();
    IF NOT FOUND THEN
        RAISE EXCEPTION 'financial_approval_policies: a policy row is inserted only by its own same-transaction change approval' USING ERRCODE = 'MA013';
    END IF;
    SELECT * INTO v_change FROM financial_approval_policy_changes WHERE id = NEW.change_id;
    IF NOT FOUND OR v_change.change_kind <> 'policy' THEN
        RAISE EXCEPTION 'financial_approval_policies: change % is not a policy change', NEW.change_id USING ERRCODE = 'MA013';
    END IF;

    -- Every column is copied from the approved change, never supplied.
    NEW.operation_kind := v_change.operation_kind;
    NEW.level := v_change.level;
    NEW.tenant_id := v_change.tenant_id;
    NEW.brand_id := v_change.brand_id;
    NEW.jurisdiction_id := v_change.jurisdiction_id;
    NEW.profile_code := v_change.profile_code;
    NEW.asset_code := v_change.asset_code;
    NEW.base_required_approvals := v_change.base_required_approvals;
    NEW.threshold_minor_units := v_change.threshold_minor_units;
    NEW.required_approvals_above_threshold := v_change.required_approvals_above_threshold;
    NEW.legal_review_reference := v_change.legal_review_reference;
    NEW.author_person_id := v_change.requested_by_person_id;
    NEW.approver_person_id := v_appr.decided_by_person_id;
    -- Never back-dated: a past evaluation stays reproducible.
    NEW.effective_from := GREATEST(v_change.effective_from, now());
    NEW.created_at := now();

    -- HD-PRH2-8 interim (b), LF ruling 7: never below one independent
    -- approver for the mandatory class, at every level.
    SELECT class INTO v_class FROM financial_control_classifications WHERE operation_kind = NEW.operation_kind;
    IF v_class = 'mandatory_four_eyes' AND NEW.base_required_approvals < 1 THEN
        RAISE EXCEPTION 'financial_approval_policies: base_required_approvals must be >= 1 for the mandatory four-eyes class (HD-PRH2-8 interim (b))' USING ERRCODE = 'MA013';
    END IF;

    -- §3.1 matrix, binding: platform-level-family rows need platform/platform.
    IF NEW.level IN ('platform', 'jurisdiction', 'profile')
       AND NOT (v_change.requested_by_scope = 'platform' AND v_appr.decided_by_scope = 'platform') THEN
        RAISE EXCEPTION 'financial_approval_policies: platform/jurisdiction/profile rows need a platform requester and approver' USING ERRCODE = 'MA013';
    END IF;

    NEW.supersedes_id := financial_approval_policy_predecessor(NEW.operation_kind, NEW.level, NEW.tenant_id, NEW.brand_id,
        NEW.jurisdiction_id, NEW.profile_code, NEW.asset_code, NEW.effective_from);
    IF NEW.supersedes_id IS NOT NULL AND NEW.level IN ('tenant', 'brand')
       AND NOT (v_change.requested_by_scope = 'platform' AND v_appr.decided_by_scope = 'platform') THEN
        SELECT * INTO v_pred FROM financial_approval_policies WHERE id = NEW.supersedes_id;
        IF NOT financial_policy_is_tightening(v_pred.base_required_approvals, v_pred.threshold_minor_units, v_pred.required_approvals_above_threshold,
                NEW.base_required_approvals, NEW.threshold_minor_units, NEW.required_approvals_above_threshold) THEN
            RAISE EXCEPTION 'financial_approval_policies: a non-tightening tenant/brand row needs a platform requester AND a platform approver' USING ERRCODE = 'MA010';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER financial_approval_policies_guard
    BEFORE INSERT OR UPDATE ON financial_approval_policies
    FOR EACH ROW EXECUTE FUNCTION financial_approval_policies_guard();

CREATE TABLE tenant_financial_policy_profiles (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants (id),
    profile_code   TEXT NOT NULL,
    effective_from TIMESTAMPTZ NOT NULL,
    change_id      UUID NOT NULL UNIQUE REFERENCES financial_approval_policy_changes (id),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, effective_from)
);

CREATE FUNCTION tenant_financial_policy_profiles_guard() RETURNS TRIGGER AS $$
DECLARE
    v_appr   financial_approval_policy_change_approvals%ROWTYPE;
    v_change financial_approval_policy_changes%ROWTYPE;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'tenant_financial_policy_profiles: append-only' USING ERRCODE = 'MA013';
    END IF;
    SELECT * INTO v_appr FROM financial_approval_policy_change_approvals
     WHERE change_id = NEW.change_id AND decision = 'approve' AND decided_txid = txid_current() AND decided_by_scope = 'platform';
    IF NOT FOUND THEN
        RAISE EXCEPTION 'tenant_financial_policy_profiles: inserted only by a same-transaction platform change approval' USING ERRCODE = 'MA013';
    END IF;
    SELECT * INTO v_change FROM financial_approval_policy_changes WHERE id = NEW.change_id;
    IF NOT FOUND OR v_change.change_kind <> 'profile_assignment' OR v_change.requested_by_scope <> 'platform' THEN
        RAISE EXCEPTION 'tenant_financial_policy_profiles: change % is not a platform profile assignment', NEW.change_id USING ERRCODE = 'MA013';
    END IF;
    NEW.tenant_id := v_change.tenant_id;
    NEW.profile_code := v_change.profile_code;
    NEW.effective_from := GREATEST(v_change.effective_from, now());
    NEW.created_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER tenant_financial_policy_profiles_guard
    BEFORE INSERT OR UPDATE ON tenant_financial_policy_profiles
    FOR EACH ROW EXECUTE FUNCTION tenant_financial_policy_profiles_guard();

-- AFTER INSERT on a change approval: apply the decision in the same
-- transaction (the K1 staff_capability_grant_approvals precedent).
CREATE FUNCTION financial_approval_policy_change_approvals_apply() RETURNS TRIGGER AS $$
DECLARE
    v_change financial_approval_policy_changes%ROWTYPE;
BEGIN
    SELECT * INTO v_change FROM financial_approval_policy_changes WHERE id = NEW.change_id;
    IF NEW.decision = 'approve' THEN
        IF v_change.change_kind = 'policy' THEN
            INSERT INTO financial_approval_policies (operation_kind, level, tenant_id, brand_id, jurisdiction_id, profile_code, asset_code,
                base_required_approvals, threshold_minor_units, required_approvals_above_threshold, effective_from, change_id,
                author_person_id, approver_person_id, legal_review_reference)
            VALUES (v_change.operation_kind, v_change.level, v_change.tenant_id, v_change.brand_id, v_change.jurisdiction_id,
                v_change.profile_code, v_change.asset_code, v_change.base_required_approvals, v_change.threshold_minor_units,
                v_change.required_approvals_above_threshold, v_change.effective_from, v_change.id,
                v_change.requested_by_person_id, NEW.decided_by_person_id, v_change.legal_review_reference);
        ELSE
            INSERT INTO tenant_financial_policy_profiles (tenant_id, profile_code, effective_from, change_id)
            VALUES (v_change.tenant_id, v_change.profile_code, v_change.effective_from, v_change.id);
        END IF;
    END IF;
    UPDATE financial_approval_policy_changes
       SET status = CASE WHEN NEW.decision = 'approve' THEN 'approved' ELSE 'rejected' END
     WHERE id = NEW.change_id;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER financial_approval_policy_change_approvals_apply
    AFTER INSERT ON financial_approval_policy_change_approvals
    FOR EACH ROW EXECUTE FUNCTION financial_approval_policy_change_approvals_apply();

-- =========================================================================
-- 6. Policy evaluation (§3.2): the ONE implementation (no Go copy)
-- =========================================================================

-- financial_policy_required_approvals: for the mandatory class,
-- required = GREATEST(1, MAX(row requirement over applicable rows)) (LF
-- ruling 7); enabled only with an in-force platform-level row (fail
-- closed, nothing seeded); a jurisdiction row + unresolvable tenant
-- jurisdiction => disabled; K2-1 (§6.7): payment_force_resolve on a
-- non-active tenant ignores tenant/brand rows.
CREATE FUNCTION financial_policy_required_approvals(
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

-- The Persons who authored or approved any of the given policy rows (S-2(iii)).
CREATE FUNCTION financial_policy_author_persons(p_policy_ids uuid[]) RETURNS uuid[] AS $$
    SELECT COALESCE(array_agg(DISTINCT x), '{}') FROM (
        SELECT author_person_id AS x FROM financial_approval_policies WHERE id = ANY (p_policy_ids)
        UNION ALL
        SELECT approver_person_id FROM financial_approval_policies WHERE id = ANY (p_policy_ids)) s;
$$ LANGUAGE sql STABLE;

-- =========================================================================
-- 7. The preventive exposure check (§5.2; LF F4, ruling 2, K2-a)
-- =========================================================================

-- Tombstone-only clearing key (K2-a): the tombstone writer keys on the
-- ORIGINAL reference (provider_id, provider_tx_id = provider_reference).
-- A NULL provider_reference keeps the exposure true (fail closed).
CREATE FUNCTION player_open_payment_exposure(p_tenant uuid, p_player uuid) RETURNS boolean AS $$
    SELECT EXISTS (
        SELECT 1
          FROM payment_attempts a
          JOIN deposit_intents i ON i.id = a.deposit_intent_id AND i.tenant_id = a.tenant_id
         WHERE a.tenant_id = p_tenant AND i.player_account_id = p_player
           AND a.operation = 'deposit' AND a.state = 'disputed'
           AND a.terminal_reason = 'multiple_success_for_intent'
           AND NOT EXISTS (
               SELECT 1 FROM ledger_transactions t
                WHERE t.tenant_id = a.tenant_id
                  AND t.transaction_type = 'tombstone'
                  AND t.provider_id = a.provider_id
                  AND t.provider_tx_id = a.provider_reference));
$$ LANGUAGE sql STABLE;

-- =========================================================================
-- 8. Suspended asset (§5.2, LF ruling 3 + K2-b; exact 0045 predicate pinned)
-- =========================================================================

-- Suspended when the platform layers are off (assets.active = false OR
-- assets.platform_authorized = false; 0044) OR the 0045 TENANT layer is
-- not in force: there is no tenant-scope, all-products (product IS NULL)
-- asset_authorizations row for (tenant, asset) with eligible = true. (0045
-- rows are not effective-dated; "not in force" = absent or eligible =
-- false. A product-specific row does not authorize a cash adjustment.)
CREATE FUNCTION ledger_adjustment_asset_suspended(p_tenant uuid, p_asset text) RETURNS boolean AS $$
    SELECT NOT EXISTS (SELECT 1 FROM assets a WHERE a.code = p_asset AND a.active AND a.platform_authorized)
        OR NOT EXISTS (
            SELECT 1 FROM asset_authorizations aa
             WHERE aa.tenant_id = p_tenant AND aa.asset_code = p_asset
               AND aa.scope_kind = 'tenant' AND aa.product IS NULL AND aa.eligible);
$$ LANGUAGE sql STABLE;

-- =========================================================================
-- 9. ledger_adjustment_requests (§10.5; families T, A - no P)
-- =========================================================================

CREATE TABLE ledger_adjustment_requests (
    id                          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Payload (§5.1), immutable after submission.
    tenant_id                   UUID NOT NULL REFERENCES tenants (id),
    wallet_id                   UUID NOT NULL,
    player_account_id           UUID NOT NULL,
    brand_id                    UUID NOT NULL,
    account_type                TEXT NOT NULL DEFAULT 'player_cash' CHECK (account_type = 'player_cash'),
    asset_code                  TEXT NOT NULL REFERENCES assets (code),
    direction                   TEXT NOT NULL CHECK (direction IN ('credit_player', 'debit_player')),
    amount                      NUMERIC(38,0) NOT NULL CHECK (amount > 0 AND amount <= 9223372036854775807),
    reason_code                 TEXT NOT NULL REFERENCES ledger_adjustment_reason_codes (reason_code),
    causation_transaction_id    UUID NULL,
    evidence_ref_hash           TEXT NULL CHECK (evidence_ref_hash ~ '^[0-9a-f]{64}$'),
    note_hash                   TEXT NOT NULL CHECK (note_hash ~ '^[0-9a-f]{64}$'),
    payload_hash                TEXT NOT NULL,
    -- Forced actor (§7.2).
    initiated_by                UUID NOT NULL,
    initiated_by_scope          TEXT NOT NULL CHECK (initiated_by_scope IN ('tenant', 'platform_acting')),
    initiated_by_person_id      UUID NOT NULL,
    -- Pinned at submission (§3.6).
    tenant_status_at_submission TEXT NOT NULL,
    required_at_submission      INT NOT NULL CHECK (required_at_submission >= 1),
    contributing_policy_ids     UUID[] NOT NULL,
    -- Recorded at execution.
    tenant_status_at_execution  TEXT NULL,
    required_at_execution       INT NULL,
    contributing_policy_ids_at_execution UUID[] NULL,
    state                       TEXT NOT NULL DEFAULT 'pending' CHECK (state IN (
                                    'pending', 'executing', 'executed', 'refused_insufficient_funds',
                                    'refused_at_execution', 'rejected', 'cancelled', 'expired')),
    expires_at                  TIMESTAMPTZ NOT NULL,
    executed_txid               BIGINT NULL,
    ledger_transaction_id       UUID NULL UNIQUE,
    refusal_code                TEXT NULL CHECK (refusal_code IS NULL OR octet_length(refusal_code) BETWEEN 1 AND 64),
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at                   TIMESTAMPTZ NULL,
    -- LF-14: the posting key, DB-derived.
    idempotency_key             TEXT GENERATED ALWAYS AS ('manual_adjustment:' || id::text) STORED,
    UNIQUE (tenant_id, idempotency_key),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (wallet_id, tenant_id) REFERENCES wallets (id, tenant_id),
    FOREIGN KEY (causation_transaction_id, tenant_id) REFERENCES ledger_transactions (id, tenant_id),
    FOREIGN KEY (ledger_transaction_id, tenant_id) REFERENCES ledger_transactions (id, tenant_id),
    CHECK ((state = 'executed') = (ledger_transaction_id IS NOT NULL)),
    CHECK (state NOT IN ('refused_insufficient_funds', 'refused_at_execution') OR ledger_transaction_id IS NULL),
    CHECK ((state = 'refused_at_execution') = (refusal_code IS NOT NULL)),
    CHECK (state NOT IN ('executing', 'executed', 'refused_insufficient_funds') OR executed_txid IS NOT NULL)
);

CREATE INDEX ledger_adjustment_requests_tenant_state ON ledger_adjustment_requests (tenant_id, state);
CREATE INDEX ledger_adjustment_requests_causation ON ledger_adjustment_requests (tenant_id, causation_transaction_id) WHERE causation_transaction_id IS NOT NULL;

CREATE FUNCTION ledger_adjustment_payload_hash(r ledger_adjustment_requests) RETURNS text AS $$
    SELECT k2_sha256_hex(k2_canonical(
        r.tenant_id::text, r.wallet_id::text, r.player_account_id::text, r.brand_id::text, r.account_type,
        r.asset_code, r.direction, r.amount::text, r.reason_code, r.causation_transaction_id::text,
        r.evidence_ref_hash, r.note_hash));
$$ LANGUAGE sql IMMUTABLE;

-- The §5.2/§5.4 payload rules shared by submission (raised) and execution
-- (recorded as refusal_code). Returns NULL when the payload is admissible,
-- else "<SQLSTATE>:<refusal code>". p_self excludes this request from the
-- executed-compensation sum.
CREATE FUNCTION ledger_adjustment_payload_refusal(
    p_self uuid, p_tenant uuid, p_wallet uuid, p_player uuid, p_asset text, p_direction text,
    p_amount numeric, p_reason text, p_causation uuid, p_evidence text, p_tenant_status text
) RETURNS text AS $$
DECLARE
    v_rc         ledger_adjustment_reason_codes%ROWTYPE;
    v_wallet     RECORD;
    v_cause_type text;
    v_leg_sum    numeric;
    v_leg_count  int;
    v_executed   numeric;
BEGIN
    SELECT * INTO v_rc FROM ledger_adjustment_reason_codes WHERE reason_code = p_reason;
    IF NOT FOUND THEN
        RETURN 'MA022:unknown_reason_code';
    END IF;
    IF NOT (p_direction = ANY (v_rc.allowed_directions)) THEN
        RETURN 'MA022:direction_not_allowed_for_reason';
    END IF;

    SELECT w.asset_code, w.player_account_id, w.tenant_id INTO v_wallet FROM wallets w WHERE w.id = p_wallet;
    IF NOT FOUND OR v_wallet.tenant_id <> p_tenant OR v_wallet.player_account_id <> p_player THEN
        RETURN 'MA021:wallet_not_found';
    END IF;
    IF v_wallet.asset_code <> p_asset OR NOT EXISTS (SELECT 1 FROM assets WHERE code = p_asset) THEN
        RETURN 'MA021:asset_mismatch';
    END IF;
    IF NOT v_rc.allowed_in_suspended_asset AND ledger_adjustment_asset_suspended(p_tenant, p_asset) THEN
        RETURN 'MA021:asset_suspended';
    END IF;
    IF NOT v_rc.allowed_for_non_active_tenant AND p_tenant_status IS DISTINCT FROM 'active' THEN
        RETURN 'MA023:tenant_not_active';
    END IF;
    IF v_rc.evidence_required AND p_evidence IS NULL THEN
        RETURN 'MA022:evidence_required';
    END IF;

    IF v_rc.causation_rule = 'forbidden' AND p_causation IS NOT NULL THEN
        RETURN 'MA022:causation_forbidden';
    END IF;
    IF v_rc.causation_rule = 'required_compensation' AND p_causation IS NULL THEN
        RETURN 'MA022:causation_required';
    END IF;
    IF p_causation IS NOT NULL THEN
        SELECT t.transaction_type INTO v_cause_type FROM ledger_transactions t WHERE t.id = p_causation AND t.tenant_id = p_tenant;
        IF NOT FOUND THEN
            RETURN 'MA022:causation_not_found';
        END IF;
        -- Causation to deposits, reversals and tombstones is refused for
        -- every code (LF ruling 1; INV-DEP-1).
        IF v_cause_type IN ('deposit', 'deposit_reversal', 'tombstone') THEN
            RETURN 'MA022:causation_type_refused';
        END IF;
        SELECT COALESCE(sum(e.amount), 0), count(*) INTO v_leg_sum, v_leg_count
          FROM ledger_entries e
          JOIN ledger_accounts la ON la.id = e.ledger_account_id
         WHERE e.ledger_transaction_id = p_causation AND e.tenant_id = p_tenant
           AND la.wallet_id = p_wallet AND la.account_type = 'player_cash' AND e.asset_code = p_asset;
        IF v_leg_count = 0 THEN
            RETURN 'MA022:causation_not_on_wallet';
        END IF;
        IF v_rc.causation_rule = 'required_compensation' THEN
            -- INV-ADJ-6: cumulative compensation per (causation, direction)
            -- never exceeds the causation's player_cash leg on this wallet.
            SELECT COALESCE(sum(r.amount), 0) INTO v_executed
              FROM ledger_adjustment_requests r
             WHERE r.tenant_id = p_tenant AND r.causation_transaction_id = p_causation
               AND r.direction = p_direction AND r.reason_code = 'compensating_entry'
               AND r.state = 'executed' AND r.id IS DISTINCT FROM p_self;
            IF v_executed + p_amount > v_leg_sum THEN
                RETURN 'MA022:compensation_cap_exceeded';
            END IF;
        END IF;
    END IF;

    -- LF F4 / ruling 2 (PREVENTIVE, no override): every credit is refused
    -- while the player has an open captured-unposted exposure.
    IF p_direction = 'credit_player' AND player_open_payment_exposure(p_tenant, p_player) THEN
        RETURN 'MA020:open_payment_exposure';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql STABLE;

-- ledger_adjustment_actor_eligible: the actor's live staff row (visible to
-- this session) is active, has an eligible role for the capability's
-- catalogue row, sits in the right tenant for its scope, and holds a grant
-- IN FORCE AT now() for (tenant, actor, capability) whose request-time
-- grantee_person_id equals the live person_id (ADR 0099 §7.4/§7.5,
-- K2-G4, architect I-5). Returns the grant id, or NULL.
CREATE FUNCTION ledger_adjustment_eligible_grant(p_tenant uuid, p_staff uuid, p_capability text) RETURNS uuid AS $$
BEGIN
    RETURN (
        SELECT g.id
          FROM staff_capability_grants g
          JOIN staff_capability_grant_requests gr ON gr.id = g.request_id
          JOIN financial_capability_catalogue c ON c.capability = g.capability
          JOIN staff_users s ON s.id = g.grantee_staff_id
         WHERE g.tenant_id = p_tenant AND g.grantee_staff_id = p_staff AND g.capability = p_capability
           AND g.revoked_at IS NULL AND g.valid_from <= now() AND (g.valid_until IS NULL OR now() < g.valid_until)
           AND s.status = 'active'
           AND s.person_id IS NOT NULL
           AND s.person_id = gr.grantee_person_id
           AND ((g.grantee_scope = 'tenant' AND s.tenant_id = g.tenant_id AND s.role = ANY (c.eligible_tenant_roles))
             OR (g.grantee_scope = 'platform' AND s.tenant_id IS NULL AND s.role = 'platform_admin' AND c.platform_grantee_allowed))
         ORDER BY g.id
         LIMIT 1);
END;
$$ LANGUAGE plpgsql STABLE;

-- For a platform principal whose staff row this session cannot see (ADR
-- 0099 §7.6 cross-family visibility limit: a tenant session never sees a
-- platform row; an acting session sees only its own), only the grant can
-- be re-checked at use time: in force at now(), unrevoked, and its
-- request-time person snapshot equal to the approval-time person. Staff
-- status for that principal was re-checked at its own approval time. This
-- is the ADR's stated residual; grant revoke is the emergency stop.
CREATE FUNCTION ledger_adjustment_invisible_platform_grant(p_tenant uuid, p_staff uuid, p_capability text, p_person uuid) RETURNS uuid AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM staff_users s WHERE s.id = p_staff) THEN
        RETURN NULL; -- visible: the live check applies instead
    END IF;
    RETURN (
        SELECT g.id
          FROM staff_capability_grants g
          JOIN staff_capability_grant_requests gr ON gr.id = g.request_id
         WHERE g.tenant_id = p_tenant AND g.grantee_staff_id = p_staff AND g.capability = p_capability
           AND g.grantee_scope = 'platform'
           AND g.revoked_at IS NULL AND g.valid_from <= now() AND (g.valid_until IS NULL OR now() < g.valid_until)
           AND gr.grantee_person_id = p_person
         ORDER BY g.id
         LIMIT 1);
END;
$$ LANGUAGE plpgsql STABLE;

-- Live person of a staff principal visible to this session, else the
-- supplied snapshot (the §7.6 residual above).
CREATE FUNCTION ledger_adjustment_live_person(p_staff uuid, p_snapshot uuid) RETURNS uuid AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM staff_users s WHERE s.id = p_staff) THEN
        RETURN (SELECT s.person_id FROM staff_users s WHERE s.id = p_staff);
    END IF;
    RETURN p_snapshot;
END;
$$ LANGUAGE plpgsql STABLE;

CREATE FUNCTION ledger_adjustment_requests_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor   RECORD;
    v_wallet  RECORD;
    v_policy  RECORD;
    v_refusal text;
BEGIN
    SELECT * INTO v_actor FROM financial_actor_session();

    IF TG_OP = 'INSERT' THEN
        IF v_actor.scope NOT IN ('tenant', 'platform_acting') THEN
            RAISE EXCEPTION 'ledger_adjustment_requests: only a tenant or acting session may initiate (HD-PRH2-6)' USING ERRCODE = 'MA001';
        END IF;
        IF v_actor.person_id IS NULL THEN
            RAISE EXCEPTION 'ledger_adjustment_requests: initiator has no linked person_id' USING ERRCODE = 'MA002';
        END IF;
        IF NEW.tenant_id IS DISTINCT FROM v_actor.tenant THEN
            RAISE EXCEPTION 'ledger_adjustment_requests: request tenant must be the session tenant' USING ERRCODE = 'MA001';
        END IF;
        IF ledger_adjustment_eligible_grant(NEW.tenant_id, v_actor.actor, 'ledger_adjustment:initiate') IS NULL THEN
            RAISE EXCEPTION 'ledger_adjustment_requests: initiator lacks an eligible role or an in-force ledger_adjustment:initiate grant' USING ERRCODE = 'MA003';
        END IF;

        SELECT w.player_account_id, w.brand_id, w.asset_code INTO v_wallet FROM wallets w
         WHERE w.id = NEW.wallet_id AND w.tenant_id = NEW.tenant_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'ledger_adjustment_requests: wallet % not found in tenant', NEW.wallet_id USING ERRCODE = 'MA021';
        END IF;
        NEW.player_account_id := v_wallet.player_account_id;
        NEW.brand_id := v_wallet.brand_id;
        NEW.account_type := 'player_cash';

        NEW.initiated_by := v_actor.actor;
        NEW.initiated_by_scope := v_actor.scope;
        NEW.initiated_by_person_id := v_actor.person_id;
        NEW.state := 'pending';
        NEW.created_at := now();
        NEW.closed_at := NULL;
        NEW.expires_at := now() + interval '24 hours';
        NEW.executed_txid := NULL;
        NEW.ledger_transaction_id := NULL;
        NEW.refusal_code := NULL;
        NEW.tenant_status_at_execution := NULL;
        NEW.required_at_execution := NULL;
        NEW.contributing_policy_ids_at_execution := NULL;

        SELECT * INTO v_policy FROM financial_policy_required_approvals('ledger_adjustment', NEW.tenant_id, NEW.brand_id, NEW.asset_code, NEW.amount, now());
        NEW.tenant_status_at_submission := COALESCE(v_policy.tenant_status, 'unknown');
        IF NOT v_policy.enabled THEN
            RAISE EXCEPTION 'ledger_adjustment_requests: ledger_adjustment is disabled for this tenant (no in-force platform baseline, HD-PRH2-3)' USING ERRCODE = 'MA014';
        END IF;
        NEW.required_at_submission := v_policy.required;
        NEW.contributing_policy_ids := v_policy.contributing_policy_ids;

        -- S-2(iii) (C-100-1): a contributing policy's author/approver may not initiate.
        IF v_actor.person_id = ANY (financial_policy_author_persons(NEW.contributing_policy_ids)) THEN
            RAISE EXCEPTION 'ledger_adjustment_requests: the initiator authored or approved a contributing policy (S-2(iii))' USING ERRCODE = 'MA011';
        END IF;

        v_refusal := ledger_adjustment_payload_refusal(NEW.id, NEW.tenant_id, NEW.wallet_id, NEW.player_account_id, NEW.asset_code,
            NEW.direction, NEW.amount, NEW.reason_code, NEW.causation_transaction_id, NEW.evidence_ref_hash, NEW.tenant_status_at_submission);
        IF v_refusal IS NOT NULL THEN
            RAISE EXCEPTION 'ledger_adjustment_requests: refused: %', split_part(v_refusal, ':', 2) USING ERRCODE = split_part(v_refusal, ':', 1);
        END IF;

        NEW.payload_hash := ledger_adjustment_payload_hash(NEW);
        RETURN NEW;
    END IF;

    -- UPDATE --------------------------------------------------------------
    IF OLD.state NOT IN ('pending', 'executing') THEN
        RAISE EXCEPTION 'ledger_adjustment_requests: request % is terminal (%)', OLD.id, OLD.state USING ERRCODE = 'MA030';
    END IF;
    -- Payload, actor and pins are immutable (LF-10): only the state
    -- columns may change.
    IF (to_jsonb(NEW) - ARRAY['idempotency_key', 'state', 'executed_txid', 'ledger_transaction_id', 'refusal_code', 'closed_at',
                              'tenant_status_at_execution', 'required_at_execution', 'contributing_policy_ids_at_execution'])
       IS DISTINCT FROM
       (to_jsonb(OLD) - ARRAY['idempotency_key', 'state', 'executed_txid', 'ledger_transaction_id', 'refusal_code', 'closed_at',
                              'tenant_status_at_execution', 'required_at_execution', 'contributing_policy_ids_at_execution']) THEN
        RAISE EXCEPTION 'ledger_adjustment_requests: the payload, actor and pinned policy are immutable' USING ERRCODE = 'MA030';
    END IF;
    IF NEW.payload_hash IS DISTINCT FROM ledger_adjustment_payload_hash(NEW) THEN
        RAISE EXCEPTION 'ledger_adjustment_requests: payload_hash does not match the payload' USING ERRCODE = 'MA030';
    END IF;
    IF NEW.state IN ('executed', 'refused_insufficient_funds', 'refused_at_execution', 'rejected', 'cancelled', 'expired') THEN
        NEW.closed_at := now();
    END IF;
    -- Security K2-C1 (i): every NON-executed exit refuses while a ledger
    -- transaction carrying this request's governed key exists (in any
    -- state the session can see, including one posted earlier in this very
    -- transaction). Only the executed exit may leave a posting behind, and
    -- that exit runs ledger_adjustment_verify_link.
    IF NEW.state IN ('refused_insufficient_funds', 'refused_at_execution', 'rejected', 'cancelled', 'expired')
       AND EXISTS (SELECT 1 FROM ledger_transactions t
                    WHERE t.tenant_id = OLD.tenant_id AND t.idempotency_key = 'manual_adjustment:' || OLD.id::text) THEN
        RAISE EXCEPTION 'ledger_adjustment_requests: % refused - a ledger transaction with this request''s governed key exists', NEW.state
            USING ERRCODE = 'MA040';
    END IF;

    IF OLD.state = 'pending' AND NEW.state = 'cancelled' THEN
        IF v_actor.actor IS DISTINCT FROM OLD.initiated_by THEN
            RAISE EXCEPTION 'ledger_adjustment_requests: only the initiator may cancel' USING ERRCODE = 'MA030';
        END IF;
        NEW.executed_txid := NULL; NEW.ledger_transaction_id := NULL; NEW.refusal_code := NULL;
        RETURN NEW;
    ELSIF OLD.state = 'pending' AND NEW.state = 'expired' THEN
        IF now() < OLD.expires_at THEN
            RAISE EXCEPTION 'ledger_adjustment_requests: request has not expired' USING ERRCODE = 'MA030';
        END IF;
        NEW.executed_txid := NULL; NEW.ledger_transaction_id := NULL; NEW.refusal_code := NULL;
        RETURN NEW;
    ELSIF OLD.state = 'pending' AND NEW.state = 'rejected' THEN
        IF NOT EXISTS (SELECT 1 FROM ledger_adjustment_approvals a
                        WHERE a.request_id = OLD.id AND a.decision = 'reject' AND a.decided_txid = txid_current()) THEN
            RAISE EXCEPTION 'ledger_adjustment_requests: rejected only via a same-transaction reject decision' USING ERRCODE = 'MA030';
        END IF;
        NEW.executed_txid := NULL; NEW.ledger_transaction_id := NULL; NEW.refusal_code := NULL;
        RETURN NEW;
    ELSIF OLD.state = 'pending' AND NEW.state IN ('refused_at_execution', 'executing') THEN
        -- Only in the final approval's own transaction (LF-13).
        IF NOT EXISTS (SELECT 1 FROM ledger_adjustment_approvals a
                        WHERE a.request_id = OLD.id AND a.decision = 'approve' AND a.decided_txid = txid_current()) THEN
            RAISE EXCEPTION 'ledger_adjustment_requests: execution happens only in the final approval''s transaction' USING ERRCODE = 'MA030';
        END IF;
        IF now() >= OLD.expires_at THEN
            RAISE EXCEPTION 'ledger_adjustment_requests: request has expired' USING ERRCODE = 'MA030';
        END IF;
        IF NEW.state = 'refused_at_execution' THEN
            IF NEW.refusal_code IS NULL THEN
                RAISE EXCEPTION 'ledger_adjustment_requests: refused_at_execution needs a refusal_code' USING ERRCODE = 'MA030';
            END IF;
            NEW.executed_txid := NULL; NEW.ledger_transaction_id := NULL;
            RETURN NEW;
        END IF;
        -- -> executing: the DB re-verifies the count (INV-ADJ-2/3) with the
        -- same function the executor used, and the payload rules.
        DECLARE
            v_exec RECORD;
        BEGIN
            SELECT * INTO v_exec FROM ledger_adjustment_execution_status(OLD.id);
            IF NOT v_exec.enabled OR NOT v_exec.initiator_valid OR v_exec.counted < v_exec.required THEN
                RAISE EXCEPTION 'ledger_adjustment_requests: fewer counted approvals (%) than required (%), or the initiator no longer qualifies',
                    v_exec.counted, v_exec.required USING ERRCODE = 'MA030';
            END IF;
            v_refusal := ledger_adjustment_payload_refusal(OLD.id, OLD.tenant_id, OLD.wallet_id, OLD.player_account_id, OLD.asset_code,
                OLD.direction, OLD.amount, OLD.reason_code, OLD.causation_transaction_id, OLD.evidence_ref_hash, v_exec.tenant_status);
            IF v_refusal IS NOT NULL THEN
                RAISE EXCEPTION 'ledger_adjustment_requests: execution check failed: %', split_part(v_refusal, ':', 2) USING ERRCODE = 'MA030';
            END IF;
            NEW.required_at_execution := v_exec.required;
            NEW.contributing_policy_ids_at_execution := v_exec.contributing_policy_ids;
            NEW.tenant_status_at_execution := v_exec.tenant_status;
        END;
        NEW.executed_txid := txid_current();
        NEW.ledger_transaction_id := NULL;
        NEW.refusal_code := NULL;
        RETURN NEW;
    ELSIF OLD.state = 'executing' AND NEW.state IN ('executed', 'refused_insufficient_funds') THEN
        IF OLD.executed_txid IS DISTINCT FROM txid_current() OR NEW.executed_txid IS DISTINCT FROM OLD.executed_txid THEN
            RAISE EXCEPTION 'ledger_adjustment_requests: executing -> % only in the executing transaction', NEW.state USING ERRCODE = 'MA030';
        END IF;
        IF NEW.state = 'refused_insufficient_funds' THEN
            IF NEW.ledger_transaction_id IS NOT NULL THEN
                RAISE EXCEPTION 'ledger_adjustment_requests: refused_insufficient_funds posts nothing' USING ERRCODE = 'MA030';
            END IF;
            RETURN NEW;
        END IF;
        PERFORM ledger_adjustment_verify_link(NEW);
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'ledger_adjustment_requests: invalid transition % -> %', OLD.state, NEW.state USING ERRCODE = 'MA030';
END;
$$ LANGUAGE plpgsql;

-- The beneficiary guard (S-12), a separate named trigger (ADR 0100 §6.3).
-- Self-contained (it derives the beneficiary from the wallet and the
-- actor's Person from the session itself), so it does not depend on
-- BEFORE-trigger firing order (alphabetical: this one fires first).
CREATE FUNCTION ledger_adjustment_requests_beneficiary_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor         RECORD;
    v_player_person uuid;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RETURN NEW;
    END IF;
    SELECT * INTO v_actor FROM financial_actor_session();
    IF v_actor.scope NOT IN ('tenant', 'platform_acting') THEN
        RAISE EXCEPTION 'ledger_adjustment_requests: only a tenant or acting session may initiate (HD-PRH2-6)' USING ERRCODE = 'MA001';
    END IF;
    SELECT pa.person_id INTO v_player_person
      FROM wallets w JOIN player_accounts pa ON pa.id = w.player_account_id AND pa.tenant_id = w.tenant_id
     WHERE w.id = NEW.wallet_id AND w.tenant_id = NEW.tenant_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger_adjustment_requests: beneficiary player not found' USING ERRCODE = 'MA032';
    END IF;
    -- An unlinked staff Person is refused too (§6.3).
    IF v_actor.person_id IS NULL OR v_actor.person_id = v_player_person THEN
        RAISE EXCEPTION 'ledger_adjustment_requests: the initiator may not be the beneficiary (S-12)' USING ERRCODE = 'MA032';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- The link trigger body (LF-10): an executed request's ledger transaction
-- is a same-tenant manual_adjustment carrying the §5.3 keys and EXACTLY the
-- two §4 entries equal to the payload.
CREATE FUNCTION ledger_adjustment_verify_link(r ledger_adjustment_requests) RETURNS void AS $$
DECLARE
    v_tx          RECORD;
    v_player_dir  text;
    v_house_dir   text;
    v_n           int;
    v_ok          int;
BEGIN
    IF r.ledger_transaction_id IS NULL THEN
        RAISE EXCEPTION 'ledger_adjustment_requests: executed needs a ledger_transaction_id' USING ERRCODE = 'MA040';
    END IF;
    SELECT * INTO v_tx FROM ledger_transactions t WHERE t.id = r.ledger_transaction_id AND t.tenant_id = r.tenant_id;
    IF NOT FOUND
       OR v_tx.transaction_type <> 'manual_adjustment'
       OR v_tx.idempotency_key <> 'manual_adjustment:' || r.id::text
       OR v_tx.correlation_id <> r.id
       OR v_tx.causation_id IS DISTINCT FROM r.causation_transaction_id
       OR v_tx.reason_code IS DISTINCT FROM r.reason_code
       OR v_tx.provider_id IS NOT NULL OR v_tx.provider_tx_id IS NOT NULL
       OR v_tx.reverses_transaction_id IS NOT NULL THEN
        RAISE EXCEPTION 'ledger_adjustment_requests: linked transaction does not carry the request''s keys' USING ERRCODE = 'MA040';
    END IF;
    v_player_dir := CASE WHEN r.direction = 'credit_player' THEN 'credit' ELSE 'debit' END;
    v_house_dir  := CASE WHEN r.direction = 'credit_player' THEN 'debit' ELSE 'credit' END;
    SELECT count(*) INTO v_n FROM ledger_entries e WHERE e.ledger_transaction_id = r.ledger_transaction_id;
    SELECT count(*) INTO v_ok
      FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.ledger_transaction_id = r.ledger_transaction_id
       AND e.tenant_id = r.tenant_id AND e.asset_code = r.asset_code AND e.amount = r.amount
       AND ((la.account_type = 'player_cash' AND la.wallet_id = r.wallet_id AND e.direction = v_player_dir)
         OR (la.account_type = 'manual_adjustment' AND la.wallet_id IS NULL AND la.tenant_id = r.tenant_id AND e.direction = v_house_dir));
    IF v_n <> 2 OR v_ok <> 2
       OR NOT EXISTS (SELECT 1 FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
                       WHERE e.ledger_transaction_id = r.ledger_transaction_id AND la.account_type = 'player_cash')
       OR NOT EXISTS (SELECT 1 FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
                       WHERE e.ledger_transaction_id = r.ledger_transaction_id AND la.account_type = 'manual_adjustment') THEN
        RAISE EXCEPTION 'ledger_adjustment_requests: linked transaction entries are not exactly the approved §4 shape' USING ERRCODE = 'MA040';
    END IF;
END;
$$ LANGUAGE plpgsql STABLE;

CREATE TRIGGER ledger_adjustment_requests_guard
    BEFORE INSERT OR UPDATE ON ledger_adjustment_requests
    FOR EACH ROW EXECUTE FUNCTION ledger_adjustment_requests_guard();

CREATE TRIGGER ledger_adjustment_requests_beneficiary_guard
    BEFORE INSERT ON ledger_adjustment_requests
    FOR EACH ROW EXECUTE FUNCTION ledger_adjustment_requests_beneficiary_guard();

-- A DEFERRABLE INITIALLY DEFERRED constraint trigger refuses to commit any
-- request left in 'executing' (§6.5).
CREATE FUNCTION ledger_adjustment_requests_no_executing_commit() RETURNS TRIGGER AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM ledger_adjustment_requests r WHERE r.id = NEW.id AND r.state = 'executing') THEN
        RAISE EXCEPTION 'ledger_adjustment_requests: request % may not commit in state executing', NEW.id USING ERRCODE = 'MA041';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER ledger_adjustment_requests_no_executing_commit
    AFTER INSERT OR UPDATE ON ledger_adjustment_requests
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_adjustment_requests_no_executing_commit();

-- =========================================================================
-- 10. ledger_adjustment_approvals (§10.6; T, A - no P)
-- =========================================================================

CREATE TABLE ledger_adjustment_approvals (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL,
    request_id            UUID NOT NULL,
    decision              TEXT NOT NULL CHECK (decision IN ('approve', 'reject')),
    payload_hash          TEXT NOT NULL,
    decided_by            UUID NOT NULL,
    decided_by_scope      TEXT NOT NULL CHECK (decided_by_scope IN ('tenant', 'platform_acting')),
    decided_by_person_id  UUID NOT NULL,
    decided_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_txid          BIGINT NOT NULL,
    reason_code           TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64),
    UNIQUE (request_id, decided_by),
    FOREIGN KEY (tenant_id, request_id) REFERENCES ledger_adjustment_requests (tenant_id, id)
);

CREATE INDEX ledger_adjustment_approvals_tenant ON ledger_adjustment_approvals (tenant_id);

CREATE FUNCTION ledger_adjustment_approvals_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor  RECORD;
    v_req    ledger_adjustment_requests%ROWTYPE;
    v_policy RECORD;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'ledger_adjustment_approvals: immutable' USING ERRCODE = 'MA031';
    END IF;
    SELECT * INTO v_actor FROM financial_actor_session();
    IF v_actor.scope NOT IN ('tenant', 'platform_acting') THEN
        RAISE EXCEPTION 'ledger_adjustment_approvals: only a tenant or acting session may decide (HD-PRH2-6)' USING ERRCODE = 'MA001';
    END IF;
    IF v_actor.person_id IS NULL THEN
        RAISE EXCEPTION 'ledger_adjustment_approvals: approver has no linked person_id' USING ERRCODE = 'MA002';
    END IF;

    SELECT * INTO v_req FROM ledger_adjustment_requests WHERE id = NEW.request_id;
    IF NOT FOUND OR v_req.tenant_id IS DISTINCT FROM v_actor.tenant THEN
        RAISE EXCEPTION 'ledger_adjustment_approvals: request % not found in the session tenant', NEW.request_id USING ERRCODE = 'MA031';
    END IF;
    IF v_req.state <> 'pending' OR now() >= v_req.expires_at THEN
        RAISE EXCEPTION 'ledger_adjustment_approvals: request % is not pending or has expired', NEW.request_id USING ERRCODE = 'MA031';
    END IF;
    -- LF-10 / B-4: the decision pins exactly the payload it saw.
    IF NEW.payload_hash IS DISTINCT FROM v_req.payload_hash THEN
        RAISE EXCEPTION 'ledger_adjustment_approvals: payload_hash does not match the request' USING ERRCODE = 'MA031';
    END IF;
    IF ledger_adjustment_eligible_grant(v_req.tenant_id, v_actor.actor, 'ledger_adjustment:approve') IS NULL THEN
        RAISE EXCEPTION 'ledger_adjustment_approvals: approver lacks an eligible role or an in-force ledger_adjustment:approve grant' USING ERRCODE = 'MA003';
    END IF;
    -- LF-11 distinct-Person floor (no distinct_principal option).
    IF v_actor.actor = v_req.initiated_by
       OR v_actor.person_id = ledger_adjustment_live_person(v_req.initiated_by, v_req.initiated_by_person_id)
       OR v_actor.person_id = v_req.initiated_by_person_id THEN
        RAISE EXCEPTION 'ledger_adjustment_approvals: the approver must be a distinct principal and Person from the initiator' USING ERRCODE = 'MA031';
    END IF;
    IF EXISTS (SELECT 1 FROM ledger_adjustment_approvals a
                WHERE a.request_id = v_req.id AND a.decided_by_person_id = v_actor.person_id) THEN
        RAISE EXCEPTION 'ledger_adjustment_approvals: this Person already decided this request' USING ERRCODE = 'MA031';
    END IF;
    -- S-2(iii) for approvers (security ruling 4), pinned and current.
    SELECT * INTO v_policy FROM financial_policy_required_approvals('ledger_adjustment', v_req.tenant_id, v_req.brand_id, v_req.asset_code, v_req.amount, now());
    IF v_actor.person_id = ANY (financial_policy_author_persons(v_req.contributing_policy_ids || v_policy.contributing_policy_ids)) THEN
        RAISE EXCEPTION 'ledger_adjustment_approvals: the approver authored or approved a contributing policy (S-2(iii))' USING ERRCODE = 'MA011';
    END IF;

    NEW.tenant_id := v_req.tenant_id;
    NEW.decided_by := v_actor.actor;
    NEW.decided_by_scope := v_actor.scope;
    NEW.decided_by_person_id := v_actor.person_id;
    NEW.decided_at := now();
    NEW.decided_txid := txid_current();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE FUNCTION ledger_adjustment_approvals_beneficiary_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor         RECORD;
    v_player_person uuid;
BEGIN
    SELECT * INTO v_actor FROM financial_actor_session();
    IF v_actor.scope NOT IN ('tenant', 'platform_acting') THEN
        RAISE EXCEPTION 'ledger_adjustment_approvals: only a tenant or acting session may decide (HD-PRH2-6)' USING ERRCODE = 'MA001';
    END IF;
    SELECT pa.person_id INTO v_player_person
      FROM ledger_adjustment_requests r JOIN player_accounts pa ON pa.id = r.player_account_id AND pa.tenant_id = r.tenant_id
     WHERE r.id = NEW.request_id;
    IF NOT FOUND OR v_actor.person_id IS NULL OR v_actor.person_id = v_player_person THEN
        RAISE EXCEPTION 'ledger_adjustment_approvals: the approver may not be the beneficiary (S-12)' USING ERRCODE = 'MA032';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Both guards are self-contained (session-derived actor), so BEFORE
-- trigger firing order (alphabetical) does not matter.
CREATE TRIGGER ledger_adjustment_approvals_guard
    BEFORE INSERT OR UPDATE ON ledger_adjustment_approvals
    FOR EACH ROW EXECUTE FUNCTION ledger_adjustment_approvals_guard();
CREATE TRIGGER ledger_adjustment_approvals_beneficiary_guard
    BEFORE INSERT ON ledger_adjustment_approvals
    FOR EACH ROW EXECUTE FUNCTION ledger_adjustment_approvals_beneficiary_guard();

CREATE FUNCTION ledger_adjustment_approvals_apply_reject() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.decision = 'reject' THEN
        UPDATE ledger_adjustment_requests SET state = 'rejected' WHERE id = NEW.request_id;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_adjustment_approvals_apply_reject
    AFTER INSERT ON ledger_adjustment_approvals
    FOR EACH ROW EXECUTE FUNCTION ledger_adjustment_approvals_apply_reject();

-- =========================================================================
-- 11. Counting at execution (§6.2) - the ONE implementation, used by the
--     executor after its FOR SHARE locks AND by the -> executing guard.
-- =========================================================================

-- An approval counts only if: decision approve; payload_hash equal; the
-- approver's in-force approve grant at now() with an active, eligible,
-- tenant-consistent staff row whose LIVE person equals the grant's
-- request-time snapshot (or, for an invisible platform principal, the
-- §7.6 residual); the Person is non-NULL, distinct from the initiator's
-- live Person and from every other counted approver's, not the
-- beneficiary, and not an author/approver of a contributing policy
-- (pinned or current). The initiator is re-checked the same way.
CREATE FUNCTION ledger_adjustment_execution_status(p_request uuid,
    OUT required int, OUT counted int, OUT counted_approval_ids uuid[], OUT initiator_valid boolean,
    OUT contributing_policy_ids uuid[], OUT tenant_status text, OUT enabled boolean
) AS $$
DECLARE
    v_req            ledger_adjustment_requests%ROWTYPE;
    v_policy         RECORD;
    v_authors        uuid[];
    v_player_person  uuid;
    v_init_person    uuid;
BEGIN
    SELECT * INTO v_req FROM ledger_adjustment_requests WHERE id = p_request;
    IF NOT FOUND THEN
        required := 1; counted := 0; counted_approval_ids := '{}'; initiator_valid := false;
        contributing_policy_ids := '{}'; enabled := false;
        RETURN;
    END IF;
    SELECT * INTO v_policy FROM financial_policy_required_approvals('ledger_adjustment', v_req.tenant_id, v_req.brand_id, v_req.asset_code, v_req.amount, now());
    -- §3.6: never below the pinned value.
    required := GREATEST(v_req.required_at_submission, v_policy.required);
    contributing_policy_ids := v_policy.contributing_policy_ids;
    tenant_status := v_policy.tenant_status;
    enabled := v_policy.enabled;
    v_authors := financial_policy_author_persons(v_req.contributing_policy_ids || v_policy.contributing_policy_ids);
    SELECT pa.person_id INTO v_player_person FROM player_accounts pa WHERE pa.id = v_req.player_account_id AND pa.tenant_id = v_req.tenant_id;
    v_init_person := ledger_adjustment_live_person(v_req.initiated_by, v_req.initiated_by_person_id);

    initiator_valid := v_init_person IS NOT NULL
        AND v_player_person IS NOT NULL
        AND v_init_person <> v_player_person
        AND NOT (v_init_person = ANY (v_authors))
        AND COALESCE(ledger_adjustment_eligible_grant(v_req.tenant_id, v_req.initiated_by, 'ledger_adjustment:initiate'),
                     ledger_adjustment_invisible_platform_grant(v_req.tenant_id, v_req.initiated_by, 'ledger_adjustment:initiate', v_req.initiated_by_person_id)) IS NOT NULL;

    WITH cand AS (
        SELECT a.id, a.decided_at,
               ledger_adjustment_live_person(a.decided_by, a.decided_by_person_id) AS person
          FROM ledger_adjustment_approvals a
         WHERE a.request_id = v_req.id
           AND a.decision = 'approve'
           AND a.payload_hash = v_req.payload_hash
           AND a.decided_by <> v_req.initiated_by
           AND COALESCE(ledger_adjustment_eligible_grant(v_req.tenant_id, a.decided_by, 'ledger_adjustment:approve'),
                        ledger_adjustment_invisible_platform_grant(v_req.tenant_id, a.decided_by, 'ledger_adjustment:approve', a.decided_by_person_id)) IS NOT NULL
    ), qualified AS (
        SELECT DISTINCT ON (c.person) c.id
          FROM cand c
         WHERE c.person IS NOT NULL
           AND c.person IS DISTINCT FROM v_init_person
           AND c.person IS DISTINCT FROM v_player_person
           AND NOT (c.person = ANY (v_authors))
         ORDER BY c.person, c.decided_at, c.id
    )
    SELECT count(*)::int, COALESCE(array_agg(q.id ORDER BY q.id), '{}') INTO counted, counted_approval_ids FROM qualified q;
    IF v_init_person IS NULL OR NOT initiator_valid THEN
        initiator_valid := false;
    END IF;
END;
$$ LANGUAGE plpgsql STABLE;

-- =========================================================================
-- 12. LF C-K1-2: the ADR 0099 §6.6 / §6.7 fences, BEFORE any acting
--     permissive policy on the four money tables below.
-- =========================================================================

-- §6.6 branch (a) ONLY (K3's 0115 replaces this function with (a)+(b)+(c)).
CREATE FUNCTION ledger_governed_fence_allows(
    p_tenant uuid, p_type text, p_idempotency_key text, p_correlation uuid, p_provider_id text, p_provider_tx_id text
) RETURNS boolean AS $$
    SELECT p_type = 'manual_adjustment' AND EXISTS (
        SELECT 1 FROM ledger_adjustment_requests r
         WHERE r.tenant_id = p_tenant
           AND p_idempotency_key = 'manual_adjustment:' || r.id::text
           AND p_correlation = r.id
           AND r.state = 'executing' AND r.executed_txid = txid_current());
$$ LANGUAGE sql STABLE;

CREATE FUNCTION ledger_transactions_governed_fence() RETURNS TRIGGER AS $$
BEGIN
    IF financial_acting_gucs_present() THEN
        IF NOT ledger_governed_fence_allows(NEW.tenant_id, NEW.transaction_type, NEW.idempotency_key, NEW.correlation_id,
                                            NEW.provider_id, NEW.provider_tx_id) THEN
            RAISE EXCEPTION 'ledger_transactions_governed_fence: an acting session may post only a governed, executing request''s transaction' USING ERRCODE = 'CG030';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_transactions_governed_fence
    BEFORE INSERT ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_transactions_governed_fence();

-- Entries can never be appended to a pre-existing (or non-governed)
-- transaction by an acting session: the parent must pass the same fence.
-- Security K2-C1 (ii): and EACH acting-inserted entry must be one leg of
-- the executing request's closed §4 shape - the request wallet's
-- player_cash in the player direction, or the tenant's manual_adjustment
-- house account (wallet_id IS NULL) in the opposite direction - in the
-- request's asset and amount; at most one leg per direction and at most
-- two entries per linked transaction. (Rows inserted earlier by the same
-- INSERT statement are visible here: row-level BEFORE triggers see the
-- outer command's previously processed rows.)
CREATE FUNCTION ledger_entries_governed_fence() RETURNS TRIGGER AS $$
DECLARE
    v_tx          RECORD;
    v_req         RECORD;
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
        IF EXISTS (SELECT 1 FROM ledger_entries e
                    WHERE e.ledger_transaction_id = NEW.ledger_transaction_id
                      AND (e.direction = NEW.direction
                           OR (SELECT count(*) FROM ledger_entries e2 WHERE e2.ledger_transaction_id = NEW.ledger_transaction_id) >= 2)) THEN
            RAISE EXCEPTION 'ledger_entries_governed_fence: the linked transaction already holds this leg (at most two entries, one per direction)' USING ERRCODE = 'CG030';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_entries_governed_fence
    BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_governed_fence();

-- §6.7 projection fence (LF F1): under acting, only the zero-row ensure
-- (ensureProjectionRowSQL) or the 0023 entries trigger (depth >= 2) may
-- write; DELETE never.
CREATE FUNCTION wallet_balance_projection_acting_fence() RETURNS TRIGGER AS $$
BEGIN
    IF financial_acting_gucs_present() THEN
        IF TG_OP = 'DELETE' THEN
            RAISE EXCEPTION 'wallet_balance_projection_acting_fence: delete refused' USING ERRCODE = 'CG031';
        ELSIF TG_OP = 'INSERT' THEN
            IF NOT ((NEW.debit_total = 0 AND NEW.credit_total = 0) OR pg_trigger_depth() >= 2) THEN
                RAISE EXCEPTION 'wallet_balance_projection_acting_fence: a direct non-zero projection insert is refused' USING ERRCODE = 'CG031';
            END IF;
        ELSE
            IF pg_trigger_depth() < 2 THEN
                RAISE EXCEPTION 'wallet_balance_projection_acting_fence: a direct projection update is refused' USING ERRCODE = 'CG031';
            END IF;
        END IF;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wallet_balance_projection_acting_fence
    BEFORE INSERT OR UPDATE OR DELETE ON wallet_balance_projection
    FOR EACH ROW EXECUTE FUNCTION wallet_balance_projection_acting_fence();

-- =========================================================================
-- 13. Acting permissive policies on existing tables (ADR 0099 §6.5,
--     ADR 0100 §10.7). Every one calls financial_acting_session_valid()
--     (as an InitPlan, evaluated once per statement).
-- =========================================================================

CREATE POLICY acting_read ON player_accounts FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_read ON wallets FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));

CREATE POLICY acting_read ON ledger_accounts FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
-- Restricted by account type to what K2's governed posting resolves
-- (stricter than ADR 0099 §6.5's four types; K3 widens it with its own).
CREATE POLICY acting_insert ON ledger_accounts FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
                AND account_type IN ('player_cash', 'manual_adjustment')
                AND (SELECT financial_acting_session_valid()));

CREATE POLICY acting_read ON ledger_transactions FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_insert ON ledger_transactions FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
-- Only for the L2 causation lock (SELECT ... FOR UPDATE); WITH CHECK false
-- and 0082's immutability trigger refuse any real update.
CREATE POLICY acting_lock ON ledger_transactions FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()))
    WITH CHECK (false);

CREATE POLICY acting_read ON ledger_entries FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_insert ON ledger_entries FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));

CREATE POLICY acting_read ON wallet_balance_projection FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_insert ON wallet_balance_projection FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_update ON wallet_balance_projection FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));

-- LF ruling 2: the open_payment_exposure check reads these.
CREATE POLICY acting_read ON payment_attempts FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_read ON deposit_intents FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));

-- Implementation-record additions (ADR 0100 §20; ADR 0099 §6.5 amendment):
-- the acting executor's jurisdiction resolution (§3.2), the K2-b tenant
-- asset layer, and the K2-G4 live-person-vs-snapshot re-check.
CREATE POLICY acting_read_own_licence ON licences FOR SELECT
    USING (EXISTS (SELECT 1 FROM tenants t
                    WHERE t.id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND t.licence_id = licences.id)
           AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_read ON asset_authorizations FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_read ON staff_capability_grant_requests FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));

-- =========================================================================
-- 14. RLS on the new tables
-- =========================================================================

-- financial_approval_policy_changes / _change_approvals: T (own tenant's
-- tenant/brand changes) and P (all). No A.
ALTER TABLE financial_approval_policy_changes ENABLE ROW LEVEL SECURITY;
ALTER TABLE financial_approval_policy_changes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope_select ON financial_approval_policy_changes FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_scope_insert ON financial_approval_policy_changes FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_scope_update ON financial_approval_policy_changes FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present())
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY platform_scope_select ON financial_approval_policy_changes FOR SELECT
    USING (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY platform_scope_insert ON financial_approval_policy_changes FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY platform_scope_update ON financial_approval_policy_changes FOR UPDATE
    USING (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present())
    WITH CHECK (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());

ALTER TABLE financial_approval_policy_change_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE financial_approval_policy_change_approvals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope_select ON financial_approval_policy_change_approvals FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_scope_insert ON financial_approval_policy_change_approvals FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY platform_scope_select ON financial_approval_policy_change_approvals FOR SELECT
    USING (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY platform_scope_insert ON financial_approval_policy_change_approvals FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());

-- financial_approval_policies: SELECT platform-level (tenant_id NULL) rows
-- to T, P and A; tenant/brand rows to T (own), A (X), P. INSERT P (any
-- level), T (own tenant/brand rows) - only ever via the approval trigger.
ALTER TABLE financial_approval_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE financial_approval_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope_select ON financial_approval_policies FOR SELECT
    USING ((tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_scope_insert ON financial_approval_policies FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY platform_scope_select ON financial_approval_policies FOR SELECT
    USING (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY platform_scope_insert ON financial_approval_policies FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY acting_read ON financial_approval_policies FOR SELECT
    USING ((tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid)
           AND (SELECT financial_acting_session_valid()));

ALTER TABLE tenant_financial_policy_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_financial_policy_profiles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope_select ON tenant_financial_policy_profiles FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY platform_scope_select ON tenant_financial_policy_profiles FOR SELECT
    USING (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY platform_scope_insert ON tenant_financial_policy_profiles FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY acting_read ON tenant_financial_policy_profiles FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));

-- ledger_adjustment_requests / _approvals: T and A only; NO P (§6.6: the
-- plain platform session has no policy on any K2 request table).
ALTER TABLE ledger_adjustment_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_adjustment_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope_select ON ledger_adjustment_requests FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_scope_insert ON ledger_adjustment_requests FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_scope_update ON ledger_adjustment_requests FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present())
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
-- §12 detector read (ADR 0100 §20 implementation record): the
-- reconciliation sweep runs in the system-tenant session shape
-- (db.WithTenant: app.tenant_id only, no principal). It may read ONLY
-- executed requests (their ledger_transaction_id link), never pending
-- payloads, and never write.
CREATE POLICY tenant_system_read_executed ON ledger_adjustment_requests FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND state = 'executed'
           AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY acting_read ON ledger_adjustment_requests FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_insert ON ledger_adjustment_requests FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_update ON ledger_adjustment_requests FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));

ALTER TABLE ledger_adjustment_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_adjustment_approvals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope_select ON ledger_adjustment_approvals FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_scope_insert ON ledger_adjustment_approvals FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY acting_read ON ledger_adjustment_approvals FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_insert ON ledger_adjustment_approvals FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));

-- =========================================================================
-- 15. DELETE / TRUNCATE refused on every new table
-- =========================================================================

DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['financial_control_classifications', 'ledger_adjustment_reason_codes',
                             'financial_approval_policy_changes', 'financial_approval_policy_change_approvals',
                             'financial_approval_policies', 'tenant_financial_policy_profiles',
                             'ledger_adjustment_requests', 'ledger_adjustment_approvals']
    LOOP
        EXECUTE format('CREATE TRIGGER %I BEFORE DELETE ON %I FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation()', t || '_no_delete', t);
        EXECUTE format('CREATE TRIGGER %I BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation()', t || '_no_truncate', t);
    END LOOP;
END $$;

-- Reference tables are immutable too (migration-written only).
CREATE TRIGGER financial_control_classifications_immutable
    BEFORE UPDATE ON financial_control_classifications FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER ledger_adjustment_reason_codes_immutable
    BEFORE UPDATE ON ledger_adjustment_reason_codes FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

-- =========================================================================
-- 16. Reconciliation kind (LF ruling 4; §10.8): a strict superset of 0107
-- =========================================================================

ALTER TABLE reconciliation_mismatches DROP CONSTRAINT reconciliation_mismatches_mismatch_kind_check;
ALTER TABLE reconciliation_mismatches ADD CONSTRAINT reconciliation_mismatches_mismatch_kind_check
    CHECK (mismatch_kind IN (
        'missing_projection', 'balance_mismatch',
        'sb_locked_mismatch', 'sb_bet_net_mismatch', 'sb_orphan_ledger', 'sb_orphan_history',
        'sb_status_mismatch', 'sb_mock_statement_mismatch',
        'cas_round_binding_mismatch', 'cas_posting_shape_mismatch', 'cas_orphan_win',
        'cas_rollback_linkage_mismatch', 'cas_tombstone_conflict', 'cas_unposted_provider_event',
        'cas_tombstone_late_original', 'cas_mock_statement_mismatch',
        'pay_missing_platform_record', 'pay_missing_provider_record', 'pay_amount_mismatch',
        'pay_asset_mismatch', 'pay_reference_mismatch', 'pay_status_mismatch', 'pay_duplicate',
        'pay_unresolved', 'pay_captured_unposted',
        'ledger_unlinked_manual_adjustment'
    ));

-- =========================================================================
-- 17. Runtime role grants (mirrors deploy/init-app-role.sql's 0113 block)
-- =========================================================================

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON financial_control_classifications FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON financial_control_classifications TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON ledger_adjustment_reason_codes FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON ledger_adjustment_reason_codes TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON financial_approval_policy_changes FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE ON financial_approval_policy_changes TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON financial_approval_policy_change_approvals FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON financial_approval_policy_change_approvals TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON financial_approval_policies FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON financial_approval_policies TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON tenant_financial_policy_profiles FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON tenant_financial_policy_profiles TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON ledger_adjustment_requests FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE ON ledger_adjustment_requests TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON ledger_adjustment_approvals FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON ledger_adjustment_approvals TO igaming_runtime';
    END IF;
END $$;
