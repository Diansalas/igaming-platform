-- Licensing slice 1 (docs/plans/lic1-record.md; audit gaps G-4, G-5; triage L-22). Three things, nothing else:
--
--   1. licences gains review_due_at (DATE), evidence_reference (TEXT, an opaque business-supplied reference, NEVER
--      fabricated, nullable, 1..200 characters when present, like launch determination_reference) and updated_at.
--      The status vocabulary is UNCHANGED (active / suspended / expired); a regulator revocation is recorded as
--      'expired' with a reason code (a distinct 'revoked' status is a pending human decision).
--   2. licence_versions: an append-only history. A trigger on licences writes ONE version row for every INSERT and
--      every change of any tracked column (status, dates, review date, evidence reference, number, products,
--      markets), whoever made it. RLS: platform-admin SELECT and INSERT only; no UPDATE / DELETE policy, a
--      deny trigger on UPDATE / DELETE and a deny trigger on TRUNCATE. Existing licences get one 'baseline' version.
--   3. Two alert Kinds for the read-only expiry / review sweep: licence.expired (p1) and licence.expiring (p2),
--      platform-scoped and SUBJECTLESS (a licence can be shared by many tenants), raised ONLY under the
--      alert_dispatcher platform-service identity (the 0156 reconciliation.sweep_failed pattern: alert_kinds CHECK
--      widened by exactly these two Kinds, the dispatcher INSERT policies widened by exactly these two Kinds, and a
--      RESTRICTIVE policy so a platform admin cannot raise them). Severities are ENGINEERING DEFAULTS pending the
--      owner decision on recipients (ALERT-DELIVERY-1 OPEN). Attributes are ids, counts and dates only.
--
-- FORCE RLS stays on every table. No other policy, grant or function is changed.

SET LOCAL lock_timeout = '5s';

-- ======================================================================
-- 1. licences columns
-- ======================================================================
ALTER TABLE licences ADD COLUMN review_due_at DATE;
ALTER TABLE licences ADD COLUMN evidence_reference TEXT;
ALTER TABLE licences ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE licences ADD CONSTRAINT licences_evidence_reference_shape
    CHECK (evidence_reference IS NULL OR (evidence_reference = btrim(evidence_reference) AND char_length(evidence_reference) BETWEEN 1 AND 200));
COMMENT ON COLUMN licences.review_due_at IS 'Date by which the licence (or its regulatory conditions) must be reviewed; read by the licence expiry/review sweep (licence.expiring). NULL = none recorded.';
COMMENT ON COLUMN licences.evidence_reference IS 'Opaque business-supplied reference to the licence evidence held elsewhere (never the document, never fabricated by the platform). NULL = none recorded.';

CREATE FUNCTION licences_touch_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER licences_touch_updated_at
    BEFORE UPDATE ON licences
    FOR EACH ROW EXECUTE FUNCTION licences_touch_updated_at();

-- ======================================================================
-- 2. licence_versions (append-only history)
-- ======================================================================
CREATE TABLE licence_versions (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    licence_id         UUID NOT NULL REFERENCES licences(id),
    version_no         INTEGER NOT NULL CHECK (version_no >= 1),
    operation          TEXT NOT NULL CHECK (operation IN ('baseline', 'insert', 'update')),
    changed_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    changed_by         UUID,
    status             TEXT NOT NULL,
    issued_at          DATE,
    expires_at         DATE,
    review_due_at      DATE,
    evidence_reference TEXT,
    licence_number     TEXT NOT NULL,
    permitted_products JSONB NOT NULL,
    permitted_markets  JSONB NOT NULL,
    UNIQUE (licence_id, version_no)
);
COMMENT ON TABLE licence_versions IS 'Append-only history of licences (migration 0176): one row per insert / change, written by trigger. changed_by is the platform-admin principal of the writing session (NULL for a baseline row).';

ALTER TABLE licence_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE licence_versions FORCE ROW LEVEL SECURITY;

CREATE POLICY licence_versions_platform_admin_read ON licence_versions
    FOR SELECT USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
CREATE POLICY licence_versions_platform_admin_insert ON licence_versions
    FOR INSERT WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
-- No UPDATE, DELETE or FOR ALL policy.

CREATE FUNCTION licence_versions_deny_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'licence_versions is append-only: % is not permitted', TG_OP USING ERRCODE = 'LV001';
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER licence_versions_deny_update_delete
    BEFORE UPDATE OR DELETE ON licence_versions
    FOR EACH ROW EXECUTE FUNCTION licence_versions_deny_mutation();
CREATE TRIGGER licence_versions_deny_truncate
    BEFORE TRUNCATE ON licence_versions
    FOR EACH STATEMENT EXECUTE FUNCTION licence_versions_deny_mutation();

-- The version writer. SECURITY INVOKER on purpose: it runs in the session that changed the licence, which the
-- licences write policies already restrict to a platform-admin session, so the history insert needs no extra power.
CREATE FUNCTION licences_write_version() RETURNS TRIGGER AS $$
DECLARE
    v_no INTEGER;
BEGIN
    IF TG_OP = 'UPDATE' AND ROW(NEW.status, NEW.issued_at, NEW.expires_at, NEW.review_due_at, NEW.evidence_reference,
                                NEW.licence_number, NEW.permitted_products, NEW.permitted_markets)
        IS NOT DISTINCT FROM ROW(OLD.status, OLD.issued_at, OLD.expires_at, OLD.review_due_at, OLD.evidence_reference,
                                 OLD.licence_number, OLD.permitted_products, OLD.permitted_markets) THEN
        RETURN NEW;
    END IF;
    SELECT COALESCE(max(version_no), 0) + 1 INTO v_no FROM licence_versions WHERE licence_id = NEW.id;
    INSERT INTO licence_versions (licence_id, version_no, operation, changed_by, status, issued_at, expires_at,
                                  review_due_at, evidence_reference, licence_number, permitted_products, permitted_markets)
    VALUES (NEW.id, v_no, lower(TG_OP), NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid,
            NEW.status, NEW.issued_at, NEW.expires_at, NEW.review_due_at, NEW.evidence_reference,
            NEW.licence_number, NEW.permitted_products, NEW.permitted_markets);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER licences_write_version
    AFTER INSERT OR UPDATE ON licences
    FOR EACH ROW EXECUTE FUNCTION licences_write_version();

-- One baseline version for each licence that already exists. FORCE RLS binds the owner, so it is lifted for this
-- statement only (inside the migration transaction).
ALTER TABLE licence_versions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE licences NO FORCE ROW LEVEL SECURITY;
INSERT INTO licence_versions (licence_id, version_no, operation, changed_at, changed_by, status, issued_at, expires_at,
                              review_due_at, evidence_reference, licence_number, permitted_products, permitted_markets)
SELECT id, 1, 'baseline', now(), NULL, status, issued_at, expires_at, review_due_at, evidence_reference,
       licence_number, permitted_products, permitted_markets
  FROM licences;
ALTER TABLE licences FORCE ROW LEVEL SECURITY;
ALTER TABLE licence_versions FORCE ROW LEVEL SECURITY;

-- ======================================================================
-- 3. Alert Kinds licence.expired / licence.expiring
-- ======================================================================
ALTER TABLE alert_kinds DROP CONSTRAINT alert_kinds_check1;
ALTER TABLE alert_kinds ADD CONSTRAINT alert_kinds_check1 CHECK (
    requires_subject = true
    OR kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed',
                'staff.platform_mfa_integrity', 'staff.platform_session_integrity', 'staff.platform_mfa_hard_locked',
                'staff.platform_mfa_recovery_used', 'staff.platform_mfa_failure_pattern',
                'staff.platform_break_glass_used',
                'reconciliation.sweep_failed',
                'licence.expired', 'licence.expiring'));

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.alert_kinds WHERE kind IN ('licence.expired', 'licence.expiring')) THEN
        RAISE EXCEPTION 'migration 0176 pre-flight: a licence.* alert kind already exists';
    END IF;
    CREATE POLICY alert_kinds_seed_0176 ON alert_kinds
        FOR INSERT TO CURRENT_USER
        WITH CHECK (kind IN ('licence.expired', 'licence.expiring'));
    INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject, in_tx_raisable_by_tenant, allowed_keys, raise_mode) VALUES
        ('licence.expired', 'p1', 'platform', false, false, false,
         ARRAY['licence_id', 'trigger', 'expires_at', 'review_due_at', 'tenant_count', 'brand_count', 'tenant_ids', 'brand_ids', 'truncated'], 'detached'),
        ('licence.expiring', 'p2', 'platform', false, false, false,
         ARRAY['licence_id', 'trigger', 'expires_at', 'review_due_at', 'tenant_count', 'brand_count', 'tenant_ids', 'brand_ids', 'truncated'], 'detached');
    DROP POLICY alert_kinds_seed_0176 ON alert_kinds;
END
$$;

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
        AND kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed', 'reconciliation.sweep_failed',
                     'licence.expired', 'licence.expiring')
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
        AND kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed', 'reconciliation.sweep_failed',
                     'licence.expired', 'licence.expiring')
    );

CREATE POLICY alerts_licence_sweep_dispatcher_only ON alerts
    AS RESTRICTIVE
    FOR INSERT
    WITH CHECK (
        kind NOT IN ('licence.expired', 'licence.expiring')
        OR NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
    );
CREATE POLICY alerts_licence_sweep_dispatcher_only ON alert_occurrences
    AS RESTRICTIVE
    FOR INSERT
    WITH CHECK (
        kind NOT IN ('licence.expired', 'licence.expiring')
        OR NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
    );
