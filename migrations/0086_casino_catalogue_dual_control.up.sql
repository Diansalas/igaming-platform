-- Stage 9.2, Workstream A: closes ARCH-DB-2 Phase 2 (docs/decisions/0081
-- §5, §5.2) - four-eyes governance for the fail-open/compliance-widening
-- direction of two casino_games columns:
--
--   * jurisdiction_blocklist: REMOVING a code re-permits play in a
--     jurisdiction previously recorded as a licence problem. This is the
--     precise analog of assets.platform_authorized going false -> true
--     (migration 0044) and requires four-eyes. ADDING a code is the
--     fail-closed direction (a compliance kill-switch) and stays
--     single-actor, unchanged - requiring a second approver there would
--     delay exactly the action that must be instant.
--   * status: 'disabled' -> 'active' is a platform-wide re-enablement and
--     requires four-eyes. 'active' -> 'disabled' is fail-closed and stays
--     single-actor.
--   * Row creation is NOT dual-controlled (ADR 0081 §2.5.1/§5) - unchanged.
--
-- Structure is a verbatim-as-possible copy of migration 0044's
-- asset_change_requests/asset_change_approvals/assets_dual_control
-- precedent, keyed on game_id instead of asset_code, per ADR 0081 §5.2's
-- explicit instruction. Two tables, platform-scoped (no tenant_id - these
-- govern platform-scoped operations on a platform-scoped table), ENABLE +
-- FORCE RLS with the identical platform_admin_scope FOR ALL policy shape.
--
-- Deliberately mirrors migration 0044's ORIGINAL self-approval/platform-
-- principal check shape (principal + person_id-when-both-non-null), NOT
-- migration 0047's later hardening (mandatory Person linkage + active
-- status on both sides) - per ADR 0081 §5.2's explicit "mirror migration
-- 0044's asset_change_approvals_deny_self_approval exactly" instruction,
-- and consistent with this same package's own most recent precedent:
-- migration 0085's casino_games_require_platform_principal (Stage 9.1,
-- i.e. AFTER 0047 existed) also deliberately used the simpler 0044-style
-- check rather than 0047's, for the same "casino_games has no person-
-- linking deployment dependency to manage" reasoning. Recorded here so a
-- future reviewer does not read the gap as an oversight; if the platform
-- later requires mandatory Person linkage for every platform-scoped
-- four-eyes decision, that is a follow-up in the shape of 0047, applied
-- uniformly, not a casino-specific patch.

-- ----------------------------------------------------------------------
-- 1. casino_catalogue_change_requests / casino_catalogue_change_approvals
-- ----------------------------------------------------------------------

CREATE TABLE casino_catalogue_change_requests (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Only the two dual-controlled operations are representable.
    operation                 TEXT NOT NULL CHECK (operation IN ('jurisdiction_unblock', 'status_activate')),
    -- A real FK is possible here, unlike asset_change_requests.asset_code
    -- (migration 0044's own comment on this difference): the casino_games
    -- row always already exists before a change request can be filed
    -- against it - creation is not dual-controlled (ADR 0081 §2.5.1).
    game_id                   UUID NOT NULL REFERENCES casino_games (id),
    -- For 'jurisdiction_unblock', payload.removed_codes is the exact set
    -- of jurisdiction codes the approver is approving the removal of -
    -- verified against the codes actually removed by
    -- casino_games_enforce_dual_control() below, so approving "unblock
    -- DE" can never be spent applying "unblock DE, FR" (the identical
    -- payload-match discipline migration 0044 applies to an approved
    -- 'create').
    payload                   JSONB NOT NULL DEFAULT '{}'::jsonb,
    reason_code               TEXT NOT NULL,
    requested_by_principal_id UUID NOT NULL,
    requested_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    state                     TEXT NOT NULL DEFAULT 'pending'
                                  CHECK (state IN ('pending', 'applied', 'rejected', 'cancelled')),
    applied_by_principal_id   UUID,
    applied_at                TIMESTAMPTZ,
    CHECK (operation <> 'jurisdiction_unblock' OR payload ? 'removed_codes'),
    CHECK ((state = 'applied') = (applied_at IS NOT NULL))
);

CREATE INDEX idx_casino_catalogue_change_requests_pending
    ON casino_catalogue_change_requests (game_id, operation)
    WHERE state = 'pending';

CREATE TABLE casino_catalogue_change_approvals (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id            UUID NOT NULL REFERENCES casino_catalogue_change_requests (id),
    approver_principal_id UUID NOT NULL,
    decision              TEXT NOT NULL CHECK (decision IN ('approve', 'reject')),
    reason_code           TEXT,
    decided_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- reason_code required on 'reject', optional on 'approve' - migration
    -- 0026/0044's rule, unchanged.
    CHECK (decision = 'approve' OR reason_code IS NOT NULL),
    -- THE distinct-approver constraint: one principal can never count as
    -- two approvals of the same request.
    UNIQUE (request_id, approver_principal_id)
);

CREATE INDEX idx_casino_catalogue_change_approvals_request ON casino_catalogue_change_approvals (request_id);

-- Both tables are platform-admin-scoped: a tenant-scoped or player-scoped
-- connection can neither read nor write them, mirroring migration 0044's
-- identical predicate exactly.
ALTER TABLE casino_catalogue_change_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE casino_catalogue_change_requests FORCE ROW LEVEL SECURITY;
ALTER TABLE casino_catalogue_change_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE casino_catalogue_change_approvals FORCE ROW LEVEL SECURITY;

CREATE POLICY platform_admin_scope ON casino_catalogue_change_requests
    FOR ALL
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY platform_admin_scope ON casino_catalogue_change_approvals
    FOR ALL
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- A request is append-only except for its own terminal transition, applied
-- BY the casino_games trigger below (for 'applied') or by an explicit
-- reject/cancel. Mirrors migration 0044's asset_change_requests_enforce_
-- immutability() exactly, adapted to this table's columns.
CREATE FUNCTION casino_catalogue_change_requests_enforce_immutability() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION 'casino_catalogue_change_requests is append-only: % is not permitted', TG_OP;
    END IF;
    IF NEW.id <> OLD.id
        OR NEW.operation <> OLD.operation
        OR NEW.game_id <> OLD.game_id
        OR NEW.payload <> OLD.payload
        OR NEW.reason_code <> OLD.reason_code
        OR NEW.requested_by_principal_id <> OLD.requested_by_principal_id
        OR NEW.requested_at <> OLD.requested_at
    THEN
        RAISE EXCEPTION 'casino_catalogue_change_requests: operation/game_id/payload/reason_code/requester/requested_at are immutable after insert';
    END IF;
    IF OLD.state <> 'pending' THEN
        RAISE EXCEPTION 'casino_catalogue_change_requests: request % is already %, it cannot be re-decided or re-applied', OLD.id, OLD.state;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER casino_catalogue_change_requests_immutable
    BEFORE UPDATE ON casino_catalogue_change_requests
    FOR EACH ROW EXECUTE FUNCTION casino_catalogue_change_requests_enforce_immutability();

CREATE TRIGGER casino_catalogue_change_requests_deny_delete
    BEFORE DELETE ON casino_catalogue_change_requests
    FOR EACH ROW EXECUTE FUNCTION casino_catalogue_change_requests_enforce_immutability();

CREATE TRIGGER casino_catalogue_change_requests_no_truncate
    BEFORE TRUNCATE ON casino_catalogue_change_requests
    FOR EACH STATEMENT EXECUTE FUNCTION casino_catalogue_change_requests_enforce_immutability();

-- The requester must itself be a platform-scoped staff principal. Under
-- staff_users' own dual_scope_isolation policy (migration 0011), a
-- platform-scoped transaction sees exactly the tenant_id IS NULL rows, so
-- a tenant-scoped staff id is simply not resolvable here and the request
-- is refused - mirrors migration 0044's asset_change_requests_require_
-- platform_principal exactly.
CREATE FUNCTION casino_catalogue_change_requests_require_platform_principal() RETURNS TRIGGER AS $$
DECLARE
    v_is_platform_scoped BOOLEAN;
BEGIN
    SELECT su.tenant_id IS NULL INTO v_is_platform_scoped
      FROM staff_users su
     WHERE su.id = NEW.requested_by_principal_id;
    IF NOT FOUND OR NOT v_is_platform_scoped THEN
        RAISE EXCEPTION 'casino_catalogue_change_requests: requesting principal % is not a platform-scoped staff principal (ADR 0081 §5.2: catalogue governance is never reachable by a tenant-scoped role)', NEW.requested_by_principal_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER casino_catalogue_change_requests_platform_principal
    BEFORE INSERT ON casino_catalogue_change_requests
    FOR EACH ROW EXECUTE FUNCTION casino_catalogue_change_requests_require_platform_principal();

-- Approvals are fully immutable (a reconsideration is a new request, never
-- an edited decision) - reusing ledger_deny_mutation() (migration 0021),
-- this codebase's established convention for "no UPDATE/DELETE/TRUNCATE,
-- full stop", exactly as migration 0044's asset_change_approvals does.
CREATE TRIGGER casino_catalogue_change_approvals_immutable
    BEFORE UPDATE OR DELETE ON casino_catalogue_change_approvals
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER casino_catalogue_change_approvals_no_truncate
    BEFORE TRUNCATE ON casino_catalogue_change_approvals
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- The self-approval guard, mirroring migration 0044's original asset_
-- change_approvals_deny_self_approval() exactly (see this file's header
-- for why 0047's later hardening is deliberately not carried over here):
-- block the same PRINCIPAL, and also the same PERSON reached through
-- staff_users.person_id when both sides happen to carry one (two staff
-- accounts held by one human are one human).
CREATE FUNCTION casino_catalogue_change_approvals_deny_self_approval() RETURNS TRIGGER AS $$
DECLARE
    v_requester_principal_id UUID;
    v_requester_person_id    UUID;
    v_approver_person_id     UUID;
    v_approver_is_platform   BOOLEAN;
BEGIN
    SELECT r.requested_by_principal_id INTO v_requester_principal_id
      FROM casino_catalogue_change_requests r
     WHERE r.id = NEW.request_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'casino_catalogue_change_approvals: request % is not visible in this scope', NEW.request_id;
    END IF;

    IF NEW.approver_principal_id = v_requester_principal_id THEN
        RAISE EXCEPTION 'casino_catalogue_change_approvals: the requesting principal may not approve or reject its own catalogue change (self-approval)';
    END IF;

    SELECT su.tenant_id IS NULL, su.person_id INTO v_approver_is_platform, v_approver_person_id
      FROM staff_users su
     WHERE su.id = NEW.approver_principal_id;
    IF NOT FOUND OR NOT v_approver_is_platform THEN
        RAISE EXCEPTION 'casino_catalogue_change_approvals: approving principal % is not a platform-scoped staff principal', NEW.approver_principal_id;
    END IF;

    SELECT su.person_id INTO v_requester_person_id
      FROM staff_users su
     WHERE su.id = v_requester_principal_id;

    IF v_requester_person_id IS NOT NULL
        AND v_approver_person_id IS NOT NULL
        AND v_requester_person_id = v_approver_person_id
    THEN
        RAISE EXCEPTION 'casino_catalogue_change_approvals: approver resolves to the same person as the requester (self-approval through a second staff account)';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER casino_catalogue_change_approvals_deny_self_approval
    BEFORE INSERT ON casino_catalogue_change_approvals
    FOR EACH ROW EXECUTE FUNCTION casino_catalogue_change_approvals_deny_self_approval();

-- ----------------------------------------------------------------------
-- 2. Consume-and-apply, and the casino_games dual-control trigger
-- ----------------------------------------------------------------------

-- Consumes exactly one pending, independently-approved request for
-- (game_id, operation) and marks it applied in the SAME statement that
-- performs the mutation - a verbatim structural copy of migration 0044's
-- asset_change_consume_approved_request, keyed on game_id instead of
-- asset_code. Consuming inside the trigger, rather than leaving the Go
-- service to mark it applied afterwards, is what makes the control
-- non-forgettable and non-replayable: a single approval authorizes a
-- single mutation, once.
CREATE FUNCTION casino_catalogue_change_consume_approved_request(p_game_id UUID, p_operation TEXT) RETURNS UUID AS $$
DECLARE
    v_request_id UUID;
BEGIN
    SELECT r.id INTO v_request_id
      FROM casino_catalogue_change_requests r
     WHERE r.game_id = p_game_id
       AND r.operation = p_operation
       AND r.state = 'pending'
       AND EXISTS (
           SELECT 1
             FROM casino_catalogue_change_approvals a
            WHERE a.request_id = r.id
              AND a.decision = 'approve'
              -- Distinct from the requester: the UNIQUE constraint stops
              -- one principal counting twice, this stops the requester
              -- counting at all (defence in depth behind the
              -- BEFORE INSERT self-approval trigger).
              AND a.approver_principal_id <> r.requested_by_principal_id
       )
       -- A rejection by anyone blocks the request outright: a second
       -- approver's "no" is not something a third approver's "yes" may
       -- silently override.
       AND NOT EXISTS (
           SELECT 1 FROM casino_catalogue_change_approvals a
            WHERE a.request_id = r.id AND a.decision = 'reject'
       )
     ORDER BY r.requested_at
     FOR UPDATE
     LIMIT 1;

    IF v_request_id IS NULL THEN
        RAISE EXCEPTION 'casino_games: % of game % requires a pending casino_catalogue_change_requests row approved by a DIFFERENT platform principal (four-eyes, ADR 0081 §5)', p_operation, p_game_id;
    END IF;

    UPDATE casino_catalogue_change_requests
       SET state = 'applied',
           applied_at = now(),
           applied_by_principal_id = NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid
     WHERE id = v_request_id;

    RETURN v_request_id;
END;
$$ LANGUAGE plpgsql;

-- The dual-control gate itself, per ADR 0081 §5.2:
--   * status 'disabled' -> 'active' consumes a 'status_activate' request.
--   * A non-empty removal from jurisdiction_blocklist consumes a
--     'jurisdiction_unblock' request, whose approved payload.removed_codes
--     must equal the codes actually removed, compared as SORTED SETS (so
--     approving "unblock DE" can never be spent applying "unblock DE,
--     FR").
--   * Additions to the blocklist with no removals, and every other column
--     change (including status 'active' -> 'disabled', the fail-closed
--     direction), pass through UNCHANGED - this trigger must never
--     interfere with newUpsertCasinoGameHandler's existing single-actor
--     path for anything except these two widening transitions.
CREATE FUNCTION casino_games_enforce_dual_control() RETURNS TRIGGER AS $$
DECLARE
    v_removed    TEXT[];
    v_request_id UUID;
    v_payload    JSONB;
BEGIN
    IF NEW.status = 'active' AND OLD.status = 'disabled' THEN
        PERFORM casino_catalogue_change_consume_approved_request(NEW.id, 'status_activate');
    END IF;

    v_removed := ARRAY(
        SELECT unnest(OLD.jurisdiction_blocklist)
        EXCEPT
        SELECT unnest(NEW.jurisdiction_blocklist)
    );

    IF array_length(v_removed, 1) IS NOT NULL THEN
        v_request_id := casino_catalogue_change_consume_approved_request(NEW.id, 'jurisdiction_unblock');

        SELECT payload INTO v_payload FROM casino_catalogue_change_requests WHERE id = v_request_id;

        IF (SELECT ARRAY(SELECT jsonb_array_elements_text(v_payload -> 'removed_codes') ORDER BY 1))
            IS DISTINCT FROM
           (SELECT ARRAY(SELECT unnest(v_removed) ORDER BY 1))
        THEN
            RAISE EXCEPTION 'casino_games: approved jurisdiction_unblock request % payload removed_codes does not match the codes actually being removed from game % (approving "unblock X" must never authorize "unblock X, Y")', v_request_id, NEW.id;
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- BEFORE UPDATE, alongside casino_games_immutable_identity (migration
-- 0084) and casino_games_platform_principal (migration 0085). Trigger
-- execution order for the same event is alphabetical by trigger name in
-- PostgreSQL - casino_games_dual_control < casino_games_immutable_identity
-- < casino_games_platform_principal - which is immaterial here: none of
-- the three triggers' checks depend on another having already run.
CREATE TRIGGER casino_games_dual_control
    BEFORE UPDATE ON casino_games
    FOR EACH ROW EXECUTE FUNCTION casino_games_enforce_dual_control();
