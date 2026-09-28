-- PRH-2 workstream I-core: durable, provider-neutral alerting (ADR 0102
-- "Durable Alerting and Provider-Neutral Delivery", ALERT-DELIVERY-1,
-- revision 2, ACCEPTED). NUMBERING (the migration-0105 precedent): the
-- ADR allocates this migration number 0110, after A's 0108 (casino launch
-- session revoke-consumed, merged) and G1's 0109 (tenant-visible audit,
-- not yet merged at authoring time). This file was originally authored
-- and locally verified as 0108 on a branch where 0108/0109 did not yet
-- exist, then renamed to its allocated number 0110 once A's 0108 merged -
-- see internal/alerting's own commit history and migration 0105's header
-- for the identical renumbering precedent. `migrate verify` on this
-- branch alone still shows a 0108->0110 gap until G1's 0109 also merges;
-- the orchestrator resolves that at final merge.
--
-- Five tables, all tenant/platform scoped and RLS-hardened per ADR §3.2
-- and §4.1:
--   alert_kinds       - immutable, migration-seeded vocabulary (ADR §3.1).
--   alerts            - one row per open, deduplicated condition.
--   alert_occurrences - append-only, one row per successful Raise.
--   alert_routes      - versioned routing config. NO SEED ROWS (HD-PRH2-4).
--   alert_deliveries  - append-only delivery/escalation state.
--
-- No recipient, person, email, phone number or rota is named or seeded
-- anywhere in this migration (HD-PRH2-4). alert_routes ships empty.

-- ======================================================================
-- 0. Session-scope helpers
-- ======================================================================
-- alerting_validated_platform_admin mirrors migration 0105's
-- payment_kill_switch_session precedent: app.platform_admin_principal_id
-- is never trusted by itself - it must independently resolve to a real,
-- platform-scoped (tenant_id IS NULL) staff_users row. Used by the
-- alerts state guard (ack/resolve) and by alert_routes writes.
CREATE FUNCTION alerting_validated_platform_admin() RETURNS UUID AS $$
DECLARE
    v_platform TEXT := NULLIF(current_setting('app.platform_admin_principal_id', true), '');
BEGIN
    IF v_platform IS NULL THEN
        RAISE EXCEPTION 'alerting: no platform-admin principal in session';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM staff_users su WHERE su.id = v_platform::uuid AND su.tenant_id IS NULL) THEN
        RAISE EXCEPTION 'alerting: % is not a platform-scoped staff principal', v_platform;
    END IF;
    RETURN v_platform::uuid;
END;
$$ LANGUAGE plpgsql STABLE;

-- alerting_session_scope resolves the ADR §3.2 alert_occurrences.
-- raised_by_scope enum from session GUCs ONLY (SR-1: never from Go, never
-- from the Alert payload). It independently enforces the C-102-9 common
-- exclusion set (app.acting_* unset; no mixed-GUC session) so a session
-- that does not cleanly resolve to exactly one of the four shapes is
-- refused outright, rather than silently picked.
CREATE FUNCTION alerting_session_scope(OUT scope TEXT) AS $$
DECLARE
    v_tenant           TEXT := NULLIF(current_setting('app.tenant_id', true), '');
    v_principal        TEXT := NULLIF(current_setting('app.principal_id', true), '');
    v_player           TEXT := NULLIF(current_setting('app.player_account_id', true), '');
    v_platform         TEXT := NULLIF(current_setting('app.platform_admin_principal_id', true), '');
    v_service          TEXT := NULLIF(current_setting('app.platform_service_id', true), '');
    v_acting_tenant    TEXT := NULLIF(current_setting('app.acting_tenant_id', true), '');
    v_acting_platform  TEXT := NULLIF(current_setting('app.acting_platform_principal_id', true), '');
BEGIN
    IF v_acting_tenant IS NOT NULL OR v_acting_platform IS NOT NULL THEN
        RAISE EXCEPTION 'alerting: an acting (app.acting_*) session may not raise an alert occurrence directly';
    END IF;
    IF v_player IS NOT NULL THEN
        RAISE EXCEPTION 'alerting: a player session may not raise an alert occurrence';
    END IF;

    IF v_service IS NOT NULL THEN
        IF v_service <> 'alert_dispatcher' THEN
            RAISE EXCEPTION 'alerting: unknown platform service identity %', v_service;
        END IF;
        IF v_tenant IS NOT NULL OR v_platform IS NOT NULL THEN
            RAISE EXCEPTION 'alerting: mixed platform-service/tenant/platform-admin session';
        END IF;
        scope := 'platform_service';
        RETURN;
    END IF;

    IF v_platform IS NOT NULL THEN
        IF v_tenant IS NOT NULL THEN
            RAISE EXCEPTION 'alerting: mixed tenant/platform-admin session';
        END IF;
        PERFORM alerting_validated_platform_admin();
        scope := 'platform_admin';
        RETURN;
    END IF;

    IF v_tenant IS NOT NULL THEN
        IF v_principal IS NOT NULL THEN
            IF NOT EXISTS (SELECT 1 FROM staff_users su WHERE su.id = v_principal::uuid AND su.tenant_id = v_tenant::uuid) THEN
                RAISE EXCEPTION 'alerting: % is not a staff principal of tenant %', v_principal, v_tenant;
            END IF;
            scope := 'tenant_principal';
            RETURN;
        END IF;
        scope := 'tenant';
        RETURN;
    END IF;

    RAISE EXCEPTION 'alerting: unresolvable session scope (tenant=%, principal=%, player=%, platform=%, service=%)',
        v_tenant, v_principal, v_player, v_platform, v_service;
END;
$$ LANGUAGE plpgsql STABLE;

-- alert_attributes_flat_scalars checks the ADR §3.2 "flat scalar object"
-- shape: every top-level value is a JSON scalar (string/number/boolean/
-- null), never an object or array - closing off any nested-payload
-- channel for PII/secrets/raw error text to hide in.
CREATE FUNCTION alerting_attributes_are_flat_scalars(attrs JSONB) RETURNS BOOLEAN AS $$
    SELECT NOT EXISTS (
        SELECT 1 FROM jsonb_each(attrs) e
        WHERE jsonb_typeof(e.value) IN ('object', 'array')
    );
$$ LANGUAGE sql IMMUTABLE;

-- ======================================================================
-- 1. alert_kinds - immutable, migration-seeded vocabulary (ADR §3.1/§9.1)
-- ======================================================================
CREATE TABLE alert_kinds (
    kind                       TEXT PRIMARY KEY CHECK (kind ~ '^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$'),
    severity                   TEXT NOT NULL CHECK (severity IN ('p1', 'p2', 'p3')),
    scope                      TEXT NOT NULL CHECK (scope IN ('platform', 'tenant')),
    simulation                 BOOLEAN NOT NULL DEFAULT false,
    -- New (C-102-1/F2): per-Kind subject/raisable flags.
    requires_subject           BOOLEAN NOT NULL,
    in_tx_raisable_by_tenant   BOOLEAN NOT NULL DEFAULT false,
    allowed_keys               TEXT[] NOT NULL DEFAULT '{}',
    raise_mode                 TEXT NOT NULL CHECK (raise_mode IN ('in_tx', 'detached', 'post_commit')),
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Simulation Kinds are always p3 (ADR §3.1).
    CHECK (NOT simulation OR severity = 'p3'),
    -- Scope/subject consistency (LF F2, walked exhaustively by the Go
    -- migration test): every seeded Kind in PRH-2 is scope='platform' and
    -- requires_subject=true, EXCEPT exactly the three meta-Kinds, which
    -- require NO subject (SR-1: a subject would re-open scope laundering).
    -- No tenant-scope Kind is seeded in PRH-2 (ADR §4.3), so this CHECK is
    -- deliberately written against the concrete PRH-2 vocabulary rather
    -- than a more permissive rule that would not catch a future mistake.
    CHECK (
        requires_subject = true
        OR kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed')
    ),
    -- Only the three meta-Kinds may ever be raised by the dispatcher
    -- identity; nothing else is in_tx_raisable_by_tenant=false AND
    -- scope=platform AND simultaneously reachable only from the
    -- dispatcher - enforced structurally by the RLS WITH CHECK below, not
    -- by this table alone.
    CHECK (
        kind NOT IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed')
        OR NOT in_tx_raisable_by_tenant
    )
);

COMMENT ON TABLE alert_kinds IS 'ADR 0102 closed alert-Kind vocabulary. Seeds vocabulary only - never a recipient, person or rota (HD-PRH2-4). Only a migration ever writes this table.';

INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject, in_tx_raisable_by_tenant, allowed_keys, raise_mode) VALUES
    -- ADR §8 row 1-2: payment multiple-success / index backstop.
    ('payment.multiple_success_for_intent', 'p1', 'platform', false, true, true, ARRAY['attempt_id','evidence_kind','provider_id','request_id'], 'in_tx'),
    ('payment.deposit_intent_index_backstop_fired', 'p1', 'platform', false, true, true, ARRAY['attempt_id','request_id'], 'in_tx'),
    -- ADR §8 rows 3-7, 14: reconciliation.
    ('reconciliation.ledger_projection_drift', 'p1', 'platform', false, true, true, ARRAY['run_id','mismatch_count','request_id'], 'in_tx'),
    ('reconciliation.sportsbook_settlement_mismatch', 'p1', 'platform', false, true, true, ARRAY['run_id','mismatch_count','statement_source','request_id'], 'in_tx'),
    ('reconciliation.casino_consistency_mismatch', 'p1', 'platform', false, true, true, ARRAY['run_id','mismatch_count','request_id'], 'in_tx'),
    ('reconciliation.casino_statement_mismatch', 'p1', 'platform', false, true, true, ARRAY['run_id','mismatch_count','statement_source','request_id'], 'post_commit'),
    ('reconciliation.payment_statement_mismatch', 'p1', 'platform', false, true, true, ARRAY['run_id','mismatch_count','statement_source','import_id','request_id'], 'post_commit'),
    ('reconciliation.run_failed', 'p1', 'platform', false, true, true, ARRAY['stream','phase','sqlstate_class','request_id'], 'detached'),
    -- ADR §8 row 8: kill switch (post-commit, detached only).
    ('payment.kill_switch_engaged', 'p2', 'platform', false, true, true, ARRAY['provider_scope','operation_scope','reason_code','changed_by_scope','is_platform_takeover','request_id'], 'post_commit'),
    -- ADR §8 rows 9-10: handler integrity (failure-path, detached).
    ('casino.callback_integrity', 'p1', 'platform', false, true, true, ARRAY['provider_id','request_id'], 'detached'),
    ('payment.webhook_integrity', 'p1', 'platform', false, true, true, ARRAY['provider_id','request_id'], 'detached'),
    -- ADR §8 rows 11-13: simulation-only, never delivered (p3, sim=true).
    ('simulation.payment.payload_mismatch', 'p3', 'platform', true, true, true, ARRAY['provider_id','request_id'], 'detached'),
    ('simulation.casino_play', 'p3', 'platform', true, true, true, ARRAY['action','request_id'], 'detached'),
    ('simulation.sportsbook_settlement', 'p3', 'platform', true, true, true, ARRAY['reason','event_type','generation','bet_status','request_id'], 'detached'),
    -- ADR §6.3: the three accepted meta-Kinds. requires_subject=false;
    -- raised ONLY by the alert_dispatcher platform-service identity.
    ('alerting.unrouted', 'p2', 'platform', false, false, false, ARRAY[]::TEXT[], 'detached'),
    ('alerting.delivery_dead', 'p2', 'platform', false, false, false, ARRAY[]::TEXT[], 'detached'),
    ('alerting.raise_failed', 'p1', 'platform', false, false, false, ARRAY['kind','sqlstate_class'], 'detached');

-- The immutability triggers below are why seeding happens BEFORE
-- ENABLE/FORCE RLS is applied at the end of this section, mirroring
-- migration 0044's own "seed the fixed rows, THEN force RLS" ordering:
-- FORCE RLS binds the table owner too, so an INSERT issued afterwards by
-- the (owner) migration role would itself be denied by the SELECT-only
-- policy below.
CREATE FUNCTION alert_kinds_deny_write() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'alert_kinds is immutable: TRUNCATE is not permitted';
    END IF;
    RAISE EXCEPTION 'alert_kinds is immutable: % is not permitted (only a migration may write this table)', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER alert_kinds_deny_update_delete
    BEFORE UPDATE OR DELETE ON alert_kinds
    FOR EACH ROW EXECUTE FUNCTION alert_kinds_deny_write();

CREATE TRIGGER alert_kinds_deny_truncate
    BEFORE TRUNCATE ON alert_kinds
    FOR EACH STATEMENT EXECUTE FUNCTION alert_kinds_deny_write();

ALTER TABLE alert_kinds ENABLE ROW LEVEL SECURITY;
ALTER TABLE alert_kinds FORCE ROW LEVEL SECURITY;

-- Every session may read the vocabulary (it names no tenant, no
-- recipient, no secret); there is no write policy of any kind, so no
-- role - not even the table owner under FORCE RLS - can INSERT here
-- outside this migration.
CREATE POLICY alert_kinds_read_all ON alert_kinds
    FOR SELECT
    USING (true);

-- ======================================================================
-- 2. alerts - one row per open, deduplicated condition (ADR §3.2)
-- ======================================================================
CREATE TABLE alerts (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NULL REFERENCES tenants(id),
    subject_tenant_id     UUID NULL REFERENCES tenants(id),
    CHECK (subject_tenant_id IS NULL OR tenant_id IS NULL),
    kind                  TEXT NOT NULL REFERENCES alert_kinds(kind),
    severity              TEXT NOT NULL CHECK (severity IN ('p1', 'p2', 'p3')),
    simulation            BOOLEAN NOT NULL DEFAULT false,
    discriminator         TEXT NOT NULL CHECK (discriminator ~ '^[A-Za-z0-9:_.-]{1,160}$'),
    dedup_key             TEXT GENERATED ALWAYS AS (kind || '|' || COALESCE(subject_tenant_id::text, '-') || '|' || discriminator) STORED,
    attributes            JSONB NOT NULL DEFAULT '{}'::jsonb,
    CHECK (octet_length(attributes::text) <= 2048),
    CHECK (alerting_attributes_are_flat_scalars(attributes)),
    -- SR-3: a request_id attribute, wherever present, is charset-checked
    -- at the database as a backstop - the Go constructor (N-1) drops a
    -- non-conforming request_id BEFORE this INSERT is ever issued, so
    -- this should never actually fire on the sanctioned path.
    CHECK (
        NOT (attributes ? 'request_id')
        OR (attributes->>'request_id') ~ '^[A-Za-z0-9_.:-]{1,128}$'
    ),
    state                 TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'acked', 'resolved')),
    acked_by              UUID NULL,
    acked_at              TIMESTAMPTZ NULL,
    resolved_by           UUID NULL,
    resolved_at           TIMESTAMPTZ NULL,
    resolve_reason_code   TEXT NULL CHECK (resolve_reason_code IS NULL OR octet_length(resolve_reason_code) BETWEEN 1 AND 64),
    first_seen_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE alerts IS 'ADR 0102: one row per open, deduplicated alert condition. dedup_key embeds Kind + subject tenant (AL-3). Platform-owned alerts (tenant_id IS NULL) about a tenant carry subject_tenant_id; that tenant may read them read-only, never ack/resolve them (AL-2).';

-- AL-3/S-7.1: UNIQUE NULLS NOT DISTINCT over non-resolved rows. A resolve
-- lets the next Raise open a fresh alert (a new occurrence sequence),
-- rather than silently reopening history.
CREATE UNIQUE INDEX alerts_dedup_open ON alerts (tenant_id, dedup_key) NULLS NOT DISTINCT WHERE state <> 'resolved';
CREATE INDEX alerts_tenant_idx ON alerts (tenant_id) WHERE tenant_id IS NOT NULL;
CREATE INDEX alerts_subject_tenant_idx ON alerts (subject_tenant_id) WHERE subject_tenant_id IS NOT NULL;
CREATE INDEX alerts_kind_idx ON alerts (kind);

CREATE FUNCTION alerts_guard() RETURNS TRIGGER AS $$
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
$$ LANGUAGE plpgsql;

CREATE TRIGGER alerts_guard
    BEFORE INSERT OR UPDATE OR DELETE ON alerts
    FOR EACH ROW EXECUTE FUNCTION alerts_guard();

CREATE TRIGGER alerts_deny_truncate
    BEFORE TRUNCATE ON alerts
    FOR EACH STATEMENT EXECUTE FUNCTION alerts_guard();

-- ======================================================================
-- 3. alert_occurrences - append-only, one row per successful Raise
-- ======================================================================
CREATE TABLE alert_occurrences (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    alert_id          UUID NOT NULL REFERENCES alerts(id),
    kind              TEXT NOT NULL REFERENCES alert_kinds(kind),
    tenant_id         UUID NULL REFERENCES tenants(id),
    subject_tenant_id UUID NULL REFERENCES tenants(id),
    raised_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    raised_by_scope   TEXT NOT NULL CHECK (raised_by_scope IN ('tenant', 'tenant_principal', 'platform_admin', 'platform_service'))
);

COMMENT ON TABLE alert_occurrences IS 'ADR 0102: append-only, one row per successful Raise. kind/tenant_id/subject_tenant_id are copied from the parent alert by trigger (SR-2) so the meta-only dispatcher WITH CHECK is directly expressible here too.';

CREATE INDEX alert_occurrences_alert_idx ON alert_occurrences (alert_id);
CREATE INDEX alert_occurrences_tenant_idx ON alert_occurrences (tenant_id) WHERE tenant_id IS NOT NULL;

CREATE FUNCTION alert_occurrences_guard() RETURNS TRIGGER AS $$
DECLARE
    v_alert RECORD;
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'alert_occurrences is append-only: TRUNCATE is not permitted';
    END IF;
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        RAISE EXCEPTION 'alert_occurrences is append-only: % is not permitted', TG_OP;
    END IF;

    SELECT * INTO v_alert FROM alerts WHERE id = NEW.alert_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'alert_occurrences: alert % does not exist', NEW.alert_id;
    END IF;

    -- SR-2: copied from the parent alert, never caller-supplied.
    NEW.kind := v_alert.kind;
    NEW.tenant_id := v_alert.tenant_id;
    NEW.subject_tenant_id := v_alert.subject_tenant_id;
    NEW.raised_at := now();
    -- SR-1: forced from the session GUCs, never from Go.
    NEW.raised_by_scope := alerting_session_scope();

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER alert_occurrences_guard
    BEFORE INSERT OR UPDATE OR DELETE ON alert_occurrences
    FOR EACH ROW EXECUTE FUNCTION alert_occurrences_guard();

CREATE TRIGGER alert_occurrences_deny_truncate
    BEFORE TRUNCATE ON alert_occurrences
    FOR EACH STATEMENT EXECUTE FUNCTION alert_occurrences_guard();

-- ======================================================================
-- 4. alert_routes - versioned, effective-dated, append-only, NO SEED ROWS
-- ======================================================================
CREATE TABLE alert_routes (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope             TEXT NOT NULL CHECK (scope IN ('platform', 'tenant')),
    tenant_id         UUID NULL REFERENCES tenants(id),
    -- HD-PRH2-4 / ADR §3.2: always NULL in PRH-2 (no per-tenant routing
    -- surface is built yet); kept nullable so the column need not be
    -- added later when it is.
    CHECK (tenant_id IS NULL),
    severity          TEXT NOT NULL CHECK (severity IN ('p1', 'p2', 'p3')),
    escalation_step   INT NOT NULL DEFAULT 0 CHECK (escalation_step >= 0),
    channel_kind      TEXT NOT NULL CHECK (channel_kind IN ('log', 'mock')),
    -- Opaque recipient reference (HD-PRH2-4): refuses email and phone
    -- shapes outright, so no fictional recipient can even be typed in.
    recipient_ref     TEXT NOT NULL CHECK (recipient_ref ~ '^[a-z0-9][a-z0-9_.:-]{0,127}$'),
    CHECK (recipient_ref !~ '@' AND recipient_ref !~ '^\+?[0-9][0-9()\s-]{6,}$'),
    escalate_after    INTERVAL NULL,
    effective_from    TIMESTAMPTZ NOT NULL DEFAULT now(),
    superseded_at     TIMESTAMPTZ NULL,
    superseded_by     UUID NULL REFERENCES alert_routes(id),
    CHECK ((superseded_at IS NULL) = (superseded_by IS NULL)),
    created_by        UUID NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE alert_routes IS 'ADR 0102 §3.2/§6.2: versioned (severity, scope, step) -> channel + opaque recipient_ref routing. NEVER seeded with a route (HD-PRH2-4): an alert with no effective route is "unrouted", visible and counted, until a platform admin configures one.';

CREATE INDEX alert_routes_lookup_idx ON alert_routes (scope, severity, escalation_step) WHERE superseded_at IS NULL;

CREATE FUNCTION alert_routes_guard() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'alert_routes is append-only: TRUNCATE is not permitted';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'alert_routes is append-only: DELETE is not permitted';
    END IF;

    IF TG_OP = 'INSERT' THEN
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
$$ LANGUAGE plpgsql;

CREATE TRIGGER alert_routes_guard
    BEFORE INSERT OR UPDATE OR DELETE ON alert_routes
    FOR EACH ROW EXECUTE FUNCTION alert_routes_guard();

CREATE TRIGGER alert_routes_deny_truncate
    BEFORE TRUNCATE ON alert_routes
    FOR EACH STATEMENT EXECUTE FUNCTION alert_routes_guard();

-- ======================================================================
-- 5. alert_deliveries - append-only delivery/escalation state
-- ======================================================================
CREATE TABLE alert_deliveries (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    alert_id            UUID NOT NULL REFERENCES alerts(id),
    tenant_id           UUID NULL REFERENCES tenants(id),
    subject_tenant_id   UUID NULL REFERENCES tenants(id),
    escalation_step     INT NOT NULL DEFAULT 0 CHECK (escalation_step >= 0),
    attempt_no          INT NOT NULL DEFAULT 0 CHECK (attempt_no >= 0),
    event               TEXT NOT NULL CHECK (event IN ('claimed', 'sent', 'failed', 'unrouted', 'dead', 'suppressed_simulation')),
    -- LF F9: pins attempt_no=0 for unrouted at the database, making
    -- "one unrouted row per (alert, step)" structural, not conventional.
    CHECK (event <> 'unrouted' OR attempt_no = 0),
    route_id            UUID NULL REFERENCES alert_routes(id),
    channel_kind        TEXT NULL CHECK (channel_kind IN ('log', 'mock')),
    last_error_class    TEXT NULL CHECK (last_error_class IN ('timeout', 'unavailable', 'rejected', 'misconfigured', 'unknown')),
    -- 'dead' also carries the last observed error class (the one that
    -- exhausted the retry budget) - only 'failed' and 'dead' may.
    CHECK (event IN ('failed', 'dead') OR last_error_class IS NULL),
    next_attempt_at     TIMESTAMPTZ NULL,
    next_escalation_at  TIMESTAMPTZ NULL,
    recorded_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (alert_id, escalation_step, attempt_no, event)
);

COMMENT ON TABLE alert_deliveries IS 'ADR 0102 §3.2: append-only delivery/escalation state; an alerts current delivery state is its latest row. CHECK (event <> unrouted OR attempt_no = 0) makes one-unrouted-row-per-(alert,step) structural (LF F9).';

CREATE INDEX alert_deliveries_alert_idx ON alert_deliveries (alert_id, recorded_at DESC);
CREATE INDEX alert_deliveries_tenant_idx ON alert_deliveries (tenant_id) WHERE tenant_id IS NOT NULL;

CREATE FUNCTION alert_deliveries_guard() RETURNS TRIGGER AS $$
DECLARE
    v_alert RECORD;
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'alert_deliveries is append-only: TRUNCATE is not permitted';
    END IF;
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        RAISE EXCEPTION 'alert_deliveries is append-only: % is not permitted', TG_OP;
    END IF;

    SELECT * INTO v_alert FROM alerts WHERE id = NEW.alert_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'alert_deliveries: alert % does not exist', NEW.alert_id;
    END IF;

    NEW.tenant_id := v_alert.tenant_id;
    NEW.subject_tenant_id := v_alert.subject_tenant_id;
    NEW.recorded_at := now();

    -- AL-9: a simulation alert is NEVER delivered - the only permitted
    -- delivery row for one is the explicit suppression marker.
    IF v_alert.simulation AND NEW.event <> 'suppressed_simulation' THEN
        RAISE EXCEPTION 'alert_deliveries: alert % is a simulation Kind and may only receive a suppressed_simulation row', NEW.alert_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER alert_deliveries_guard
    BEFORE INSERT OR UPDATE OR DELETE ON alert_deliveries
    FOR EACH ROW EXECUTE FUNCTION alert_deliveries_guard();

CREATE TRIGGER alert_deliveries_deny_truncate
    BEFORE TRUNCATE ON alert_deliveries
    FOR EACH STATEMENT EXECUTE FUNCTION alert_deliveries_guard();

-- ======================================================================
-- 6. Row-level security (ADR §4.1 named families; C-102-9 common
--    exclusion set applied to every predicate below)
-- ======================================================================
ALTER TABLE alerts ENABLE ROW LEVEL SECURITY;
ALTER TABLE alerts FORCE ROW LEVEL SECURITY;
ALTER TABLE alert_occurrences ENABLE ROW LEVEL SECURITY;
ALTER TABLE alert_occurrences FORCE ROW LEVEL SECURITY;
ALTER TABLE alert_routes ENABLE ROW LEVEL SECURITY;
ALTER TABLE alert_routes FORCE ROW LEVEL SECURITY;
ALTER TABLE alert_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE alert_deliveries FORCE ROW LEVEL SECURITY;

-- --- alerts_tenant_owned (alerts, alert_occurrences write; alert_deliveries SELECT only) ---
CREATE POLICY alerts_tenant_owned ON alerts
    FOR ALL
    USING (
        tenant_id IS NOT NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id IS NOT NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

CREATE POLICY alerts_tenant_owned ON alert_occurrences
    FOR ALL
    USING (
        tenant_id IS NOT NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id IS NOT NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

-- alert_deliveries: SELECT only for the owner tenant - "no tenant UPDATE
-- path" (C-102-7) and tenants never see deliveries of platform-owned
-- alerts of which they are merely the subject (ADR §4.1).
CREATE POLICY alerts_tenant_owned ON alert_deliveries
    FOR SELECT
    USING (
        tenant_id IS NOT NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

-- --- alerts_subject_tenant_read (alerts, alert_occurrences; SELECT only) ---
CREATE POLICY alerts_subject_tenant_read ON alerts
    FOR SELECT
    USING (
        tenant_id IS NULL
        AND subject_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

CREATE POLICY alerts_subject_tenant_read ON alert_occurrences
    FOR SELECT
    USING (
        tenant_id IS NULL
        AND subject_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

-- --- alerts_subject_tenant_raise (alerts, alert_occurrences; INSERT only) ---
CREATE POLICY alerts_subject_tenant_raise ON alerts
    FOR INSERT
    WITH CHECK (
        tenant_id IS NULL
        AND subject_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND EXISTS (SELECT 1 FROM alert_kinds ak WHERE ak.kind = alerts.kind AND ak.in_tx_raisable_by_tenant)
    );

CREATE POLICY alerts_subject_tenant_raise ON alert_occurrences
    FOR INSERT
    WITH CHECK (
        tenant_id IS NULL
        AND subject_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND EXISTS (SELECT 1 FROM alert_kinds ak WHERE ak.kind = alert_occurrences.kind AND ak.in_tx_raisable_by_tenant)
    );

-- --- alerts_platform_admin (all five tables) ---
CREATE POLICY alerts_platform_admin ON alerts
    FOR ALL
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND EXISTS (SELECT 1 FROM staff_users su WHERE su.id = NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid AND su.tenant_id IS NULL)
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND EXISTS (SELECT 1 FROM staff_users su WHERE su.id = NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid AND su.tenant_id IS NULL)
        AND tenant_id IS NULL
    );

CREATE POLICY alerts_platform_admin ON alert_occurrences
    FOR ALL
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND EXISTS (SELECT 1 FROM staff_users su WHERE su.id = NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid AND su.tenant_id IS NULL)
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND EXISTS (SELECT 1 FROM staff_users su WHERE su.id = NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid AND su.tenant_id IS NULL)
        AND tenant_id IS NULL
    );

CREATE POLICY alerts_platform_admin ON alert_deliveries
    FOR SELECT
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND EXISTS (SELECT 1 FROM staff_users su WHERE su.id = NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid AND su.tenant_id IS NULL)
    );

CREATE POLICY alerts_platform_admin ON alert_routes
    FOR ALL
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND EXISTS (SELECT 1 FROM staff_users su WHERE su.id = NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid AND su.tenant_id IS NULL)
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND EXISTS (SELECT 1 FROM staff_users su WHERE su.id = NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid AND su.tenant_id IS NULL)
    );

-- --- alerts_platform_service_dispatcher (all five tables) ---
CREATE POLICY alerts_platform_service_dispatcher ON alert_kinds
    FOR SELECT
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );
-- (alert_kinds already has alert_kinds_read_all granting SELECT to
-- everyone; this policy is additive/redundant there but keeps the
-- dispatcher family complete and self-documenting per table, per ADR §4.1.)

CREATE POLICY alerts_platform_service_dispatcher ON alerts
    FOR SELECT
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

-- Q1 (ACCEPTED)/security Part 1: the meta-only dispatcher INSERT, on
-- BOTH tables (SR-2), restricted to exactly the three accepted meta-Kinds
-- and no tenant, no subject.
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

CREATE POLICY alerts_platform_service_dispatcher ON alert_occurrences
    FOR SELECT
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

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
        -- SR-2: the copied kind column makes this expressible directly.
        AND kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed')
    );

CREATE POLICY alerts_platform_service_dispatcher ON alert_routes
    FOR SELECT
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

-- alert_deliveries: the dispatcher's core write surface - SELECT plus
-- INSERT, no restriction beyond the guard trigger/CHECKs above. No
-- UPDATE or DELETE anywhere (AL-10) - there is deliberately no policy
-- granting either.
CREATE POLICY alerts_platform_service_dispatcher ON alert_deliveries
    FOR SELECT
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

CREATE POLICY alerts_platform_service_dispatcher_insert ON alert_deliveries
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );
