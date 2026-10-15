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
--      DISABLED route that a DIFFERENT validated platform admin created within the last 7 days and that is
--      identical in scope, severity, escalation step, channel kind, recipient_ref and escalate_after, and
--      each staged route can be consumed once (unique index). One admin stages, a second admin enables:
--      neither can route a page to a person alone. Without that, enabling still fails with AR001 exactly as
--      0117 did (message prefix "SR-7: four-eyes approval required" is unchanged).
--
--   3. A new platform, subjectless P1 Kind reconciliation.sweep_failed (attributes phase, sqlstate_class):
--      the reconciliation scheduler could not LIST tenants (or panicked), which previously only logged
--      (R3-LISTING-FAILURE-LOG-ONLY-1). Raised by the alert_dispatcher platform-service identity (the
--      scheduler has no platform-admin principal); the dispatcher raise policies on alerts and
--      alert_occurrences are widened by exactly this Kind. Tenant-session and platform-admin raise paths for
--      it do not exist (in_tx_raisable_by_tenant = false; the alerts_platform_admin policy is unchanged).
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

        IF NEW.enabled AND alerting_channel_kind_is_human_notification(NEW.channel_kind) THEN
            IF NEW.staged_route_id IS NULL THEN
                RAISE EXCEPTION 'SR-7: four-eyes approval required: stage the route disabled, then have a different platform admin enable that staged version'
                    USING ERRCODE = 'AR001';
            END IF;
            SELECT * INTO v_staged FROM alert_routes WHERE id = NEW.staged_route_id;
            IF NOT FOUND
               OR v_staged.enabled
               OR v_staged.superseded_at IS NOT NULL
               OR v_staged.scope IS DISTINCT FROM NEW.scope
               OR v_staged.severity IS DISTINCT FROM NEW.severity
               OR v_staged.escalation_step IS DISTINCT FROM NEW.escalation_step
               OR v_staged.channel_kind IS DISTINCT FROM NEW.channel_kind
               OR v_staged.recipient_ref IS DISTINCT FROM NEW.recipient_ref
               OR v_staged.escalate_after IS DISTINCT FROM NEW.escalate_after
            THEN
                RAISE EXCEPTION 'SR-7: four-eyes approval required: staged_route_id must name a current DISABLED route identical in severity, step, channel, recipient and escalation'
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
