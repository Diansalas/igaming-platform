-- RV-PRH-I1 kill-switch security review, finding M1 (fail-open): §10.3's
-- L1 "misbound context fails closed" argument claimed
-- payment_kill_switches.tenant_scope has "the same GUC predicate" as
-- payment_attempts.tenant_staff_scope (migration 0101). It does not:
-- tenant_scope also requires app.platform_admin_principal_id to be unset;
-- tenant_staff_scope (migration 0101) never checked it.
--
-- A transaction with BOTH app.tenant_id and app.platform_admin_principal_id
-- set could therefore claim and write payment_attempts (tenant_staff_scope
-- is satisfied - it only looks at app.tenant_id/app.player_account_id) while
-- seeing ZERO payment_kill_switches rows (tenant_scope's own USING clause
-- requires app.platform_admin_principal_id unset, so it evaluates false for
-- this mixed session and the platform family also does not apply, since
-- that requires app.tenant_id unset). The kill-switch NOT EXISTS predicate
-- then evaluates to "not engaged" - a latent fail-open on a mixed-GUC
-- context, security review probe P1 (RV-PRH-I1, 2026-09-27).
--
-- No production code sets both GUCs in the same transaction today (the
-- review grepped every set_config call), but internal/kyc/enforcement_
-- admin.go's setActingPrincipal shows in-transaction GUC switching is an
-- established pattern in this codebase, so nothing currently prevents a
-- future call site from doing so. This migration is defence in depth,
-- fixing the fail-open at the RLS layer directly rather than relying on
-- "nothing does this yet": option (a) from the review (align
-- payment_attempts' write policy with payment_kill_switches' own, rather
-- than option (b) splitting the switch table's policy family, since (a)
-- is the smaller, more conservative change and does not touch the
-- migration 0105 tables at all).
--
-- Applied to BOTH tenant_staff_scope policies migration 0101 created
-- (payment_attempts and payment_provider_events): they share the exact
-- same policy shape and would share the exact same mixed-GUC gap.
--
-- Migration 0101 itself is left untouched (already applied); this is a
-- new, additive migration per CLAUDE.md's "never UPDATE" spirit applied to
-- schema changes on an already-shipped migration - DROP POLICY + CREATE
-- POLICY is the standard pattern this codebase already uses for hardening
-- an existing RLS policy (see migration 0018's identical DROP/CREATE
-- pattern on `sessions`).

DROP POLICY tenant_staff_scope ON payment_attempts;
CREATE POLICY tenant_staff_scope ON payment_attempts
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
    );

DROP POLICY tenant_staff_scope ON payment_provider_events;
CREATE POLICY tenant_staff_scope ON payment_provider_events
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
    );

-- L2 (part 2, DB defence in depth): force expected_version to the switch's
-- current version at INSERT, and require the switch to be engaged, closing
-- security review R3 (a future-version request pre-authorising a later
-- re-engagement) and half of R2 (a request filed on a non-engaged switch)
-- at the database layer, not just the HTTP handler's own server-side read.
CREATE OR REPLACE FUNCTION payment_kill_switch_release_requests_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor  UUID;
    v_scope  TEXT;
    v_sw_scope TEXT;
    v_sw_engaged BOOLEAN;
    v_sw_version BIGINT;
BEGIN
    SELECT actor, scope INTO v_actor, v_scope FROM payment_kill_switch_session();

    IF TG_OP = 'INSERT' THEN
        SELECT tenant_id, engaged_by_scope, engaged, version
          INTO NEW.tenant_id, v_sw_scope, v_sw_engaged, v_sw_version
          FROM payment_kill_switches WHERE id = NEW.kill_switch_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'payment_kill_switch_release_requests: kill_switch_id % does not exist', NEW.kill_switch_id;
        END IF;
        -- L2 (R2): a request may only ever be filed against a currently
        -- engaged switch - closes the "request squats the one-open slot
        -- across a later engage" gap at the database layer.
        IF NOT v_sw_engaged THEN
            RAISE EXCEPTION 'payment_kill_switch_release_requests: switch % is not currently engaged', NEW.kill_switch_id;
        END IF;
        -- L2 (R3): expected_version is FORCED to the switch's own current
        -- version, never accepted from the client/application - closes the
        -- "future version pre-authorises a later re-engagement" gap even if
        -- a caller other than this codebase's own HTTP handler ever inserts
        -- a request directly.
        NEW.expected_version := v_sw_version;
        -- L5(a): a tenant session may never hold the one open slot against
        -- a platform-engaged row.
        IF v_sw_scope = 'platform' AND v_scope <> 'platform' THEN
            RAISE EXCEPTION 'payment_kill_switch_release_requests: switch is platform-engaged; only a platform session may request its release';
        END IF;
        NEW.requested_by := v_actor;
        NEW.requested_by_scope := v_scope;
        NEW.created_at := now();
        NEW.expires_at := LEAST(COALESCE(NEW.expires_at, now() + interval '24 hours'), now() + interval '24 hours');
        NEW.status := 'open';
        NEW.approved_by := NULL;
        NEW.approved_by_scope := NULL;
        NEW.decided_at := NULL;
        NEW.decided_txid := NULL;
        RETURN NEW;
    END IF;

    -- UPDATE from here on. A terminal row is fully immutable.
    IF OLD.status <> 'open' THEN
        RAISE EXCEPTION 'payment_kill_switch_release_requests: request % is in terminal status % and is immutable', OLD.id, OLD.status;
    END IF;

    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.kill_switch_id IS DISTINCT FROM OLD.kill_switch_id
        OR NEW.expected_version IS DISTINCT FROM OLD.expected_version
        OR NEW.reason_code IS DISTINCT FROM OLD.reason_code
        OR NEW.requested_by IS DISTINCT FROM OLD.requested_by
        OR NEW.requested_by_scope IS DISTINCT FROM OLD.requested_by_scope
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
        OR NEW.expires_at IS DISTINCT FROM OLD.expires_at
    THEN
        RAISE EXCEPTION 'payment_kill_switch_release_requests: identity/request columns are immutable';
    END IF;

    -- L1: a non-platform session may never change the status of a request
    -- (cancel included) whose switch is platform-engaged, or which was
    -- itself filed by a platform session - mirrors the approve-path check
    -- below, closing the "tenant cancels the platform's own open request"
    -- gap (security review L1/R1c).
    IF (OLD.requested_by_scope = 'platform') AND v_scope <> 'platform' THEN
        RAISE EXCEPTION 'payment_kill_switch_release_requests: only a platform session may change the status of a platform-filed request';
    END IF;
    SELECT engaged_by_scope INTO v_sw_scope FROM payment_kill_switches WHERE id = OLD.kill_switch_id;
    IF v_sw_scope = 'platform' AND v_scope <> 'platform' THEN
        RAISE EXCEPTION 'payment_kill_switch_release_requests: only a platform session may change the status of a request against a platform-engaged switch';
    END IF;

    IF NEW.status = 'approved' THEN
        IF v_actor = OLD.requested_by THEN
            RAISE EXCEPTION 'payment_kill_switch_release_requests: the approver must be a distinct principal from the requester (four-eyes)';
        END IF;
        NEW.approved_by := v_actor;
        NEW.approved_by_scope := v_scope;
        NEW.decided_at := now();
        NEW.decided_txid := txid_current();
    ELSIF NEW.status IN ('cancelled', 'expired') THEN
        NEW.approved_by := NULL;
        NEW.approved_by_scope := NULL;
        NEW.decided_at := NULL;
        NEW.decided_txid := NULL;
    ELSE
        RAISE EXCEPTION 'payment_kill_switch_release_requests: status may only move open -> approved|cancelled|expired, got %', NEW.status;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- L2 (R2, remaining half): KS-L6 (cancel any open tenant request) already
-- fires on a true->true takeover; extend it to also fire on a false->true
-- ENGAGE, so a request filed while the switch was disengaged can never
-- squat the one-open slot across a later platform engage.
CREATE OR REPLACE FUNCTION payment_kill_switches_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor UUID;
    v_scope TEXT;
    v_req   RECORD;
BEGIN
    SELECT actor, scope INTO v_actor, v_scope FROM payment_kill_switch_session();
    NEW.changed_by := v_actor;
    NEW.changed_by_scope := v_scope;
    -- L3: changed_at is forced from the clock on every INSERT and UPDATE,
    -- never client-supplied (security review L3).
    NEW.changed_at := now();

    IF TG_OP = 'INSERT' THEN
        NEW.release_request_id := NULL;
        NEW.version := 1;
        NEW.engaged_by_scope := CASE WHEN NEW.engaged THEN v_scope ELSE NULL END;
        RETURN NEW;
    END IF;

    -- UPDATE from here on.

    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.provider_scope IS DISTINCT FROM OLD.provider_scope
        OR NEW.operation_scope IS DISTINCT FROM OLD.operation_scope
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'payment_kill_switches: id/tenant_id/provider_scope/operation_scope/created_at are immutable';
    END IF;

    IF OLD.engaged_by_scope = 'platform' AND v_scope <> 'platform' THEN
        RAISE EXCEPTION 'payment_kill_switches: row is platform-engaged; only a platform session may modify it';
    END IF;

    NEW.version := OLD.version + 1;

    IF NOT OLD.engaged AND NEW.engaged THEN
        -- Point 4: engage (false -> true).
        NEW.engaged_by_scope := v_scope;
        NEW.release_request_id := NULL;
        -- L2 (R2): a platform false->true engage also cancels any open
        -- tenant request that predates it, so it can never squat the one-
        -- open slot the platform's own request needs.
        IF v_scope = 'platform' THEN
            UPDATE payment_kill_switch_release_requests
               SET status = 'cancelled'
             WHERE kill_switch_id = OLD.id AND status = 'open';
        END IF;

    ELSIF OLD.engaged AND NEW.engaged THEN
        NEW.engaged_by_scope := v_scope;
        NEW.release_request_id := NULL;

        IF OLD.engaged_by_scope = 'tenant' AND v_scope = 'platform' THEN
            UPDATE payment_kill_switch_release_requests
               SET status = 'cancelled'
             WHERE kill_switch_id = OLD.id AND status = 'open';
        END IF;

    ELSIF OLD.engaged AND NOT NEW.engaged THEN
        IF NEW.release_request_id IS NULL THEN
            RAISE EXCEPTION 'payment_kill_switches: release requires an approved release_request_id';
        END IF;
        SELECT * INTO v_req FROM payment_kill_switch_release_requests r WHERE r.id = NEW.release_request_id;
        IF NOT FOUND
            OR v_req.kill_switch_id <> OLD.id
            OR v_req.status <> 'approved'
            OR v_req.expected_version <> OLD.version
            OR v_req.approved_by = v_req.requested_by
            OR now() >= v_req.expires_at
            OR v_req.decided_txid <> txid_current()
        THEN
            RAISE EXCEPTION 'payment_kill_switches: release_request_id % is not a valid, same-transaction approval for this switch at version %', NEW.release_request_id, OLD.version;
        END IF;
        IF OLD.engaged_by_scope = 'platform' AND (v_req.requested_by_scope <> 'platform' OR v_req.approved_by_scope <> 'platform') THEN
            RAISE EXCEPTION 'payment_kill_switches: a platform-engaged switch may only be released by a platform requester and approver';
        END IF;
        NEW.engaged_by_scope := NULL;

    ELSE
        NEW.engaged_by_scope := NULL;
        NEW.release_request_id := NULL;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- L4: reject a suspended staff principal as actor at the database layer.
-- JWTs are stateless, so a suspended user's unexpired token could otherwise
-- still engage or approve through the application layer alone.
CREATE OR REPLACE FUNCTION payment_kill_switch_session(OUT actor UUID, OUT scope TEXT) AS $$
DECLARE
    v_tenant   TEXT := NULLIF(current_setting('app.tenant_id', true), '');
    v_principal TEXT := NULLIF(current_setting('app.principal_id', true), '');
    v_player   TEXT := NULLIF(current_setting('app.player_account_id', true), '');
    v_platform TEXT := NULLIF(current_setting('app.platform_admin_principal_id', true), '');
BEGIN
    IF v_platform IS NOT NULL AND v_tenant IS NULL AND v_player IS NULL THEN
        IF NOT EXISTS (SELECT 1 FROM staff_users su WHERE su.id = v_platform::uuid AND su.tenant_id IS NULL AND su.status = 'active') THEN
            RAISE EXCEPTION 'payment_kill_switch_session: platform principal % is not an active platform-scoped staff principal', v_platform;
        END IF;
        actor := v_platform::uuid;
        scope := 'platform';
        RETURN;
    END IF;

    IF v_tenant IS NOT NULL AND v_principal IS NOT NULL AND v_player IS NULL AND v_platform IS NULL THEN
        IF NOT EXISTS (SELECT 1 FROM staff_users su WHERE su.id = v_principal::uuid AND su.tenant_id = v_tenant::uuid AND su.status = 'active') THEN
            RAISE EXCEPTION 'payment_kill_switch_session: principal % is not an active tenant-scoped staff principal of tenant %', v_principal, v_tenant;
        END IF;
        actor := v_principal::uuid;
        scope := 'tenant';
        RETURN;
    END IF;

    RAISE EXCEPTION 'payment_kill_switch_session: unresolvable session scope (tenant=%, principal=%, player=%, platform=%)', v_tenant, v_principal, v_player, v_platform;
END;
$$ LANGUAGE plpgsql STABLE;
