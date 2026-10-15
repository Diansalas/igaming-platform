-- A9 (observability + alert delivery). Four things, all in the durable-alerting tables of migrations
-- 0110/0117, and nothing else:
--
--   1. Channel vocabulary: alert_routes.channel_kind and alert_deliveries.channel_kind admit 'email' and
--      'webhook' (provider-neutral channels; internal/alerting EmailSink / WebhookSink). Both are
--      HUMAN-NOTIFICATION kinds: alerting_channel_kind_is_human_notification() is fail-closed (only log and
--      mock answer false) and is deliberately NOT replaced here.
--
--   2. SR-7 four-eyes for human-notification routes (the guard 0117 left as "not built"). A route can be
--      ENABLED for a human-notification kind only by INSERTing a version that names, in staged_route_id, a
--      DISABLED route that was explicitly STAGED FOR ENABLING (staged_for_enable = true, a flag valid only on
--      a disabled human-notification version and set only by the staging path; a route disabled for another
--      reason, for example a decommission, is not a staging record: SR9-A9-3) by a DIFFERENT validated
--      platform admin within the last 7 days, identical in scope, severity, escalation step, channel kind,
--      recipient_ref and escalate_after; each staged route can be consumed once (unique index). One admin
--      stages, a second admin enables: neither can route a page to a person alone. effective_from is forced
--      to now() on every INSERT (a version never starts in the past). Without a staged record, enabling
--      still fails with AR001 exactly as 0117 did (message prefix "SR-7: four-eyes approval required" is
--      unchanged). Residual (NULL-ARM-WRITE-1, accepted R1 class): this binds the application path, not an
--      arbitrary-SQL session of the runtime role, which can still create platform admins.
--
--   3. A new platform, subjectless P1 Kind reconciliation.sweep_failed (attributes phase, sqlstate_class):
--      the reconciliation scheduler could not LIST tenants (or panicked), which previously only logged
--      (R3-LISTING-FAILURE-LOG-ONLY-1). Raised by the alert_dispatcher platform-service identity (the
--      scheduler has no platform-admin principal); the dispatcher raise policies on alerts and
--      alert_occurrences are widened by exactly this Kind. Tenant-session and platform-admin raise paths for
--      it do not exist: in_tx_raisable_by_tenant = false, and (SR9-A9-4) RESTRICTIVE INSERT policies on
--      alerts and alert_occurrences admit this Kind only to the dispatcher identity, because the
--      alerts_platform_admin policy is FOR ALL and would otherwise let a platform admin raise it. Its
--      payload is validated in alerts_guard() with the alerting.raise_failed posture: exactly the keys
--      phase and sqlstate_class, string values, phase in a closed vocabulary, sqlstate_class matching
--      ^[0-9A-Z]{2}$, and the discriminator FORCED to 'phase:' || phase (no free-text channel into a P1).
--
--   4. Nothing seeds a route, recipient, secret or address. Recipients and the webhook URL are runtime
--      configuration (ALERT_EMAIL_RECIPIENTS, ALERT_WEBHOOK_URL); the webhook signing secret is a
--      secret-store reference.
--
-- Every function created or replaced here pins search_path = pg_catalog, public, pg_temp. FORCE RLS stays
-- on every table; no RLS family other than the two dispatcher raise policies is touched.

-- ======================================================================
-- 1. Channel vocabulary
-- ======================================================================
ALTER TABLE alert_routes DROP CONSTRAINT alert_routes_channel_kind_check;
ALTER TABLE alert_routes ADD CONSTRAINT alert_routes_channel_kind_check
    CHECK (channel_kind IN ('log', 'mock', 'email', 'webhook'));
ALTER TABLE alert_deliveries DROP CONSTRAINT alert_deliveries_channel_kind_check;
ALTER TABLE alert_deliveries ADD CONSTRAINT alert_deliveries_channel_kind_check
    CHECK (channel_kind IN ('log', 'mock', 'email', 'webhook'));

-- ======================================================================
-- 2. SR-7 four-eyes staging
-- ======================================================================
ALTER TABLE alert_routes ADD COLUMN staged_route_id UUID NULL REFERENCES alert_routes(id);
CREATE UNIQUE INDEX alert_routes_staged_route_once ON alert_routes (staged_route_id) WHERE staged_route_id IS NOT NULL;
ALTER TABLE alert_routes ADD COLUMN staged_for_enable BOOLEAN NOT NULL DEFAULT false;
COMMENT ON COLUMN alert_routes.staged_for_enable IS 'SR-7 staging INTENT (SR9-A9-3): true only on a DISABLED human-notification version written by the staging path. Only such a row may be named by staged_route_id; a route disabled for another reason (decommission) carries false and cannot be approved by one admin.';
COMMENT ON COLUMN alert_routes.staged_route_id IS 'SR-7 four-eyes: the DISABLED staged route (created by a different platform admin) this ENABLED human-notification route approves. Required to enable a human-notification channel kind; each staged route is consumed at most once.';

CREATE OR REPLACE FUNCTION alert_routes_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor  UUID;
    v_staged alert_routes%ROWTYPE;
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'alert_routes is append-only: TRUNCATE is not permitted';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'alert_routes is append-only: DELETE is not permitted';
    END IF;

    IF TG_OP = 'INSERT' THEN
        IF NEW.reason_code IS NULL THEN
            RAISE EXCEPTION 'alert_routes: reason_code is required on every route version' USING ERRCODE = 'AR002';
        END IF;
        -- created_by is FORCED from the validated platform-admin actor (§3.2), never application-supplied.
        v_actor := alerting_validated_platform_admin();

        IF NEW.staged_for_enable AND (NEW.enabled OR NOT alerting_channel_kind_is_human_notification(NEW.channel_kind)) THEN
            RAISE EXCEPTION 'alert_routes: staged_for_enable is only valid on a disabled human-notification version'
                USING ERRCODE = 'AR001';
        END IF;

        IF NEW.enabled AND alerting_channel_kind_is_human_notification(NEW.channel_kind) THEN
            IF NEW.staged_route_id IS NULL THEN
                RAISE EXCEPTION 'SR-7: four-eyes approval required: stage the route disabled, then have a different platform admin enable that staged version'
                    USING ERRCODE = 'AR001';
            END IF;
            SELECT * INTO v_staged FROM alert_routes WHERE id = NEW.staged_route_id;
            IF NOT FOUND
               OR v_staged.enabled
               OR v_staged.superseded_at IS NOT NULL
               OR NOT v_staged.staged_for_enable
               OR v_staged.scope IS DISTINCT FROM NEW.scope
               OR v_staged.severity IS DISTINCT FROM NEW.severity
               OR v_staged.escalation_step IS DISTINCT FROM NEW.escalation_step
               OR v_staged.channel_kind IS DISTINCT FROM NEW.channel_kind
               OR v_staged.recipient_ref IS DISTINCT FROM NEW.recipient_ref
               OR v_staged.escalate_after IS DISTINCT FROM NEW.escalate_after
            THEN
                RAISE EXCEPTION 'SR-7: four-eyes approval required: staged_route_id must name a current DISABLED route that was staged for enabling, identical in severity, step, channel, recipient and escalation'
                    USING ERRCODE = 'AR001';
            END IF;
            IF v_staged.created_by = v_actor THEN
                RAISE EXCEPTION 'SR-7: four-eyes approval required: the approver must differ from the admin who staged the route'
                    USING ERRCODE = 'AR001';
            END IF;
            IF v_staged.created_at < now() - INTERVAL '7 days' THEN
                RAISE EXCEPTION 'SR-7: four-eyes approval required: the staged route is older than 7 days; stage it again'
                    USING ERRCODE = 'AR001';
            END IF;
        ELSIF NEW.staged_route_id IS NOT NULL THEN
            -- Fail closed: a staged reference on anything but an enabling human-notification version is meaningless.
            RAISE EXCEPTION 'alert_routes: staged_route_id is only valid when enabling a human-notification route'
                USING ERRCODE = 'AR001';
        END IF;

        NEW.created_by := v_actor;
        NEW.created_at := now();
        -- A version starts now, never in the past (SR9-A9-3): effective_from decides which current route wins.
        NEW.effective_from := now();
        NEW.superseded_at := NULL;
        NEW.superseded_by := NULL;
        RETURN NEW;
    END IF;

    -- UPDATE: one-way supersession only. Every other column is immutable,
    -- and a superseded row can never be un-superseded or re-superseded.
    IF alerting_validated_platform_admin() IS NULL THEN
        RAISE EXCEPTION 'alert_routes: only a validated platform-admin session may write here';
    END IF;
    IF OLD.superseded_at IS NOT NULL THEN
        RAISE EXCEPTION 'alert_routes: a superseded route is immutable';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.scope IS DISTINCT FROM OLD.scope
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.severity IS DISTINCT FROM OLD.severity
        OR NEW.escalation_step IS DISTINCT FROM OLD.escalation_step
        OR NEW.channel_kind IS DISTINCT FROM OLD.channel_kind
        OR NEW.recipient_ref IS DISTINCT FROM OLD.recipient_ref
        OR NEW.escalate_after IS DISTINCT FROM OLD.escalate_after
        OR NEW.effective_from IS DISTINCT FROM OLD.effective_from
        OR NEW.enabled IS DISTINCT FROM OLD.enabled
        OR NEW.reason_code IS DISTINCT FROM OLD.reason_code
        OR NEW.staged_route_id IS DISTINCT FROM OLD.staged_route_id
        OR NEW.staged_for_enable IS DISTINCT FROM OLD.staged_for_enable
        OR NEW.created_by IS DISTINCT FROM OLD.created_by
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'alert_routes: only superseded_at/superseded_by may change, and only once';
    END IF;
    IF NEW.superseded_at IS NULL OR NEW.superseded_by IS NULL THEN
        RAISE EXCEPTION 'alert_routes: supersession must set both superseded_at and superseded_by together';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

-- ======================================================================
-- 3. Kind reconciliation.sweep_failed
-- ======================================================================
ALTER TABLE alert_kinds DROP CONSTRAINT alert_kinds_check1;
ALTER TABLE alert_kinds ADD CONSTRAINT alert_kinds_check1 CHECK (
    requires_subject = true
    OR kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed',
                'staff.platform_mfa_integrity', 'staff.platform_session_integrity', 'staff.platform_mfa_hard_locked',
                'staff.platform_mfa_recovery_used', 'staff.platform_mfa_failure_pattern',
                'staff.platform_break_glass_used',
                'reconciliation.sweep_failed'));
DO $$
BEGIN
    CREATE POLICY alert_kinds_seed_0156 ON alert_kinds
        FOR INSERT TO CURRENT_USER
        WITH CHECK (kind = 'reconciliation.sweep_failed');
    INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject, in_tx_raisable_by_tenant, allowed_keys, raise_mode) VALUES
        ('reconciliation.sweep_failed', 'p1', 'platform', false, false, false,
         ARRAY['phase', 'sqlstate_class'], 'detached');
    DROP POLICY alert_kinds_seed_0156 ON alert_kinds;
END
$$;

-- The dispatcher identity's meta-only INSERT policies (0110) are widened by exactly this Kind, on both tables.
DROP POLICY alerts_platform_service_dispatcher_raise ON alerts;
CREATE POLICY alerts_platform_service_dispatcher_raise ON alerts
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND tenant_id IS NULL
        AND subject_tenant_id IS NULL
        AND kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed', 'reconciliation.sweep_failed')
    );

DROP POLICY alerts_platform_service_dispatcher_raise ON alert_occurrences;
CREATE POLICY alerts_platform_service_dispatcher_raise ON alert_occurrences
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND tenant_id IS NULL
        AND subject_tenant_id IS NULL
        AND kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed', 'reconciliation.sweep_failed')
    );

-- SR9-A9-4: the alerts_platform_admin policy is FOR ALL with no Kind restriction, so without more a platform
-- admin could raise this Kind (and bypass the validation below only by being refused by it, but the header
-- promised there is no such path). RESTRICTIVE policies are ANDed with every permissive policy: the Kind is
-- insertable only under the alert_dispatcher platform-service identity. Role-independent (GUC based), like the
-- rest of the alert RLS families. Reads and acks are unaffected (INSERT only).
CREATE POLICY alerts_sweep_failed_dispatcher_only ON alerts
    AS RESTRICTIVE
    FOR INSERT
    WITH CHECK (
        kind <> 'reconciliation.sweep_failed'
        OR NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
    );
CREATE POLICY alerts_sweep_failed_dispatcher_only ON alert_occurrences
    AS RESTRICTIVE
    FOR INSERT
    WITH CHECK (
        kind <> 'reconciliation.sweep_failed'
        OR NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
    );

-- SR9-A9-4: database-level validation of the sweep_failed payload. This is alerts_guard() from 0110 (search_path
-- pin from 0117 kept) with ONE added block after the raise_failed one; everything else is unchanged.
CREATE OR REPLACE FUNCTION alerts_guard() RETURNS TRIGGER AS $$
DECLARE
    v_kind RECORD;
    v_key  TEXT;
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'alerts is append-mostly: TRUNCATE is not permitted';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'alerts: DELETE is not permitted; corrections are resolve + a new alert, never a deletion';
    END IF;

    SELECT * INTO v_kind FROM alert_kinds WHERE kind = NEW.kind;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'alerts: unknown kind %', NEW.kind;
    END IF;

    IF TG_OP = 'INSERT' THEN
        -- Forced from alert_kinds, never trusted from the caller (§3.2).
        NEW.severity := v_kind.severity;
        NEW.simulation := v_kind.simulation;

        -- Scope/subject consistency trigger (§3.2, C-102-1).
        IF v_kind.scope = 'platform' THEN
            IF NEW.tenant_id IS NOT NULL THEN
                RAISE EXCEPTION 'alerts: kind % is platform-scoped; tenant_id must be NULL', NEW.kind;
            END IF;
        ELSE
            IF NEW.tenant_id IS NULL THEN
                RAISE EXCEPTION 'alerts: kind % is tenant-scoped; tenant_id is required', NEW.kind;
            END IF;
            IF NEW.subject_tenant_id IS NOT NULL THEN
                RAISE EXCEPTION 'alerts: a tenant-scoped kind never carries a subject_tenant_id';
            END IF;
        END IF;

        IF v_kind.requires_subject AND NEW.subject_tenant_id IS NULL THEN
            RAISE EXCEPTION 'alerts: kind % requires a subject_tenant_id', NEW.kind;
        END IF;
        IF NOT v_kind.requires_subject AND NEW.subject_tenant_id IS NOT NULL THEN
            RAISE EXCEPTION 'alerts: kind % must not carry a subject_tenant_id', NEW.kind;
        END IF;

        -- Attribute allowlist (§3.2, payload tests). Every key must be in
        -- the Kind's allowlist.
        IF EXISTS (
            SELECT 1 FROM jsonb_object_keys(NEW.attributes) k
            WHERE NOT (k = ANY(v_kind.allowed_keys))
        ) THEN
            RAISE EXCEPTION 'alerts: attribute key not in the allowlist for kind %', NEW.kind;
        END IF;

        -- §6.3: the raise_failed attribute trigger, this Kind only.
        IF NEW.kind = 'alerting.raise_failed' THEN
            IF (SELECT count(*) FROM jsonb_object_keys(NEW.attributes)) <> 2
                OR NOT (NEW.attributes ? 'kind')
                OR NOT (NEW.attributes ? 'sqlstate_class')
            THEN
                RAISE EXCEPTION 'alerts: alerting.raise_failed requires attributes exactly {kind, sqlstate_class}';
            END IF;
            IF NOT EXISTS (SELECT 1 FROM alert_kinds ak WHERE ak.kind = (NEW.attributes->>'kind')) THEN
                RAISE EXCEPTION 'alerts: alerting.raise_failed attributes.kind must name an existing alert_kinds.kind';
            END IF;
            IF (NEW.attributes->>'sqlstate_class') <> 'go_validation'
                AND (NEW.attributes->>'sqlstate_class') !~ '^[0-9A-Z]{2}$'
            THEN
                RAISE EXCEPTION 'alerts: alerting.raise_failed attributes.sqlstate_class must be go_validation or match ^[0-9A-Z]{2}$';
            END IF;
            -- The discriminator is FORCED, never honoured from the
            -- caller (security Part 1): no free-text channel into a P1.
            NEW.discriminator := 'kind:' || (NEW.attributes->>'kind');
        END IF;

        -- SR9-A9-4: the reconciliation.sweep_failed attribute trigger, this Kind only (raise_failed posture).
        IF NEW.kind = 'reconciliation.sweep_failed' THEN
            IF (SELECT count(*) FROM jsonb_object_keys(NEW.attributes)) <> 2
                OR NOT (NEW.attributes ? 'phase')
                OR NOT (NEW.attributes ? 'sqlstate_class')
                OR jsonb_typeof(NEW.attributes->'phase') IS DISTINCT FROM 'string'
                OR jsonb_typeof(NEW.attributes->'sqlstate_class') IS DISTINCT FROM 'string'
            THEN
                RAISE EXCEPTION 'alerts: reconciliation.sweep_failed requires attributes exactly {phase, sqlstate_class}, both strings';
            END IF;
            IF (NEW.attributes->>'phase') NOT IN ('list_active', 'list_non_active', 'panic', 'unknown') THEN
                RAISE EXCEPTION 'alerts: reconciliation.sweep_failed attributes.phase must be one of list_active, list_non_active, panic, unknown';
            END IF;
            IF (NEW.attributes->>'sqlstate_class') !~ '^[0-9A-Z]{2}$' THEN
                RAISE EXCEPTION 'alerts: reconciliation.sweep_failed attributes.sqlstate_class must match ^[0-9A-Z]{2}$';
            END IF;
            -- FORCED, never honoured from the caller: no free-text channel into a P1.
            NEW.discriminator := 'phase:' || (NEW.attributes->>'phase');
        END IF;

        NEW.state := 'open';
        NEW.acked_by := NULL;
        NEW.acked_at := NULL;
        NEW.resolved_by := NULL;
        NEW.resolved_at := NULL;
        NEW.resolve_reason_code := NULL;
        NEW.first_seen_at := now();
        NEW.created_at := now();
        RETURN NEW;
    END IF;

    -- UPDATE from here on: the ack/resolve state guard (§4.2, C-102-7).
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.subject_tenant_id IS DISTINCT FROM OLD.subject_tenant_id
        OR NEW.kind IS DISTINCT FROM OLD.kind
        OR NEW.severity IS DISTINCT FROM OLD.severity
        OR NEW.simulation IS DISTINCT FROM OLD.simulation
        OR NEW.discriminator IS DISTINCT FROM OLD.discriminator
        OR NEW.attributes IS DISTINCT FROM OLD.attributes
        OR NEW.first_seen_at IS DISTINCT FROM OLD.first_seen_at
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'alerts: identity, payload and dedup columns are immutable';
    END IF;

    IF OLD.state = 'resolved' THEN
        RAISE EXCEPTION 'alerts: a resolved alert is terminal and immutable';
    END IF;
    IF NOT (
        (OLD.state = 'open' AND NEW.state IN ('acked', 'resolved'))
        OR (OLD.state = 'acked' AND NEW.state = 'resolved')
    ) THEN
        RAISE EXCEPTION 'alerts: invalid state transition % -> %', OLD.state, NEW.state;
    END IF;

    -- The actor must resolve to a validated platform admin (C-102-7).
    -- There is no tenant ack endpoint in PRH-2.
    DECLARE
        v_actor UUID := alerting_validated_platform_admin();
    BEGIN
        IF NEW.state = 'acked' THEN
            NEW.acked_by := v_actor;
            NEW.acked_at := now();
        ELSIF NEW.state = 'resolved' THEN
            IF NEW.resolve_reason_code IS NULL THEN
                RAISE EXCEPTION 'alerts: resolve requires a resolve_reason_code';
            END IF;
            IF OLD.state = 'open' THEN
                NEW.acked_by := v_actor;
                NEW.acked_at := now();
            ELSE
                NEW.acked_by := OLD.acked_by;
                NEW.acked_at := OLD.acked_at;
            END IF;
            NEW.resolved_by := v_actor;
            NEW.resolved_at := now();
        END IF;
    END;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;
