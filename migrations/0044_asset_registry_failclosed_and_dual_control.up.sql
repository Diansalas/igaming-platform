-- Stage 4H-B0-R6, Workstream A (architect). Implements ADR 0037 Part C's
-- Asset Authorization boundary for LAYERS 1-3 (existence, activation,
-- platform authorization) and closes three named Stage 4H-B0-R5 security
-- P1s against the live schema. Layers 4-7 are migration 0045.
--
-- Findings closed here, each named:
--
--  * S-4 (assets.active defaults true, contradicting ADR 0037 §C.5.5's
--    "creation forces active = false"). The default is flipped to false
--    and platform_authorized is added NOT NULL DEFAULT false, so an
--    insert path that omits either column now fails CLOSED. The seven
--    rows migration 0003 seeded are handled EXPLICITLY below, never left
--    to an implicit default.
--
--  * S-3 (layers 1-3 have no database backstop - `assets` carries no
--    tenant_id and had no RLS, so the two-tier split was an application
--    permission check only). Closed with row-level security on `assets`
--    itself, keyed on a platform-admin session GUC, plus a dual-control
--    trigger chain whose requesting AND approving principals must both
--    resolve to PLATFORM-SCOPED staff_users rows. Design choice and its
--    limits are justified in the "S-3: which backstop, and why" block.
--
--  * S-5a (four-eyes asserted in ADR 0037 §C.5.3 with no enforcement
--    mechanism). Closed with asset_change_requests/asset_change_approvals,
--    mirroring migration 0026's withdrawal_requests/withdrawal_approvals
--    precedent exactly: a distinct-approver UNIQUE constraint, approval
--    immutability, and a self-approval guard that resolves approver ->
--    person the way migration 0029's
--    withdrawal_approvals_deny_self_approval does.
--
-- Also enforced here: ADR 0037 §C.5.4's immutable identity fields
-- (`code`, `decimal_exponent`, `asset_type`, `network`) at the DATABASE,
-- not merely by the absence of an "update asset identity" API operation.
--
-- ----------------------------------------------------------------------
-- S-3: which backstop, and why (the ADR asked for a real one, not
-- application-code discipline)
-- ----------------------------------------------------------------------
-- The dispatch named two candidate designs:
--   (a) a dedicated Postgres role holding the only INSERT/UPDATE grant on
--       `assets`, with the ordinary application role holding none;
--   (b) a database guard keyed on a platform-admin session-level GUC,
--       mirroring how app.tenant_id already binds tenant context.
--
-- (a) is NOT IMPLEMENTABLE in this platform as it stands, and this is a
-- verified fact, not a preference: migrations run as the application role
-- (`igaming`), which is also the OWNER of every table. A table owner can
-- always re-grant itself any privilege it revoked, so a grant-based split
-- inside one role is decorative; and creating a second role requires
-- CREATEROLE, which the application role does not hold (`\du` shows no
-- attributes on `igaming`). Doing (a) properly needs a second database
-- role AND a second connection pool with separate credentials - a
-- deployment/configuration change outside this workstream's scope.
-- Recorded as a follow-up in ADR 0037 §C.6, not silently dropped.
--
-- (b) is implemented, in the strongest available form and doubled up:
--
--   1. `assets` gets ENABLE + FORCE ROW LEVEL SECURITY. FORCE is what
--      makes this real rather than nominal: without it the table owner
--      (the application role) bypasses every policy. Write policies
--      require app.platform_admin_principal_id to be set AND both
--      app.tenant_id and app.player_account_id to be UNSET - so a
--      tenant-scoped connection (db.WithTenant, which always sets
--      app.tenant_id for the whole transaction) is structurally unable to
--      satisfy them even if it also forged the platform GUC. That is the
--      "tenant-scoped role cannot reach platform-scoped data" claim ADR
--      0037 §C.1 made, now actually mechanically true instead of asserted.
--      SELECT stays open (USING (true)): the registry is read-only
--      reference data that every money-handling path reads for
--      decimal_exponent, with no tenant context available at that point,
--      and PostgreSQL bypasses RLS for foreign-key checks regardless.
--
--   2. Independently of the GUC, no row can be created, activated, or
--      platform-authorized without an approved asset_change_requests row
--      whose requester and approver BOTH resolve to a platform-scoped
--      (tenant_id IS NULL) staff_users row. A tenant-scoped staff
--      principal therefore cannot appear on either side of a layer-1-3
--      mutation, enforced by trigger.
--
-- Honest limitation, stated rather than hidden: like app.tenant_id itself
-- (i.e. like the platform's ENTIRE isolation model), this binds a code
-- path, not an operating-system boundary - code that can execute
-- arbitrary SQL on the application connection can call set_config. The
-- residual gap is exactly the one (a) would close, and it is recorded as
-- a follow-up. What (b) does close, completely, is the finding as
-- written: there is now a database-level guard on layers 1-3 where before
-- there was none, and no application code path can reach a layer-1-3
-- write without going through db.Pool.WithPlatformAdmin.

-- ----------------------------------------------------------------------
-- 1. Fail-closed defaults (S-4)
-- ----------------------------------------------------------------------

ALTER TABLE assets ALTER COLUMN active SET DEFAULT false;

ALTER TABLE assets ADD COLUMN platform_authorized BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN assets.active IS 'Layer 2 (ADR 0037 §A.2): the platform-admin on/off switch. Defaults FALSE - asset creation never implies activation (§C.5.5). Flipping it to true requires dual control; flipping it to false is deliberately single-actor (an incident kill-switch, §C.5.3).';
COMMENT ON COLUMN assets.platform_authorized IS 'Layer 3 (ADR 0037 §A.2/§A.4): the slower-moving compliance/business gate for "has this asset cleared platform review to be offered to any tenant at all". Distinct from active on purpose - collapsing an incident toggle and a clearance toggle into one bit is the layer-collapse ADR 0037 exists to prevent. Defaults FALSE; granting requires dual control, revoking does not.';

ALTER TABLE assets ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- The seven rows migration 0003 seeded (EUR, USD, GBP, BRL, MXN, BTC,
-- USDT) are set EXPLICITLY here, per the dispatch's requirement that
-- pre-existing rows never inherit an implicit default. The decision is
-- deliberately SPLIT across the two layers, and the reasoning for each
-- half is recorded because ADR 0037 §C.5.5 forbids asset creation from
-- auto-authorizing financial operations:
--
--   * active = true  -- GRANDFATHERED, explicitly. These seven rows
--     predate the fail-closed rule, and unlike a newly created asset they
--     are already referenced by live schema and fixtures: `wallets`,
--     `ledger_accounts`, `withdrawal_policies`, `risk_rules` and
--     `provider_capability_amount_limits` all carry FKs to assets(code).
--     Flipping them inactive would be a functional change to already-
--     working wallet/ledger behaviour, which is neither this migration's
--     job nor this stage's authorization. Setting the value explicitly
--     (rather than relying on the old DEFAULT true) is what makes this a
--     recorded decision instead of an accident of column order.
--
--   * platform_authorized = false -- NOT grandfathered, deliberately.
--     This is the layer ADR 0037 §C.5.5 is really about, and nothing in
--     the codebase reads it yet, so fail-closed costs nothing operational
--     here. Every one of the seven assets therefore requires an explicit,
--     dual-controlled "configure platform authorization" act (ADR 0037
--     §C.5.1 op 5) before AssetAuthorization.CheckEligibility will pass
--     layer 3 for it. Until then CheckEligibility denies with
--     asset_not_platform_authorized - which is the correct, intended
--     behaviour of a fail-closed registry on the day it is switched on,
--     not a regression.
UPDATE assets
   SET active = true,
       platform_authorized = false
 WHERE code IN ('EUR', 'USD', 'GBP', 'BRL', 'MXN', 'BTC', 'USDT');

-- ----------------------------------------------------------------------
-- 2. Four-eyes / dual control (S-5a) - modelled on migration 0026
-- ----------------------------------------------------------------------
-- Scope, from ADR 0037 §C.5.3: dual control is required for exactly the
-- three operations that bring a new identity fact into existence or flip
-- a platform-wide gate from off to on (create, activate, grant platform
-- authorization). It is deliberately NOT required for the reverse
-- direction (suspend, revoke) - turning something off is the fail-closed
-- direction and an emergency kill-switch must not need a second approver.
-- That asymmetry is intentional and is why no request/approval row exists
-- for a deactivation path.
--
-- No tenant_id column: these rows govern platform-scoped operations on a
-- platform-scoped table. RLS below binds them to platform-admin scope
-- instead, which is the analogous constraint.

CREATE TABLE asset_change_requests (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Only the three dual-controlled operations are representable. A
    -- suspend/revoke can never be filed here, so it can never be made to
    -- look like it needed (or received) an approval it didn't.
    operation                 TEXT NOT NULL CHECK (operation IN ('create', 'activate', 'platform_authorize')),
    -- For 'create' this is the code the asset WILL have (no assets row
    -- exists yet, so no FK is possible - the FK-less column is deliberate,
    -- matching how withdrawal_requests carries provider_reference before
    -- the provider fact exists). For the other two it names an existing
    -- row; the assets trigger below resolves it at apply time.
    asset_code                TEXT NOT NULL,
    -- Creation payload for operation='create': asset_type,
    -- decimal_exponent, network, display_name. Present so the approver
    -- approves the EXACT identity facts that will be inserted, not just
    -- "some asset called BTC" - ADR 0037 §C.5.4 makes three of those four
    -- fields permanently immutable, so approving them sight-unseen would
    -- make dual control theatre. Verified against the assets row at apply
    -- time by assets_enforce_dual_control().
    payload                   JSONB NOT NULL DEFAULT '{}'::jsonb,
    reason_code               TEXT NOT NULL,
    requested_by_principal_id UUID NOT NULL,
    requested_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    state                     TEXT NOT NULL DEFAULT 'pending'
                                  CHECK (state IN ('pending', 'applied', 'rejected', 'cancelled')),
    applied_by_principal_id   UUID,
    applied_at                TIMESTAMPTZ,
    CHECK (operation <> 'create' OR (payload ? 'asset_type' AND payload ? 'decimal_exponent')),
    CHECK ((state = 'applied') = (applied_at IS NOT NULL))
);

CREATE INDEX idx_asset_change_requests_pending
    ON asset_change_requests (asset_code, operation)
    WHERE state = 'pending';

CREATE TABLE asset_change_approvals (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id            UUID NOT NULL REFERENCES asset_change_requests (id),
    approver_principal_id UUID NOT NULL,
    decision              TEXT NOT NULL CHECK (decision IN ('approve', 'reject')),
    reason_code           TEXT,
    decided_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- reason_code required on 'reject', optional on 'approve' - migration
    -- 0026's withdrawal_approvals rule, unchanged.
    CHECK (decision = 'approve' OR reason_code IS NOT NULL),
    -- THE distinct-approver constraint: migration 0026's
    -- UNIQUE (withdrawal_request_id, approver_principal_id), same shape,
    -- same purpose - one principal can never count as two approvals.
    UNIQUE (request_id, approver_principal_id)
);

CREATE INDEX idx_asset_change_approvals_request ON asset_change_approvals (request_id);

-- Both tables are platform-admin-scoped: a tenant-scoped or player-scoped
-- connection can neither read nor write them. Combined with the
-- requester/approver platform-scope checks below, this is the second,
-- independent half of the S-3 backstop.
ALTER TABLE asset_change_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE asset_change_requests FORCE ROW LEVEL SECURITY;
ALTER TABLE asset_change_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE asset_change_approvals FORCE ROW LEVEL SECURITY;

CREATE POLICY platform_admin_scope ON asset_change_requests
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

CREATE POLICY platform_admin_scope ON asset_change_approvals
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

-- A request is append-only except for its own terminal transition, and
-- that transition is applied BY the assets trigger below (for 'applied')
-- or by an explicit reject/cancel. Without this, the four-eyes control is
-- bypassable exactly the way migration 0026's own immutability trigger
-- comment describes: file a request for a harmless asset, collect the
-- approval, then rewrite the payload/operation before applying it.
CREATE FUNCTION asset_change_requests_enforce_immutability() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION 'asset_change_requests is append-only: % is not permitted', TG_OP;
    END IF;
    IF NEW.id <> OLD.id
        OR NEW.operation <> OLD.operation
        OR NEW.asset_code <> OLD.asset_code
        OR NEW.payload <> OLD.payload
        OR NEW.reason_code <> OLD.reason_code
        OR NEW.requested_by_principal_id <> OLD.requested_by_principal_id
        OR NEW.requested_at <> OLD.requested_at
    THEN
        RAISE EXCEPTION 'asset_change_requests: operation/asset_code/payload/reason_code/requester/requested_at are immutable after insert';
    END IF;
    IF OLD.state <> 'pending' THEN
        RAISE EXCEPTION 'asset_change_requests: request % is already %, it cannot be re-decided or re-applied', OLD.id, OLD.state;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER asset_change_requests_immutable
    BEFORE UPDATE ON asset_change_requests
    FOR EACH ROW EXECUTE FUNCTION asset_change_requests_enforce_immutability();

CREATE TRIGGER asset_change_requests_deny_delete
    BEFORE DELETE ON asset_change_requests
    FOR EACH ROW EXECUTE FUNCTION asset_change_requests_enforce_immutability();

CREATE TRIGGER asset_change_requests_no_truncate
    BEFORE TRUNCATE ON asset_change_requests
    FOR EACH STATEMENT EXECUTE FUNCTION asset_change_requests_enforce_immutability();

-- The requester must itself be a platform-scoped staff principal. Under
-- staff_users' own dual_scope_isolation policy (migration 0011), a
-- platform-scoped transaction sees exactly the tenant_id IS NULL rows, so
-- a tenant-scoped staff id is simply not resolvable here and the request
-- is refused. This is the database half of ADR 0037 §C.1's "never a
-- tenant-scoped role, full stop".
CREATE FUNCTION asset_change_requests_require_platform_principal() RETURNS TRIGGER AS $$
DECLARE
    v_is_platform_scoped BOOLEAN;
BEGIN
    SELECT su.tenant_id IS NULL INTO v_is_platform_scoped
      FROM staff_users su
     WHERE su.id = NEW.requested_by_principal_id;
    IF NOT FOUND OR NOT v_is_platform_scoped THEN
        RAISE EXCEPTION 'asset_change_requests: requesting principal % is not a platform-scoped staff principal (ADR 0037 C.1: layers 1-3 are never reachable by a tenant-scoped role)', NEW.requested_by_principal_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER asset_change_requests_platform_principal
    BEFORE INSERT ON asset_change_requests
    FOR EACH ROW EXECUTE FUNCTION asset_change_requests_require_platform_principal();

-- Approvals are fully immutable (a reconsideration is a new request, never
-- an edited decision) - migration 0026's withdrawal_approvals treatment,
-- reusing the same ledger_deny_mutation() function that table uses.
CREATE TRIGGER asset_change_approvals_immutable
    BEFORE UPDATE OR DELETE ON asset_change_approvals
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER asset_change_approvals_no_truncate
    BEFORE TRUNCATE ON asset_change_approvals
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- The self-approval guard, mirroring migration 0029's
-- withdrawal_approvals_deny_self_approval: block the same PRINCIPAL, and
-- also the same PERSON reached through staff_users.person_id (two staff
-- accounts held by one human are one human). Deliberately NOT "check in
-- Go and hope every future call site remembers" - 0029's own comment
-- explains why the trigger is the authoritative layer.
CREATE FUNCTION asset_change_approvals_deny_self_approval() RETURNS TRIGGER AS $$
DECLARE
    v_requester_principal_id UUID;
    v_requester_person_id    UUID;
    v_approver_person_id     UUID;
    v_approver_is_platform   BOOLEAN;
BEGIN
    SELECT r.requested_by_principal_id INTO v_requester_principal_id
      FROM asset_change_requests r
     WHERE r.id = NEW.request_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'asset_change_approvals: request % is not visible in this scope', NEW.request_id;
    END IF;

    IF NEW.approver_principal_id = v_requester_principal_id THEN
        RAISE EXCEPTION 'asset_change_approvals: the requesting principal may not approve or reject its own asset change (self-approval)';
    END IF;

    SELECT su.tenant_id IS NULL, su.person_id INTO v_approver_is_platform, v_approver_person_id
      FROM staff_users su
     WHERE su.id = NEW.approver_principal_id;
    IF NOT FOUND OR NOT v_approver_is_platform THEN
        RAISE EXCEPTION 'asset_change_approvals: approving principal % is not a platform-scoped staff principal', NEW.approver_principal_id;
    END IF;

    SELECT su.person_id INTO v_requester_person_id
      FROM staff_users su
     WHERE su.id = v_requester_principal_id;

    IF v_requester_person_id IS NOT NULL
        AND v_approver_person_id IS NOT NULL
        AND v_requester_person_id = v_approver_person_id
    THEN
        RAISE EXCEPTION 'asset_change_approvals: approver resolves to the same person as the requester (self-approval through a second staff account)';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER asset_change_approvals_deny_self_approval
    BEFORE INSERT ON asset_change_approvals
    FOR EACH ROW EXECUTE FUNCTION asset_change_approvals_deny_self_approval();

-- ----------------------------------------------------------------------
-- 3. `assets` row-level security (S-3)
-- ----------------------------------------------------------------------

ALTER TABLE assets ENABLE ROW LEVEL SECURITY;
ALTER TABLE assets FORCE ROW LEVEL SECURITY;

-- Read is open by design: `assets` is platform-wide read-only reference
-- data (migration 0003's own table comment), every money-handling path
-- looks up decimal_exponent from it, and most of those paths legitimately
-- run with no tenant context at all. Restricting SELECT would break
-- wallet/ledger reads without adding protection - the finding S-3 raises
-- is about WRITES reaching a platform-wide table from a tenant-scoped
-- role, not about reads.
CREATE POLICY assets_read ON assets
    FOR SELECT
    USING (true);

CREATE POLICY assets_platform_admin_insert ON assets
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY assets_platform_admin_update ON assets
    FOR UPDATE
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

-- DELETE gets scope VISIBILITY only, never a grant of mutation: the
-- assets_deny_delete trigger below is what actually refuses. This mirrors
-- migration 0041's tenant_and_platform_delete_visibility and exists for
-- the identical reason its own comment gives - with no DELETE policy at
-- all, RLS makes every row invisible to DELETE, so an attempted delete
-- silently affects 0 rows instead of failing loudly with the trigger's
-- exception. An asset row is referenced by wallets/ledger_accounts/
-- ledger_entries by code, so a silent no-op is a worse answer than a
-- loud refusal. Deletion is never a registry operation: ADR 0037 §C.5.1
-- has no delete op, and suspension (active=false) is the whole mechanism.
CREATE POLICY assets_platform_admin_delete_visibility ON assets
    FOR DELETE
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- ----------------------------------------------------------------------
-- 4. Immutable identity fields + activation-bypass prevention
-- ----------------------------------------------------------------------

-- ADR 0037 §C.5.4 established that no "update asset identity" operation
-- exists in the API catalogue. That is necessary but not sufficient: the
-- dispatch requires the same rule at the database, so a future handler,
-- migration, or manual UPDATE cannot reinterpret every historical
-- balance's minor-unit meaning by changing decimal_exponent.
CREATE FUNCTION assets_enforce_immutable_identity() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION 'assets: % is not permitted - an asset row is referenced by wallets/ledger history; use active=false to suspend', TG_OP;
    END IF;
    IF NEW.code IS DISTINCT FROM OLD.code THEN
        RAISE EXCEPTION 'assets: code is immutable after creation (ADR 0037 C.5.4)';
    END IF;
    IF NEW.decimal_exponent IS DISTINCT FROM OLD.decimal_exponent THEN
        RAISE EXCEPTION 'assets: decimal_exponent is immutable after creation (ADR 0037 C.5.4) - changing it would silently reinterpret every existing balance';
    END IF;
    IF NEW.asset_type IS DISTINCT FROM OLD.asset_type THEN
        RAISE EXCEPTION 'assets: asset_type is immutable after creation (ADR 0037 C.5.4)';
    END IF;
    IF NEW.network IS DISTINCT FROM OLD.network THEN
        RAISE EXCEPTION 'assets: network is immutable after creation (ADR 0037 C.5.4) - changing it would misattribute funds to the wrong chain';
    END IF;
    IF NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'assets: created_at is immutable after creation';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER assets_immutable_identity
    BEFORE UPDATE ON assets
    FOR EACH ROW EXECUTE FUNCTION assets_enforce_immutable_identity();

CREATE TRIGGER assets_deny_delete
    BEFORE DELETE ON assets
    FOR EACH ROW EXECUTE FUNCTION assets_enforce_immutable_identity();

CREATE TRIGGER assets_no_truncate
    BEFORE TRUNCATE ON assets
    FOR EACH STATEMENT EXECUTE FUNCTION assets_enforce_immutable_identity();

-- Consumes exactly one pending, independently-approved request for
-- (asset_code, operation) and marks it applied in the SAME statement that
-- performs the mutation. Consuming inside the trigger - rather than
-- leaving the Go service to mark it applied afterwards - is what makes the
-- control non-forgettable and non-replayable: a single approval can
-- authorize a single mutation, once.
CREATE FUNCTION asset_change_consume_approved_request(p_asset_code TEXT, p_operation TEXT) RETURNS UUID AS $$
DECLARE
    v_request_id UUID;
BEGIN
    SELECT r.id INTO v_request_id
      FROM asset_change_requests r
     WHERE r.asset_code = p_asset_code
       AND r.operation = p_operation
       AND r.state = 'pending'
       AND EXISTS (
           SELECT 1
             FROM asset_change_approvals a
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
           SELECT 1 FROM asset_change_approvals a
            WHERE a.request_id = r.id AND a.decision = 'reject'
       )
     ORDER BY r.requested_at
     FOR UPDATE
     LIMIT 1;

    IF v_request_id IS NULL THEN
        RAISE EXCEPTION 'assets: % of asset % requires a pending asset_change_requests row approved by a DIFFERENT platform principal (four-eyes, ADR 0037 C.5.3)', p_operation, p_asset_code;
    END IF;

    UPDATE asset_change_requests
       SET state = 'applied',
           applied_at = now(),
           applied_by_principal_id = NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid
     WHERE id = v_request_id;

    RETURN v_request_id;
END;
$$ LANGUAGE plpgsql;

-- The activation-bypass guard. Three separate things are enforced:
--   * INSERT can never carry active/platform_authorized = true. ADR 0037
--     §C.5.5: "asset creation must never automatically authorize any
--     financial operation" - so create -> self-authorize -> activate is
--     not merely undocumented, it is rejected.
--   * INSERT requires an approved 'create' request whose payload matches
--     the identity fields actually being inserted (approving "BTC,
--     exponent 8" must not let "BTC, exponent 2" through).
--   * Each off->on transition of active / platform_authorized requires its
--     own approved request. off transitions require none (§C.5.3's
--     deliberate asymmetry).
CREATE FUNCTION assets_enforce_dual_control() RETURNS TRIGGER AS $$
DECLARE
    v_request_id UUID;
    v_payload    JSONB;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.active THEN
            RAISE EXCEPTION 'assets: a new asset can never be created active (ADR 0037 C.5.5) - activate it with its own dual-controlled operation';
        END IF;
        IF NEW.platform_authorized THEN
            RAISE EXCEPTION 'assets: a new asset can never be created platform-authorized (ADR 0037 C.5.5) - grant it with its own dual-controlled operation';
        END IF;

        v_request_id := asset_change_consume_approved_request(NEW.code, 'create');

        SELECT payload INTO v_payload FROM asset_change_requests WHERE id = v_request_id;
        IF v_payload ->> 'asset_type' IS DISTINCT FROM NEW.asset_type
            OR (v_payload ->> 'decimal_exponent')::smallint IS DISTINCT FROM NEW.decimal_exponent
            OR NULLIF(v_payload ->> 'network', '') IS DISTINCT FROM NEW.network
        THEN
            RAISE EXCEPTION 'assets: insert does not match the approved request payload for % (approved identity facts are asset_type/decimal_exponent/network and are immutable afterwards)', NEW.code;
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.active AND NOT OLD.active THEN
        PERFORM asset_change_consume_approved_request(NEW.code, 'activate');
    END IF;
    IF NEW.platform_authorized AND NOT OLD.platform_authorized THEN
        PERFORM asset_change_consume_approved_request(NEW.code, 'platform_authorize');
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER assets_dual_control
    BEFORE INSERT OR UPDATE ON assets
    FOR EACH ROW EXECUTE FUNCTION assets_enforce_dual_control();
