-- PRH-I1 kill switch (ADR 0095 §10.2/§10.2.1/§10.2.2). NOTE ON NUMBERING:
-- the ADR text (as last edited) still says "migration 0102" for this
-- table; the orchestrator's migration-number allocation changed twice
-- since that text was written (0102 was reallocated to payment statement
-- reconciliation, PRH-I5) and this is migration 0105 on disk. This
-- comment is the implementation record of that renumbering; ADR 0095's
-- own text is updated separately (see the ADR's implementation-record
-- addendum).
--
-- Two tables:
--   payment_kill_switches                 - the switch itself.
--   payment_kill_switch_release_requests  - the four-eyes release request.
--
-- Every actor/scope column is FORCED from the database session (never
-- application-supplied) by payment_kill_switch_session(), which derives
-- exactly one of 'tenant'/'platform' from the same GUCs
-- internal/db.WithPrincipalScope/WithPlatformAdmin set
-- (app.tenant_id/app.principal_id/app.platform_admin_principal_id/
-- app.player_account_id), independently re-validated against staff_users
-- (the migration 0044 precedent), and raises for any other combination -
-- nothing is ever written under an unresolvable session shape.

-- --- session-scope resolution ------------------------------------------

CREATE FUNCTION payment_kill_switch_session(OUT actor UUID, OUT scope TEXT) AS $$
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

-- --- payment_kill_switches -----------------------------------------------

CREATE TABLE payment_kill_switches (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL,
    provider_scope      TEXT NOT NULL CHECK (provider_scope = '*' OR octet_length(provider_scope) BETWEEN 1 AND 64),
    operation_scope     TEXT NOT NULL CHECK (operation_scope IN ('deposit', 'payout', '*')),
    engaged             BOOLEAN NOT NULL DEFAULT false,
    engaged_by_scope    TEXT NULL CHECK (engaged_by_scope IN ('tenant', 'platform')),
    CHECK (engaged = (engaged_by_scope IS NOT NULL)),
    release_request_id  UUID NULL,
    reason_code         TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64),
    changed_by          UUID NOT NULL,
    changed_by_scope    TEXT NOT NULL CHECK (changed_by_scope IN ('tenant', 'platform')),
    changed_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    version             BIGINT NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, provider_scope, operation_scope),
    -- RV-0095 L5(b): at most one switch may hold a given release request at
    -- once (defence in depth; the composite FK plus the request's own
    -- kill_switch_id already bind a request to exactly one switch).
    UNIQUE (release_request_id)
);

CREATE INDEX payment_kill_switches_tenant ON payment_kill_switches (tenant_id);
CREATE INDEX payment_kill_switches_engaged ON payment_kill_switches (tenant_id, provider_scope, operation_scope) WHERE engaged;

CREATE FUNCTION payment_kill_switches_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor UUID;
    v_scope TEXT;
    v_req   RECORD;
BEGIN
    SELECT actor, scope INTO v_actor, v_scope FROM payment_kill_switch_session();
    NEW.changed_by := v_actor;
    NEW.changed_by_scope := v_scope;

    IF TG_OP = 'INSERT' THEN
        -- Point 8 (RV-0095 L5(b)): a new row can never pre-consume a
        -- request meant for a different (or a not-yet-existing) switch.
        NEW.release_request_id := NULL;
        NEW.version := 1;
        NEW.engaged_by_scope := CASE WHEN NEW.engaged THEN v_scope ELSE NULL END;
        RETURN NEW;
    END IF;

    -- UPDATE from here on.

    -- Point 2 (N1): scope is immutable.
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.provider_scope IS DISTINCT FROM OLD.provider_scope
        OR NEW.operation_scope IS DISTINCT FROM OLD.operation_scope
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'payment_kill_switches: id/tenant_id/provider_scope/operation_scope/created_at are immutable';
    END IF;

    -- A tenant-scope session may never touch a platform-engaged row at
    -- all (N2(b)/S95-C7), whatever the requested change.
    IF OLD.engaged_by_scope = 'platform' AND v_scope <> 'platform' THEN
        RAISE EXCEPTION 'payment_kill_switches: row is platform-engaged; only a platform session may modify it';
    END IF;

    -- Point 3: version is strictly monotonic. Forced, never trusted from
    -- the client, so a re-engage between request and approval always
    -- invalidates any in-flight release request through this bump.
    NEW.version := OLD.version + 1;

    IF NOT OLD.engaged AND NEW.engaged THEN
        -- Point 4: engage (false -> true).
        NEW.engaged_by_scope := v_scope;
        NEW.release_request_id := NULL;

    ELSIF OLD.engaged AND NEW.engaged THEN
        -- Point 5: re-engage / reason update (true -> true). engaged_by_scope
        -- may only ever equal the acting session's own scope going forward -
        -- a platform session re-engaging takes over (tenant -> platform);
        -- a tenant session can only ever reach this branch when
        -- OLD.engaged_by_scope was already 'tenant' (platform-engaged rows
        -- were refused above), so this can never assign 'platform' -> 'tenant'.
        NEW.engaged_by_scope := v_scope;
        NEW.release_request_id := NULL;

        -- KS-L6: a platform takeover of a tenant containment cancels any
        -- open tenant release request for this switch, so the platform's
        -- own containment can never be raced open by a request the tenant
        -- filed before the takeover.
        IF OLD.engaged_by_scope = 'tenant' AND v_scope = 'platform' THEN
            UPDATE payment_kill_switch_release_requests
               SET status = 'cancelled'
             WHERE kill_switch_id = OLD.id AND status = 'open';
        END IF;

    ELSIF OLD.engaged AND NOT NEW.engaged THEN
        -- Point 6 (N4): release. NEW.release_request_id must reference an
        -- approved request for this exact switch/version, decided in THIS
        -- transaction, by a distinct principal from the requester.
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
        -- false -> false: a reason-code correction on a never-engaged row.
        NEW.engaged_by_scope := NULL;
        NEW.release_request_id := NULL;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER payment_kill_switches_guard
    BEFORE INSERT OR UPDATE ON payment_kill_switches
    FOR EACH ROW EXECUTE FUNCTION payment_kill_switches_guard();

-- Point 1: DELETE is always rejected.
CREATE TRIGGER payment_kill_switches_no_delete
    BEFORE DELETE ON payment_kill_switches
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER payment_kill_switches_no_truncate
    BEFORE TRUNCATE ON payment_kill_switches
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

ALTER TABLE payment_kill_switches ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_kill_switches FORCE ROW LEVEL SECURITY;

-- §10.2.1 tenant family.
CREATE POLICY tenant_scope ON payment_kill_switches
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

-- §10.2.1 platform family (migration 0075 precedent): visible/writable for
-- ANY tenant's rows from a genuine platform session; the target tenant is
-- taken from the platform route path (§10.5), never asserted here.
CREATE POLICY platform_scope ON payment_kill_switches
    FOR ALL
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- --- payment_kill_switch_release_requests --------------------------------

CREATE TABLE payment_kill_switch_release_requests (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL,
    kill_switch_id     UUID NOT NULL REFERENCES payment_kill_switches (id),
    expected_version   BIGINT NOT NULL,
    reason_code        TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64),
    requested_by       UUID NOT NULL,
    requested_by_scope TEXT NOT NULL CHECK (requested_by_scope IN ('tenant', 'platform')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at         TIMESTAMPTZ NOT NULL,
    status             TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'approved', 'cancelled', 'expired')),
    approved_by        UUID NULL,
    approved_by_scope  TEXT NULL CHECK (approved_by_scope IN ('tenant', 'platform')),
    decided_at         TIMESTAMPTZ NULL,
    decided_txid       BIGINT NULL,
    CHECK ((status = 'approved') = (approved_by IS NOT NULL)),
    CHECK ((status = 'approved') = (approved_by_scope IS NOT NULL)),
    CHECK ((status = 'approved') = (decided_at IS NOT NULL)),
    CHECK ((status = 'approved') = (decided_txid IS NOT NULL)),
    -- Required for the composite FK from payment_kill_switches.
    UNIQUE (tenant_id, id)
);

ALTER TABLE payment_kill_switches
    ADD CONSTRAINT payment_kill_switches_release_request_fk
    FOREIGN KEY (tenant_id, release_request_id)
    REFERENCES payment_kill_switch_release_requests (tenant_id, id);

-- At most one OPEN request per switch (also what makes KS-L6's cancellation
-- meaningful: there is never more than one open slot to cancel).
CREATE UNIQUE INDEX payment_kill_switch_release_requests_one_open
    ON payment_kill_switch_release_requests (kill_switch_id) WHERE status = 'open';

CREATE INDEX payment_kill_switch_release_requests_tenant ON payment_kill_switch_release_requests (tenant_id);

CREATE FUNCTION payment_kill_switch_release_requests_guard() RETURNS TRIGGER AS $$
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

CREATE TRIGGER payment_kill_switch_release_requests_guard
    BEFORE INSERT OR UPDATE ON payment_kill_switch_release_requests
    FOR EACH ROW EXECUTE FUNCTION payment_kill_switch_release_requests_guard();

CREATE TRIGGER payment_kill_switch_release_requests_no_delete
    BEFORE DELETE ON payment_kill_switch_release_requests
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER payment_kill_switch_release_requests_no_truncate
    BEFORE TRUNCATE ON payment_kill_switch_release_requests
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

ALTER TABLE payment_kill_switch_release_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_kill_switch_release_requests FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_scope ON payment_kill_switch_release_requests
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

CREATE POLICY platform_scope ON payment_kill_switch_release_requests
    FOR ALL
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
