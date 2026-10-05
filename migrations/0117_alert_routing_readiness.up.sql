-- ALERT-DELIVERY-1 routing readiness, smallest cut (ADR 0102 section 18;
-- design docs/plans/prh2-hardening-round/analysis/alert-delivery-design-v1.md,
-- adopted cut = scope review + security review conditions).
--
-- NUMBERING: 0116 (REVOKE TEMP) is written on another branch; until it
-- merges `migrate verify` on this branch alone shows a gap at 0116. Never
-- commit a 0116 file from here.
--
-- What this migration does (and ONLY this):
--   1. alert_routes: enabled boolean NOT NULL DEFAULT false (every existing
--      row becomes DISABLED: fail closed), reason_code (closed vocabulary),
--      recipient_ref nullable (a disabled route can be staged before any
--      recipient exists - no placeholder recipient is ever invented), CHECK
--      R1 (enabled requires a recipient_ref and a channel_kind).
--   2. SR-7 structural guard: a route can never be ENABLED for a channel
--      kind whose human_notification flag is true. No such kind exists yet
--      (channel_kind stays limited to log/mock, both non-human). The
--      migration that first admits a real kind must consciously replace
--      alerting_channel_kind_is_human_notification() AND build four-eyes.
--   3. alert_deliveries.unrouted_reason: a route with no wired channel (N-4)
--      becomes a visible, counted row instead of a log line.
--   4. Security H-1: every function created or replaced here, and every
--      0110 alert function, pins search_path = pg_catalog, public, pg_temp
--      (a TEMP table can no longer shadow staff_users / alerts / ...).
--
-- NOT in this migration (deferred, ADR 0102 section 18 "Deferred"): channels
-- table, secret refs, channel-kind table, kind-specific routes, unique
-- current route, last-p1-route guard, route_changed Kind, status view.
-- No route, recipient, channel or secret is seeded. Grants and RLS are
-- UNCHANGED: platform-admin/dispatcher policies from 0110 still apply, FORCE
-- RLS stays on, tenant sessions see 0 rows of routing configuration.

-- ======================================================================
-- 1. Security H-1: pin search_path on the 0110 alert functions
-- ======================================================================
ALTER FUNCTION alerting_validated_platform_admin() SET search_path = pg_catalog, public, pg_temp;
ALTER FUNCTION alerting_session_scope() SET search_path = pg_catalog, public, pg_temp;
ALTER FUNCTION alerting_attributes_are_flat_scalars(JSONB) SET search_path = pg_catalog, public, pg_temp;
ALTER FUNCTION alert_kinds_deny_write() SET search_path = pg_catalog, public, pg_temp;
ALTER FUNCTION alerts_guard() SET search_path = pg_catalog, public, pg_temp;
ALTER FUNCTION alert_occurrences_guard() SET search_path = pg_catalog, public, pg_temp;
ALTER FUNCTION alert_deliveries_guard() SET search_path = pg_catalog, public, pg_temp;

-- ======================================================================
-- 2. alert_routes
-- ======================================================================
ALTER TABLE alert_routes
    ADD COLUMN enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN reason_code text NULL;

-- Closed vocabulary (never free text; it lands in the audit row too).
ALTER TABLE alert_routes
    ADD CONSTRAINT alert_routes_reason_code_check
    CHECK (reason_code IS NULL OR reason_code IN
        ('initial_setup', 'on_call_change', 'escalation_change', 'correction', 'decommission', 'drill'));

-- A disabled route may be staged before the recipient is known.
ALTER TABLE alert_routes ALTER COLUMN recipient_ref DROP NOT NULL;

-- R1: fail-closed activation.
ALTER TABLE alert_routes
    ADD CONSTRAINT alert_routes_enabled_requires_target_check
    CHECK (NOT enabled OR (recipient_ref IS NOT NULL AND channel_kind IS NOT NULL));

COMMENT ON COLUMN alert_routes.enabled IS 'ALERT-DELIVERY-1: DEFAULT false. The dispatcher delivers only through enabled routes. Immutable per row: activation is always a new version (supersession), never an in-place flip.';

-- Whether a channel kind reaches a PERSON. Fail-closed: only the two kinds
-- known to be non-human answer false; ANY other value answers true. The
-- migration that admits a real kind replaces this function together with the
-- four-eyes approval flow (security H-2 / SR-7).
CREATE FUNCTION alerting_channel_kind_is_human_notification(p_kind TEXT) RETURNS BOOLEAN AS $$
    SELECT CASE p_kind WHEN 'log' THEN false WHEN 'mock' THEN false ELSE true END;
$$ LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp;

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
        -- created_by is FORCED from the validated platform-admin actor
        -- (§3.2), never application-supplied.
        NEW.created_by := alerting_validated_platform_admin();
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
-- 3. alert_deliveries.unrouted_reason (N-4: visible, not a log line)
-- ======================================================================
ALTER TABLE alert_deliveries ADD COLUMN unrouted_reason text NULL;

ALTER TABLE alert_deliveries
    ADD CONSTRAINT alert_deliveries_unrouted_reason_check
    CHECK (unrouted_reason IS NULL OR unrouted_reason IN ('no_route', 'channel_disabled', 'no_sink'));
ALTER TABLE alert_deliveries
    ADD CONSTRAINT alert_deliveries_unrouted_reason_event_check
    CHECK (unrouted_reason IS NULL OR event = 'unrouted');
-- NOT VALID: pre-0117 unrouted rows keep NULL (the table is append-only, no
-- backfill); every row inserted from now on must carry a reason.
ALTER TABLE alert_deliveries
    ADD CONSTRAINT alert_deliveries_unrouted_has_reason_check
    CHECK (event <> 'unrouted' OR unrouted_reason IS NOT NULL) NOT VALID;

COMMENT ON COLUMN alert_deliveries.unrouted_reason IS 'ALERT-DELIVERY-1: why an unrouted row exists. no_route = no current route for the step; channel_disabled = a current route exists but is not enabled; no_sink = an enabled route names a channel this binary has no sink for. NULL only on pre-0117 rows.';
