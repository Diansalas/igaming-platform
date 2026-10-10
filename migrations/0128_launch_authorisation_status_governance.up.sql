-- ADR 0112 SLICE 1 (brand and tenant launch authorisation; sections 3.4, 4,
-- 4.5, 4.6, 7, 13 slice 1). Status governance for tenants.status and
-- brands.status: a status change is only possible through a governed,
-- same-transaction, request-bound decision record, enforced by the database for
-- every writer.
--
-- NUMBERING: 0128 (next free after 0127; confirmed against migrations/ at
-- branch time). Migration numbers are allocated by the orchestrator.
--
-- ORDER (ADR 0112 section 3.4, binding):
--   1. helper functions, the three append-only tables, vocabularies and guards,
--      WITHOUT row-level security and WITHOUT the subject status guard; every FK
--      from the new tables is ON DELETE RESTRICT (S13).
--   2. widen both status CHECKs to include pending_launch. No row is updated.
--   3. S12: backfill BEFORE enabling RLS on the new tables: one legacy_baseline
--      transition per existing tenant and brand (from_status NULL, to_status =
--      the row's current status, system actor). The source rows are counted
--      first and the migration RAISEs unless the inserted count equals the
--      source count (the 0008 lesson: never silently zero).
--   4. enable and FORCE RLS on the new tables, create policies and grants.
--   5. only now ALTER COLUMN status SET DEFAULT 'pending_launch' on both tables
--      and install zz_launch_status_governed and the subject INSERT guard.
--
-- WHAT IS (NOT) IN SLICE 1. In: the tables, vocabularies, RLS and grants, the
-- request/approval/transition guards that make S1/S2/S10/S12/S13 true (a
-- request can only reach 'executing' through the S1 conditions, and a status
-- change needs a same-transaction governed transition of an executing request),
-- the LF2 owner-provisioning path, the LF5 trigger-order contract. Not in slice
-- 1 (later slices CREATE OR REPLACE the functions named here): the per-brand
-- gameplay lock (slice 2, LF1; the status guard on brands is the place that
-- takes it exclusively), readiness evaluation and the H-2/H-3/H-5/H-6
-- hard-precondition re-checks and the closed reason catalogue (slice 3, Go
-- launchgov), and the ADR 0110 signed-actor-proof extension
-- (zz_actor_proof_guard on the governed tables, slice 3). Until slice 3 lands no
-- Go code creates a request, so no tenant or brand can be launched except by the
-- table-owner provisioning path.
--
-- SQLSTATE class 'LA' (callers branch on the code only):
--   LA001 session shape / actor            LA010 request content / legality
--   LA011 request UPDATE state machine     LA012 approval rules
--   LA013 transition INSERT (S2)           LA020 subject status guard (S10)
--   LA021 subject INSERT guard             LA030 deferred commit check
--   LA099 append-only / down refusal
--
-- Every function pins search_path (ADR 0108); none is SECURITY DEFINER.

-- ---------------------------------------------------------------------------
-- 1a. Helpers
-- ---------------------------------------------------------------------------

-- The owner of the tenants table (the migration role). Used for the owner-only
-- paths of S2 and LF2. Strict equality with current_user: membership in the
-- owner role does not count.
CREATE FUNCTION launch_table_owner() RETURNS name
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT pg_get_userbyid(c.relowner)::name FROM pg_class c WHERE c.oid = 'public.tenants'::regclass
$$;

CREATE FUNCTION launch_is_table_owner() RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT current_user::name = public.launch_table_owner()
$$;

-- Section 3.1 subject status graph.
CREATE FUNCTION launch_status_move_legal(p_from text, p_to text) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT (p_from, p_to) IN (
        ('pending_launch', 'active'), ('pending_launch', 'closed'),
        ('active', 'suspended'), ('active', 'closed'),
        ('suspended', 'active'), ('suspended', 'closed'))
$$;

-- Section 3.1 action table: which (from, to) an action may carry.
CREATE FUNCTION launch_action_move_legal(p_action text, p_from text, p_to text) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT CASE p_action
        WHEN 'activate'   THEN p_from = 'pending_launch' AND p_to = 'active'
        WHEN 'suspend'    THEN p_from = 'active' AND p_to = 'suspended'
        WHEN 'reactivate' THEN p_from = 'suspended' AND p_to = 'active'
        WHEN 'close'      THEN p_from IN ('pending_launch', 'active', 'suspended') AND p_to = 'closed'
        WHEN 'ratify'     THEN p_from IN ('active', 'suspended') AND p_to = p_from
        ELSE false
    END
$$;

CREATE FUNCTION launch_uuid_array_distinct(p_a uuid[]) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT p_a IS NOT NULL AND cardinality(p_a) = (SELECT count(DISTINCT x) FROM unnest(p_a) AS x)
$$;

CREATE FUNCTION launch_deny_mutation() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted', TG_TABLE_NAME, TG_OP USING ERRCODE = 'LA099';
END
$$;

-- The session actor, resolved from the session GUCs: exactly a tenant principal
-- or exactly a platform principal, an ACTIVE staff row, never an acting / player
-- / service / mixed shape (LA001). The sole source of every forced actor column.
CREATE FUNCTION launch_actor_session(
    OUT actor uuid, OUT scope text, OUT tenant uuid, OUT person_id uuid
) LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
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
        RAISE EXCEPTION 'launch authorisation: an acting session may not act here' USING ERRCODE = 'LA001';
    END IF;
    IF v_tenant IS NOT NULL AND v_principal IS NOT NULL
       AND v_platform IS NULL AND v_player IS NULL AND v_service IS NULL THEN
        SELECT su.id, su.person_id, su.status INTO rec
          FROM public.staff_users su WHERE su.id = v_principal::uuid AND su.tenant_id = v_tenant::uuid;
        IF NOT FOUND OR rec.status <> 'active' THEN
            RAISE EXCEPTION 'launch authorisation: the tenant principal is not an active staff member of the tenant' USING ERRCODE = 'LA001';
        END IF;
        actor := rec.id; scope := 'tenant'; tenant := v_tenant::uuid; person_id := rec.person_id;
        RETURN;
    END IF;
    IF v_platform IS NOT NULL AND v_tenant IS NULL AND v_principal IS NULL
       AND v_player IS NULL AND v_service IS NULL THEN
        SELECT su.id, su.person_id, su.status INTO rec
          FROM public.staff_users su WHERE su.id = v_platform::uuid AND su.tenant_id IS NULL;
        IF NOT FOUND OR rec.status <> 'active' THEN
            RAISE EXCEPTION 'launch authorisation: the platform principal is not an active platform staff member' USING ERRCODE = 'LA001';
        END IF;
        actor := rec.id; scope := 'platform'; tenant := NULL; person_id := rec.person_id;
        RETURN;
    END IF;
    RAISE EXCEPTION 'launch authorisation: the session is not a valid tenant or platform principal session' USING ERRCODE = 'LA001';
END
$$;

-- ---------------------------------------------------------------------------
-- 1b. launch_authorisation_requests (ADR 0112 section 4.2)
-- ---------------------------------------------------------------------------
CREATE TABLE launch_authorisation_requests (
    id                                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                         UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    subject_kind                      TEXT NOT NULL CHECK (subject_kind IN ('tenant', 'brand')),
    brand_id                          UUID NULL,
    action                            TEXT NOT NULL CHECK (action IN ('activate', 'suspend', 'reactivate', 'close', 'ratify')),
    from_status                       TEXT NOT NULL CHECK (from_status IN ('pending_launch', 'active', 'suspended', 'closed')),
    to_status                         TEXT NOT NULL CHECK (to_status IN ('pending_launch', 'active', 'suspended', 'closed')),
    licensing_model                   TEXT NULL CHECK (licensing_model IN ('platform_licence', 'tenant_licence', 'other_manually_approved', 'not_applicable_recorded_determination')),
    licensing_status                  TEXT NULL CHECK (licensing_status IN ('in_force', 'applied_pending', 'conditional', 'suspended', 'expired', 'not_required_per_determination')),
    licence_id                        UUID NULL REFERENCES licences (id) ON DELETE RESTRICT,
    determination_reference           TEXT NULL CHECK (char_length(determination_reference) BETWEEN 1 AND 200),
    determination_by_role             TEXT NULL CHECK (char_length(determination_by_role) BETWEEN 1 AND 100),
    responsible_operator_name         TEXT NULL CHECK (char_length(responsible_operator_name) BETWEEN 1 AND 200),
    responsible_operator_registration TEXT NOT NULL DEFAULT '' CHECK (char_length(responsible_operator_registration) <= 100),
    -- uuid[] cannot carry a foreign key; each id is validated against the
    -- jurisdiction registry by the slice-3 hard preconditions (H-3).
    jurisdiction_ids                  UUID[] NOT NULL DEFAULT '{}',
    conditions_note                   TEXT NOT NULL DEFAULT '' CHECK (char_length(conditions_note) <= 2000),
    readiness_snapshot                JSONB NOT NULL,
    readiness_snapshot_hash           TEXT NOT NULL CHECK (readiness_snapshot_hash ~ '^[0-9a-f]{64}$'),
    acknowledged_check_codes          TEXT[] NOT NULL DEFAULT '{}',
    reason_code                       TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64),
    requested_by                      UUID NOT NULL,
    requested_by_scope                TEXT NOT NULL CHECK (requested_by_scope IN ('platform', 'tenant')),
    requested_by_person_id            UUID NOT NULL,
    required_approvals                INT NOT NULL CHECK (required_approvals BETWEEN 0 AND 2),
    payload_hash                      TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    status                            TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'executing', 'executed', 'refused_at_execution', 'rejected', 'cancelled', 'expired', 'superseded')),
    created_at                        TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at                        TIMESTAMPTZ NOT NULL,
    decided_at                        TIMESTAMPTZ NULL,
    refusal_code                      TEXT NULL CHECK (octet_length(refusal_code) BETWEEN 1 AND 64),
    executing_txid                    BIGINT NULL,
    UNIQUE (tenant_id, id),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id) ON DELETE RESTRICT,
    CHECK ((subject_kind = 'brand') = (brand_id IS NOT NULL)),
    CHECK (octet_length(readiness_snapshot::text) <= 32768),
    CHECK (launch_action_move_legal(action, from_status, to_status)),
    CHECK ((action = 'suspend') = (required_approvals = 0)),
    CHECK (action NOT IN ('activate', 'reactivate', 'ratify') OR (
        licensing_model IS NOT NULL AND licensing_status IS NOT NULL
        AND responsible_operator_name IS NOT NULL
        AND cardinality(jurisdiction_ids) BETWEEN 1 AND 32
        AND launch_uuid_array_distinct(jurisdiction_ids))),
    CHECK (action IN ('activate', 'reactivate', 'ratify') OR (licensing_model IS NULL AND licensing_status IS NULL)),
    CHECK ((status = 'refused_at_execution') = (refusal_code IS NOT NULL)),
    CHECK (status NOT IN ('executing', 'executed', 'refused_at_execution') OR executing_txid IS NOT NULL)
);

CREATE INDEX launch_requests_tenant ON launch_authorisation_requests (tenant_id);
-- S3: one pending request per subject; a suspension is excluded so it can never
-- be blocked by a pending activation or closure.
CREATE UNIQUE INDEX launch_requests_one_pending
    ON launch_authorisation_requests (subject_kind, COALESCE(brand_id, tenant_id))
    WHERE status IN ('pending', 'executing') AND action <> 'suspend';

CREATE FUNCTION launch_request_payload_hash(r launch_authorisation_requests) RETURNS text
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT public.k2_sha256_hex(public.k2_canonical(
        r.tenant_id::text, r.subject_kind, r.brand_id::text, r.action, r.from_status, r.to_status,
        r.licensing_model, r.licensing_status, r.licence_id::text,
        r.determination_reference, r.determination_by_role,
        r.responsible_operator_name, r.responsible_operator_registration,
        (SELECT string_agg(j::text, ',' ORDER BY j) FROM unnest(r.jurisdiction_ids) AS j),
        r.conditions_note, r.readiness_snapshot_hash,
        (SELECT string_agg(c, ',' ORDER BY c) FROM unnest(r.acknowledged_check_codes) AS c),
        r.reason_code))
$$;

-- ---------------------------------------------------------------------------
-- 1c. launch_authorisation_approvals (section 4.3, append-only)
-- ---------------------------------------------------------------------------
CREATE TABLE launch_authorisation_approvals (
    id                                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                             UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    request_id                            UUID NOT NULL,
    decision                              TEXT NOT NULL CHECK (decision IN ('approve', 'reject')),
    decided_by                            UUID NOT NULL,
    decided_by_scope                      TEXT NOT NULL CHECK (decided_by_scope = 'platform'),
    decided_by_person_id                  UUID NOT NULL,
    payload_hash                          TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    readiness_snapshot_hash_at_decision   TEXT NOT NULL CHECK (readiness_snapshot_hash_at_decision ~ '^[0-9a-f]{64}$'),
    reason_code                           TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64),
    decided_at                            TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_txid                          BIGINT NOT NULL,
    UNIQUE (request_id, decided_by_person_id),
    FOREIGN KEY (tenant_id, request_id) REFERENCES launch_authorisation_requests (tenant_id, id) ON DELETE RESTRICT
);
CREATE INDEX launch_approvals_tenant ON launch_authorisation_approvals (tenant_id);

-- ---------------------------------------------------------------------------
-- 1d. launch_status_transitions (section 4.4, append-only history)
-- ---------------------------------------------------------------------------
CREATE TABLE launch_status_transitions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    subject_kind  TEXT NOT NULL CHECK (subject_kind IN ('tenant', 'brand')),
    brand_id      UUID NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('governed', 'legacy_baseline', 'owner_provisioned')),
    from_status   TEXT NULL CHECK (from_status IN ('pending_launch', 'active', 'suspended', 'closed')),
    to_status     TEXT NOT NULL CHECK (to_status IN ('pending_launch', 'active', 'suspended', 'closed')),
    request_id    UUID NULL,
    executed_by   UUID NULL,
    approver_ids  UUID[] NOT NULL DEFAULT '{}',
    actor_type    TEXT NOT NULL CHECK (actor_type IN ('system', 'platform', 'tenant')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    txid          BIGINT NOT NULL,
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id) ON DELETE RESTRICT,
    FOREIGN KEY (tenant_id, request_id) REFERENCES launch_authorisation_requests (tenant_id, id) ON DELETE RESTRICT,
    CHECK ((subject_kind = 'brand') = (brand_id IS NOT NULL)),
    -- S2 shape: a governed row carries its from_status and its request; the two
    -- owner-only kinds carry neither.
    CHECK ((kind = 'governed') = (request_id IS NOT NULL)),
    CHECK ((kind = 'governed') = (from_status IS NOT NULL)),
    CHECK (kind = 'governed' OR (actor_type = 'system' AND executed_by IS NULL))
);
CREATE INDEX launch_transitions_tenant ON launch_status_transitions (tenant_id);
CREATE INDEX launch_transitions_subject ON launch_status_transitions (subject_kind, (COALESCE(brand_id, tenant_id)), created_at);
-- UNIQUE (subject, txid): at most one status change per subject per transaction.
CREATE UNIQUE INDEX launch_transitions_one_per_txid
    ON launch_status_transitions (subject_kind, (COALESCE(brand_id, tenant_id)), txid);

-- ---------------------------------------------------------------------------
-- 1e. Guards on the three tables
-- ---------------------------------------------------------------------------

-- Request INSERT / UPDATE (4.5 'Request INSERT', 'Request UPDATE'; S1, S3, S8).
CREATE FUNCTION launch_requests_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor   RECORD;
    v_status  text;
    v_n       int;
    v_new     public.launch_authorisation_requests%ROWTYPE;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'launch_authorisation_requests is append-only: DELETE is not permitted' USING ERRCODE = 'LA099';
    END IF;

    IF TG_OP = 'INSERT' THEN
        SELECT * INTO v_actor FROM public.launch_actor_session();
        IF v_actor.person_id IS NULL THEN
            RAISE EXCEPTION 'launch request: the requester has no linked person_id' USING ERRCODE = 'LA001';
        END IF;
        IF v_actor.scope = 'tenant' AND (NEW.subject_kind IS DISTINCT FROM 'brand' OR NEW.tenant_id IS DISTINCT FROM v_actor.tenant) THEN
            RAISE EXCEPTION 'launch request: a tenant session may only request for a brand of its own tenant' USING ERRCODE = 'LA001';
        END IF;

        -- Forced columns, never caller-supplied.
        NEW.requested_by := v_actor.actor;
        NEW.requested_by_scope := v_actor.scope;
        NEW.requested_by_person_id := v_actor.person_id;
        NEW.status := 'pending';
        NEW.created_at := now();
        NEW.expires_at := now() + interval '72 hours';
        NEW.decided_at := NULL;
        NEW.refusal_code := NULL;
        NEW.executing_txid := NULL;
        IF NEW.id IS NULL THEN NEW.id := gen_random_uuid(); END IF;

        -- from_status is read from the subject; to_status is derived from the action.
        IF NEW.subject_kind = 'tenant' THEN
            IF NEW.brand_id IS NOT NULL THEN
                RAISE EXCEPTION 'launch request: a tenant subject carries no brand_id' USING ERRCODE = 'LA010';
            END IF;
            SELECT t.status INTO v_status FROM public.tenants t WHERE t.id = NEW.tenant_id;
        ELSIF NEW.subject_kind = 'brand' THEN
            SELECT b.status INTO v_status FROM public.brands b WHERE b.id = NEW.brand_id AND b.tenant_id = NEW.tenant_id;
        ELSE
            RAISE EXCEPTION 'launch request: unknown subject_kind' USING ERRCODE = 'LA010';
        END IF;
        IF v_status IS NULL THEN
            RAISE EXCEPTION 'launch request: the subject is not readable' USING ERRCODE = 'LA010';
        END IF;
        NEW.from_status := v_status;
        NEW.to_status := CASE NEW.action
            WHEN 'activate' THEN 'active' WHEN 'reactivate' THEN 'active'
            WHEN 'suspend' THEN 'suspended' WHEN 'close' THEN 'closed'
            WHEN 'ratify' THEN v_status ELSE NULL END;
        IF NEW.to_status IS NULL OR NOT public.launch_action_move_legal(NEW.action, NEW.from_status, NEW.to_status) THEN
            RAISE EXCEPTION 'launch request: action % is not legal from status %', NEW.action, NEW.from_status USING ERRCODE = 'LA010';
        END IF;
        IF NEW.action = 'ratify' THEN
            -- Only a row whose latest transition is the legacy baseline can be ratified.
            SELECT t.kind INTO v_status FROM public.launch_status_transitions t
             WHERE t.subject_kind = NEW.subject_kind AND COALESCE(t.brand_id, t.tenant_id) = COALESCE(NEW.brand_id, NEW.tenant_id)
             ORDER BY t.created_at DESC, t.id DESC LIMIT 1;
            IF v_status IS DISTINCT FROM 'legacy_baseline' THEN
                RAISE EXCEPTION 'launch request: only a subject whose latest transition is legacy_baseline can be ratified' USING ERRCODE = 'LA010';
            END IF;
        END IF;

        -- S3: stale pending requests of this subject are moved to expired first, so
        -- the one-pending UNIQUE never deadlocks a new request.
        UPDATE public.launch_authorisation_requests r
           SET status = 'expired', decided_at = now()
         WHERE r.subject_kind = NEW.subject_kind
           AND COALESCE(r.brand_id, r.tenant_id) = COALESCE(NEW.brand_id, NEW.tenant_id)
           AND r.status = 'pending' AND r.expires_at <= now();

        -- 6.2 four-eyes counts, forced and not lowerable by any caller.
        NEW.required_approvals := CASE
            WHEN NEW.action = 'suspend' THEN 0
            WHEN NEW.action = 'close' THEN 2
            WHEN NEW.subject_kind = 'tenant' THEN 2
            WHEN NEW.action IN ('activate', 'reactivate') AND NEW.licensing_model = 'platform_licence'
                 AND v_actor.scope = 'tenant' THEN 2
            ELSE 1 END;

        NEW.payload_hash := public.launch_request_payload_hash(NEW);
        RETURN NEW;
    END IF;

    -- UPDATE: only the state columns may change, and only along the 3.2 machine.
    v_new := NEW;
    v_new.status := OLD.status;
    v_new.decided_at := OLD.decided_at;
    v_new.refusal_code := OLD.refusal_code;
    v_new.executing_txid := OLD.executing_txid;
    IF v_new IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'launch request: content is immutable after INSERT' USING ERRCODE = 'LA011';
    END IF;
    IF NEW.status = OLD.status THEN
        IF NEW.decided_at IS DISTINCT FROM OLD.decided_at OR NEW.refusal_code IS DISTINCT FROM OLD.refusal_code
           OR NEW.executing_txid IS DISTINCT FROM OLD.executing_txid THEN
            RAISE EXCEPTION 'launch request: state columns change only with the status' USING ERRCODE = 'LA011';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status NOT IN ('pending', 'executing') THEN
        RAISE EXCEPTION 'launch request: % is a terminal status', OLD.status USING ERRCODE = 'LA011';
    END IF;

    IF OLD.status = 'pending' AND NEW.status = 'executing' THEN
        -- S1 / S8.
        IF now() >= OLD.expires_at THEN
            RAISE EXCEPTION 'launch request: the request has expired' USING ERRCODE = 'LA011';
        END IF;
        SELECT * INTO v_actor FROM public.launch_actor_session();
        IF v_actor.scope = 'tenant' THEN
            -- S8: a tenant session drives execution only for a suspension of its own brand.
            IF OLD.action <> 'suspend' OR OLD.required_approvals <> 0 OR OLD.subject_kind <> 'brand'
               OR OLD.tenant_id IS DISTINCT FROM v_actor.tenant THEN
                RAISE EXCEPTION 'launch request: a tenant session may execute only a suspension of its own brand' USING ERRCODE = 'LA011';
            END IF;
        ELSIF OLD.action = 'suspend' AND OLD.required_approvals = 0 THEN
            NULL; -- S1(b)
        ELSE
            -- S1(a): enough approvals by distinct Persons (other than the requester) whose
            -- payload hash matches, the final one inserted in THIS transaction.
            SELECT count(DISTINCT a.decided_by_person_id) INTO v_n
              FROM public.launch_authorisation_approvals a
             WHERE a.request_id = OLD.id AND a.decision = 'approve' AND a.payload_hash = OLD.payload_hash
               AND a.decided_by_person_id <> OLD.requested_by_person_id AND a.decided_by <> OLD.requested_by;
            -- C-3(a): a recorded 'reject' decision blocks execution of this request for good.
            IF EXISTS (SELECT 1 FROM public.launch_authorisation_approvals a
                        WHERE a.request_id = OLD.id AND a.decision = 'reject') THEN
                RAISE EXCEPTION 'launch request: a reject decision is recorded; the request cannot execute' USING ERRCODE = 'LA011';
            END IF;
            IF v_n < OLD.required_approvals OR OLD.required_approvals < 1 THEN
                RAISE EXCEPTION 'launch request: % of % required approvals', v_n, OLD.required_approvals USING ERRCODE = 'LA011';
            END IF;
            IF NOT EXISTS (SELECT 1 FROM public.launch_authorisation_approvals a
                            WHERE a.request_id = OLD.id AND a.decision = 'approve' AND a.decided_txid = txid_current()) THEN
                RAISE EXCEPTION 'launch request: the final approval must be decided in the executing transaction' USING ERRCODE = 'LA011';
            END IF;
        END IF;
        NEW.executing_txid := txid_current();
        NEW.decided_at := NULL;
        NEW.refusal_code := NULL;
    ELSIF OLD.status = 'pending' AND NEW.status = 'rejected' THEN
        IF NOT EXISTS (SELECT 1 FROM public.launch_authorisation_approvals a
                        WHERE a.request_id = OLD.id AND a.decision = 'reject' AND a.decided_txid = txid_current()) THEN
            RAISE EXCEPTION 'launch request: rejection needs a reject decision in the same transaction' USING ERRCODE = 'LA011';
        END IF;
        NEW.decided_at := now(); NEW.refusal_code := NULL; NEW.executing_txid := NULL;
    ELSIF OLD.status = 'pending' AND NEW.status = 'cancelled' THEN
        SELECT * INTO v_actor FROM public.launch_actor_session();
        IF v_actor.actor IS DISTINCT FROM OLD.requested_by OR v_actor.scope IS DISTINCT FROM OLD.requested_by_scope THEN
            RAISE EXCEPTION 'launch request: only the requester may cancel' USING ERRCODE = 'LA011';
        END IF;
        NEW.decided_at := now(); NEW.refusal_code := NULL; NEW.executing_txid := NULL;
    ELSIF OLD.status = 'pending' AND NEW.status = 'expired' THEN
        IF now() < OLD.expires_at THEN
            RAISE EXCEPTION 'launch request: not yet expired' USING ERRCODE = 'LA011';
        END IF;
        NEW.decided_at := now(); NEW.refusal_code := NULL; NEW.executing_txid := NULL;
    ELSIF OLD.status = 'pending' AND NEW.status = 'superseded' THEN
        IF NOT EXISTS (SELECT 1 FROM public.launch_authorisation_requests s
                        WHERE s.id <> OLD.id AND s.tenant_id = OLD.tenant_id AND s.subject_kind = OLD.subject_kind
                          AND s.brand_id IS NOT DISTINCT FROM OLD.brand_id
                          AND s.action = 'suspend' AND s.status IN ('executing', 'executed')
                          AND s.executing_txid = txid_current()
                          -- security S-2: only once the suspension's own governed transition exists
                          -- in this transaction (an 'executing' request alone, e.g. a tenant
                          -- session's own suspend, proves nothing yet).
                          AND EXISTS (SELECT 1 FROM public.launch_status_transitions st
                                       WHERE st.request_id = s.id AND st.kind = 'governed'
                                         AND st.txid = txid_current())) THEN
            RAISE EXCEPTION 'launch request: superseded only in the transaction that executes a suspension of the same subject' USING ERRCODE = 'LA011';
        END IF;
        NEW.decided_at := now(); NEW.refusal_code := NULL; NEW.executing_txid := NULL;
    ELSIF OLD.status = 'executing' AND NEW.status = 'executed' THEN
        IF OLD.executing_txid IS DISTINCT FROM txid_current() THEN
            RAISE EXCEPTION 'launch request: executing is closed in the executing transaction only' USING ERRCODE = 'LA011';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM public.launch_status_transitions t
                        WHERE t.request_id = OLD.id AND t.kind = 'governed' AND t.txid = txid_current()) THEN
            RAISE EXCEPTION 'launch request: executed needs its governed transition in the same transaction' USING ERRCODE = 'LA011';
        END IF;
        NEW.decided_at := now(); NEW.refusal_code := NULL; NEW.executing_txid := OLD.executing_txid;
    ELSIF OLD.status = 'executing' AND NEW.status = 'refused_at_execution' THEN
        IF OLD.executing_txid IS DISTINCT FROM txid_current() THEN
            RAISE EXCEPTION 'launch request: executing is closed in the executing transaction only' USING ERRCODE = 'LA011';
        END IF;
        NEW.decided_at := now(); NEW.executing_txid := OLD.executing_txid;
    ELSE
        RAISE EXCEPTION 'launch request: % -> % is not a legal transition', OLD.status, NEW.status USING ERRCODE = 'LA011';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER launch_requests_guard
    BEFORE INSERT OR UPDATE ON launch_authorisation_requests
    FOR EACH ROW EXECUTE FUNCTION launch_requests_guard();
CREATE TRIGGER launch_requests_no_delete
    BEFORE DELETE ON launch_authorisation_requests
    FOR EACH ROW EXECUTE FUNCTION launch_deny_mutation();
CREATE TRIGGER launch_requests_no_truncate
    BEFORE TRUNCATE ON launch_authorisation_requests
    FOR EACH STATEMENT EXECUTE FUNCTION launch_deny_mutation();

-- LA030 (S1): a request may never remain 'executing' at commit. The row is re-read
-- because the trigger event carries the row as it was when the event was queued.
CREATE FUNCTION launch_requests_not_executing_at_commit() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
BEGIN
    -- FAIL CLOSED (security S-1): the row is read under the committing session's own RLS. If the
    -- session no longer sees it (its GUCs were cleared or changed before COMMIT), that is NOT a pass:
    -- an invisible request could be 'executing' and stay stuck. No SECURITY DEFINER read is used
    -- (it would bypass the isolation this table exists to keep), so NOT FOUND refuses the commit.
    SELECT r.status INTO v_status FROM public.launch_authorisation_requests r WHERE r.id = NEW.id;
    IF NOT FOUND OR v_status = 'executing' THEN
        RAISE EXCEPTION 'launch request % is still executing, or not visible, at commit', NEW.id USING ERRCODE = 'LA030';
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER launch_requests_not_executing_at_commit
    AFTER INSERT OR UPDATE ON launch_authorisation_requests
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION launch_requests_not_executing_at_commit();

-- Approval INSERT (4.5 'Approval INSERT'; S9: the request is locked FOR UPDATE here).
CREATE FUNCTION launch_approvals_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor RECORD;
    v_req   public.launch_authorisation_requests%ROWTYPE;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'launch_authorisation_approvals is append-only: % is not permitted', TG_OP USING ERRCODE = 'LA099';
    END IF;
    SELECT * INTO v_actor FROM public.launch_actor_session();
    IF v_actor.scope <> 'platform' THEN
        RAISE EXCEPTION 'launch approval: only a platform session may decide a request' USING ERRCODE = 'LA001';
    END IF;
    IF v_actor.person_id IS NULL THEN
        RAISE EXCEPTION 'launch approval: the approver has no linked person_id' USING ERRCODE = 'LA012';
    END IF;
    SELECT * INTO v_req FROM public.launch_authorisation_requests r WHERE r.id = NEW.request_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'launch approval: the request does not exist' USING ERRCODE = 'LA012';
    END IF;
    IF v_req.status <> 'pending' OR now() >= v_req.expires_at THEN
        RAISE EXCEPTION 'launch approval: the request is not pending' USING ERRCODE = 'LA012';
    END IF;
    IF v_actor.actor = v_req.requested_by OR v_actor.person_id = v_req.requested_by_person_id THEN
        RAISE EXCEPTION 'launch approval: the requester may not decide (staff id or Person)' USING ERRCODE = 'LA012';
    END IF;
    IF NEW.payload_hash IS DISTINCT FROM v_req.payload_hash THEN
        RAISE EXCEPTION 'launch approval: payload_hash does not match the request' USING ERRCODE = 'LA012';
    END IF;
    IF EXISTS (SELECT 1 FROM public.launch_authorisation_approvals a
                WHERE a.request_id = NEW.request_id AND a.decided_by_person_id = v_actor.person_id) THEN
        RAISE EXCEPTION 'launch approval: this Person already decided the request' USING ERRCODE = 'LA012';
    END IF;
    NEW.tenant_id := v_req.tenant_id;
    NEW.decided_by := v_actor.actor;
    NEW.decided_by_scope := v_actor.scope;
    NEW.decided_by_person_id := v_actor.person_id;
    NEW.decided_at := now();
    NEW.decided_txid := txid_current();
    IF NEW.id IS NULL THEN NEW.id := gen_random_uuid(); END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER launch_approvals_guard
    BEFORE INSERT OR UPDATE ON launch_authorisation_approvals
    FOR EACH ROW EXECUTE FUNCTION launch_approvals_guard();
CREATE TRIGGER launch_approvals_no_delete
    BEFORE DELETE ON launch_authorisation_approvals
    FOR EACH ROW EXECUTE FUNCTION launch_deny_mutation();
CREATE TRIGGER launch_approvals_no_truncate
    BEFORE TRUNCATE ON launch_authorisation_approvals
    FOR EACH STATEMENT EXECUTE FUNCTION launch_deny_mutation();

-- Transition INSERT (4.5 'Transition INSERT'; S2). governed: a same-transaction
-- executing request of the same subject with matching from/to. legacy_baseline and
-- owner_provisioned: from_status NULL and current_user is the table owner;
-- legacy_baseline is additionally refused if any transition exists for the subject.
CREATE FUNCTION launch_transitions_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor RECORD;
    v_req   public.launch_authorisation_requests%ROWTYPE;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'launch_status_transitions is append-only: % is not permitted', TG_OP USING ERRCODE = 'LA099';
    END IF;
    NEW.txid := txid_current();
    NEW.created_at := now();
    IF NEW.id IS NULL THEN NEW.id := gen_random_uuid(); END IF;

    IF NEW.kind = 'governed' THEN
        SELECT * INTO v_req FROM public.launch_authorisation_requests r WHERE r.id = NEW.request_id;
        IF NOT FOUND
           OR v_req.status <> 'executing' OR v_req.executing_txid IS DISTINCT FROM txid_current()
           OR v_req.tenant_id <> NEW.tenant_id OR v_req.subject_kind <> NEW.subject_kind
           OR v_req.brand_id IS DISTINCT FROM NEW.brand_id
           OR v_req.from_status IS DISTINCT FROM NEW.from_status OR v_req.to_status <> NEW.to_status THEN
            RAISE EXCEPTION 'launch transition: a governed transition needs a same-transaction executing request for the same subject and statuses' USING ERRCODE = 'LA013';
        END IF;
        SELECT * INTO v_actor FROM public.launch_actor_session();
        NEW.executed_by := v_actor.actor;
        NEW.actor_type := v_actor.scope;
        NEW.approver_ids := COALESCE((SELECT array_agg(DISTINCT a.decided_by ORDER BY a.decided_by)
                                        FROM public.launch_authorisation_approvals a
                                       WHERE a.request_id = v_req.id AND a.decision = 'approve'), '{}');
    ELSE
        IF NEW.from_status IS NOT NULL OR NOT public.launch_is_table_owner() THEN
            RAISE EXCEPTION 'launch transition: % is writable only by the table owner with from_status NULL', NEW.kind USING ERRCODE = 'LA013';
        END IF;
        IF NEW.kind = 'legacy_baseline' AND EXISTS (
            SELECT 1 FROM public.launch_status_transitions t
             WHERE t.subject_kind = NEW.subject_kind AND COALESCE(t.brand_id, t.tenant_id) = COALESCE(NEW.brand_id, NEW.tenant_id)) THEN
            RAISE EXCEPTION 'launch transition: legacy_baseline is refused when the subject already has a transition' USING ERRCODE = 'LA013';
        END IF;
        NEW.request_id := NULL; NEW.executed_by := NULL; NEW.actor_type := 'system'; NEW.approver_ids := '{}';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER launch_transitions_guard
    BEFORE INSERT OR UPDATE ON launch_status_transitions
    FOR EACH ROW EXECUTE FUNCTION launch_transitions_guard();
CREATE TRIGGER launch_transitions_no_delete
    BEFORE DELETE ON launch_status_transitions
    FOR EACH ROW EXECUTE FUNCTION launch_deny_mutation();
CREATE TRIGGER launch_transitions_no_truncate
    BEFORE TRUNCATE ON launch_status_transitions
    FOR EACH STATEMENT EXECUTE FUNCTION launch_deny_mutation();

-- LA030: every transition of this transaction matches its subject's status at
-- commit, and a governed transition's request ended 'executed'.
CREATE FUNCTION launch_transitions_match_subject_at_commit() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
    v_req    text;
BEGIN
    IF NEW.subject_kind = 'tenant' THEN
        SELECT t.status INTO v_status FROM public.tenants t WHERE t.id = NEW.tenant_id;
    ELSE
        SELECT b.status INTO v_status FROM public.brands b WHERE b.id = NEW.brand_id;
    END IF;
    IF v_status IS DISTINCT FROM NEW.to_status THEN
        RAISE EXCEPTION 'launch transition % does not match the subject status at commit', NEW.id USING ERRCODE = 'LA030';
    END IF;
    IF NEW.kind = 'governed' THEN
        SELECT r.status INTO v_req FROM public.launch_authorisation_requests r WHERE r.id = NEW.request_id;
        IF v_req IS DISTINCT FROM 'executed' THEN
            RAISE EXCEPTION 'launch transition % has a request that did not end executed', NEW.id USING ERRCODE = 'LA030';
        END IF;
    END IF;
    RETURN NULL;
END
$$;

-- ---------------------------------------------------------------------------
-- 2. Widen both status CHECKs (no row is updated).
-- ---------------------------------------------------------------------------
ALTER TABLE tenants DROP CONSTRAINT tenants_status_check;
ALTER TABLE tenants ADD CONSTRAINT tenants_status_check
    CHECK (status IN ('pending_launch', 'active', 'suspended', 'closed'));
ALTER TABLE brands DROP CONSTRAINT brands_status_check;
ALTER TABLE brands ADD CONSTRAINT brands_status_check
    CHECK (status IN ('pending_launch', 'active', 'suspended', 'closed'));

-- ---------------------------------------------------------------------------
-- 3. S12: legacy_baseline backfill, BEFORE row-level security is enabled on the
--    new tables. The migration role is the owner of the new tables and RLS is not
--    yet enabled on them, so the INSERT is not filtered. Counts are asserted.
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    v_tenants bigint;
    v_brands  bigint;
    v_ins_t   bigint;
    v_ins_b   bigint;
    v_total   bigint;
BEGIN
    SELECT count(*) INTO v_tenants FROM tenants;
    SELECT count(*) INTO v_brands FROM brands;

    INSERT INTO launch_status_transitions (tenant_id, subject_kind, brand_id, kind, from_status, to_status, actor_type, txid)
    SELECT t.id, 'tenant', NULL, 'legacy_baseline', NULL, t.status, 'system', txid_current() FROM tenants t;
    GET DIAGNOSTICS v_ins_t = ROW_COUNT;

    INSERT INTO launch_status_transitions (tenant_id, subject_kind, brand_id, kind, from_status, to_status, actor_type, txid)
    SELECT b.tenant_id, 'brand', b.id, 'legacy_baseline', NULL, b.status, 'system', txid_current() FROM brands b;
    GET DIAGNOSTICS v_ins_b = ROW_COUNT;

    SELECT count(*) INTO v_total FROM launch_status_transitions WHERE kind = 'legacy_baseline';
    IF v_ins_t <> v_tenants OR v_ins_b <> v_brands OR v_total <> v_tenants + v_brands THEN
        RAISE EXCEPTION 'migration 0128: legacy_baseline backfill count mismatch (tenants % -> %, brands % -> %, rows %); refusing',
            v_tenants, v_ins_t, v_brands, v_ins_b, v_total;
    END IF;
END $$;

-- ---------------------------------------------------------------------------
-- 4. Row-level security, policies and grants (section 4.6). Per-command policies,
--    never FOR ALL; no NULL-arm policy; no new policy on brands (S6).
-- ---------------------------------------------------------------------------
ALTER TABLE launch_authorisation_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE launch_authorisation_requests FORCE ROW LEVEL SECURITY;
ALTER TABLE launch_authorisation_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE launch_authorisation_approvals FORCE ROW LEVEL SECURITY;
ALTER TABLE launch_status_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE launch_status_transitions FORCE ROW LEVEL SECURITY;

DO $$
DECLARE
    -- tenant principal shape, bound to the row's tenant
    t_pred constant text :=
        'tenant_id = NULLIF(current_setting(''app.tenant_id'', true), '''')::uuid '
        'AND NULLIF(current_setting(''app.principal_id'', true), '''') IS NOT NULL '
        'AND NULLIF(current_setting(''app.platform_admin_principal_id'', true), '''') IS NULL '
        'AND NULLIF(current_setting(''app.player_account_id'', true), '''') IS NULL '
        'AND NULLIF(current_setting(''app.platform_service_id'', true), '''') IS NULL '
        'AND NOT financial_acting_gucs_present()';
    -- platform principal shape
    p_pred constant text :=
        'NULLIF(current_setting(''app.platform_admin_principal_id'', true), '''') IS NOT NULL '
        'AND NULLIF(current_setting(''app.tenant_id'', true), '''') IS NULL '
        'AND NULLIF(current_setting(''app.player_account_id'', true), '''') IS NULL '
        'AND NULLIF(current_setting(''app.platform_service_id'', true), '''') IS NULL '
        'AND NOT financial_acting_gucs_present()';
    t text;
BEGIN
    -- requests: tenant SELECT/INSERT/UPDATE (cancel; own-brand suspension), platform SELECT/INSERT/UPDATE
    EXECUTE format('CREATE POLICY tenant_scope_select ON launch_authorisation_requests FOR SELECT USING (%s)', t_pred);
    EXECUTE format('CREATE POLICY tenant_scope_insert ON launch_authorisation_requests FOR INSERT WITH CHECK (%s)', t_pred);
    EXECUTE format('CREATE POLICY tenant_scope_update ON launch_authorisation_requests FOR UPDATE USING (%s) WITH CHECK (%s)', t_pred, t_pred);
    EXECUTE format('CREATE POLICY platform_scope_select ON launch_authorisation_requests FOR SELECT USING (%s)', p_pred);
    EXECUTE format('CREATE POLICY platform_scope_insert ON launch_authorisation_requests FOR INSERT WITH CHECK (%s)', p_pred);
    EXECUTE format('CREATE POLICY platform_scope_update ON launch_authorisation_requests FOR UPDATE USING (%s) WITH CHECK (%s)', p_pred, p_pred);
    -- approvals: tenant SELECT; platform SELECT/INSERT
    EXECUTE format('CREATE POLICY tenant_scope_select ON launch_authorisation_approvals FOR SELECT USING (%s)', t_pred);
    EXECUTE format('CREATE POLICY platform_scope_select ON launch_authorisation_approvals FOR SELECT USING (%s)', p_pred);
    EXECUTE format('CREATE POLICY platform_scope_insert ON launch_authorisation_approvals FOR INSERT WITH CHECK (%s)', p_pred);
    -- transitions: tenant SELECT; tenant INSERT only governed rows of a brand (S8); platform SELECT/INSERT
    EXECUTE format('CREATE POLICY tenant_scope_select ON launch_status_transitions FOR SELECT USING (%s)', t_pred);
    EXECUTE format('CREATE POLICY tenant_scope_insert ON launch_status_transitions FOR INSERT WITH CHECK ((%s) AND subject_kind = ''brand'' AND kind = ''governed'')', t_pred);
    EXECUTE format('CREATE POLICY platform_scope_select ON launch_status_transitions FOR SELECT USING (%s)', p_pred);
    EXECUTE format('CREATE POLICY platform_scope_insert ON launch_status_transitions FOR INSERT WITH CHECK ((%s) AND kind = ''governed'')', p_pred);
    -- S2/LF2: the two owner-only kinds. The guard enforces the same predicate; the
    -- policy is needed because FORCE RLS binds the table owner too. It admits only
    -- the table owner role itself (strict equality), never the runtime role.
    CREATE POLICY owner_provisioning_insert ON launch_status_transitions FOR INSERT
        WITH CHECK (kind IN ('legacy_baseline', 'owner_provisioned') AND from_status IS NULL
                    AND launch_is_table_owner() AND NOT financial_acting_gucs_present());
    -- ADR 0099 acting fence: an acting session has no access to any of the three tables.
    FOREACH t IN ARRAY ARRAY['launch_authorisation_requests', 'launch_authorisation_approvals', 'launch_status_transitions']
    LOOP
        EXECUTE format('CREATE POLICY acting_fence_select ON %I AS RESTRICTIVE FOR SELECT USING (NOT financial_acting_gucs_present())', t);
        EXECUTE format('CREATE POLICY acting_fence_insert ON %I AS RESTRICTIVE FOR INSERT WITH CHECK (NOT financial_acting_gucs_present())', t);
        EXECUTE format('CREATE POLICY acting_fence_update ON %I AS RESTRICTIVE FOR UPDATE USING (NOT financial_acting_gucs_present()) WITH CHECK (NOT financial_acting_gucs_present())', t);
        EXECUTE format('CREATE POLICY acting_fence_delete ON %I AS RESTRICTIVE FOR DELETE USING (NOT financial_acting_gucs_present())', t);
    END LOOP;
END $$;

-- Grants to igaming_runtime (mirrors deploy/init-app-role.sql): SELECT, INSERT on all
-- three; UPDATE only on the request state columns; no DELETE / TRUNCATE.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON launch_authorisation_requests FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON launch_authorisation_requests TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (status, decided_at, refusal_code, executing_txid) ON launch_authorisation_requests TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON launch_authorisation_approvals FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON launch_authorisation_approvals TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON launch_status_transitions FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON launch_status_transitions TO igaming_runtime';
    END IF;
END $$;

-- ---------------------------------------------------------------------------
-- 5. Defaults and the subject guards (only now).
-- ---------------------------------------------------------------------------
ALTER TABLE tenants ALTER COLUMN status SET DEFAULT 'pending_launch';
ALTER TABLE brands ALTER COLUMN status SET DEFAULT 'pending_launch';

-- LA021: a subject INSERT carries pending_launch unless the table owner provisions it.
CREATE FUNCTION launch_subject_insert_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM 'pending_launch' AND NOT public.launch_is_table_owner() THEN
        RAISE EXCEPTION '% rows are created pending_launch; a launch decision moves them (ADR 0112)', TG_TABLE_NAME
            USING ERRCODE = 'LA021';
    END IF;
    RETURN NEW;
END
$$;

-- LF2: an owner INSERT with another status writes an owner_provisioned transition.
-- AFTER INSERT so the subject row exists for the transition's foreign key.
CREATE FUNCTION launch_subject_owner_provisioned() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF TG_TABLE_NAME = 'tenants' THEN
        INSERT INTO public.launch_status_transitions (tenant_id, subject_kind, brand_id, kind, from_status, to_status, actor_type, txid)
        VALUES (NEW.id, 'tenant', NULL, 'owner_provisioned', NULL, NEW.status, 'system', txid_current());
    ELSE
        INSERT INTO public.launch_status_transitions (tenant_id, subject_kind, brand_id, kind, from_status, to_status, actor_type, txid)
        VALUES (NEW.tenant_id, 'brand', NEW.id, 'owner_provisioned', NULL, NEW.status, 'system', txid_current());
    END IF;
    RETURN NULL;
END
$$;

-- S10 / S2 / section 4.5: BEFORE UPDATE with NO column list; it acts only when the
-- status value changes. A change needs exactly one governed transition for this
-- subject in this transaction (from = OLD.status, to = NEW.status) whose request is
-- executing in this transaction; closed is never left. On brands, slice 2 replaces
-- this function to also take the per-brand gameplay lock exclusively (LF1).
CREATE FUNCTION launch_subject_status_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_kind        text;
    v_tenant      uuid;
    v_brand       uuid;
    v_prev_tenant text;
    v_n           bigint;
BEGIN
    IF NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'closed' THEN
        RAISE EXCEPTION '% status: closed is terminal', TG_TABLE_NAME USING ERRCODE = 'LA020';
    END IF;
    IF NOT public.launch_status_move_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION '% status: % -> % is not a legal move', TG_TABLE_NAME, OLD.status, NEW.status USING ERRCODE = 'LA020';
    END IF;
    IF TG_TABLE_NAME = 'tenants' THEN
        v_kind := 'tenant'; v_tenant := OLD.id; v_brand := NULL;
    ELSE
        v_kind := 'brand'; v_tenant := OLD.tenant_id; v_brand := OLD.id;
    END IF;

    -- S6 executor shape: a platform principal with app.tenant_id bound to the subject's
    -- own tenant (the brand_tenant_update technique). Reads of the decision records use
    -- the platform policies, so clear the tenant GUC for the reads and restore it.
    v_prev_tenant := NULLIF(current_setting('app.tenant_id', true), '');
    IF v_prev_tenant IS NOT NULL
       AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL THEN
        PERFORM set_config('app.tenant_id', '', true);
    ELSE
        v_prev_tenant := NULL;
    END IF;
    SELECT count(*) INTO v_n
      FROM public.launch_status_transitions t
      JOIN public.launch_authorisation_requests r ON r.id = t.request_id AND r.tenant_id = t.tenant_id
     WHERE t.tenant_id = v_tenant AND t.subject_kind = v_kind AND t.brand_id IS NOT DISTINCT FROM v_brand
       AND t.kind = 'governed' AND t.txid = txid_current()
       AND t.from_status = OLD.status AND t.to_status = NEW.status
       AND r.status = 'executing' AND r.executing_txid = txid_current()
       AND r.subject_kind = v_kind AND r.brand_id IS NOT DISTINCT FROM v_brand
       AND r.from_status = OLD.status AND r.to_status = NEW.status AND r.action <> 'ratify';
    IF v_prev_tenant IS NOT NULL THEN
        PERFORM set_config('app.tenant_id', v_prev_tenant, true);
    END IF;
    IF v_n <> 1 THEN
        RAISE EXCEPTION '% status change refused: no same-transaction governed transition of an executing request (ADR 0112)', TG_TABLE_NAME
            USING ERRCODE = 'LA020';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER zz_launch_status_insert_guard
    BEFORE INSERT ON tenants
    FOR EACH ROW EXECUTE FUNCTION launch_subject_insert_guard();
CREATE TRIGGER zz_launch_status_insert_guard
    BEFORE INSERT ON brands
    FOR EACH ROW EXECUTE FUNCTION launch_subject_insert_guard();
CREATE TRIGGER zz_launch_owner_provisioned
    AFTER INSERT ON tenants
    FOR EACH ROW WHEN (NEW.status <> 'pending_launch')
    EXECUTE FUNCTION launch_subject_owner_provisioned();
CREATE TRIGGER zz_launch_owner_provisioned
    AFTER INSERT ON brands
    FOR EACH ROW WHEN (NEW.status <> 'pending_launch')
    EXECUTE FUNCTION launch_subject_owner_provisioned();
CREATE TRIGGER zz_launch_status_governed
    BEFORE UPDATE ON tenants
    FOR EACH ROW EXECUTE FUNCTION launch_subject_status_guard();
CREATE TRIGGER zz_launch_status_governed
    BEFORE UPDATE ON brands
    FOR EACH ROW EXECUTE FUNCTION launch_subject_status_guard();

-- Installed last (after the backfill): the deferred commit check on transitions.
CREATE CONSTRAINT TRIGGER launch_transitions_match_subject_at_commit
    AFTER INSERT ON launch_status_transitions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION launch_transitions_match_subject_at_commit();
