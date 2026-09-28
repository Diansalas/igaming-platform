-- PRH-2 K1 (ADR 0099): scoped financial capability grants.
--
-- Two layers, both required for any future governed financial action
-- (ADR 0100/0101, K2/K3): the static HTTP permission (checked by
-- internal/auth) AND an in-force grant row, read in the action's own
-- transaction, never trusted from a JWT. This migration builds the grant
-- model (HD-PRH2-2 = (c): tenant-requested, platform co-approved) and the
-- "platform principal acting in tenant X" session family (HD-PRH2-6),
-- fenced by the ADR 0099 §6.4 restrictive policies. K1 does NOT touch the
-- ledger or projection tables - those fences arrive with the K2 migration
-- (0112) per ADR 0099 §6.6/§6.7.
--
-- Every RAISE in this file carries a dedicated SQLSTATE, class 'CG'
-- (capability grants), so application code classifies by code, never by
-- message text:
--   CG001 unresolvable/invalid actor session (financial_actor_session)
--   CG002 actor has no linked person_id on a capability-grant write
--   CG010 grant-request guard violation (R-1/4/5/6/9/10/11/12/13/14, TTL)
--   CG011 grant-approval guard violation (R-2/3/4/7/8/10/12/14, I-5)
--   CG012 grant guard violation (insert/revoke invariants, R-10/12/13/14)
--   CG020 acting session is not valid (financial_acting_session_open)
--   CG099 down-migration refused while rows exist
--
-- No SECURITY DEFINER anywhere (the 0096 B7 precedent). FORCE ROW LEVEL
-- SECURITY on every new table. No FOR ALL permissive policy. DELETE and
-- TRUNCATE refused by trigger on every new table.

-- =========================================================================
-- 1. GUC-shape and actor-derivation functions (§10.1)
-- =========================================================================

-- True if either "acting" GUC is set. Used to gate the §6.4 restrictive
-- fence on the seven exposed existing tables.
CREATE FUNCTION financial_acting_gucs_present() RETURNS boolean AS $$
    SELECT NULLIF(current_setting('app.acting_tenant_id', true), '') IS NOT NULL
        OR NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NOT NULL;
$$ LANGUAGE sql STABLE;

-- The exact C-1 shape (security confirmation, binding on K1): both acting
-- GUCs set AND every other principal-shaped GUC unset. Used only by the
-- two recursion-exempt tables' acting policies (staff_users,
-- staff_capability_grants) so their policies never call
-- financial_acting_session_valid() (which itself reads those tables).
CREATE FUNCTION financial_acting_gucs_exact() RETURNS boolean AS $$
    SELECT NULLIF(current_setting('app.acting_tenant_id', true), '') IS NOT NULL
       AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NOT NULL
       AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
       AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
       AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
       AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
       AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL;
$$ LANGUAGE sql STABLE;

-- staff_capability_grant_in_force: any unrevoked, in-window grant for
-- (p_tenant, p_staff[, p_capability]) at p_at. p_capability = NULL means
-- "any financial capability" (used by financial_acting_session_valid()).
-- LANGUAGE plpgsql (not sql) is deliberate: staff_capability_grants is
-- created later in this same migration, and a "LANGUAGE sql" function
-- body is parsed (and its referenced relations resolved) eagerly at
-- CREATE FUNCTION time, which would fail on the forward reference.
-- plpgsql function bodies are parsed lazily, at first call.
CREATE FUNCTION staff_capability_grant_in_force(
    p_tenant uuid, p_staff uuid, p_capability text, p_at timestamptz
) RETURNS boolean AS $$
BEGIN
    RETURN EXISTS (
        SELECT 1 FROM staff_capability_grants g
         WHERE g.tenant_id = p_tenant
           AND g.grantee_staff_id = p_staff
           AND (p_capability IS NULL OR g.capability = p_capability)
           AND g.revoked_at IS NULL
           AND g.valid_from <= p_at
           AND (g.valid_until IS NULL OR p_at < g.valid_until)
    );
END;
$$ LANGUAGE plpgsql STABLE;

-- staff_capability_grant_overlaps: R-12 ("no overlapping validity
-- ranges"). True if any unrevoked grant for (p_tenant, p_staff,
-- p_capability) has a half-open [valid_from, valid_until) range that
-- overlaps [p_from, p_until) (a NULL end means unbounded/infinity).
-- Expired grants do not block (this is NOT "in force at now()" - queued,
-- back-to-back renewals are allowed). Used as a legible, non-binding
-- pre-check by the request and approval guards, and - after the per-key
-- pg_advisory_xact_lock - as the binding control in the grant INSERT
-- trigger. Same lazy-parse reasoning as staff_capability_grant_in_force
-- above: LANGUAGE plpgsql because staff_capability_grants does not exist
-- yet at CREATE FUNCTION time.
CREATE FUNCTION staff_capability_grant_overlaps(
    p_tenant uuid, p_staff uuid, p_capability text, p_from timestamptz, p_until timestamptz
) RETURNS boolean AS $$
BEGIN
    RETURN EXISTS (
        SELECT 1 FROM staff_capability_grants g
         WHERE g.tenant_id = p_tenant
           AND g.grantee_staff_id = p_staff
           AND g.capability = p_capability
           AND g.revoked_at IS NULL
           AND g.valid_from < COALESCE(p_until, 'infinity'::timestamptz)
           AND p_from < COALESCE(g.valid_until, 'infinity'::timestamptz)
    );
END;
$$ LANGUAGE plpgsql STABLE;

-- financial_acting_session_valid(): true only if the exact GUC shape
-- holds (§6.3, C-1), the acting principal is an active platform_admin
-- staff row, and it holds an in-force financial grant for the acting
-- tenant. NOT SECURITY DEFINER: runs under the caller's own RLS, which is
-- exactly why staff_users/staff_capability_grants need their GUC-only
-- recursion-exempt acting policies below.
CREATE FUNCTION financial_acting_session_valid() RETURNS boolean AS $$
DECLARE
    v_principal text := NULLIF(current_setting('app.acting_platform_principal_id', true), '');
    v_tenant    text := NULLIF(current_setting('app.acting_tenant_id', true), '');
    v_ok        boolean;
BEGIN
    IF NOT financial_acting_gucs_exact() THEN
        RETURN false;
    END IF;
    IF v_principal IS NULL OR v_tenant IS NULL THEN
        RETURN false;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM staff_users su
         WHERE su.id = v_principal::uuid
           AND su.tenant_id IS NULL
           AND su.role = 'platform_admin'
           AND su.status = 'active'
    ) THEN
        RETURN false;
    END IF;

    SELECT staff_capability_grant_in_force(v_tenant::uuid, v_principal::uuid, NULL, now()) INTO v_ok;
    RETURN coalesce(v_ok, false);
END;
$$ LANGUAGE plpgsql STABLE;

-- Raises CG020 unless financial_acting_session_valid(). This is the ONLY
-- thing WithPlatformActingInTenant calls to decide whether to proceed.
CREATE FUNCTION financial_acting_session_open() RETURNS boolean AS $$
BEGIN
    IF NOT financial_acting_session_valid() THEN
        RAISE EXCEPTION 'financial_acting_session_open: acting session is not valid for tenant %, principal %',
            NULLIF(current_setting('app.acting_tenant_id', true), ''),
            NULLIF(current_setting('app.acting_platform_principal_id', true), '')
            USING ERRCODE = 'CG020';
    END IF;
    RETURN true;
END;
$$ LANGUAGE plpgsql STABLE;

-- financial_actor_session (§7.1): resolves exactly one of tenant/
-- platform/platform_acting from the session GUCs, raising CG001 for
-- anything else (including any mixed shape). Actor status must be
-- 'active'. This is the sole source for every forced *_by/*_by_scope/
-- *_by_person_id column on 0112's own tables (the 0105:88-89 precedent).
-- An acting-shaped session DOES resolve here (K2/K3 need it); K1's own
-- request/approval/grant guard triggers separately refuse it under R-10.
CREATE FUNCTION financial_actor_session(
    OUT actor uuid, OUT scope text, OUT tenant uuid, OUT person_id uuid, OUT role text
) AS $$
DECLARE
    v_tenant           text := NULLIF(current_setting('app.tenant_id', true), '');
    v_principal        text := NULLIF(current_setting('app.principal_id', true), '');
    v_platform         text := NULLIF(current_setting('app.platform_admin_principal_id', true), '');
    v_player           text := NULLIF(current_setting('app.player_account_id', true), '');
    v_service          text := NULLIF(current_setting('app.platform_service_id', true), '');
    v_acting_tenant    text := NULLIF(current_setting('app.acting_tenant_id', true), '');
    v_acting_principal text := NULLIF(current_setting('app.acting_platform_principal_id', true), '');
    rec RECORD;
BEGIN
    IF v_acting_tenant IS NOT NULL OR v_acting_principal IS NOT NULL THEN
        IF NOT financial_acting_session_valid() THEN
            RAISE EXCEPTION 'financial_actor_session: acting session is not valid' USING ERRCODE = 'CG020';
        END IF;
        SELECT su.id, su.person_id, su.role INTO rec FROM staff_users su WHERE su.id = v_acting_principal::uuid;
        actor := rec.id; scope := 'platform_acting'; tenant := v_acting_tenant::uuid;
        person_id := rec.person_id; role := rec.role;
        RETURN;
    END IF;

    IF v_tenant IS NOT NULL AND v_principal IS NOT NULL
       AND v_platform IS NULL AND v_player IS NULL AND v_service IS NULL THEN
        SELECT su.id, su.person_id, su.role, su.status INTO rec
          FROM staff_users su WHERE su.id = v_principal::uuid AND su.tenant_id = v_tenant::uuid;
        IF NOT FOUND OR rec.status <> 'active' THEN
            RAISE EXCEPTION 'financial_actor_session: tenant principal % is not an active staff member of tenant %', v_principal, v_tenant
                USING ERRCODE = 'CG001';
        END IF;
        actor := rec.id; scope := 'tenant'; tenant := v_tenant::uuid;
        person_id := rec.person_id; role := rec.role;
        RETURN;
    END IF;

    IF v_platform IS NOT NULL
       AND v_tenant IS NULL AND v_player IS NULL AND v_service IS NULL THEN
        SELECT su.id, su.person_id, su.role, su.status INTO rec
          FROM staff_users su WHERE su.id = v_platform::uuid AND su.tenant_id IS NULL;
        IF NOT FOUND OR rec.status <> 'active' THEN
            RAISE EXCEPTION 'financial_actor_session: platform principal % is not an active platform staff member', v_platform
                USING ERRCODE = 'CG001';
        END IF;
        actor := rec.id; scope := 'platform'; tenant := NULL;
        person_id := rec.person_id; role := rec.role;
        RETURN;
    END IF;

    RAISE EXCEPTION 'financial_actor_session: unresolvable session shape (tenant=%, principal=%, platform=%, player=%, service=%, acting_tenant=%, acting_principal=%)',
        v_tenant, v_principal, v_platform, v_player, v_service, v_acting_tenant, v_acting_principal
        USING ERRCODE = 'CG001';
END;
$$ LANGUAGE plpgsql STABLE;

-- =========================================================================
-- 2. Reference tables (family R; §10.2)
-- =========================================================================

CREATE TABLE financial_capability_catalogue (
    capability                TEXT PRIMARY KEY,
    operation_kind            TEXT NOT NULL CHECK (operation_kind IN ('ledger_adjustment', 'payment_force_resolve')),
    action                    TEXT NOT NULL CHECK (action IN ('initiate', 'request', 'approve')),
    eligible_tenant_roles     TEXT[] NOT NULL,
    platform_grantee_allowed  BOOLEAN NOT NULL
);

-- Seed rows are inserted BEFORE FORCE ROW LEVEL SECURITY is applied (the
-- 0110 alert_kinds precedent) - once FORCE is on, even the owning
-- migration role is bound by RLS, and these tables' only policy is
-- SELECT-only.
INSERT INTO financial_capability_catalogue (capability, operation_kind, action, eligible_tenant_roles, platform_grantee_allowed) VALUES
    ('ledger_adjustment:initiate',     'ledger_adjustment',      'initiate', '{finance}', true),
    ('ledger_adjustment:approve',      'ledger_adjustment',      'approve',  '{finance}', true),
    ('payment_force_resolve:request',  'payment_force_resolve',  'request',  '{finance}', true),
    ('payment_force_resolve:approve',  'payment_force_resolve',  'approve',  '{finance}', true);

ALTER TABLE financial_capability_catalogue ENABLE ROW LEVEL SECURITY;
ALTER TABLE financial_capability_catalogue FORCE ROW LEVEL SECURITY;
CREATE POLICY reference_read ON financial_capability_catalogue FOR SELECT USING (true);

CREATE TABLE financial_governance_permissions (
    permission TEXT PRIMARY KEY,
    roles      TEXT[] NOT NULL
);

-- Mirrors internal/auth/permission.go's static role -> permission map for
-- these seven governance permissions exactly (§3.2). Test A-15 pins the
-- two in agreement.
INSERT INTO financial_governance_permissions (permission, roles) VALUES
    ('capability_grant:request', '{tenant_admin,platform_admin}'),
    ('capability_grant:approve', '{platform_admin}'),
    ('capability_grant:revoke',  '{tenant_admin,platform_admin}'),
    ('capability_grant:read',    '{tenant_admin,compliance,platform_admin}'),
    ('financial_policy:author',  '{platform_admin}'),
    ('financial_policy:tighten', '{tenant_admin}'),
    ('financial_policy:read',    '{tenant_admin,finance,compliance,platform_admin}');

ALTER TABLE financial_governance_permissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE financial_governance_permissions FORCE ROW LEVEL SECURITY;
CREATE POLICY reference_read ON financial_governance_permissions FOR SELECT USING (true);

-- financial_capability_settings: no row is inserted by the ADR itself -
-- with no row, G-P2 requests fail closed (R-13 needs the max lifetime to
-- validate against). K1 inserts the technical security default below and
-- records it in the ADR 0099 implementation-status section: 4 hours. This
-- is a technical security default (bounds how long a platform principal
-- can act in a tenant on a single grant), not a business/money policy
-- value, and is configurable by a future migration or STAFF-LIFECYCLE-1
-- admin surface.
CREATE TABLE financial_capability_settings (
    key            TEXT PRIMARY KEY CHECK (key IN ('acting_grant_max_lifetime')),
    value_interval INTERVAL NOT NULL CHECK (value_interval > interval '0')
);

INSERT INTO financial_capability_settings (key, value_interval) VALUES
    ('acting_grant_max_lifetime', interval '4 hours');

ALTER TABLE financial_capability_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE financial_capability_settings FORCE ROW LEVEL SECURITY;
CREATE POLICY reference_read ON financial_capability_settings FOR SELECT USING (true);

-- =========================================================================
-- 3. staff_capability_grant_requests (families T, P; no A) — §10.3
-- =========================================================================

CREATE TABLE staff_capability_grant_requests (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID NOT NULL REFERENCES tenants(id),
    grantee_staff_id        UUID NOT NULL REFERENCES staff_users(id),
    grantee_scope           TEXT NOT NULL CHECK (grantee_scope IN ('tenant', 'platform')),
    capability              TEXT NOT NULL REFERENCES financial_capability_catalogue (capability),
    valid_from              TIMESTAMPTZ NOT NULL,
    valid_until             TIMESTAMPTZ NULL CHECK (valid_until IS NULL OR valid_until > valid_from),
    reason_code             TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64),
    requested_by            UUID NOT NULL,
    requested_by_scope      TEXT NOT NULL CHECK (requested_by_scope IN ('tenant', 'platform')),
    requested_by_person_id  UUID NOT NULL,
    -- Captured (forced) from the grantee's OWN staff_users row at INSERT
    -- time, when the acting session doing the inserting can actually see
    -- it (a tenant session sees its own tenant's staff; see the trigger
    -- below). This is what lets the LATER platform approval decision
    -- (staff_capability_grant_approvals_guard) run its own R-4/R-8
    -- distinct-Person check without needing to re-read staff_users itself -
    -- migration 0011's dual_scope_isolation policy leaves a PLAIN platform
    -- session structurally blind to any tenant-scoped staff_users row (see
    -- internal/httpserver/admin_routes.go's newLinkPlatformStaffPersonHandler
    -- doc comment for the same, independently-discovered fact), so a
    -- platform approver could never otherwise learn a tenant grantee's
    -- person_id at decision time. Carrying it forward from request-creation
    -- time needs no new tenant-isolation mechanism and widens no RLS policy.
    grantee_person_id       UUID NOT NULL,
    status                  TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'cancelled', 'expired')),
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at              TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, id)
);

CREATE UNIQUE INDEX staff_capability_grant_requests_one_pending
    ON staff_capability_grant_requests (tenant_id, grantee_staff_id, capability) WHERE status = 'pending';
CREATE INDEX staff_capability_grant_requests_tenant ON staff_capability_grant_requests (tenant_id);

CREATE FUNCTION staff_capability_grant_requests_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor  RECORD;
    v_grantee RECORD;
    v_max_lifetime INTERVAL;
BEGIN
    SELECT * INTO v_actor FROM financial_actor_session();

    IF TG_OP = 'INSERT' THEN
        -- R-10: an acting session never requests a grant.
        IF v_actor.scope = 'platform_acting' THEN
            RAISE EXCEPTION 'staff_capability_grant_requests: an acting session may not request a grant' USING ERRCODE = 'CG010';
        END IF;
        IF v_actor.person_id IS NULL THEN
            RAISE EXCEPTION 'staff_capability_grant_requests: requester has no linked person_id' USING ERRCODE = 'CG002';
        END IF;

        NEW.requested_by := v_actor.actor;
        NEW.requested_by_scope := v_actor.scope;
        NEW.requested_by_person_id := v_actor.person_id;
        NEW.status := 'pending';
        NEW.created_at := now();
        -- Request TTL: a technical default (24h, the 0105 precedent),
        -- never longer than requested, never a money value.
        NEW.expires_at := LEAST(COALESCE(NEW.expires_at, now() + interval '24 hours'), now() + interval '24 hours');

        -- R-14 (no backdated or already-expired grant), part 1: valid_from
        -- defaults to now() and must sit within a 5-minute clock-skew
        -- tolerance of now() (the same technical value as
        -- PaymentCoverageMaxClockSkew) and no later than the request's own
        -- expiry. valid_until, if given, must not already be in the past.
        -- This is a legible, non-binding pre-check: the grant INSERT
        -- trigger's own GREATEST(valid_from, now()) clamp plus its
        -- CHECK (valid_from >= granted_at) is the binding control, so this
        -- 5-minute value grants no authority by itself.
        NEW.valid_from := COALESCE(NEW.valid_from, now());
        IF NEW.valid_from < now() - interval '5 minutes' THEN
            RAISE EXCEPTION 'staff_capability_grant_requests: valid_from is backdated beyond the clock-skew tolerance (R-14)' USING ERRCODE = 'CG010';
        END IF;
        IF NEW.valid_from > NEW.expires_at THEN
            RAISE EXCEPTION 'staff_capability_grant_requests: valid_from is after the request''s own expiry (R-14)' USING ERRCODE = 'CG010';
        END IF;
        IF NEW.valid_until IS NOT NULL AND NEW.valid_until <= now() THEN
            RAISE EXCEPTION 'staff_capability_grant_requests: valid_until is already in the past (R-14)' USING ERRCODE = 'CG010';
        END IF;

        SELECT id, tenant_id, role, status, person_id INTO v_grantee
          FROM staff_users WHERE id = NEW.grantee_staff_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'staff_capability_grant_requests: grantee % does not exist', NEW.grantee_staff_id USING ERRCODE = 'CG010';
        END IF;
        NEW.grantee_scope := CASE WHEN v_grantee.tenant_id IS NULL THEN 'platform' ELSE 'tenant' END;
        NEW.grantee_person_id := v_grantee.person_id;

        -- R-1: grantee != requester.
        IF NEW.grantee_staff_id = v_actor.actor THEN
            RAISE EXCEPTION 'staff_capability_grant_requests: a staff member may not request a grant for themselves (R-1)' USING ERRCODE = 'CG010';
        END IF;
        -- R-4: requester and grantee have non-NULL, distinct person_id.
        IF v_grantee.person_id IS NULL THEN
            RAISE EXCEPTION 'staff_capability_grant_requests: grantee has no linked person_id (R-4)' USING ERRCODE = 'CG010';
        END IF;
        IF v_grantee.person_id = v_actor.person_id THEN
            RAISE EXCEPTION 'staff_capability_grant_requests: requester and grantee must be distinct Persons (R-4)' USING ERRCODE = 'CG010';
        END IF;
        IF v_grantee.status <> 'active' THEN
            RAISE EXCEPTION 'staff_capability_grant_requests: grantee is not active (R-9)' USING ERRCODE = 'CG010';
        END IF;

        DECLARE
            v_cat RECORD;
        BEGIN
            SELECT * INTO v_cat FROM financial_capability_catalogue WHERE capability = NEW.capability;
            IF NOT FOUND THEN
                RAISE EXCEPTION 'staff_capability_grant_requests: unknown capability %', NEW.capability USING ERRCODE = 'CG010';
            END IF;

            IF v_actor.scope = 'tenant' THEN
                -- R-5: a tenant requester's tenant = request tenant = grantee tenant.
                IF NEW.tenant_id <> v_actor.tenant OR v_grantee.tenant_id IS DISTINCT FROM v_actor.tenant THEN
                    RAISE EXCEPTION 'staff_capability_grant_requests: a tenant requester may only request within its own tenant (R-5)' USING ERRCODE = 'CG010';
                END IF;
                -- R-6: a tenant requester may not name a platform grantee.
                IF NEW.grantee_scope = 'platform' THEN
                    RAISE EXCEPTION 'staff_capability_grant_requests: a tenant requester may not name a platform grantee (R-6)' USING ERRCODE = 'CG010';
                END IF;
            END IF;

            -- R-9: grantee role eligible; platform grantee only via G-P2.
            IF NEW.grantee_scope = 'platform' THEN
                IF NOT v_cat.platform_grantee_allowed THEN
                    RAISE EXCEPTION 'staff_capability_grant_requests: capability % has no platform grantee (R-9)', NEW.capability USING ERRCODE = 'CG010';
                END IF;
                IF v_grantee.role <> 'platform_admin' THEN
                    RAISE EXCEPTION 'staff_capability_grant_requests: a platform grantee must be platform_admin (R-9)' USING ERRCODE = 'CG010';
                END IF;
                -- R-13: a G-P2 grant has NOT NULL valid_until, bounded by
                -- the configured max lifetime. Fail closed with no row.
                IF NEW.valid_until IS NULL THEN
                    RAISE EXCEPTION 'staff_capability_grant_requests: a platform (G-P2) grant requires a valid_until (R-13)' USING ERRCODE = 'CG010';
                END IF;
                SELECT value_interval INTO v_max_lifetime FROM financial_capability_settings WHERE key = 'acting_grant_max_lifetime';
                IF v_max_lifetime IS NULL THEN
                    RAISE EXCEPTION 'staff_capability_grant_requests: no acting_grant_max_lifetime setting; refusing G-P2 request (fail closed)' USING ERRCODE = 'CG010';
                END IF;
                IF NEW.valid_until > NEW.valid_from + v_max_lifetime THEN
                    RAISE EXCEPTION 'staff_capability_grant_requests: valid_until exceeds acting_grant_max_lifetime (R-13)' USING ERRCODE = 'CG010';
                END IF;
            ELSE
                IF NOT (v_grantee.role = ANY (v_cat.eligible_tenant_roles)) THEN
                    RAISE EXCEPTION 'staff_capability_grant_requests: role % is not eligible for capability % (R-9)', v_grantee.role, NEW.capability USING ERRCODE = 'CG010';
                END IF;
            END IF;
        END;

        -- R-12 ("no overlapping validity ranges"; legible, non-binding
        -- pre-check - the grant INSERT trigger's own per-key
        -- pg_advisory_xact_lock plus overlap check is the binding
        -- control): no unrevoked grant already occupies an overlapping
        -- window. Expired grants and queued back-to-back renewals do not
        -- block.
        IF staff_capability_grant_overlaps(NEW.tenant_id, NEW.grantee_staff_id, NEW.capability, NEW.valid_from, NEW.valid_until) THEN
            RAISE EXCEPTION 'staff_capability_grant_requests: grantee already holds an overlapping grant for this capability (R-12)' USING ERRCODE = 'CG010';
        END IF;

        RETURN NEW;
    END IF;

    -- UPDATE: cancel-only by the requester (or any eligible actor in the
    -- request's own tenant/platform scope), pending -> cancelled/expired
    -- only. No other column may change.
    IF OLD.status <> 'pending' THEN
        RAISE EXCEPTION 'staff_capability_grant_requests: request % is in terminal status % and is immutable', OLD.id, OLD.status USING ERRCODE = 'CG010';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.grantee_staff_id IS DISTINCT FROM OLD.grantee_staff_id
        OR NEW.grantee_scope IS DISTINCT FROM OLD.grantee_scope
        OR NEW.capability IS DISTINCT FROM OLD.capability
        OR NEW.valid_from IS DISTINCT FROM OLD.valid_from
        OR NEW.valid_until IS DISTINCT FROM OLD.valid_until
        OR NEW.reason_code IS DISTINCT FROM OLD.reason_code
        OR NEW.requested_by IS DISTINCT FROM OLD.requested_by
        OR NEW.requested_by_scope IS DISTINCT FROM OLD.requested_by_scope
        OR NEW.requested_by_person_id IS DISTINCT FROM OLD.requested_by_person_id
        OR NEW.grantee_person_id IS DISTINCT FROM OLD.grantee_person_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
        OR NEW.expires_at IS DISTINCT FROM OLD.expires_at
    THEN
        RAISE EXCEPTION 'staff_capability_grant_requests: only status may change, and only pending -> cancelled/expired' USING ERRCODE = 'CG010';
    END IF;
    IF NEW.status IN ('approved', 'rejected') THEN
        -- The ONLY legitimate way to reach this branch is the approvals
        -- guard trigger's own same-statement UPDATE (staff_capability_
        -- grant_approvals_guard), which always inserts a matching,
        -- same-transaction decision first. A direct client UPDATE setting
        -- status='approved'/'rejected' finds no such row and is refused -
        -- this is what makes "approved is set by the approval trigger,
        -- never directly" an enforced fact, not just a comment.
        IF NOT EXISTS (
            SELECT 1 FROM staff_capability_grant_approvals a
             WHERE a.request_id = OLD.id
               AND a.decided_txid = txid_current()
               AND a.decision = CASE WHEN NEW.status = 'approved' THEN 'approve' ELSE 'reject' END
        ) THEN
            RAISE EXCEPTION 'staff_capability_grant_requests: status may only move to approved/rejected via a same-transaction approval decision' USING ERRCODE = 'CG010';
        END IF;
    ELSIF NEW.status NOT IN ('cancelled', 'expired') THEN
        RAISE EXCEPTION 'staff_capability_grant_requests: status may only move pending -> cancelled|expired|approved|rejected' USING ERRCODE = 'CG010';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER staff_capability_grant_requests_guard
    BEFORE INSERT OR UPDATE ON staff_capability_grant_requests
    FOR EACH ROW EXECUTE FUNCTION staff_capability_grant_requests_guard();

CREATE TRIGGER staff_capability_grant_requests_no_delete
    BEFORE DELETE ON staff_capability_grant_requests
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER staff_capability_grant_requests_no_truncate
    BEFORE TRUNCATE ON staff_capability_grant_requests
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

ALTER TABLE staff_capability_grant_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_capability_grant_requests FORCE ROW LEVEL SECURITY;

-- K1-C2 (security review of 0f34d36): per-command policies, never
-- FOR ALL, matching ADR 0099 §10.3's own "T: SELECT, INSERT, UPDATE"
-- (§10 preamble also states "no FOR ALL permissive policy" as a common
-- rule). Splitting also makes the down migration's restoration exact.
CREATE POLICY tenant_scope_select ON staff_capability_grant_requests
    FOR SELECT
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    );
CREATE POLICY tenant_scope_insert ON staff_capability_grant_requests
    FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    );
CREATE POLICY tenant_scope_update ON staff_capability_grant_requests
    FOR UPDATE
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    );

CREATE POLICY platform_scope_select ON staff_capability_grant_requests
    FOR SELECT
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    );
CREATE POLICY platform_scope_insert ON staff_capability_grant_requests
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    );
CREATE POLICY platform_scope_update ON staff_capability_grant_requests
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    );

-- =========================================================================
-- 4. staff_capability_grant_approvals (T read, P write; no A) — §10.4
-- =========================================================================

CREATE TABLE staff_capability_grant_approvals (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL,
    request_id             UUID NOT NULL UNIQUE,
    decision               TEXT NOT NULL CHECK (decision IN ('approve', 'reject')),
    decided_by             UUID NOT NULL,
    decided_by_scope       TEXT NOT NULL CHECK (decided_by_scope = 'platform'),
    decided_by_person_id   UUID NOT NULL,
    decided_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_txid           BIGINT NOT NULL,
    reason_code            TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64),
    FOREIGN KEY (tenant_id, request_id) REFERENCES staff_capability_grant_requests (tenant_id, id)
);

CREATE INDEX staff_capability_grant_approvals_tenant ON staff_capability_grant_approvals (tenant_id);

CREATE FUNCTION staff_capability_grant_approvals_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor         RECORD;
    v_req           RECORD;
    v_live_grantee  RECORD;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'staff_capability_grant_approvals: approvals are immutable (append-only)' USING ERRCODE = 'CG011';
    END IF;

    SELECT * INTO v_actor FROM financial_actor_session();
    -- R-10: an acting session never approves a grant. R-7: the approval's
    -- derived scope is platform (also enforced by the decided_by_scope
    -- CHECK, and by there being no platform_acting policy on this table).
    IF v_actor.scope <> 'platform' THEN
        RAISE EXCEPTION 'staff_capability_grant_approvals: only a platform session may decide a grant request (R-7)' USING ERRCODE = 'CG011';
    END IF;
    IF v_actor.person_id IS NULL THEN
        RAISE EXCEPTION 'staff_capability_grant_approvals: approver has no linked person_id' USING ERRCODE = 'CG002';
    END IF;

    SELECT * INTO v_req FROM staff_capability_grant_requests WHERE id = NEW.request_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'staff_capability_grant_approvals: request % does not exist', NEW.request_id USING ERRCODE = 'CG011';
    END IF;
    IF v_req.status <> 'pending' THEN
        RAISE EXCEPTION 'staff_capability_grant_approvals: request % is not pending', NEW.request_id USING ERRCODE = 'CG011';
    END IF;
    IF now() >= v_req.expires_at THEN
        RAISE EXCEPTION 'staff_capability_grant_approvals: request % has expired', NEW.request_id USING ERRCODE = 'CG011';
    END IF;
    -- R-14 (no backdated or already-expired grant), approval-side: a
    -- request whose window has already ended by decision time is refused.
    -- This is a legible, non-binding pre-check (the grant INSERT trigger's
    -- own clamp plus CHECK is binding); NULL valid_until is open-ended.
    IF NEW.decision = 'approve' AND v_req.valid_until IS NOT NULL AND v_req.valid_until <= now() THEN
        RAISE EXCEPTION 'staff_capability_grant_approvals: request %''s validity window has already ended (R-14)', NEW.request_id USING ERRCODE = 'CG011';
    END IF;

    -- R-2: approver != requester (principal). R-3: approver not in
    -- {requester, grantee} (principal). R-4/R-8: distinct, non-NULL
    -- Persons for requester, approver and grantee (three-way).
    IF v_actor.actor = v_req.requested_by OR v_actor.actor = v_req.grantee_staff_id THEN
        RAISE EXCEPTION 'staff_capability_grant_approvals: the approver must be neither the requester nor the grantee (R-2/R-3)' USING ERRCODE = 'CG011';
    END IF;
    IF v_actor.person_id = v_req.requested_by_person_id THEN
        RAISE EXCEPTION 'staff_capability_grant_approvals: the approver must be a distinct Person from the requester (R-4/R-8)' USING ERRCODE = 'CG011';
    END IF;
    -- v_req.grantee_person_id (not a fresh staff_users lookup): a plain
    -- platform approver session is structurally blind to a tenant-scoped
    -- grantee's staff_users row (migration 0011 dual_scope_isolation) -
    -- see that column's own doc comment on staff_capability_grant_requests
    -- for why it was captured at request-creation time instead.
    IF v_req.grantee_person_id IS NULL THEN
        RAISE EXCEPTION 'staff_capability_grant_approvals: grantee has no linked person_id (R-4)' USING ERRCODE = 'CG011';
    END IF;
    IF v_actor.person_id = v_req.grantee_person_id THEN
        RAISE EXCEPTION 'staff_capability_grant_approvals: the approver must be a distinct Person from the grantee (R-4/R-8)' USING ERRCODE = 'CG011';
    END IF;

    -- I-5 (architect ruling k1-architect-ruling-gp1.md, "approval,
    -- platform grantee"): unlike a tenant grantee, a plain platform
    -- approval session CAN see the grantee's own staff_users row (both
    -- are platform-scoped, so 0011 dual_scope_isolation does not blind
    -- it). Re-read it live and require status='active', role=
    -- 'platform_admin', and person_id still equal to the snapshot taken
    -- at request time - otherwise the grantee is no longer eligible for a
    -- G-P2 grant and the decision is refused, even though the request
    -- guard already checked all three at request time.
    IF NEW.decision = 'approve' AND v_req.grantee_scope = 'platform' THEN
        SELECT status, role, person_id INTO v_live_grantee
          FROM staff_users WHERE id = v_req.grantee_staff_id;
        IF NOT FOUND
           OR v_live_grantee.status <> 'active'
           OR v_live_grantee.role <> 'platform_admin'
           OR v_live_grantee.person_id IS DISTINCT FROM v_req.grantee_person_id
        THEN
            RAISE EXCEPTION 'staff_capability_grant_approvals: platform grantee % is no longer eligible for a G-P2 grant (I-5)', v_req.grantee_staff_id USING ERRCODE = 'CG011';
        END IF;
    END IF;

    -- R-12 ("no overlapping validity ranges"; legible, non-binding
    -- pre-check - the grant INSERT trigger's own per-key
    -- pg_advisory_xact_lock plus overlap check is the binding control):
    -- re-checked at decision time against the request's own window.
    IF NEW.decision = 'approve'
       AND staff_capability_grant_overlaps(v_req.tenant_id, v_req.grantee_staff_id, v_req.capability, v_req.valid_from, v_req.valid_until)
    THEN
        RAISE EXCEPTION 'staff_capability_grant_approvals: grantee already holds an overlapping grant for this capability (R-12)' USING ERRCODE = 'CG011';
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

CREATE TRIGGER staff_capability_grant_approvals_guard
    BEFORE INSERT OR UPDATE ON staff_capability_grant_approvals
    FOR EACH ROW EXECUTE FUNCTION staff_capability_grant_approvals_guard();

-- AFTER INSERT (not part of the BEFORE guard above): by the time this
-- runs, the approval row itself is already visible to other statements
-- in this same transaction, which is exactly what the requests table's
-- OWN guard trigger (staff_capability_grant_requests_guard) needs to see
-- to let this UPDATE move status to approved/rejected at all - see that
-- trigger's own comment.
CREATE FUNCTION staff_capability_grant_approvals_apply_to_request() RETURNS TRIGGER AS $$
BEGIN
    UPDATE staff_capability_grant_requests
       SET status = CASE WHEN NEW.decision = 'approve' THEN 'approved' ELSE 'rejected' END
     WHERE id = NEW.request_id;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER staff_capability_grant_approvals_apply_to_request
    AFTER INSERT ON staff_capability_grant_approvals
    FOR EACH ROW EXECUTE FUNCTION staff_capability_grant_approvals_apply_to_request();

CREATE TRIGGER staff_capability_grant_approvals_no_delete
    BEFORE DELETE ON staff_capability_grant_approvals
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER staff_capability_grant_approvals_no_truncate
    BEFORE TRUNCATE ON staff_capability_grant_approvals
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- Deferred constraint trigger (LF precedent): an 'approve' row commits
-- only if a grant row referencing it exists (the grant insert trigger,
-- below, is what actually creates that row in the same transaction).
CREATE FUNCTION staff_capability_grant_approvals_require_grant() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.decision = 'approve' AND NOT EXISTS (
        SELECT 1 FROM staff_capability_grants g WHERE g.approval_id = NEW.id
    ) THEN
        RAISE EXCEPTION 'staff_capability_grant_approvals: an approve decision must be accompanied by a grant row in the same transaction' USING ERRCODE = 'CG011';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER staff_capability_grant_approvals_require_grant
    AFTER INSERT ON staff_capability_grant_approvals
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION staff_capability_grant_approvals_require_grant();

ALTER TABLE staff_capability_grant_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_capability_grant_approvals FORCE ROW LEVEL SECURITY;

-- K1-C2 (security review of 0f34d36): per-command (T: SELECT only; P:
-- SELECT, INSERT - never FOR ALL, per ADR 0099 §10.4), and the missing
-- app.platform_service_id IS NULL arm added to every predicate on this
-- table (its absence meant a platform_admin + platform_service mixed
-- session could read every row).
CREATE POLICY tenant_scope_select ON staff_capability_grant_approvals
    FOR SELECT
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    );

CREATE POLICY platform_scope_select ON staff_capability_grant_approvals
    FOR SELECT
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    );
CREATE POLICY platform_scope_insert ON staff_capability_grant_approvals
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    );

-- =========================================================================
-- 5. staff_capability_grants (T, P, A-read) — §10.5
-- =========================================================================

CREATE TABLE staff_capability_grants (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL,
    grantee_staff_id    UUID NOT NULL REFERENCES staff_users(id),
    grantee_scope       TEXT NOT NULL CHECK (grantee_scope IN ('tenant', 'platform')),
    capability          TEXT NOT NULL REFERENCES financial_capability_catalogue (capability),
    request_id          UUID NOT NULL UNIQUE,
    approval_id         UUID NOT NULL UNIQUE,
    valid_from          TIMESTAMPTZ NOT NULL,
    valid_until         TIMESTAMPTZ NULL,
    granted_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at          TIMESTAMPTZ NULL,
    revoked_by          UUID NULL,
    revoked_by_scope    TEXT NULL CHECK (revoked_by_scope IN ('tenant', 'platform')),
    revoke_reason_code  TEXT NULL CHECK (revoke_reason_code IS NULL OR octet_length(revoke_reason_code) BETWEEN 1 AND 64),
    CHECK ((revoked_at IS NULL) = (revoked_by IS NULL)),
    CHECK ((revoked_at IS NULL) = (revoked_by_scope IS NULL)),
    CHECK ((revoked_at IS NULL) = (revoke_reason_code IS NULL)),
    -- R-13, restated as a CHECK: a platform (G-P2) grant always has a
    -- valid_until.
    CHECK (grantee_scope <> 'platform' OR valid_until IS NOT NULL),
    -- R-14: the INSERT trigger's GREATEST(requested valid_from, now())
    -- clamp is what makes this hold; the CHECK is defence in depth against
    -- a trigger bypass (accepted residual, ADR 0099 §16) - no grant can
    -- ever record a validity start before the moment it was actually
    -- granted.
    CHECK (valid_from >= granted_at),
    UNIQUE (tenant_id, id)
);

-- R-12 ("no overlapping validity ranges"): the partial unique index on
-- revoked_at IS NULL is DELETED (architect ruling k1-architect-ruling-
-- r12.md). It could only ever express "at most one unrevoked row", which
-- is stricter than the actual invariant (queued, back-to-back renewals
-- for the same key must be allowed) and would have refused a valid
-- renewal made before the prior grant's natural expiry. The binding
-- control is now the grant INSERT trigger's own per-key
-- pg_advisory_xact_lock followed by an overlap check (security-accepted
-- in place of a unique index, with conditions R12-a..R12-d below).
-- EXCLUDE USING gist is deferred pending the btree_gist extension
-- decision (PHASE-D-ARCH/SEC-P3-2); no CREATE EXTENSION here.
CREATE INDEX staff_capability_grants_tenant ON staff_capability_grants (tenant_id);
CREATE INDEX staff_capability_grants_grantee ON staff_capability_grants (grantee_staff_id);

CREATE FUNCTION staff_capability_grants_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor        RECORD;
    v_appr         RECORD;
    v_req          RECORD;
    v_max_lifetime INTERVAL;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO v_appr FROM staff_capability_grant_approvals WHERE id = NEW.approval_id;
        IF NOT FOUND OR v_appr.decision <> 'approve' OR v_appr.decided_txid <> txid_current() THEN
            RAISE EXCEPTION 'staff_capability_grants: a grant may only be inserted alongside its own approve decision, in the same transaction' USING ERRCODE = 'CG012';
        END IF;
        SELECT * INTO v_req FROM staff_capability_grant_requests WHERE id = v_appr.request_id;
        IF NOT FOUND OR v_req.id <> NEW.request_id THEN
            RAISE EXCEPTION 'staff_capability_grants: request/approval mismatch' USING ERRCODE = 'CG012';
        END IF;

        -- R12-a (security condition): under REPEATABLE READ, two
        -- concurrent overlapping INSERTs could each take the advisory
        -- lock in turn but neither would see the other's row (snapshot
        -- taken at transaction start), producing write skew. Refuse
        -- outright unless READ COMMITTED or SERIALIZABLE. This function
        -- stays VOLATILE (the default for this trigger; never STABLE),
        -- so it is re-evaluated per row and cannot be constant-folded.
        IF current_setting('transaction_isolation') NOT IN ('read committed', 'serializable') THEN
            RAISE EXCEPTION 'staff_capability_grants: grant insert is refused under isolation level % (must be read committed or serializable, R12-a)', current_setting('transaction_isolation') USING ERRCODE = 'CG012';
        END IF;

        -- R-12 binding control (R12-b): take the per-key advisory lock,
        -- keyed on all three parts of (tenant, grantee, capability),
        -- BEFORE the overlap SELECT below, so two concurrent approvals
        -- for the same key serialize on this lock rather than both
        -- reading "no overlap" and both committing.
        PERFORM pg_advisory_xact_lock(hashtextextended(
            'staff_capability_grant:' || v_req.tenant_id::text || ':' || v_req.grantee_staff_id::text || ':' || v_req.capability,
            0));

        IF staff_capability_grant_overlaps(v_req.tenant_id, v_req.grantee_staff_id, v_req.capability, v_req.valid_from, v_req.valid_until) THEN
            RAISE EXCEPTION 'staff_capability_grants: an unrevoked grant with an overlapping validity range already exists for this grantee/capability (R-12)' USING ERRCODE = 'CG012';
        END IF;

        -- Every grant column is copied from, and checked against, the
        -- request/approval pair - never independently supplied.
        NEW.tenant_id := v_req.tenant_id;
        NEW.grantee_staff_id := v_req.grantee_staff_id;
        NEW.grantee_scope := v_req.grantee_scope;
        NEW.capability := v_req.capability;
        NEW.granted_at := now();
        -- R-14: no backdating. valid_from is clamped forward to now() if
        -- the requested start has already passed by grant time (e.g. the
        -- request sat pending for a while); it is never clamped backward.
        -- Combined with the table's CHECK (valid_from >= granted_at),
        -- this is the binding control - the request/approval guards'
        -- 5-minute tolerance checks above are non-binding pre-checks only.
        NEW.valid_from := GREATEST(v_req.valid_from, now());
        NEW.valid_until := v_req.valid_until;
        NEW.revoked_at := NULL;
        NEW.revoked_by := NULL;
        NEW.revoked_by_scope := NULL;
        NEW.revoke_reason_code := NULL;

        -- R-13 must still hold after the R-14 clamp: a clamped G-P2 grant
        -- never ends later than the ORIGINAL requested_from + max
        -- lifetime would have allowed. Because the clamp only ever moves
        -- valid_from forward (never back), a clamped grant's window is
        -- always <= the originally requested window, so this can only
        -- ever re-confirm what the request guard already checked - but it
        -- is re-checked here, against the live setting, as the binding
        -- layer rather than trusting the non-binding request-time check.
        IF NEW.grantee_scope = 'platform' THEN
            SELECT value_interval INTO v_max_lifetime FROM financial_capability_settings WHERE key = 'acting_grant_max_lifetime';
            IF v_max_lifetime IS NULL OR NEW.valid_until IS NULL OR NEW.valid_until > NEW.valid_from + v_max_lifetime THEN
                RAISE EXCEPTION 'staff_capability_grants: clamped valid_until exceeds acting_grant_max_lifetime (R-13)' USING ERRCODE = 'CG012';
            END IF;
        END IF;

        RETURN NEW;
    END IF;

    -- UPDATE: the one-way revoke only (§8.1), whole-row equality on
    -- every other column.
    SELECT * INTO v_actor FROM financial_actor_session();
    -- R-10: an acting session never revokes a grant.
    IF v_actor.scope = 'platform_acting' THEN
        RAISE EXCEPTION 'staff_capability_grants: an acting session may not revoke a grant' USING ERRCODE = 'CG012';
    END IF;
    IF OLD.revoked_at IS NOT NULL THEN
        RAISE EXCEPTION 'staff_capability_grants: grant % is already revoked; there is no un-revoke', OLD.id USING ERRCODE = 'CG012';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.grantee_staff_id IS DISTINCT FROM OLD.grantee_staff_id
        OR NEW.grantee_scope IS DISTINCT FROM OLD.grantee_scope
        OR NEW.capability IS DISTINCT FROM OLD.capability
        OR NEW.request_id IS DISTINCT FROM OLD.request_id
        OR NEW.approval_id IS DISTINCT FROM OLD.approval_id
        OR NEW.valid_from IS DISTINCT FROM OLD.valid_from
        OR NEW.valid_until IS DISTINCT FROM OLD.valid_until
        OR NEW.granted_at IS DISTINCT FROM OLD.granted_at
    THEN
        RAISE EXCEPTION 'staff_capability_grants: only revoked_at/revoked_by/revoked_by_scope/revoke_reason_code may change, and only from NULL' USING ERRCODE = 'CG012';
    END IF;
    IF NEW.revoked_at IS NULL THEN
        RAISE EXCEPTION 'staff_capability_grants: an UPDATE must set revoked_at (the one-way revoke)' USING ERRCODE = 'CG012';
    END IF;
    -- A tenant session may revoke only within its own tenant; a platform
    -- session may revoke any tenant's grant (including G-P2 rows).
    IF v_actor.scope = 'tenant' AND v_actor.tenant <> OLD.tenant_id THEN
        RAISE EXCEPTION 'staff_capability_grants: a tenant session may only revoke grants in its own tenant' USING ERRCODE = 'CG012';
    END IF;
    IF NEW.revoke_reason_code IS NULL OR octet_length(NEW.revoke_reason_code) NOT BETWEEN 1 AND 64 THEN
        RAISE EXCEPTION 'staff_capability_grants: revoke requires a reason code' USING ERRCODE = 'CG012';
    END IF;
    NEW.revoked_at := now();
    NEW.revoked_by := v_actor.actor;
    NEW.revoked_by_scope := v_actor.scope;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER staff_capability_grants_guard
    BEFORE INSERT OR UPDATE ON staff_capability_grants
    FOR EACH ROW EXECUTE FUNCTION staff_capability_grants_guard();

CREATE TRIGGER staff_capability_grants_no_delete
    BEFORE DELETE ON staff_capability_grants
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER staff_capability_grants_no_truncate
    BEFORE TRUNCATE ON staff_capability_grants
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

ALTER TABLE staff_capability_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_capability_grants FORCE ROW LEVEL SECURITY;

-- K1-C2 (security review of 0f34d36): per-command (T: SELECT, UPDATE; P:
-- SELECT, INSERT, UPDATE - never FOR ALL, per ADR 0099 §10.5), and the
-- missing app.platform_service_id IS NULL arm added throughout (its
-- absence meant a platform_admin + platform_service mixed session could
-- read/write every row).
CREATE POLICY tenant_scope_select ON staff_capability_grants
    FOR SELECT
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    );
CREATE POLICY tenant_scope_update ON staff_capability_grants
    FOR UPDATE
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    );

CREATE POLICY platform_scope_select ON staff_capability_grants
    FOR SELECT
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    );
CREATE POLICY platform_scope_insert ON staff_capability_grants
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    );
CREATE POLICY platform_scope_update ON staff_capability_grants
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    );

-- Recursion-exempt acting policy (§6.3): GUC-only, never calls
-- financial_acting_session_valid(). SELECT is narrowed to the acting
-- tenant's rows; UPDATE USING admits the same rows (so
-- "SELECT ... FOR SHARE" can lock them at K2/K3 execution time), but
-- WITH CHECK false refuses every actual write from an acting session
-- (revoke stays a tenant/platform-only action, R-10).
CREATE POLICY acting_read ON staff_capability_grants
    FOR SELECT
    USING (
        financial_acting_gucs_exact()
        AND tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
    );

CREATE POLICY acting_lock ON staff_capability_grants
    FOR UPDATE
    USING (
        financial_acting_gucs_exact()
        AND tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
    )
    WITH CHECK (false);

-- =========================================================================
-- 6. The restrictive fence (§6.4) on the seven exposed existing tables
-- =========================================================================

-- staff_users: acting sessions may see their own row (so the validator
-- itself can resolve) or tenant X's rows (added acting-read policy
-- below); every write is refused outright. UPDATE USING mirrors SELECT
-- (needed for "FOR SHARE" locking at K2/K3 execution); WITH CHECK is
-- unconditionally refused.
CREATE POLICY acting_fence_select ON staff_users AS RESTRICTIVE FOR SELECT
    USING (
        NOT financial_acting_gucs_present()
        OR id = NULLIF(current_setting('app.acting_platform_principal_id', true), '')::uuid
        OR tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
    );
CREATE POLICY acting_fence_insert ON staff_users AS RESTRICTIVE FOR INSERT
    WITH CHECK (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_update ON staff_users AS RESTRICTIVE FOR UPDATE
    USING (
        NOT financial_acting_gucs_present()
        OR id = NULLIF(current_setting('app.acting_platform_principal_id', true), '')::uuid
        OR tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
    )
    WITH CHECK (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_delete ON staff_users AS RESTRICTIVE FOR DELETE
    USING (NOT financial_acting_gucs_present());

-- The acting-read permissive policy on staff_users (§6.5): GUC-only
-- (recursion exempt, same reasoning as staff_capability_grants above),
-- gives an acting session visibility into tenant X's own staff rows
-- (e.g. to validate a grantee/approver in K2/K3). Combined with the
-- restrictive fence above, a valid acting session sees exactly its own
-- platform row plus tenant X's staff rows - nothing else.
CREATE POLICY acting_read ON staff_users
    FOR SELECT
    USING (
        financial_acting_gucs_exact()
        AND tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
    );
CREATE POLICY acting_lock ON staff_users
    FOR UPDATE
    USING (
        financial_acting_gucs_exact()
        AND tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
    )
    WITH CHECK (false);

-- audit_log: acting sessions may never read any audit row (platform or
-- tenant), and may only insert their own tenant's audit row (the actor
-- trigger below forces every other column). UPDATE/DELETE are already
-- refused by audit_log_immutable; the restrictive policies are
-- belt-and-braces so A-18's replay finds this table properly fenced too.
CREATE POLICY acting_fence_select ON audit_log AS RESTRICTIVE FOR SELECT
    USING (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_insert ON audit_log AS RESTRICTIVE FOR INSERT
    WITH CHECK (
        NOT financial_acting_gucs_present()
        OR (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid)
    );
CREATE POLICY acting_fence_update ON audit_log AS RESTRICTIVE FOR UPDATE
    USING (NOT financial_acting_gucs_present())
    WITH CHECK (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_delete ON audit_log AS RESTRICTIVE FOR DELETE
    USING (NOT financial_acting_gucs_present());

CREATE POLICY acting_insert ON audit_log
    FOR INSERT
    WITH CHECK (
        financial_acting_gucs_exact()
        AND tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
    );

-- sessions, login_attempts, persons, player_restrictions, risk_rules:
-- fully denied to any acting session on every command (K1-1). No acting
-- code needs any of these - the beneficiary/distinct-Person checks read
-- staff_users.person_id and player_accounts.person_id, never persons
-- directly.
DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['sessions', 'login_attempts', 'persons', 'player_restrictions', 'risk_rules']
    LOOP
        EXECUTE format(
            'CREATE POLICY acting_fence_select ON %I AS RESTRICTIVE FOR SELECT USING (NOT financial_acting_gucs_present())', t);
        EXECUTE format(
            'CREATE POLICY acting_fence_insert ON %I AS RESTRICTIVE FOR INSERT WITH CHECK (NOT financial_acting_gucs_present())', t);
        EXECUTE format(
            'CREATE POLICY acting_fence_update ON %I AS RESTRICTIVE FOR UPDATE USING (NOT financial_acting_gucs_present()) WITH CHECK (NOT financial_acting_gucs_present())', t);
        EXECUTE format(
            'CREATE POLICY acting_fence_delete ON %I AS RESTRICTIVE FOR DELETE USING (NOT financial_acting_gucs_present())', t);
    END LOOP;
END $$;

-- =========================================================================
-- 7. audit_log_acting_actor (§10.6, C-99-3)
-- =========================================================================

CREATE FUNCTION audit_log_acting_actor() RETURNS TRIGGER AS $$
BEGIN
    IF financial_acting_gucs_present() THEN
        IF NOT financial_acting_session_valid() THEN
            RAISE EXCEPTION 'audit_log_acting_actor: acting session is not valid' USING ERRCODE = 'CG020';
        END IF;
        NEW.actor_type := 'staff';
        NEW.actor_id := NULLIF(current_setting('app.acting_platform_principal_id', true), '')::uuid;
        NEW.tenant_id := NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid;
        NEW.metadata := coalesce(NEW.metadata, '{}'::jsonb) || '{"actor_scope":"platform_acting"}'::jsonb;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_log_acting_actor
    BEFORE INSERT ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_acting_actor();

-- =========================================================================
-- 8. Runtime role grants (L-1, k1-security.md): mirrored here, at the
--    migration itself, the same way migrations 0102/0110/0111 narrow
--    their own tables - never relying solely on deploy/init-app-role.sql.
--    RLS (FORCE on all three new tables) is the binding control; this is
--    defence in depth, narrower than the blanket ALTER DEFAULT PRIVILEGES
--    backfill deploy/init-app-role.sql's own preceding blocks otherwise
--    leave in place. Matches deploy/init-app-role.sql's own 0112 block
--    exactly:
--      financial_capability_catalogue,
--      financial_governance_permissions,
--      financial_capability_settings   - immutable seed vocabulary, SELECT
--                                        only, no write grant at all.
--      staff_capability_grant_requests - SELECT/INSERT/UPDATE (the
--                                        pending -> cancelled/expired/
--                                        approved/rejected transition).
--                                        Never DELETE.
--      staff_capability_grant_approvals - append-only: SELECT/INSERT only.
--      staff_capability_grants          - SELECT/INSERT/UPDATE (the
--                                        one-way revoke). Never DELETE -
--                                        the guard trigger refuses it
--                                        anyway.
-- =========================================================================
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON financial_capability_catalogue FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON financial_capability_catalogue TO igaming_runtime';

        EXECUTE 'REVOKE ALL ON financial_governance_permissions FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON financial_governance_permissions TO igaming_runtime';

        EXECUTE 'REVOKE ALL ON financial_capability_settings FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON financial_capability_settings TO igaming_runtime';

        EXECUTE 'REVOKE ALL ON staff_capability_grant_requests FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE ON staff_capability_grant_requests TO igaming_runtime';

        EXECUTE 'REVOKE ALL ON staff_capability_grant_approvals FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON staff_capability_grant_approvals TO igaming_runtime';

        EXECUTE 'REVOKE ALL ON staff_capability_grants FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE ON staff_capability_grants TO igaming_runtime';
    END IF;
END $$;
