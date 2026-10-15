-- MUST RUN INSIDE A SINGLE TRANSACTION (db.MigrateDown does this). Run by hand, use
-- `psql --single-transaction -v ON_ERROR_STOP=1 -f <this file>`.
--
-- Reverses 0156. REFUSES (AR099) while any route or delivery row uses the email or webhook channel, any route
-- carries a staged_route_id (the four-eyes record would be lost), or any alert / occurrence of the Kind
-- reconciliation.sweep_failed exists (append-only history is never deleted by a down). Otherwise restores the
-- 0117 shapes. The search_path pin on alert_routes_guard is kept, as 0117's own down keeps it.
--
-- FORCE RLS binds the owner, so the refusal checks would otherwise see zero rows (the 0110/0117 downs'
-- identical reasoning). NO FORCE is lifted only inside this transactional script: a refusal rolls it back,
-- success restores it.
ALTER TABLE alert_routes NO FORCE ROW LEVEL SECURITY;
ALTER TABLE alert_deliveries NO FORCE ROW LEVEL SECURITY;
ALTER TABLE alerts NO FORCE ROW LEVEL SECURITY;
ALTER TABLE alert_occurrences NO FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM alert_routes WHERE channel_kind IN ('email', 'webhook') OR staged_route_id IS NOT NULL) THEN
        RAISE EXCEPTION '0156 down refused: alert_routes has email/webhook routes or four-eyes staging records' USING ERRCODE = 'AR099';
    END IF;
    IF EXISTS (SELECT 1 FROM alert_deliveries WHERE channel_kind IN ('email', 'webhook')) THEN
        RAISE EXCEPTION '0156 down refused: alert_deliveries has email/webhook delivery rows' USING ERRCODE = 'AR099';
    END IF;
    IF EXISTS (SELECT 1 FROM alerts WHERE kind = 'reconciliation.sweep_failed')
       OR EXISTS (SELECT 1 FROM alert_occurrences WHERE kind = 'reconciliation.sweep_failed') THEN
        RAISE EXCEPTION '0156 down refused: alerts of kind reconciliation.sweep_failed exist' USING ERRCODE = 'AR099';
    END IF;
END $$;

ALTER TABLE alert_routes FORCE ROW LEVEL SECURITY;
ALTER TABLE alert_deliveries FORCE ROW LEVEL SECURITY;
ALTER TABLE alerts FORCE ROW LEVEL SECURITY;
ALTER TABLE alert_occurrences FORCE ROW LEVEL SECURITY;

-- 3. Kind and dispatcher policies.
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
        AND kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed')
    );
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
        AND kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed')
    );

DO $$
BEGIN
    ALTER TABLE alert_kinds DISABLE TRIGGER alert_kinds_deny_update_delete;
    CREATE POLICY alert_kinds_unseed_0156 ON alert_kinds
        FOR DELETE TO CURRENT_USER USING (kind = 'reconciliation.sweep_failed');
    DELETE FROM alert_kinds WHERE kind = 'reconciliation.sweep_failed';
    DROP POLICY alert_kinds_unseed_0156 ON alert_kinds;
    ALTER TABLE alert_kinds ENABLE TRIGGER alert_kinds_deny_update_delete;
END
$$;
ALTER TABLE alert_kinds DROP CONSTRAINT alert_kinds_check1;
ALTER TABLE alert_kinds ADD CONSTRAINT alert_kinds_check1 CHECK (
    requires_subject = true
    OR kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed',
                'staff.platform_mfa_integrity', 'staff.platform_session_integrity', 'staff.platform_mfa_hard_locked',
                'staff.platform_mfa_recovery_used', 'staff.platform_mfa_failure_pattern',
                'staff.platform_break_glass_used'));

-- 2. Four-eyes staging: restore the 0117 guard body (pin kept).
CREATE OR REPLACE FUNCTION alert_routes_guard() RETURNS TRIGGER AS $$
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
        IF NEW.enabled AND alerting_channel_kind_is_human_notification(NEW.channel_kind) THEN
            RAISE EXCEPTION 'SR-7: four-eyes approval required; not built' USING ERRCODE = 'AR001';
        END IF;
        NEW.created_by := alerting_validated_platform_admin();
        NEW.created_at := now();
        NEW.superseded_at := NULL;
        NEW.superseded_by := NULL;
        RETURN NEW;
    END IF;

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

DROP INDEX alert_routes_staged_route_once;
ALTER TABLE alert_routes DROP COLUMN staged_route_id;

-- 1. Channel vocabulary.
ALTER TABLE alert_deliveries DROP CONSTRAINT alert_deliveries_channel_kind_check;
ALTER TABLE alert_deliveries ADD CONSTRAINT alert_deliveries_channel_kind_check CHECK (channel_kind IN ('log', 'mock'));
ALTER TABLE alert_routes DROP CONSTRAINT alert_routes_channel_kind_check;
ALTER TABLE alert_routes ADD CONSTRAINT alert_routes_channel_kind_check CHECK (channel_kind IN ('log', 'mock'));
