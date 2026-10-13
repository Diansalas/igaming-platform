-- ADR 0116 SLICE S3 (part 2 of 3): the AML case model, proof-bound four-eyes, notes and the officer API. COMPLIANCE /
-- SECURITY MIGRATION: FOR `security` (BLOCKING), `identity-compliance`, `qa`, `code-reviewer` REVIEW BEFORE MERGE.
--
-- NUMBERING: 0142 (0137-0139 reserved for ADR 0119). Depends on 0140 (aml_authorize) and 0141 (aml_holds, helpers).
--
-- NO LEGAL VALUE and NO AML VENDOR. No row is seeded. SLA durations (ADR 0116 s3.2) are configuration with no default and
-- are NOT IMPLEMENTED here (S7). SAR export is slice S5 and NOT IMPLEMENTED: `closed_sar_filed` records an external filing
-- REFERENCE a human entered; this system files nothing.
--
-- What it adds, and only this (ADR 0116 s3.1, s3.2, s5.4, s8 S3):
--   0. Pre-flight (AM098).
--   1. aml_alerts, aml_cases, aml_case_alerts, aml_case_events, aml_case_notes: ENABLE + FORCE ROW LEVEL SECURITY, NO privilege
--      and NO policy for igaming_runtime, an OWNER-only policy bound to the session tenant, all FKs ON DELETE RESTRICT (C9),
--      append-only (UPDATE / DELETE / TRUNCATE refused, AM005, binding on the owner). Case STATE is a projection of the event
--      trail; the case row is immutable identity.
--   2. The case state machine, validated by a BEFORE INSERT trigger against the case's last event under a case-row lock:
--      open -> in_review -> escalated -> sar_decision_pending -> {closed_no_action | closed_sar_not_filed | closed_sar_filed};
--      in_review -> closed_no_action (never for a SAR-flagged case); reopen closed_* -> in_review; sar_flagged is
--      irreversible. A per-case gap-free `seq` is the CAS (UNIQUE (case_id, seq)).
--   3. B2 proof-bound four-eyes in the ADR 0110 family: a SAR closure (closed_sar_filed / closed_sar_not_filed) and a
--      confirmed-sanctions hold lift are a PROPOSAL event and a separate APPROVAL event, each by its own signed proof. The
--      approval binds to the proposal's seq AND its SHA-256 content hash, must be the event IMMEDIATELY after the proposal
--      (any intervening event, and any hold change for the subject since the proposal, invalidates it: AM012), the approver
--      must differ from the proposer by actor id AND by Person, and neither Person may be the case subject (AM011).
--   4. The definer officer API. Every function calls aml_authorize FIRST (0140): actor from the session, SQL re-check of the
--      in-force aml_officer grant, signed proof bound to the exact arguments. A row outside the actor's scope is reported as
--      not found (AM040), identical to a missing row.
--   5. aml_hold_lifts.approval_event_id gains its FK and the lift guard (0141) is replaced: a confirmed_sanctions lift
--      needs an approval event of kind lift_hold for THAT hold (the 0141 body is restored verbatim by the down).
--   6. aml_person_has_records(): the erasure check of ADR 0116 s5.4 C9 (session-tenant bound). The erasure / pseudonymisation
--      job itself (HD-RET-1) is NOT IMPLEMENTED; the RESTRICT FKs already refuse a hard delete.
--
-- SQLSTATE class AM: AM001 AM002 AM004 AM005 AM006 AM010 AM011 AM012 AM013 AM026 AM040 AM098 AM099 (see 0140).

SET LOCAL lock_timeout = '5s';

-- =========================================================================
-- 0. Pre-flight
-- =========================================================================
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_class WHERE relnamespace = 'public'::regnamespace
                AND relname IN ('aml_alerts', 'aml_cases', 'aml_case_alerts', 'aml_case_events', 'aml_case_notes')) THEN
        RAISE EXCEPTION 'migration 0142 pre-flight: an AML case table already exists' USING ERRCODE = 'AM098';
    END IF;
    IF to_regprocedure('public.aml_hold_lifts_guard()') IS NULL OR to_regclass('public.aml_holds') IS NULL THEN
        RAISE EXCEPTION 'migration 0142 pre-flight: migration 0141 is not applied' USING ERRCODE = 'AM098';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_proc WHERE pronamespace = 'public'::regnamespace
                AND proname IN ('aml_case_open', 'aml_case_propose', 'aml_case_approve', 'aml_person_has_records')) THEN
        RAISE EXCEPTION 'migration 0142 pre-flight: an AML case function already exists' USING ERRCODE = 'AM098';
    END IF;
END
$$;

-- =========================================================================
-- 1. Tables
-- =========================================================================
CREATE TABLE aml_alerts (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    owner_scope          TEXT NOT NULL CHECK (owner_scope IN ('platform', 'tenant')),
    person_id            UUID NOT NULL REFERENCES persons (id) ON DELETE RESTRICT,
    -- Slice S3 ships the officer-raised alert only; S4 (monitoring) and S2 (screening) extend this CHECK.
    source               TEXT NOT NULL CHECK (source IN ('manual')),
    severity             TEXT NOT NULL CHECK (severity IN ('low', 'medium', 'high')),
    reason_code          TEXT NOT NULL CHECK (reason_code ~ '^[a-z0-9_]{1,64}$'),
    created_by           UUID NOT NULL,
    created_by_person_id UUID NOT NULL,
    created_by_scope     TEXT NOT NULL CHECK (created_by_scope IN ('tenant', 'platform_acting')),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, id)
);

CREATE TABLE aml_cases (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    owner_scope          TEXT NOT NULL CHECK (owner_scope IN ('platform', 'tenant')),
    person_id            UUID NOT NULL REFERENCES persons (id) ON DELETE RESTRICT,
    case_kind            TEXT NOT NULL CHECK (case_kind IN ('manual', 'alert')),
    opened_by            UUID NOT NULL,
    opened_by_person_id  UUID NOT NULL,
    opened_by_scope      TEXT NOT NULL CHECK (opened_by_scope IN ('tenant', 'platform_acting')),
    opened_at            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, id)
);
CREATE INDEX aml_cases_person ON aml_cases (tenant_id, person_id);
CREATE INDEX aml_cases_queue ON aml_cases (tenant_id, opened_at DESC);

CREATE TABLE aml_case_alerts (
    case_id    UUID NOT NULL,
    alert_id   UUID NOT NULL,
    tenant_id  UUID NOT NULL,
    linked_at  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (case_id, alert_id),
    FOREIGN KEY (tenant_id, case_id) REFERENCES aml_cases (tenant_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, alert_id) REFERENCES aml_alerts (tenant_id, id) ON DELETE RESTRICT
);

CREATE TABLE aml_case_notes (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    owner_scope          TEXT NOT NULL CHECK (owner_scope IN ('platform', 'tenant')),
    case_id              UUID NOT NULL,
    body                 TEXT NOT NULL CHECK (octet_length(body) BETWEEN 1 AND 4000 AND body !~ '[\x01-\x08\x0B\x0C\x0E-\x1F\x7F]'),
    -- The lint version recorded by the server-side lint that approved the body (aml.LintNote). The proof bound to the
    -- insert covers the body and this version, and is signed only after the lint ran.
    lint_version         TEXT NOT NULL CHECK (lint_version ~ '^[a-z0-9_.-]{1,32}$'),
    created_by           UUID NOT NULL,
    created_by_person_id UUID NOT NULL,
    created_by_scope     TEXT NOT NULL CHECK (created_by_scope IN ('tenant', 'platform_acting')),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, case_id) REFERENCES aml_cases (tenant_id, id) ON DELETE RESTRICT
);

CREATE TABLE aml_case_events (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    owner_scope               TEXT NOT NULL CHECK (owner_scope IN ('platform', 'tenant')),
    case_id                   UUID NOT NULL,
    seq                       INTEGER NOT NULL CHECK (seq >= 1),
    event_type                TEXT NOT NULL CHECK (event_type IN ('opened', 'status_changed', 'note_added', 'sar_flagged', 'proposal', 'approval')),
    from_status               TEXT NULL CHECK (from_status IN ('open', 'in_review', 'escalated', 'sar_decision_pending', 'closed_no_action', 'closed_sar_not_filed', 'closed_sar_filed')),
    to_status                 TEXT NULL CHECK (to_status IN ('open', 'in_review', 'escalated', 'sar_decision_pending', 'closed_no_action', 'closed_sar_not_filed', 'closed_sar_filed')),
    proposal_kind             TEXT NULL CHECK (proposal_kind IN ('close_sar_not_filed', 'close_sar_filed', 'lift_hold')),
    proposal_seq              INTEGER NULL CHECK (proposal_seq >= 1),
    content_hash              TEXT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    subject_hold_id           UUID NULL,
    note_id                   UUID NULL,
    external_filing_reference TEXT NULL CHECK (external_filing_reference ~ '^[A-Za-z0-9._/-]{1,64}$'),
    reason_code               TEXT NOT NULL CHECK (reason_code ~ '^[a-z0-9_]{1,64}$'),
    actor_id                  UUID NOT NULL,
    actor_person_id           UUID NOT NULL,
    actor_scope               TEXT NOT NULL CHECK (actor_scope IN ('tenant', 'platform_acting')),
    ip_address                INET NULL,
    request_id                TEXT NULL CHECK (request_id ~ '^[A-Za-z0-9_.:-]{1,128}$'),
    created_at                TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (case_id, seq),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, case_id) REFERENCES aml_cases (tenant_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, subject_hold_id) REFERENCES aml_holds (tenant_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, note_id) REFERENCES aml_case_notes (tenant_id, id) ON DELETE RESTRICT,
    -- Event shape, per type: every field of the type explicit, every other field absent.
    CHECK (event_type <> 'opened' OR (seq = 1 AND from_status IS NULL AND to_status = 'open' AND proposal_kind IS NULL AND proposal_seq IS NULL
                                      AND content_hash IS NULL AND subject_hold_id IS NULL AND note_id IS NULL AND external_filing_reference IS NULL)),
    CHECK (event_type <> 'status_changed' OR (seq > 1 AND from_status IS NOT NULL AND to_status IS NOT NULL AND from_status <> to_status
                                              AND proposal_kind IS NULL AND proposal_seq IS NULL AND content_hash IS NULL
                                              AND subject_hold_id IS NULL AND note_id IS NULL AND external_filing_reference IS NULL)),
    CHECK (event_type <> 'note_added' OR (seq > 1 AND note_id IS NOT NULL AND from_status IS NULL AND to_status IS NULL AND proposal_kind IS NULL
                                          AND proposal_seq IS NULL AND content_hash IS NULL AND subject_hold_id IS NULL AND external_filing_reference IS NULL)),
    CHECK (event_type <> 'sar_flagged' OR (seq > 1 AND from_status IS NULL AND to_status IS NULL AND proposal_kind IS NULL AND proposal_seq IS NULL
                                           AND content_hash IS NULL AND subject_hold_id IS NULL AND note_id IS NULL AND external_filing_reference IS NULL)),
    CHECK (event_type <> 'proposal' OR (seq > 1 AND proposal_kind IS NOT NULL AND content_hash IS NOT NULL AND proposal_seq IS NULL
                                        AND from_status IS NULL AND to_status IS NULL AND note_id IS NULL
                                        AND (proposal_kind = 'lift_hold') = (subject_hold_id IS NOT NULL)
                                        AND (proposal_kind = 'close_sar_filed') = (external_filing_reference IS NOT NULL))),
    CHECK (event_type <> 'approval' OR (seq > 2 AND proposal_kind IS NOT NULL AND proposal_seq = seq - 1 AND content_hash IS NOT NULL AND note_id IS NULL
                                        AND (proposal_kind = 'lift_hold') = (subject_hold_id IS NOT NULL)
                                        AND (proposal_kind = 'close_sar_filed') = (external_filing_reference IS NOT NULL)
                                        AND ((proposal_kind = 'lift_hold') = (from_status IS NULL AND to_status IS NULL))
                                        AND (proposal_kind = 'lift_hold' OR (from_status = 'sar_decision_pending'
                                             AND to_status = CASE proposal_kind WHEN 'close_sar_filed' THEN 'closed_sar_filed' ELSE 'closed_sar_not_filed' END))))
);
CREATE INDEX aml_case_events_case ON aml_case_events (case_id, seq);

-- RLS: enabled and FORCED; no privilege and no policy for the runtime role; an owner-only policy bound to the session tenant.
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['aml_alerts', 'aml_cases', 'aml_case_alerts', 'aml_case_notes', 'aml_case_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('REVOKE ALL ON %I FROM PUBLIC', t);
        EXECUTE format($f$CREATE POLICY aml_owner_session ON %I FOR ALL TO CURRENT_USER
                          USING (tenant_id = public.aml_session_tenant()) WITH CHECK (tenant_id = public.aml_session_tenant())$f$, t);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
            EXECUTE format('REVOKE ALL ON %I FROM igaming_runtime', t);
        END IF;
        EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION aml_deny_mutation()', t || '_append_only', t);
        EXECUTE format('CREATE TRIGGER %I BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION aml_deny_mutation()', t || '_no_truncate', t);
    END LOOP;
END
$$;

-- =========================================================================
-- 2. Projection and state-machine helpers (invoker; they read under the caller's session)
-- =========================================================================
CREATE FUNCTION aml_case_status(p_case uuid) RETURNS text
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT e.to_status FROM public.aml_case_events e WHERE e.case_id = p_case AND e.to_status IS NOT NULL ORDER BY e.seq DESC LIMIT 1
$$;

CREATE FUNCTION aml_case_last_seq(p_case uuid) RETURNS integer
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT COALESCE(max(e.seq), 0) FROM public.aml_case_events e WHERE e.case_id = p_case
$$;

CREATE FUNCTION aml_case_sar_flagged(p_case uuid) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT EXISTS (SELECT 1 FROM public.aml_case_events e WHERE e.case_id = p_case AND e.event_type = 'sar_flagged')
$$;

-- A plain (non-SAR-closure) transition. closed_sar_* are reachable only through an approval event.
CREATE FUNCTION aml_case_transition_allowed(p_from text, p_to text) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT (p_from, p_to) IN (
        ('open', 'in_review'), ('in_review', 'escalated'), ('in_review', 'closed_no_action'), ('escalated', 'sar_decision_pending'),
        ('closed_no_action', 'in_review'), ('closed_sar_not_filed', 'in_review'), ('closed_sar_filed', 'in_review'))
$$;

-- The proposal's content hash (ADR 0116 s3.2): what an approver reviews and binds to. Recomputed by the database from the
-- inserted proposal row, so a caller cannot store a hash that does not match the content.
CREATE FUNCTION aml_proposal_content_hash(
    p_tenant uuid, p_case uuid, p_seq integer, p_kind text, p_hold uuid, p_ref text, p_reason text, p_actor uuid
) RETURNS text
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_case::text, p_seq::text, p_kind, p_hold::text, p_ref, p_reason, p_actor::text))
$$;

-- =========================================================================
-- 3. Guards
-- =========================================================================
CREATE FUNCTION aml_alerts_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor record;
BEGIN
    IF NEW.owner_scope IS DISTINCT FROM public.aml_owner_scope_of_tenant(NEW.tenant_id) THEN
        RAISE EXCEPTION 'aml: the alert owner scope must be the tenant''s licensing owner scope' USING ERRCODE = 'AM002';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.player_accounts pa WHERE pa.tenant_id = NEW.tenant_id AND pa.person_id = NEW.person_id) THEN
        RAISE EXCEPTION 'aml: the person has no account in this tenant' USING ERRCODE = 'AM007';
    END IF;
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.created_by IS DISTINCT FROM v_actor.actor OR NEW.created_by_scope IS DISTINCT FROM v_actor.scope
       OR NEW.created_by_person_id IS DISTINCT FROM v_actor.person_id THEN
        RAISE EXCEPTION 'aml: the actor columns are not the proven session actor' USING ERRCODE = 'AM001';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER aml_alerts_10_guard BEFORE INSERT ON aml_alerts FOR EACH ROW EXECUTE FUNCTION aml_alerts_guard();

CREATE FUNCTION aml_cases_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor record;
BEGIN
    IF NEW.owner_scope IS DISTINCT FROM public.aml_owner_scope_of_tenant(NEW.tenant_id) THEN
        RAISE EXCEPTION 'aml: the case owner scope must be the tenant''s licensing owner scope' USING ERRCODE = 'AM002';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.player_accounts pa WHERE pa.tenant_id = NEW.tenant_id AND pa.person_id = NEW.person_id) THEN
        RAISE EXCEPTION 'aml: the person has no account in this tenant' USING ERRCODE = 'AM007';
    END IF;
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.opened_by IS DISTINCT FROM v_actor.actor OR NEW.opened_by_scope IS DISTINCT FROM v_actor.scope
       OR NEW.opened_by_person_id IS DISTINCT FROM v_actor.person_id THEN
        RAISE EXCEPTION 'aml: the actor columns are not the proven session actor' USING ERRCODE = 'AM001';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER aml_cases_10_guard BEFORE INSERT ON aml_cases FOR EACH ROW EXECUTE FUNCTION aml_cases_guard();

CREATE FUNCTION aml_case_alerts_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_case  record;
    v_alert record;
BEGIN
    SELECT c.person_id, c.owner_scope INTO v_case FROM public.aml_cases c WHERE c.id = NEW.case_id AND c.tenant_id = NEW.tenant_id;
    SELECT a.person_id, a.owner_scope INTO v_alert FROM public.aml_alerts a WHERE a.id = NEW.alert_id AND a.tenant_id = NEW.tenant_id;
    IF v_case.person_id IS NULL OR v_alert.person_id IS NULL OR v_case.person_id <> v_alert.person_id OR v_case.owner_scope <> v_alert.owner_scope THEN
        RAISE EXCEPTION 'aml: an alert can only be linked to a case of the same person and owner scope' USING ERRCODE = 'AM004';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER aml_case_alerts_10_guard BEFORE INSERT ON aml_case_alerts FOR EACH ROW EXECUTE FUNCTION aml_case_alerts_guard();

CREATE FUNCTION aml_case_notes_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_case  record;
    v_actor record;
BEGIN
    SELECT c.owner_scope INTO v_case FROM public.aml_cases c WHERE c.id = NEW.case_id AND c.tenant_id = NEW.tenant_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'aml: not found' USING ERRCODE = 'AM040';
    END IF;
    IF NEW.owner_scope <> v_case.owner_scope THEN
        RAISE EXCEPTION 'aml: the note owner scope must be the case''s' USING ERRCODE = 'AM002';
    END IF;
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.created_by IS DISTINCT FROM v_actor.actor OR NEW.created_by_scope IS DISTINCT FROM v_actor.scope
       OR NEW.created_by_person_id IS DISTINCT FROM v_actor.person_id THEN
        RAISE EXCEPTION 'aml: the actor columns are not the proven session actor' USING ERRCODE = 'AM001';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER aml_case_notes_10_guard BEFORE INSERT ON aml_case_notes FOR EACH ROW EXECUTE FUNCTION aml_case_notes_guard();

-- The case event guard: the state machine and the B2 four-eyes rules. See the header.
CREATE FUNCTION aml_case_events_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_case   record;
    v_actor  record;
    v_prop   record;
    v_status text;
    v_last   integer;
    v_flag   boolean;
    v_hold   record;
BEGIN
    SELECT c.owner_scope, c.person_id INTO v_case FROM public.aml_cases c WHERE c.id = NEW.case_id AND c.tenant_id = NEW.tenant_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'aml: not found' USING ERRCODE = 'AM040';
    END IF;
    IF NEW.owner_scope <> v_case.owner_scope THEN
        RAISE EXCEPTION 'aml: the event owner scope must be the case''s' USING ERRCODE = 'AM002';
    END IF;
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.actor_id IS DISTINCT FROM v_actor.actor OR NEW.actor_scope IS DISTINCT FROM v_actor.scope
       OR NEW.actor_person_id IS DISTINCT FROM v_actor.person_id THEN
        RAISE EXCEPTION 'aml: the actor columns are not the proven session actor' USING ERRCODE = 'AM001';
    END IF;
    -- Request metadata for the audit trail (ADR 0116 s3.1: actor, tenant, entity, from/to, IP, reason). The two settings are set
    -- by the officer service per transaction; they carry NO authority (they are evidence only) and an unusable value is NULL.
    BEGIN
        IF pg_catalog.current_setting('app.aml_ip', true) ~ '^[0-9a-fA-F:.]{2,45}$' THEN
            NEW.ip_address := pg_catalog.current_setting('app.aml_ip', true)::inet;
        END IF;
    EXCEPTION WHEN OTHERS THEN
        NEW.ip_address := NULL;
    END;
    IF pg_catalog.current_setting('app.aml_request_id', true) ~ '^[A-Za-z0-9_.:-]{1,128}$' THEN
        NEW.request_id := pg_catalog.current_setting('app.aml_request_id', true);
    END IF;
    v_last := public.aml_case_last_seq(NEW.case_id);
    IF NEW.seq <> v_last + 1 THEN
        RAISE EXCEPTION 'aml: the event sequence is not the next one for this case' USING ERRCODE = 'AM012';
    END IF;
    v_status := public.aml_case_status(NEW.case_id);
    v_flag := public.aml_case_sar_flagged(NEW.case_id);

    IF NEW.event_type = 'opened' THEN
        IF v_last <> 0 THEN
            RAISE EXCEPTION 'aml: a case is opened exactly once' USING ERRCODE = 'AM010';
        END IF;
        RETURN NEW;
    END IF;
    IF v_status IS NULL THEN
        RAISE EXCEPTION 'aml: the case has no opening event' USING ERRCODE = 'AM010';
    END IF;

    IF NEW.event_type = 'status_changed' THEN
        IF NEW.from_status IS DISTINCT FROM v_status THEN
            RAISE EXCEPTION 'aml: the transition does not start from the case''s current status' USING ERRCODE = 'AM010';
        END IF;
        IF NEW.to_status IN ('closed_sar_not_filed', 'closed_sar_filed') THEN
            RAISE EXCEPTION 'aml: a SAR closure needs a proof-bound four-eyes approval' USING ERRCODE = 'AM011';
        END IF;
        IF NOT public.aml_case_transition_allowed(NEW.from_status, NEW.to_status) THEN
            RAISE EXCEPTION 'aml: illegal case transition % -> %', NEW.from_status, NEW.to_status USING ERRCODE = 'AM010';
        END IF;
        IF NEW.to_status = 'closed_no_action' AND v_flag THEN
            RAISE EXCEPTION 'aml: a SAR-flagged case cannot close as no action' USING ERRCODE = 'AM010';
        END IF;
    ELSIF NEW.event_type = 'sar_flagged' THEN
        IF v_flag THEN
            RAISE EXCEPTION 'aml: the case is already SAR-flagged (irreversible)' USING ERRCODE = 'AM010';
        END IF;
        IF v_status NOT IN ('in_review', 'escalated', 'sar_decision_pending') THEN
            RAISE EXCEPTION 'aml: a case can be SAR-flagged only while it is being worked' USING ERRCODE = 'AM010';
        END IF;
    ELSIF NEW.event_type = 'proposal' THEN
        IF v_status NOT IN ('in_review', 'escalated', 'sar_decision_pending') THEN
            RAISE EXCEPTION 'aml: a proposal needs a case that is being worked' USING ERRCODE = 'AM010';
        END IF;
        IF NEW.proposal_kind IN ('close_sar_filed', 'close_sar_not_filed') AND v_status <> 'sar_decision_pending' THEN
            RAISE EXCEPTION 'aml: a SAR closure is proposed from sar_decision_pending only' USING ERRCODE = 'AM010';
        END IF;
        IF NEW.actor_person_id = v_case.person_id THEN
            RAISE EXCEPTION 'aml: the case subject may not propose on their own case' USING ERRCODE = 'AM011';
        END IF;
        IF NEW.content_hash IS DISTINCT FROM public.aml_proposal_content_hash(NEW.tenant_id, NEW.case_id, NEW.seq, NEW.proposal_kind,
               NEW.subject_hold_id, NEW.external_filing_reference, NEW.reason_code, NEW.actor_id) THEN
            RAISE EXCEPTION 'aml: the proposal content hash does not match its content' USING ERRCODE = 'AM011';
        END IF;
        IF NEW.proposal_kind = 'lift_hold' THEN
            SELECT h.person_id, h.owner_scope INTO v_hold FROM public.aml_holds h WHERE h.id = NEW.subject_hold_id AND h.tenant_id = NEW.tenant_id;
            IF NOT FOUND OR v_hold.person_id <> v_case.person_id THEN
                RAISE EXCEPTION 'aml: not found' USING ERRCODE = 'AM040';
            END IF;
            IF EXISTS (SELECT 1 FROM public.aml_hold_lifts l WHERE l.hold_id = NEW.subject_hold_id) THEN
                RAISE EXCEPTION 'aml: the hold is already lifted' USING ERRCODE = 'AM006';
            END IF;
        END IF;
    ELSIF NEW.event_type = 'approval' THEN
        SELECT p.* INTO v_prop FROM public.aml_case_events p
         WHERE p.case_id = NEW.case_id AND p.seq = NEW.proposal_seq AND p.event_type = 'proposal';
        -- The approval must be the event IMMEDIATELY after its proposal (the table CHECK pins proposal_seq = seq - 1):
        -- any intervening event (a reopen, a note, a flag, a newer proposal) means there is no such proposal here.
        IF NOT FOUND THEN
            RAISE EXCEPTION 'aml: no pending proposal at that sequence (it may have been invalidated by a later event)' USING ERRCODE = 'AM012';
        END IF;
        IF v_prop.proposal_kind <> NEW.proposal_kind OR v_prop.content_hash <> NEW.content_hash
           OR v_prop.subject_hold_id IS DISTINCT FROM NEW.subject_hold_id
           OR v_prop.external_filing_reference IS DISTINCT FROM NEW.external_filing_reference THEN
            RAISE EXCEPTION 'aml: the approval does not bind the proposal''s content' USING ERRCODE = 'AM012';
        END IF;
        IF NEW.actor_id = v_prop.actor_id THEN
            RAISE EXCEPTION 'aml: the approver may not be the proposer' USING ERRCODE = 'AM011';
        END IF;
        IF NEW.actor_person_id = v_prop.actor_person_id THEN
            RAISE EXCEPTION 'aml: the approver may not be the same Person as the proposer' USING ERRCODE = 'AM011';
        END IF;
        IF NEW.actor_person_id = v_case.person_id THEN
            RAISE EXCEPTION 'aml: the case subject may not approve on their own case' USING ERRCODE = 'AM011';
        END IF;
        IF EXISTS (SELECT 1 FROM public.aml_holds h WHERE h.tenant_id = NEW.tenant_id AND h.person_id = v_case.person_id AND h.placed_at > v_prop.created_at)
           OR EXISTS (SELECT 1 FROM public.aml_hold_lifts l JOIN public.aml_holds h ON h.id = l.hold_id
                       WHERE h.tenant_id = NEW.tenant_id AND h.person_id = v_case.person_id AND l.lifted_at > v_prop.created_at) THEN
            RAISE EXCEPTION 'aml: a hold changed since the proposal; the proposal is invalid' USING ERRCODE = 'AM012';
        END IF;
        IF NEW.proposal_kind = 'lift_hold' THEN
            IF EXISTS (SELECT 1 FROM public.aml_hold_lifts l WHERE l.hold_id = NEW.subject_hold_id) THEN
                RAISE EXCEPTION 'aml: the hold is already lifted' USING ERRCODE = 'AM006';
            END IF;
        ELSIF NEW.from_status IS DISTINCT FROM v_status THEN
            RAISE EXCEPTION 'aml: the approval does not start from the case''s current status' USING ERRCODE = 'AM010';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER aml_case_events_10_guard BEFORE INSERT ON aml_case_events FOR EACH ROW EXECUTE FUNCTION aml_case_events_guard();

-- =========================================================================
-- 4. The hold lift guard (replaces 0141's body; the down restores it verbatim) and its FK
-- =========================================================================
ALTER TABLE aml_hold_lifts ADD CONSTRAINT aml_hold_lifts_approval_event_fkey
    FOREIGN KEY (tenant_id, approval_event_id) REFERENCES aml_case_events (tenant_id, id) ON DELETE RESTRICT;

CREATE OR REPLACE FUNCTION aml_hold_lifts_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_hold  record;
    v_actor record;
    v_ev    record;
BEGIN
    SELECT h.tenant_id, h.owner_scope, h.reason_class, h.placed_at INTO v_hold FROM public.aml_holds h WHERE h.id = NEW.hold_id;
    IF NOT FOUND OR v_hold.tenant_id <> NEW.tenant_id THEN
        RAISE EXCEPTION 'aml: not found' USING ERRCODE = 'AM040';
    END IF;
    IF NEW.owner_scope <> v_hold.owner_scope THEN
        RAISE EXCEPTION 'aml: only the hold''s owner scope may lift it' USING ERRCODE = 'AM006';
    END IF;
    IF v_hold.reason_class = 'confirmed_sanctions' AND NEW.approval_event_id IS NULL THEN
        RAISE EXCEPTION 'aml: lifting a confirmed sanctions hold needs a proof-bound four-eyes approval' USING ERRCODE = 'AM006';
    END IF;
    IF v_hold.reason_class <> 'confirmed_sanctions' AND NEW.approval_event_id IS NOT NULL THEN
        RAISE EXCEPTION 'aml: an approval event is only valid for a confirmed sanctions lift' USING ERRCODE = 'AM006';
    END IF;
    IF NEW.approval_event_id IS NOT NULL THEN
        SELECT e.event_type, e.proposal_kind, e.subject_hold_id, e.tenant_id, e.actor_id INTO v_ev
          FROM public.aml_case_events e WHERE e.id = NEW.approval_event_id;
        IF NOT FOUND OR v_ev.event_type <> 'approval' OR v_ev.proposal_kind <> 'lift_hold'
           OR v_ev.subject_hold_id IS DISTINCT FROM NEW.hold_id OR v_ev.tenant_id <> NEW.tenant_id THEN
            RAISE EXCEPTION 'aml: the approval event does not authorise this hold lift' USING ERRCODE = 'AM006';
        END IF;
    END IF;
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.lifted_by IS DISTINCT FROM v_actor.actor OR NEW.lifted_by_scope IS DISTINCT FROM v_actor.scope
       OR NEW.lifted_by_person_id IS DISTINCT FROM v_actor.person_id THEN
        RAISE EXCEPTION 'aml: the lifting actor columns are not the proven session actor' USING ERRCODE = 'AM001';
    END IF;
    RETURN NEW;
END
$$;

-- =========================================================================
-- 5. The officer API (proof-bound)
-- =========================================================================

-- An alert raised by an officer (S3). Target `new`.
CREATE FUNCTION aml_alert_raise(p_tenant uuid, p_person uuid, p_severity text, p_reason_code text) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a       record;
    v_owner text;
    v_id    uuid;
BEGIN
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_alert:raise', 'new',
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_person::text, p_severity, p_reason_code)));
    v_owner := public.aml_owner_scope_of_tenant(p_tenant);
    IF public.aml_actor_owner_scope(a.o_scope) IS DISTINCT FROM v_owner THEN
        RAISE EXCEPTION 'aml: the actor does not own this tenant''s AML scope' USING ERRCODE = 'AM002';
    END IF;
    INSERT INTO public.aml_alerts (tenant_id, owner_scope, person_id, source, severity, reason_code, created_by, created_by_person_id, created_by_scope)
    VALUES (p_tenant, v_owner, p_person, 'manual', p_severity, p_reason_code, a.o_actor, a.o_person_id, a.o_scope)
    RETURNING id INTO v_id;
    RETURN v_id;
END
$$;

-- Opens a case on a person, optionally linking alerts of the same person. Target `new`.
CREATE FUNCTION aml_case_open(p_tenant uuid, p_person uuid, p_case_kind text, p_reason_code text, p_alerts uuid[]) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a       record;
    v_owner text;
    v_id    uuid;
    x       uuid;
BEGIN
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_case:open', 'new',
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_person::text, p_case_kind, p_reason_code,
            (SELECT string_agg(i::text, ',' ORDER BY i::text) FROM unnest(p_alerts) AS i))));
    v_owner := public.aml_owner_scope_of_tenant(p_tenant);
    IF public.aml_actor_owner_scope(a.o_scope) IS DISTINCT FROM v_owner THEN
        RAISE EXCEPTION 'aml: the actor does not own this tenant''s AML scope' USING ERRCODE = 'AM002';
    END IF;
    IF p_case_kind = 'alert' AND COALESCE(cardinality(p_alerts), 0) = 0 THEN
        RAISE EXCEPTION 'aml: an alert case names at least one alert' USING ERRCODE = 'AM004';
    END IF;
    IF p_case_kind = 'manual' AND COALESCE(cardinality(p_alerts), 0) > 0 THEN
        RAISE EXCEPTION 'aml: a manual case links no alert' USING ERRCODE = 'AM004';
    END IF;
    INSERT INTO public.aml_cases (tenant_id, owner_scope, person_id, case_kind, opened_by, opened_by_person_id, opened_by_scope)
    VALUES (p_tenant, v_owner, p_person, p_case_kind, a.o_actor, a.o_person_id, a.o_scope)
    RETURNING id INTO v_id;
    INSERT INTO public.aml_case_events (tenant_id, owner_scope, case_id, seq, event_type, to_status, reason_code, actor_id, actor_person_id, actor_scope)
    VALUES (p_tenant, v_owner, v_id, 1, 'opened', 'open', p_reason_code, a.o_actor, a.o_person_id, a.o_scope);
    FOREACH x IN ARRAY COALESCE(p_alerts, ARRAY[]::uuid[]) LOOP
        INSERT INTO public.aml_case_alerts (case_id, alert_id, tenant_id) VALUES (v_id, x, p_tenant);
    END LOOP;
    RETURN v_id;
END
$$;

-- Loads the case for a WRITE under a row lock, asserts the actor owns its scope and the expected sequence (CAS).
CREATE FUNCTION aml_case_lock_for_write(p_tenant uuid, p_case uuid, p_actor_scope text, p_expected_seq integer)
RETURNS TABLE (o_owner_scope text, o_person_id uuid)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_case record;
    v_last integer;
BEGIN
    SELECT c.owner_scope, c.person_id INTO v_case FROM public.aml_cases c WHERE c.id = p_case AND c.tenant_id = p_tenant FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'aml: not found' USING ERRCODE = 'AM040';
    END IF;
    PERFORM public.aml_assert_write_scope(p_actor_scope, v_case.owner_scope);
    v_last := public.aml_case_last_seq(p_case);
    IF p_expected_seq IS DISTINCT FROM v_last THEN
        RAISE EXCEPTION 'aml: the case moved on (expected sequence % but it is %)', p_expected_seq, v_last USING ERRCODE = 'AM012';
    END IF;
    o_owner_scope := v_case.owner_scope;
    o_person_id := v_case.person_id;
    RETURN NEXT;
END
$$;

CREATE FUNCTION aml_case_transition(p_tenant uuid, p_case uuid, p_expected_seq integer, p_to_status text, p_reason_code text) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a      record;
    v_case record;
    v_from text;
BEGIN
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_case:transition', p_case::text,
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_case::text, p_expected_seq::text, p_to_status, p_reason_code)));
    SELECT * INTO v_case FROM public.aml_case_lock_for_write(p_tenant, p_case, a.o_scope, p_expected_seq);
    v_from := public.aml_case_status(p_case);
    INSERT INTO public.aml_case_events (tenant_id, owner_scope, case_id, seq, event_type, from_status, to_status, reason_code, actor_id, actor_person_id, actor_scope)
    VALUES (p_tenant, v_case.o_owner_scope, p_case, p_expected_seq + 1, 'status_changed', v_from, p_to_status, p_reason_code, a.o_actor, a.o_person_id, a.o_scope);
    RETURN p_expected_seq + 1;
END
$$;

CREATE FUNCTION aml_case_add_note(p_tenant uuid, p_case uuid, p_expected_seq integer, p_body text, p_lint_version text) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a      record;
    v_case record;
    v_note uuid;
BEGIN
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_case:note', p_case::text,
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_case::text, p_expected_seq::text, p_body, p_lint_version)));
    SELECT * INTO v_case FROM public.aml_case_lock_for_write(p_tenant, p_case, a.o_scope, p_expected_seq);
    INSERT INTO public.aml_case_notes (tenant_id, owner_scope, case_id, body, lint_version, created_by, created_by_person_id, created_by_scope)
    VALUES (p_tenant, v_case.o_owner_scope, p_case, p_body, p_lint_version, a.o_actor, a.o_person_id, a.o_scope)
    RETURNING id INTO v_note;
    INSERT INTO public.aml_case_events (tenant_id, owner_scope, case_id, seq, event_type, note_id, reason_code, actor_id, actor_person_id, actor_scope)
    VALUES (p_tenant, v_case.o_owner_scope, p_case, p_expected_seq + 1, 'note_added', v_note, 'note_added', a.o_actor, a.o_person_id, a.o_scope);
    RETURN p_expected_seq + 1;
END
$$;

CREATE FUNCTION aml_case_flag_sar(p_tenant uuid, p_case uuid, p_expected_seq integer, p_reason_code text) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a      record;
    v_case record;
BEGIN
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_case:flag_sar', p_case::text,
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_case::text, p_expected_seq::text, p_reason_code)));
    SELECT * INTO v_case FROM public.aml_case_lock_for_write(p_tenant, p_case, a.o_scope, p_expected_seq);
    INSERT INTO public.aml_case_events (tenant_id, owner_scope, case_id, seq, event_type, reason_code, actor_id, actor_person_id, actor_scope)
    VALUES (p_tenant, v_case.o_owner_scope, p_case, p_expected_seq + 1, 'sar_flagged', p_reason_code, a.o_actor, a.o_person_id, a.o_scope);
    RETURN p_expected_seq + 1;
END
$$;

-- The proposal half of a four-eyes action. Returns the event seq and the content hash the approver must bind.
CREATE FUNCTION aml_case_propose(p_tenant uuid, p_case uuid, p_expected_seq integer, p_kind text, p_hold uuid, p_external_ref text, p_reason_code text)
RETURNS TABLE (o_seq integer, o_content_hash text)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a      record;
    v_case record;
    v_hash text;
BEGIN
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_case:propose', p_case::text,
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_case::text, p_expected_seq::text, p_kind, p_hold::text, p_external_ref, p_reason_code)));
    SELECT * INTO v_case FROM public.aml_case_lock_for_write(p_tenant, p_case, a.o_scope, p_expected_seq);
    v_hash := public.aml_proposal_content_hash(p_tenant, p_case, p_expected_seq + 1, p_kind, p_hold, p_external_ref, p_reason_code, a.o_actor);
    INSERT INTO public.aml_case_events (tenant_id, owner_scope, case_id, seq, event_type, proposal_kind, content_hash, subject_hold_id,
                                        external_filing_reference, reason_code, actor_id, actor_person_id, actor_scope)
    VALUES (p_tenant, v_case.o_owner_scope, p_case, p_expected_seq + 1, 'proposal', p_kind, v_hash, p_hold, p_external_ref, p_reason_code,
            a.o_actor, a.o_person_id, a.o_scope);
    o_seq := p_expected_seq + 1;
    o_content_hash := v_hash;
    RETURN NEXT;
END
$$;

-- The approval half. p_proposal_seq must be the LAST event of the case; p_content_hash is the hash the approver reviewed.
-- A close kind moves the case to closed_sar_*; lift_hold lifts the hold in this same transaction.
CREATE FUNCTION aml_case_approve(p_tenant uuid, p_case uuid, p_proposal_seq integer, p_content_hash text, p_reason_code text) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a       record;
    v_case  record;
    v_prop  record;
    v_event uuid;
    v_to    text;
BEGIN
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_case:approve', p_case::text,
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_case::text, p_proposal_seq::text, p_content_hash, p_reason_code)));
    SELECT * INTO v_case FROM public.aml_case_lock_for_write(p_tenant, p_case, a.o_scope, p_proposal_seq);
    SELECT p.proposal_kind, p.subject_hold_id, p.external_filing_reference INTO v_prop
      FROM public.aml_case_events p WHERE p.case_id = p_case AND p.seq = p_proposal_seq AND p.event_type = 'proposal';
    IF NOT FOUND THEN
        RAISE EXCEPTION 'aml: no pending proposal at that sequence' USING ERRCODE = 'AM012';
    END IF;
    v_to := CASE v_prop.proposal_kind WHEN 'close_sar_filed' THEN 'closed_sar_filed' WHEN 'close_sar_not_filed' THEN 'closed_sar_not_filed' ELSE NULL END;
    INSERT INTO public.aml_case_events (tenant_id, owner_scope, case_id, seq, event_type, from_status, to_status, proposal_kind, proposal_seq,
                                        content_hash, subject_hold_id, external_filing_reference, reason_code, actor_id, actor_person_id, actor_scope)
    VALUES (p_tenant, v_case.o_owner_scope, p_case, p_proposal_seq + 1, 'approval',
            CASE WHEN v_to IS NULL THEN NULL ELSE 'sar_decision_pending' END, v_to,
            v_prop.proposal_kind, p_proposal_seq, p_content_hash, v_prop.subject_hold_id, v_prop.external_filing_reference, p_reason_code,
            a.o_actor, a.o_person_id, a.o_scope)
    RETURNING id INTO v_event;
    IF v_prop.proposal_kind = 'lift_hold' THEN
        INSERT INTO public.aml_hold_lifts (hold_id, tenant_id, owner_scope, lifted_by, lifted_by_person_id, lifted_by_scope, reason_code, approval_event_id)
        VALUES (v_prop.subject_hold_id, p_tenant, v_case.o_owner_scope, a.o_actor, a.o_person_id, a.o_scope, p_reason_code, v_event);
    END IF;
    RETURN p_proposal_seq + 1;
END
$$;

-- ---- reads (proof-bound; target = the case id) ----
CREATE FUNCTION aml_case_read(p_tenant uuid, p_case uuid)
RETURNS TABLE (o_case_id uuid, o_owner_scope text, o_person_id uuid, o_case_kind text, o_status text, o_sar_flagged boolean, o_last_seq integer, o_opened_at timestamptz)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a record;
    c record;
BEGIN
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_case:read', p_case::text,
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_case::text, 'case')));
    SELECT x.id, x.owner_scope, x.person_id, x.case_kind, x.opened_at INTO c FROM public.aml_cases x WHERE x.id = p_case AND x.tenant_id = p_tenant;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'aml: not found' USING ERRCODE = 'AM040';
    END IF;
    PERFORM public.aml_assert_row_scope(a.o_scope, c.owner_scope);
    o_case_id := c.id; o_owner_scope := c.owner_scope; o_person_id := c.person_id; o_case_kind := c.case_kind;
    o_status := public.aml_case_status(p_case); o_sar_flagged := public.aml_case_sar_flagged(p_case);
    o_last_seq := public.aml_case_last_seq(p_case); o_opened_at := c.opened_at;
    RETURN NEXT;
END
$$;

CREATE FUNCTION aml_case_events_list(p_tenant uuid, p_case uuid)
RETURNS TABLE (o_seq integer, o_event_type text, o_from_status text, o_to_status text, o_proposal_kind text, o_proposal_seq integer,
               o_content_hash text, o_subject_hold_id uuid, o_note_id uuid, o_external_filing_reference text, o_reason_code text,
               o_actor_id uuid, o_actor_scope text, o_created_at timestamptz)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a record;
    c record;
BEGIN
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_case:read', p_case::text,
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_case::text, 'events')));
    SELECT x.owner_scope INTO c FROM public.aml_cases x WHERE x.id = p_case AND x.tenant_id = p_tenant;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'aml: not found' USING ERRCODE = 'AM040';
    END IF;
    PERFORM public.aml_assert_row_scope(a.o_scope, c.owner_scope);
    RETURN QUERY
        SELECT e.seq, e.event_type, e.from_status, e.to_status, e.proposal_kind, e.proposal_seq, e.content_hash, e.subject_hold_id, e.note_id,
               e.external_filing_reference, e.reason_code, e.actor_id, e.actor_scope, e.created_at
          FROM public.aml_case_events e WHERE e.case_id = p_case ORDER BY e.seq;
END
$$;

CREATE FUNCTION aml_case_notes_list(p_tenant uuid, p_case uuid)
RETURNS TABLE (o_note_id uuid, o_body text, o_lint_version text, o_created_by uuid, o_created_at timestamptz)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a record;
    c record;
BEGIN
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_case:read', p_case::text,
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_case::text, 'notes')));
    SELECT x.owner_scope INTO c FROM public.aml_cases x WHERE x.id = p_case AND x.tenant_id = p_tenant;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'aml: not found' USING ERRCODE = 'AM040';
    END IF;
    PERFORM public.aml_assert_row_scope(a.o_scope, c.owner_scope);
    RETURN QUERY
        SELECT n.id, n.body, n.lint_version, n.created_by, n.created_at FROM public.aml_case_notes n WHERE n.case_id = p_case ORDER BY n.created_at, n.id;
END
$$;

-- The case queue (ADR 0116 s6 "AML queue"): the cases the actor's scope may read, newest first, bounded. Proof target `new`.
CREATE FUNCTION aml_case_queue(p_tenant uuid, p_limit integer)
RETURNS TABLE (o_case_id uuid, o_owner_scope text, o_person_id uuid, o_status text, o_sar_flagged boolean, o_last_seq integer, o_opened_at timestamptz)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a record;
BEGIN
    IF p_limit IS NULL OR p_limit < 1 OR p_limit > 100 THEN
        RAISE EXCEPTION 'aml: the queue limit is 1..100' USING ERRCODE = 'AM004';
    END IF;
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_case:read', 'new',
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_limit::text, 'queue')));
    RETURN QUERY
        SELECT c.id, c.owner_scope, c.person_id, public.aml_case_status(c.id), public.aml_case_sar_flagged(c.id), public.aml_case_last_seq(c.id), c.opened_at
          FROM public.aml_cases c
         WHERE c.tenant_id = p_tenant AND public.aml_scope_compatible(a.o_scope, c.owner_scope)
         ORDER BY c.opened_at DESC, c.id DESC LIMIT p_limit;
END
$$;

-- ---- the erasure check (ADR 0116 s5.4 C9) ----
-- True if ANY AML record (hold, alert, case) exists for the person in the session tenant. An erasure / pseudonymisation job
-- calls it per tenant BEFORE it touches the person and refuses when it is true (fail closed). Not proof-bound (a job is not
-- an officer); session-tenant bound (AM026). It discloses a boolean to a caller who is already inside the tenant.
CREATE FUNCTION aml_person_has_records(p_tenant uuid, p_person uuid) RETURNS boolean
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF p_tenant IS NULL OR p_person IS NULL THEN
        RAISE EXCEPTION 'aml: invalid arguments' USING ERRCODE = 'AM004';
    END IF;
    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AM026';
    END IF;
    RETURN EXISTS (SELECT 1 FROM public.aml_holds WHERE tenant_id = p_tenant AND person_id = p_person)
        OR EXISTS (SELECT 1 FROM public.aml_alerts WHERE tenant_id = p_tenant AND person_id = p_person)
        OR EXISTS (SELECT 1 FROM public.aml_cases WHERE tenant_id = p_tenant AND person_id = p_person);
END
$$;

-- =========================================================================
-- 6. Grants (mirrored by deploy/init-app-role.sql)
-- =========================================================================
DO $$
DECLARE
    f text;
BEGIN
    FOREACH f IN ARRAY ARRAY[
        'aml_case_status(uuid)', 'aml_case_last_seq(uuid)', 'aml_case_sar_flagged(uuid)', 'aml_case_transition_allowed(text, text)',
        'aml_proposal_content_hash(uuid, uuid, integer, text, uuid, text, text, uuid)',
        'aml_alerts_guard()', 'aml_cases_guard()', 'aml_case_alerts_guard()', 'aml_case_notes_guard()', 'aml_case_events_guard()',
        'aml_case_lock_for_write(uuid, uuid, text, integer)'
    ] LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
            EXECUTE format('REVOKE ALL ON FUNCTION %s FROM igaming_runtime', f);
        END IF;
    END LOOP;
    FOREACH f IN ARRAY ARRAY[
        'aml_alert_raise(uuid, uuid, text, text)', 'aml_case_open(uuid, uuid, text, text, uuid[])',
        'aml_case_transition(uuid, uuid, integer, text, text)', 'aml_case_add_note(uuid, uuid, integer, text, text)',
        'aml_case_flag_sar(uuid, uuid, integer, text)', 'aml_case_propose(uuid, uuid, integer, text, uuid, text, text)',
        'aml_case_approve(uuid, uuid, integer, text, text)', 'aml_case_read(uuid, uuid)', 'aml_case_events_list(uuid, uuid)',
        'aml_case_notes_list(uuid, uuid)', 'aml_case_queue(uuid, integer)', 'aml_person_has_records(uuid, uuid)'
    ] LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
            EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO igaming_runtime', f);
        END IF;
    END LOOP;
END
$$;

COMMENT ON TABLE aml_case_events IS 'ADR 0116 s3.1: the append-only case audit trail (actor, scope, from/to, reason, ip, request). Case state is a projection of it.';
