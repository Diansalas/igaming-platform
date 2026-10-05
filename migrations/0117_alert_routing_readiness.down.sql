-- MUST RUN INSIDE A SINGLE TRANSACTION (db.MigrateDown does this). Run by
-- hand, use `psql --single-transaction -v ON_ERROR_STOP=1 -f <this file>`.
--
-- Reverses 0117. REFUSES (AR099) while alert_routes holds ANY row (security
-- L-2: after the down, a route 0117 left enabled=false would become LIVE
-- under 0110 semantics, which has no enabled flag) or any alert_deliveries
-- row carries an unrouted_reason. Otherwise restores the 0110 shapes.
--
-- The search_path pins on the 0110 functions are deliberately LEFT IN PLACE
-- (security H-1 point 4: leaving them is preferred); the restored
-- alert_routes_guard keeps its pin too.
--
-- FORCE RLS binds the owner, so the guard would otherwise see zero rows (the
-- 0110 down's identical reasoning). NO FORCE is lifted only inside this
-- transactional script: a refusal rolls it back, success restores it.
ALTER TABLE alert_routes NO FORCE ROW LEVEL SECURITY;
ALTER TABLE alert_deliveries NO FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM alert_routes) THEN
        RAISE EXCEPTION '0117 down refused: alert_routes has rows (they would become live without the enabled flag)' USING ERRCODE = 'AR099';
    END IF;
    IF EXISTS (SELECT 1 FROM alert_deliveries WHERE unrouted_reason IS NOT NULL) THEN
        RAISE EXCEPTION '0117 down refused: alert_deliveries rows carry an unrouted_reason' USING ERRCODE = 'AR099';
    END IF;
END $$;

ALTER TABLE alert_routes FORCE ROW LEVEL SECURITY;
ALTER TABLE alert_deliveries FORCE ROW LEVEL SECURITY;

ALTER TABLE alert_deliveries DROP CONSTRAINT alert_deliveries_unrouted_has_reason_check;
ALTER TABLE alert_deliveries DROP CONSTRAINT alert_deliveries_unrouted_reason_event_check;
ALTER TABLE alert_deliveries DROP CONSTRAINT alert_deliveries_unrouted_reason_check;
ALTER TABLE alert_deliveries DROP COLUMN unrouted_reason;

-- Restore the 0110 guard body (pin kept).
CREATE OR REPLACE FUNCTION alert_routes_guard() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'alert_routes is append-only: TRUNCATE is not permitted';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'alert_routes is append-only: DELETE is not permitted';
    END IF;

    IF TG_OP = 'INSERT' THEN
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

DROP FUNCTION alerting_channel_kind_is_human_notification(TEXT);

ALTER TABLE alert_routes DROP CONSTRAINT alert_routes_enabled_requires_target_check;
ALTER TABLE alert_routes DROP CONSTRAINT alert_routes_reason_code_check;
ALTER TABLE alert_routes ALTER COLUMN recipient_ref SET NOT NULL;
ALTER TABLE alert_routes DROP COLUMN reason_code;
ALTER TABLE alert_routes DROP COLUMN enabled;
