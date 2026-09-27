-- Reverts migration 0106 to migration 0105/0101's own original function and
-- policy bodies, verbatim.

DROP POLICY tenant_staff_scope ON payment_attempts;
CREATE POLICY tenant_staff_scope ON payment_attempts
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

DROP POLICY tenant_staff_scope ON payment_provider_events;
CREATE POLICY tenant_staff_scope ON payment_provider_events
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE OR REPLACE FUNCTION payment_kill_switch_release_requests_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor  UUID;
    v_scope  TEXT;
    v_sw_scope TEXT;
BEGIN
    SELECT actor, scope INTO v_actor, v_scope FROM payment_kill_switch_session();

    IF TG_OP = 'INSERT' THEN
        SELECT tenant_id, engaged_by_scope INTO NEW.tenant_id, v_sw_scope
          FROM payment_kill_switches WHERE id = NEW.kill_switch_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'payment_kill_switch_release_requests: kill_switch_id % does not exist', NEW.kill_switch_id;
        END IF;
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

    IF NEW.status = 'approved' THEN
        SELECT engaged_by_scope INTO v_sw_scope FROM payment_kill_switches WHERE id = OLD.kill_switch_id;
        IF v_sw_scope = 'platform' AND v_scope <> 'platform' THEN
            RAISE EXCEPTION 'payment_kill_switch_release_requests: only a platform session may approve release of a platform-engaged switch';
        END IF;
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

CREATE OR REPLACE FUNCTION payment_kill_switches_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor UUID;
    v_scope TEXT;
    v_req   RECORD;
BEGIN
    SELECT actor, scope INTO v_actor, v_scope FROM payment_kill_switch_session();
    NEW.changed_by := v_actor;
    NEW.changed_by_scope := v_scope;

    IF TG_OP = 'INSERT' THEN
        NEW.release_request_id := NULL;
        NEW.version := 1;
        NEW.engaged_by_scope := CASE WHEN NEW.engaged THEN v_scope ELSE NULL END;
        RETURN NEW;
    END IF;

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
        NEW.engaged_by_scope := v_scope;
        NEW.release_request_id := NULL;

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

CREATE OR REPLACE FUNCTION payment_kill_switch_session(OUT actor UUID, OUT scope TEXT) AS $$
DECLARE
    v_tenant   TEXT := NULLIF(current_setting('app.tenant_id', true), '');
    v_principal TEXT := NULLIF(current_setting('app.principal_id', true), '');
    v_player   TEXT := NULLIF(current_setting('app.player_account_id', true), '');
    v_platform TEXT := NULLIF(current_setting('app.platform_admin_principal_id', true), '');
BEGIN
    IF v_platform IS NOT NULL AND v_tenant IS NULL AND v_player IS NULL THEN
        IF NOT EXISTS (SELECT 1 FROM staff_users su WHERE su.id = v_platform::uuid AND su.tenant_id IS NULL) THEN
            RAISE EXCEPTION 'payment_kill_switch_session: platform principal % is not a platform-scoped staff principal', v_platform;
        END IF;
        actor := v_platform::uuid;
        scope := 'platform';
        RETURN;
    END IF;

    IF v_tenant IS NOT NULL AND v_principal IS NOT NULL AND v_player IS NULL AND v_platform IS NULL THEN
        IF NOT EXISTS (SELECT 1 FROM staff_users su WHERE su.id = v_principal::uuid AND su.tenant_id = v_tenant::uuid) THEN
            RAISE EXCEPTION 'payment_kill_switch_session: principal % is not a tenant-scoped staff principal of tenant %', v_principal, v_tenant;
        END IF;
        actor := v_principal::uuid;
        scope := 'tenant';
        RETURN;
    END IF;

    RAISE EXCEPTION 'payment_kill_switch_session: unresolvable session scope (tenant=%, principal=%, player=%, platform=%)', v_tenant, v_principal, v_player, v_platform;
END;
$$ LANGUAGE plpgsql STABLE;
