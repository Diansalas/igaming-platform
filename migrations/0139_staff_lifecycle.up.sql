-- ADR 0119 slices S5 + S6 (staff lifecycle; merges together with S6, review C6). IDENTITY / AUTHORIZATION INTEGRITY
-- CONTROL: FOR `security` (live review), `architect` (ADR 0110 verifier extension), `identity-compliance`, `code-reviewer`
-- and `qa` REVIEW BEFORE MERGE (ADR 0119 s16 gate G).
--
-- NUMBERING: 0139 was reserved for this migration (ADR 0119 s13.2). It is applied after 0138 on a fresh database and,
-- on a database that already holds 0140-0142 / 0151, as the one missing version (the runner applies every missing
-- version in order). 0140-0142 do not replace actor_proof_require and do not touch alert_kinds_check1 or staff_users,
-- so the order does not change the result.
--
-- What it adds, and only this (ADR 0119 s9.5(b), s10, s13.2-13.6):
--   0. SET LOCAL lock_timeout = '5s'.
--   1. Pre-flight (SF095): refuses unless the schema is the one this file was written against (staff_users status CHECK,
--      no staff_version column, the eight staff_users RESTRICTIVE fences, no staff_actor_self_* policy, the 0135
--      actor_proof_require, no staff.platform_break_glass_used Kind).
--   2. staff_users: status gains 'invited' and 'offboarded'; password_hash becomes nullable with two CHECKs; new columns
--      staff_version (DEFAULT 1, set only by SF012), status_changed_at, offboarded_at, invited_by. Existing rows keep
--      their status and get staff_version 1.
--   3. staff_users guard triggers (BEFORE ROW, fire in name order; plain plpgsql, search_path pinned, NOT SECURITY
--      DEFINER):
--        sf_10_staff_transition    SF010  the 10.1 graph; INSERT only as 'invited' (no password) or 'active';
--                                         offboarded is terminal; status_changed_at / offboarded_at forced
--        sf_11_staff_immutable     SF011  id, tenant_id, email, created_at, invited_by
--        sf_12_staff_version       SF012  staff_version is 1 at INSERT and +1 on every role / status / password_hash /
--                                         person_id change; any other write to it is refused
--        sf_13_staff_self_target   SF013  a status or role change of the session's own principal (app.principal_id,
--                                         app.actor_staff_id, app.platform_admin_principal_id,
--                                         app.acting_platform_principal_id); the invitee's own invited -> active is
--                                         exempt. The GUCs are forgeable by SQL: defence in depth only.
--        sf_14_staff_person_req    SF014  platform_admin / tenant_admin / finance / compliance need person_id (INSERT,
--                                         role change)
--        sf_20_last_admin          SF020  the last active platform_admin with a usable factor (advisory lock
--                                         sf:last_admin:platform, VOLATILE, re-counted after the lock)
--                                  SF021  the same per tenant for tenant_admin, only when the acting session is
--                                         tenant-scoped (no acting / platform-admin GUC, and app.actor_staff_id does
--                                         not name a platform staff row)
--      SF030 is NOT created (S-H1, owner-deferred OQ-2(a)).
--   4. The ADR 0119 s9.5(b) policy pair on staff_users: staff_actor_self_select / staff_actor_self_lock (WITH CHECK
--      false), keyed by app.actor_staff_id. Its ONLY setter is db.SetActorStaffIDForCurrentTx (static test). It makes
--      a platform actor's own row visible (and FOR SHARE lockable, never updatable) inside a tenant-scoped transaction
--      (the lifecycle routes acting on a tenant target).
--   5. staff_lifecycle_requests / staff_lifecycle_approvals (platform four-eyes, s10.4): platform session only;
--      SF031 (content, forced columns, state machine), SF032 (approval: distinct staff AND Person from the requester and
--      the target, non-NULL Persons, pending and unexpired, payload match, the request locked FOR UPDATE here), SF033
--      (deferred: no request left executing at commit), zz_actor_proof_guard (ADR 0110, last).
--   6. staff_lifecycle_events (s10.6): append-only (SF099), tenant forced (SF001), dual-scope SELECT / INSERT, the
--      staff_users fences replicated in their strict form.
--   7. actor_proof_require: the 0135 body plus the closed staff_lifecycle:{request,approve,reject,cancel} set, scope
--      'platform' with a NULL tenant only (AP004 otherwise). Governed tables +2.
--   8. Alert Kind staff.platform_break_glass_used (P1, platform, subjectless; raised by cmd/seed-admin -break-glass).
--   9. Grants (mirrored by deploy/init-app-role.sql): staff_users UPDATE becomes column-level (status, role,
--      password_hash, status_changed_at, offboarded_at, person_id, display_name, updated_at; NEVER staff_version,
--      tenant_id, email, id, created_at, invited_by); the new tables SELECT + INSERT (requests: UPDATE (state,
--      decided_at, executing_txid, target_staff_id)). No DELETE / TRUNCATE.
--
-- SQLSTATEs added: SF010-SF014, SF020, SF021, SF031-SF033, SF094 (down refused), SF095 (pre-flight).

SET LOCAL lock_timeout = '5s';

-- =========================================================================
-- 1. Pre-flight (SF095)
-- =========================================================================
DO $$
DECLARE
    v_expected text[] := ARRAY[
        'acting_fence_delete', 'acting_fence_insert', 'acting_fence_select', 'acting_fence_update',
        'kyc_worker_fence_delete', 'kyc_worker_fence_insert', 'kyc_worker_fence_select', 'kyc_worker_fence_update'];
    v_found   text[];
    v_status  text;
    v_cols    bigint;
    v_self    bigint;
    v_apr     text;
    v_kind    bigint;
BEGIN
    SELECT pg_catalog.pg_get_constraintdef(c.oid) INTO v_status FROM pg_catalog.pg_constraint c
     WHERE c.conrelid = 'public.staff_users'::pg_catalog.regclass AND c.conname = 'staff_users_status_check';
    SELECT pg_catalog.count(*) INTO v_cols FROM pg_catalog.pg_attribute
     WHERE attrelid = 'public.staff_users'::pg_catalog.regclass AND NOT attisdropped
       AND attname IN ('staff_version', 'status_changed_at', 'offboarded_at', 'invited_by');
    SELECT COALESCE(pg_catalog.array_agg(policyname::text ORDER BY policyname), ARRAY[]::text[]) INTO v_found
      FROM pg_catalog.pg_policies WHERE schemaname = 'public' AND tablename = 'staff_users' AND permissive = 'RESTRICTIVE';
    SELECT pg_catalog.count(*) INTO v_self FROM pg_catalog.pg_policies
     WHERE schemaname = 'public' AND tablename = 'staff_users' AND policyname LIKE 'staff\_actor\_self\_%';
    SELECT p.prosrc INTO v_apr FROM pg_catalog.pg_proc p
     WHERE p.oid = pg_catalog.to_regprocedure('public.actor_proof_require(uuid, text, uuid, text, text, text)');
    SELECT pg_catalog.count(*) INTO v_kind FROM public.alert_kinds WHERE kind = 'staff.platform_break_glass_used';
    IF v_status IS DISTINCT FROM 'CHECK ((status = ANY (ARRAY[''active''::text, ''suspended''::text])))'
       OR v_cols <> 0 OR v_found IS DISTINCT FROM v_expected OR v_self <> 0 OR v_apr IS NULL
       OR v_apr NOT LIKE '%rg\_limit\_config:%' OR v_apr LIKE '%staff\_lifecycle%' OR v_kind <> 0
       OR to_regclass('public.staff_lifecycle_events') IS NOT NULL THEN
        RAISE EXCEPTION 'migration 0139 pre-flight: the schema is not the one this migration was written against (ADR 0119 s13.2): status_check=%, lifecycle_columns=%, restrictive_fences=%, staff_actor_self_policies=%, actor_proof_require_present=%, break_glass_kind=%. Nothing was changed.',
            v_status, v_cols, v_found, v_self, v_apr IS NOT NULL, v_kind USING ERRCODE = 'SF095';
    END IF;
END
$$;

-- =========================================================================
-- 2. staff_users columns and CHECKs
-- =========================================================================
ALTER TABLE staff_users DROP CONSTRAINT staff_users_status_check;
ALTER TABLE staff_users ADD CONSTRAINT staff_users_status_check
    CHECK (status IN ('invited', 'active', 'suspended', 'offboarded'));
ALTER TABLE staff_users ALTER COLUMN password_hash DROP NOT NULL;
ALTER TABLE staff_users ADD CONSTRAINT staff_users_password_state
    CHECK (password_hash IS NOT NULL OR status IN ('invited', 'offboarded'));
ALTER TABLE staff_users ADD CONSTRAINT staff_users_offboarded_no_password
    CHECK (status <> 'offboarded' OR password_hash IS NULL);
ALTER TABLE staff_users ADD COLUMN staff_version BIGINT NOT NULL DEFAULT 1
    CONSTRAINT staff_users_staff_version_positive CHECK (staff_version >= 1);
ALTER TABLE staff_users ADD COLUMN status_changed_at TIMESTAMPTZ NULL;
ALTER TABLE staff_users ADD COLUMN offboarded_at TIMESTAMPTZ NULL;
ALTER TABLE staff_users ADD COLUMN invited_by UUID NULL REFERENCES staff_users (id);
ALTER TABLE staff_users ADD CONSTRAINT staff_users_offboarded_at_state
    CHECK ((status = 'offboarded') = (offboarded_at IS NOT NULL));

COMMENT ON COLUMN staff_users.staff_version IS 'ADR 0119 s7.1 / s13.2: bumped by SF012 on every role, status, password_hash or person_id change; carried as the sv claim and compared by liveness and the s9.5 re-assertion. Never written directly.';

-- =========================================================================
-- 3. staff_users guard triggers
-- =========================================================================

-- SF010: the ADR 0119 s10.1 graph.
CREATE FUNCTION sf_staff_transition() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status IS NULL OR NEW.status NOT IN ('invited', 'active') THEN
            RAISE EXCEPTION 'staff_users: a staff member is created invited or active (SF010)' USING ERRCODE = 'SF010';
        END IF;
        IF NEW.status = 'invited' AND NEW.password_hash IS NOT NULL THEN
            RAISE EXCEPTION 'staff_users: an invited staff member has no password (SF010)' USING ERRCODE = 'SF010';
        END IF;
        NEW.status_changed_at := NULL;
        NEW.offboarded_at := NULL;
        RETURN NEW;
    END IF;
    IF OLD.status = 'offboarded' AND (NEW.status IS DISTINCT FROM OLD.status OR NEW.role IS DISTINCT FROM OLD.role
       OR NEW.password_hash IS DISTINCT FROM OLD.password_hash OR NEW.person_id IS DISTINCT FROM OLD.person_id) THEN
        RAISE EXCEPTION 'staff_users: offboarded is terminal (SF010)' USING ERRCODE = 'SF010';
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NOT ((OLD.status = 'invited' AND NEW.status IN ('active', 'offboarded'))
                OR (OLD.status = 'active' AND NEW.status IN ('suspended', 'offboarded'))
                OR (OLD.status = 'suspended' AND NEW.status IN ('active', 'offboarded'))) THEN
            RAISE EXCEPTION 'staff_users: % -> % is not a legal staff transition (SF010)', OLD.status, NEW.status
                USING ERRCODE = 'SF010';
        END IF;
        NEW.status_changed_at := pg_catalog.now();
        IF NEW.status = 'offboarded' THEN
            NEW.offboarded_at := pg_catalog.now();
        END IF;
    ELSIF NEW.status_changed_at IS DISTINCT FROM OLD.status_changed_at OR NEW.offboarded_at IS DISTINCT FROM OLD.offboarded_at THEN
        RAISE EXCEPTION 'staff_users: status_changed_at and offboarded_at move only with the status (SF010)' USING ERRCODE = 'SF010';
    END IF;
    IF NEW.role IS DISTINCT FROM OLD.role AND NOT (OLD.status = 'active' AND NEW.status = 'active') THEN
        RAISE EXCEPTION 'staff_users: the role changes only on an active account (SF010)' USING ERRCODE = 'SF010';
    END IF;
    IF NEW.password_hash IS DISTINCT FROM OLD.password_hash
       AND NOT ((OLD.status = 'invited' AND NEW.status = 'active' AND NEW.password_hash IS NOT NULL)
                OR (OLD.status = 'active' AND NEW.status = 'active' AND NEW.password_hash IS NOT NULL)
                OR (NEW.status = 'offboarded' AND NEW.password_hash IS NULL)) THEN
        RAISE EXCEPTION 'staff_users: the password changes only at acceptance, on an active account, or is removed at offboarding (SF010)'
            USING ERRCODE = 'SF010';
    END IF;
    RETURN NEW;
END
$$;

-- SF011: identity columns are immutable.
CREATE FUNCTION sf_staff_immutable() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.email IS DISTINCT FROM OLD.email
       OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.invited_by IS DISTINCT FROM OLD.invited_by THEN
        RAISE EXCEPTION 'staff_users: id, tenant_id, email, created_at and invited_by are immutable (SF011)' USING ERRCODE = 'SF011';
    END IF;
    RETURN NEW;
END
$$;

-- SF012: staff_version is set by this trigger only.
CREATE FUNCTION sf_staff_version() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.staff_version IS DISTINCT FROM 1 THEN
            RAISE EXCEPTION 'staff_users: staff_version starts at 1 (SF012)' USING ERRCODE = 'SF012';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.staff_version IS DISTINCT FROM OLD.staff_version THEN
        RAISE EXCEPTION 'staff_users: staff_version is never written directly (SF012)' USING ERRCODE = 'SF012';
    END IF;
    IF NEW.role IS DISTINCT FROM OLD.role OR NEW.status IS DISTINCT FROM OLD.status
       OR NEW.password_hash IS DISTINCT FROM OLD.password_hash OR NEW.person_id IS DISTINCT FROM OLD.person_id THEN
        NEW.staff_version := OLD.staff_version + 1;
    END IF;
    RETURN NEW;
END
$$;

-- SF013: no self-targeted status or role change (defence in depth; the GUCs are forgeable by SQL).
CREATE FUNCTION sf_staff_self_target() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF (NEW.status IS DISTINCT FROM OLD.status OR NEW.role IS DISTINCT FROM OLD.role)
       AND NOT (OLD.status = 'invited' AND NEW.status = 'active')
       AND NEW.id::text = ANY (ARRAY[
             NULLIF(pg_catalog.current_setting('app.principal_id', true), ''),
             NULLIF(pg_catalog.current_setting('app.actor_staff_id', true), ''),
             NULLIF(pg_catalog.current_setting('app.platform_admin_principal_id', true), ''),
             NULLIF(pg_catalog.current_setting('app.acting_platform_principal_id', true), '')]) THEN
        RAISE EXCEPTION 'staff_users: a staff member may not change their own status or role (SF013)' USING ERRCODE = 'SF013';
    END IF;
    RETURN NEW;
END
$$;

-- SF014: the approve-capable roles need a Person (ADR 0119 s9.4).
CREATE FUNCTION sf_staff_person_required() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF NEW.role IN ('platform_admin', 'tenant_admin', 'finance', 'compliance') AND NEW.person_id IS NULL
       AND (TG_OP = 'INSERT' OR NEW.role IS DISTINCT FROM OLD.role) THEN
        RAISE EXCEPTION 'staff_users: role % requires a linked Person (SF014)', NEW.role USING ERRCODE = 'SF014';
    END IF;
    RETURN NEW;
END
$$;

-- SF020 / SF021: last-admin protection. VOLATILE (the default): the count after the advisory lock takes a fresh
-- snapshot (READ COMMITTED), so two admins removing each other at the same moment are serialised and the second
-- re-counts after the first commits. "Usable factor" is the liveness rule (an active factor that is not hard-locked).
CREATE FUNCTION sf_last_admin() RETURNS trigger
    LANGUAGE plpgsql VOLATILE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_others   bigint;
    v_actor    text := NULLIF(pg_catalog.current_setting('app.actor_staff_id', true), '');
    v_platform boolean;
BEGIN
    IF OLD.status <> 'active' OR (NEW.status = 'active' AND NEW.role = OLD.role) THEN
        RETURN NEW;
    END IF;
    IF OLD.tenant_id IS NULL AND OLD.role = 'platform_admin' THEN
        PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtext('sf:last_admin:platform'));
        SELECT pg_catalog.count(*) INTO v_others FROM public.staff_users su
         WHERE su.tenant_id IS NULL AND su.role = 'platform_admin' AND su.status = 'active' AND su.id <> OLD.id
           AND EXISTS (SELECT 1 FROM public.staff_mfa_factors f
                        WHERE f.staff_user_id = su.id AND f.status = 'active' AND f.hard_locked_at IS NULL);
        IF v_others = 0 THEN
            RAISE EXCEPTION 'staff_users: the last active, enrolled platform admin cannot be removed (SF020)' USING ERRCODE = 'SF020';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.tenant_id IS NOT NULL AND OLD.role = 'tenant_admin' THEN
        v_platform := NULLIF(pg_catalog.current_setting('app.acting_platform_principal_id', true), '') IS NOT NULL
            OR NULLIF(pg_catalog.current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
            OR (v_actor IS NOT NULL AND EXISTS (SELECT 1 FROM public.staff_users a WHERE a.id = v_actor::uuid AND a.tenant_id IS NULL));
        IF v_platform THEN
            RETURN NEW;
        END IF;
        PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtext('sf:last_admin:' || OLD.tenant_id::text));
        SELECT pg_catalog.count(*) INTO v_others FROM public.staff_users su
         WHERE su.tenant_id = OLD.tenant_id AND su.role = 'tenant_admin' AND su.status = 'active' AND su.id <> OLD.id
           AND EXISTS (SELECT 1 FROM public.staff_mfa_factors f
                        WHERE f.staff_user_id = su.id AND f.status = 'active' AND f.hard_locked_at IS NULL);
        IF v_others = 0 THEN
            RAISE EXCEPTION 'staff_users: the last active, enrolled tenant admin of the tenant cannot be removed by a tenant-scoped actor (SF021)'
                USING ERRCODE = 'SF021';
        END IF;
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER sf_10_staff_transition BEFORE INSERT OR UPDATE ON staff_users
    FOR EACH ROW EXECUTE FUNCTION sf_staff_transition();
CREATE TRIGGER sf_11_staff_immutable BEFORE UPDATE ON staff_users
    FOR EACH ROW EXECUTE FUNCTION sf_staff_immutable();
CREATE TRIGGER sf_12_staff_version BEFORE INSERT OR UPDATE ON staff_users
    FOR EACH ROW EXECUTE FUNCTION sf_staff_version();
CREATE TRIGGER sf_13_staff_self_target BEFORE UPDATE ON staff_users
    FOR EACH ROW EXECUTE FUNCTION sf_staff_self_target();
CREATE TRIGGER sf_14_staff_person_required BEFORE INSERT OR UPDATE ON staff_users
    FOR EACH ROW EXECUTE FUNCTION sf_staff_person_required();
CREATE TRIGGER sf_20_last_admin BEFORE UPDATE OF status, role ON staff_users
    FOR EACH ROW EXECUTE FUNCTION sf_last_admin();

-- =========================================================================
-- 4. The s9.5(b) actor self-visibility pair (only setter: db.SetActorStaffIDForCurrentTx)
-- =========================================================================
CREATE POLICY staff_actor_self_select ON staff_users FOR SELECT
    USING (id = NULLIF(current_setting('app.actor_staff_id', true), '')::uuid);
CREATE POLICY staff_actor_self_lock ON staff_users FOR UPDATE
    USING (id = NULLIF(current_setting('app.actor_staff_id', true), '')::uuid) WITH CHECK (false);

-- =========================================================================
-- 5. Platform four-eyes: staff_lifecycle_requests / staff_lifecycle_approvals (s10.4)
-- =========================================================================
CREATE TABLE staff_lifecycle_requests (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind                TEXT NOT NULL CHECK (kind IN ('platform_invite', 'platform_reactivate', 'platform_mfa_reset', 'platform_password_reset')),
    target_staff_id     UUID NULL REFERENCES staff_users (id),
    invite_email        TEXT NULL CHECK (invite_email IS NULL OR (invite_email = lower(invite_email) AND char_length(invite_email) BETWEEN 3 AND 320
                                         AND invite_email !~ '[[:cntrl:][:space:]]')),
    invite_person_id    UUID NULL REFERENCES persons (id),
    reason_code         TEXT NULL,
    note                TEXT NULL CHECK (note IS NULL OR (char_length(note) BETWEEN 10 AND 500 AND note !~ '[[:cntrl:]]'
                                         AND note !~ E'[‪-‮⁦-⁩​-‏⁠-⁤﻿؜]')),
    requested_by        UUID NOT NULL REFERENCES staff_users (id),
    requested_by_person UUID NOT NULL,
    required_approvals  SMALLINT NOT NULL DEFAULT 1 CHECK (required_approvals = 1),
    payload_hash        TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    state               TEXT NOT NULL DEFAULT 'pending'
                        CHECK (state IN ('pending', 'executing', 'executed', 'rejected', 'cancelled', 'expired', 'refused_at_execution')),
    executing_txid      BIGINT NULL,
    expires_at          TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL,
    decided_at          TIMESTAMPTZ NULL,
    CONSTRAINT staff_lifecycle_requests_reason CHECK (
        (kind = 'platform_invite' AND reason_code IS NULL)
        OR (kind = 'platform_reactivate' AND reason_code IN ('investigation_closed', 'returned_from_leave', 'error_correction', 'other'))
        OR (kind IN ('platform_mfa_reset', 'platform_password_reset')
            AND reason_code IN ('device_lost', 'device_replaced', 'hard_locked', 'security_compromise', 'other'))),
    CONSTRAINT staff_lifecycle_requests_note_other CHECK (reason_code IS DISTINCT FROM 'other' OR note IS NOT NULL),
    CONSTRAINT staff_lifecycle_requests_shape CHECK (
        (kind = 'platform_invite' AND invite_email IS NOT NULL AND invite_person_id IS NOT NULL)
        OR (kind <> 'platform_invite' AND target_staff_id IS NOT NULL AND invite_email IS NULL AND invite_person_id IS NULL)),
    CONSTRAINT staff_lifecycle_requests_decided CHECK ((state = 'pending') = (decided_at IS NULL)),
    CONSTRAINT staff_lifecycle_requests_executing CHECK (state NOT IN ('executing', 'executed', 'refused_at_execution') OR executing_txid IS NOT NULL)
);
CREATE INDEX idx_staff_lifecycle_requests_state ON staff_lifecycle_requests (state, created_at DESC);
CREATE INDEX idx_staff_lifecycle_requests_target ON staff_lifecycle_requests (target_staff_id) WHERE target_staff_id IS NOT NULL;

CREATE TABLE staff_lifecycle_approvals (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id        UUID NOT NULL REFERENCES staff_lifecycle_requests (id),
    decided_by        UUID NOT NULL REFERENCES staff_users (id),
    decided_by_person UUID NOT NULL,
    decision          TEXT NOT NULL CHECK (decision IN ('approve', 'reject')),
    reason_code       TEXT NULL CHECK (reason_code IS NULL OR reason_code ~ '^[a-z_]{1,64}$'),
    payload_hash      TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    decided_txid      BIGINT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL,
    CONSTRAINT staff_lifecycle_approvals_one_per_person UNIQUE (request_id, decided_by_person)
);

-- What the requester's proof binds: the request content (s10.4: kind, target, email, Person, reason code, note).
CREATE FUNCTION staff_lifecycle_request_digest(p_kind text, p_target uuid, p_email text, p_person uuid, p_reason text, p_note text)
    RETURNS text
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT public.k2_sha256_hex(public.k2_canonical(p_kind, p_target::text, p_email, p_person::text, p_reason, p_note))
$$;

-- The session actor of the four-eyes tables: exactly a platform principal session (app.platform_admin_principal_id,
-- no tenant / principal / player / service / acting GUC), an ACTIVE platform staff row. person_id may be NULL here;
-- each guard refuses a NULL Person itself.
CREATE FUNCTION sf_lifecycle_actor_session(p_code text, OUT actor uuid, OUT person_id uuid)
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_platform text := NULLIF(pg_catalog.current_setting('app.platform_admin_principal_id', true), '');
    rec RECORD;
BEGIN
    IF v_platform IS NULL
       OR NULLIF(pg_catalog.current_setting('app.tenant_id', true), '') IS NOT NULL
       OR NULLIF(pg_catalog.current_setting('app.player_account_id', true), '') IS NOT NULL
       OR NULLIF(pg_catalog.current_setting('app.platform_service_id', true), '') IS NOT NULL
       OR NULLIF(pg_catalog.current_setting('app.acting_tenant_id', true), '') IS NOT NULL
       OR NULLIF(pg_catalog.current_setting('app.acting_platform_principal_id', true), '') IS NOT NULL THEN
        IF p_code = 'SF032' THEN
            RAISE EXCEPTION 'staff lifecycle: only a platform principal session may decide (SF032)' USING ERRCODE = 'SF032';
        END IF;
        RAISE EXCEPTION 'staff lifecycle: only a platform principal session may write a request (SF031)' USING ERRCODE = 'SF031';
    END IF;
    SELECT su.id, su.person_id, su.status, su.tenant_id INTO rec FROM public.staff_users su WHERE su.id = v_platform::uuid;
    IF NOT FOUND OR rec.tenant_id IS NOT NULL OR rec.status <> 'active' THEN
        IF p_code = 'SF032' THEN
            RAISE EXCEPTION 'staff lifecycle: the platform principal is not an active platform staff member (SF032)' USING ERRCODE = 'SF032';
        END IF;
        RAISE EXCEPTION 'staff lifecycle: the platform principal is not an active platform staff member (SF031)' USING ERRCODE = 'SF031';
    END IF;
    actor := rec.id;
    person_id := rec.person_id;
END
$$;

-- SF031: request content, forced columns and the state machine; DELETE is SF099.
CREATE FUNCTION sf_lifecycle_request_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor RECORD;
    v_hash  text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'staff_lifecycle_requests is append-only: DELETE is not permitted (SF099)' USING ERRCODE = 'SF099';
    END IF;
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO v_actor FROM public.sf_lifecycle_actor_session('SF031');
        IF v_actor.person_id IS NULL THEN
            RAISE EXCEPTION 'staff_lifecycle_requests: the requester has no linked Person (SF031)' USING ERRCODE = 'SF031';
        END IF;
        IF NEW.target_staff_id IS NOT NULL THEN
            IF NEW.target_staff_id = v_actor.actor THEN
                RAISE EXCEPTION 'staff_lifecycle_requests: the requester may not target their own account (SF031)' USING ERRCODE = 'SF031';
            END IF;
            IF NOT EXISTS (SELECT 1 FROM public.staff_users su WHERE su.id = NEW.target_staff_id AND su.tenant_id IS NULL) THEN
                RAISE EXCEPTION 'staff_lifecycle_requests: the target is not a platform staff member (SF031)' USING ERRCODE = 'SF031';
            END IF;
        END IF;
        IF NEW.kind = 'platform_invite' AND NEW.invite_person_id = v_actor.person_id THEN
            RAISE EXCEPTION 'staff_lifecycle_requests: the invitee Person may not be the requester''s Person (SF031)' USING ERRCODE = 'SF031';
        END IF;
        v_hash := public.staff_lifecycle_request_digest(NEW.kind, NEW.target_staff_id, NEW.invite_email, NEW.invite_person_id,
                                                         NEW.reason_code, NEW.note);
        IF NEW.payload_hash IS NOT NULL AND NEW.payload_hash <> v_hash THEN
            RAISE EXCEPTION 'staff_lifecycle_requests: payload_hash is not the digest of the request content (SF031)' USING ERRCODE = 'SF031';
        END IF;
        NEW.payload_hash := v_hash;
        NEW.requested_by := v_actor.actor;
        NEW.requested_by_person := v_actor.person_id;
        NEW.required_approvals := 1;
        NEW.state := 'pending';
        NEW.executing_txid := NULL;
        NEW.decided_at := NULL;
        NEW.created_at := pg_catalog.clock_timestamp();
        NEW.expires_at := NEW.created_at + interval '72 hours';
        IF NEW.id IS NULL THEN
            NEW.id := gen_random_uuid();
        END IF;
        RETURN NEW;
    END IF;
    -- UPDATE: the content never changes (an invite's target is set once, at execution).
    IF NEW.id <> OLD.id OR NEW.kind <> OLD.kind OR NEW.invite_email IS DISTINCT FROM OLD.invite_email
       OR NEW.invite_person_id IS DISTINCT FROM OLD.invite_person_id OR NEW.reason_code IS DISTINCT FROM OLD.reason_code
       OR NEW.note IS DISTINCT FROM OLD.note OR NEW.requested_by <> OLD.requested_by
       OR NEW.requested_by_person <> OLD.requested_by_person OR NEW.required_approvals <> OLD.required_approvals
       OR NEW.payload_hash <> OLD.payload_hash OR NEW.expires_at <> OLD.expires_at OR NEW.created_at <> OLD.created_at
       OR (NEW.target_staff_id IS DISTINCT FROM OLD.target_staff_id
           AND NOT (OLD.kind = 'platform_invite' AND OLD.target_staff_id IS NULL AND OLD.state = 'executing'
                    AND NEW.state = 'executed' AND OLD.executing_txid = pg_catalog.txid_current())) THEN
        RAISE EXCEPTION 'staff_lifecycle_requests: the request content is immutable (SF031)' USING ERRCODE = 'SF031';
    END IF;
    IF NEW.state = OLD.state THEN
        IF NEW.decided_at IS DISTINCT FROM OLD.decided_at OR NEW.executing_txid IS DISTINCT FROM OLD.executing_txid THEN
            RAISE EXCEPTION 'staff_lifecycle_requests: decided_at and executing_txid move only with the state (SF031)' USING ERRCODE = 'SF031';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.state = 'pending' AND NEW.state = 'executing' THEN
        IF NOT EXISTS (SELECT 1 FROM public.staff_lifecycle_approvals a
                        WHERE a.request_id = OLD.id AND a.decision = 'approve' AND a.decided_txid = pg_catalog.txid_current()) THEN
            RAISE EXCEPTION 'staff_lifecycle_requests: executing needs an approval in this transaction (SF031)' USING ERRCODE = 'SF031';
        END IF;
        NEW.executing_txid := pg_catalog.txid_current();
        NEW.decided_at := pg_catalog.clock_timestamp();
        RETURN NEW;
    END IF;
    IF OLD.state = 'executing' AND NEW.state IN ('executed', 'refused_at_execution') THEN
        IF OLD.executing_txid IS DISTINCT FROM pg_catalog.txid_current() THEN
            RAISE EXCEPTION 'staff_lifecycle_requests: the execution belongs to another transaction (SF031)' USING ERRCODE = 'SF031';
        END IF;
        NEW.executing_txid := OLD.executing_txid;
        NEW.decided_at := OLD.decided_at;
        RETURN NEW;
    END IF;
    IF OLD.state = 'pending' AND NEW.state = 'rejected' THEN
        IF NOT EXISTS (SELECT 1 FROM public.staff_lifecycle_approvals a
                        WHERE a.request_id = OLD.id AND a.decision = 'reject' AND a.decided_txid = pg_catalog.txid_current()) THEN
            RAISE EXCEPTION 'staff_lifecycle_requests: a rejection needs a reject decision in this transaction (SF031)' USING ERRCODE = 'SF031';
        END IF;
        NEW.executing_txid := NULL;
        NEW.decided_at := pg_catalog.clock_timestamp();
        RETURN NEW;
    END IF;
    IF OLD.state = 'pending' AND NEW.state = 'cancelled' THEN
        SELECT * INTO v_actor FROM public.sf_lifecycle_actor_session('SF031');
        IF v_actor.actor <> OLD.requested_by THEN
            RAISE EXCEPTION 'staff_lifecycle_requests: only the requester may cancel (SF031)' USING ERRCODE = 'SF031';
        END IF;
        NEW.executing_txid := NULL;
        NEW.decided_at := pg_catalog.clock_timestamp();
        RETURN NEW;
    END IF;
    IF OLD.state = 'pending' AND NEW.state = 'expired' AND pg_catalog.clock_timestamp() >= OLD.expires_at THEN
        NEW.executing_txid := NULL;
        NEW.decided_at := pg_catalog.clock_timestamp();
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'staff_lifecycle_requests: % -> % is not a legal move (SF031)', OLD.state, NEW.state USING ERRCODE = 'SF031';
END
$$;

-- SF032: the approval (decision) row. The request is locked FOR UPDATE here, in this transaction.
CREATE FUNCTION sf_lifecycle_approval_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor  RECORD;
    v_req    public.staff_lifecycle_requests%ROWTYPE;
    v_target uuid;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'staff_lifecycle_approvals is append-only: % is not permitted (SF099)', TG_OP USING ERRCODE = 'SF099';
    END IF;
    SELECT * INTO v_actor FROM public.sf_lifecycle_actor_session('SF032');
    IF v_actor.person_id IS NULL THEN
        RAISE EXCEPTION 'staff_lifecycle_approvals: the approver has no linked Person (SF032)' USING ERRCODE = 'SF032';
    END IF;
    SELECT * INTO v_req FROM public.staff_lifecycle_requests r WHERE r.id = NEW.request_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'staff_lifecycle_approvals: the request does not exist (SF032)' USING ERRCODE = 'SF032';
    END IF;
    IF v_req.state <> 'pending' OR pg_catalog.clock_timestamp() >= v_req.expires_at THEN
        RAISE EXCEPTION 'staff_lifecycle_approvals: the request is not pending (SF032)' USING ERRCODE = 'SF032';
    END IF;
    IF v_actor.actor = v_req.requested_by OR v_actor.person_id = v_req.requested_by_person THEN
        RAISE EXCEPTION 'staff_lifecycle_approvals: the requester may not decide (staff id or Person) (SF032)' USING ERRCODE = 'SF032';
    END IF;
    IF v_req.target_staff_id IS NOT NULL THEN
        IF v_actor.actor = v_req.target_staff_id THEN
            RAISE EXCEPTION 'staff_lifecycle_approvals: the target may not decide (SF032)' USING ERRCODE = 'SF032';
        END IF;
        SELECT su.person_id INTO v_target FROM public.staff_users su WHERE su.id = v_req.target_staff_id;
        IF v_target IS NOT NULL AND v_target = v_actor.person_id THEN
            RAISE EXCEPTION 'staff_lifecycle_approvals: the target''s Person may not decide (SF032)' USING ERRCODE = 'SF032';
        END IF;
    END IF;
    IF v_req.invite_person_id IS NOT NULL AND v_req.invite_person_id = v_actor.person_id THEN
        RAISE EXCEPTION 'staff_lifecycle_approvals: the invitee''s Person may not decide (SF032)' USING ERRCODE = 'SF032';
    END IF;
    IF NEW.payload_hash IS DISTINCT FROM v_req.payload_hash THEN
        RAISE EXCEPTION 'staff_lifecycle_approvals: payload_hash does not match the request (SF032)' USING ERRCODE = 'SF032';
    END IF;
    IF EXISTS (SELECT 1 FROM public.staff_lifecycle_approvals a
                WHERE a.request_id = NEW.request_id AND a.decided_by_person = v_actor.person_id) THEN
        RAISE EXCEPTION 'staff_lifecycle_approvals: this Person already decided the request (SF032)' USING ERRCODE = 'SF032';
    END IF;
    NEW.decided_by := v_actor.actor;
    NEW.decided_by_person := v_actor.person_id;
    NEW.decided_txid := pg_catalog.txid_current();
    NEW.created_at := pg_catalog.clock_timestamp();
    IF NEW.id IS NULL THEN
        NEW.id := gen_random_uuid();
    END IF;
    RETURN NEW;
END
$$;

-- SF033: no request is left executing at commit.
CREATE FUNCTION sf_lifecycle_no_executing_at_commit() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.staff_lifecycle_requests r WHERE r.id = NEW.id AND r.state = 'executing') THEN
        RAISE EXCEPTION 'staff_lifecycle_requests: a request may not be left executing at commit (SF033)' USING ERRCODE = 'SF033';
    END IF;
    RETURN NULL;
END
$$;

CREATE FUNCTION sf_deny_update() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION '%: append-only, UPDATE is not permitted (SF099)', TG_TABLE_NAME USING ERRCODE = 'SF099';
END
$$;

-- ADR 0110: the actor-proof guards (last BEFORE trigger on each governed table).
CREATE FUNCTION actor_proof_staff_lifecycle_requests_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor RECORD;
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM public.actor_proof_require(NEW.requested_by, 'platform', NULL, 'staff_lifecycle:request', 'new', NEW.payload_hash);
    ELSIF OLD.state = 'pending' AND NEW.state = 'cancelled' THEN
        SELECT * INTO v_actor FROM public.sf_lifecycle_actor_session('SF031');
        PERFORM public.actor_proof_require(v_actor.actor, 'platform', NULL, 'staff_lifecycle:cancel', OLD.id::text, OLD.payload_hash);
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION actor_proof_staff_lifecycle_approvals_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    PERFORM public.actor_proof_require(NEW.decided_by, 'platform', NULL, 'staff_lifecycle:' || NEW.decision,
        NEW.request_id::text, NEW.payload_hash);
    RETURN NEW;
END
$$;

CREATE TRIGGER sf_31_lifecycle_request_guard BEFORE INSERT OR UPDATE OR DELETE ON staff_lifecycle_requests
    FOR EACH ROW EXECUTE FUNCTION sf_lifecycle_request_guard();
CREATE TRIGGER sf_no_truncate BEFORE TRUNCATE ON staff_lifecycle_requests
    FOR EACH STATEMENT EXECUTE FUNCTION sf_deny_truncate();
CREATE TRIGGER zz_actor_proof_guard BEFORE INSERT OR UPDATE ON staff_lifecycle_requests
    FOR EACH ROW EXECUTE FUNCTION actor_proof_staff_lifecycle_requests_guard();
CREATE CONSTRAINT TRIGGER sf_33_lifecycle_no_executing_at_commit
    AFTER INSERT OR UPDATE ON staff_lifecycle_requests
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION sf_lifecycle_no_executing_at_commit();

CREATE TRIGGER sf_32_lifecycle_approval_guard BEFORE INSERT OR UPDATE OR DELETE ON staff_lifecycle_approvals
    FOR EACH ROW EXECUTE FUNCTION sf_lifecycle_approval_guard();
CREATE TRIGGER sf_no_truncate BEFORE TRUNCATE ON staff_lifecycle_approvals
    FOR EACH STATEMENT EXECUTE FUNCTION sf_deny_truncate();
CREATE TRIGGER zz_actor_proof_guard BEFORE INSERT ON staff_lifecycle_approvals
    FOR EACH ROW EXECUTE FUNCTION actor_proof_staff_lifecycle_approvals_guard();

ALTER TABLE staff_lifecycle_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_lifecycle_requests FORCE ROW LEVEL SECURITY;
ALTER TABLE staff_lifecycle_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_lifecycle_approvals FORCE ROW LEVEL SECURITY;

-- Platform principal session only; no tenant arm (s13.3).
DO $$
DECLARE
    t    text;
    pred text := $p$NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()$p$;
BEGIN
    FOREACH t IN ARRAY ARRAY['staff_lifecycle_requests', 'staff_lifecycle_approvals'] LOOP
        EXECUTE format('CREATE POLICY platform_only_select ON %I FOR SELECT USING (%s)', t, pred);
        EXECUTE format('CREATE POLICY platform_only_insert ON %I FOR INSERT WITH CHECK (%s)', t, pred);
    END LOOP;
    EXECUTE format('CREATE POLICY platform_only_update ON staff_lifecycle_requests FOR UPDATE USING (%s) WITH CHECK (%s)', pred, pred);
END
$$;

-- =========================================================================
-- 6. staff_lifecycle_events (s10.6)
-- =========================================================================
CREATE TABLE staff_lifecycle_events (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NULL REFERENCES tenants (id),
    staff_user_id        UUID NOT NULL REFERENCES staff_users (id),
    event                TEXT NOT NULL CHECK (event IN ('invited', 'invite_revoked', 'invite_accepted', 'suspended', 'reactivated',
                             'offboarded', 'role_changed', 'person_linked', 'mfa_enrolled', 'mfa_reset', 'mfa_hard_locked',
                             'mfa_recovery_used', 'recovery_codes_regenerated', 'owner_provisioned')),
    actor_type           TEXT NOT NULL CHECK (actor_type IN ('staff', 'system', 'owner')),
    actor_staff_id       UUID NULL REFERENCES staff_users (id),
    actor_person_id      UUID NULL,
    before               JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(before) = 'object' AND octet_length(before::text) <= 4096),
    after                JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(after) = 'object' AND octet_length(after::text) <= 4096),
    reason_code          TEXT NULL CHECK (reason_code IS NULL OR reason_code IN (
                             'security_compromise', 'policy_violation', 'investigation', 'leave_of_absence', 'role_review',
                             'employment_ended', 'contract_ended', 'duplicate_account',
                             'investigation_closed', 'returned_from_leave', 'error_correction',
                             'role_change_approved', 'device_lost', 'device_replaced', 'hard_locked',
                             'no_longer_required', 'wrong_details', 'break_glass', 'other')),
    note                 TEXT NULL CHECK (note IS NULL OR (char_length(note) BETWEEN 10 AND 500 AND note !~ '[[:cntrl:]]'
                             AND note !~ E'[‪-‮⁦-⁩​-‏⁠-⁤﻿؜]')),
    lifecycle_request_id UUID NULL REFERENCES staff_lifecycle_requests (id),
    request_id           TEXT NULL CHECK (request_id IS NULL OR octet_length(request_id) <= 128),
    ip                   TEXT NULL CHECK (ip IS NULL OR octet_length(ip) <= 64),
    user_agent           TEXT NULL CHECK (user_agent IS NULL OR octet_length(user_agent) <= 512),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT staff_lifecycle_events_actor CHECK ((actor_type = 'staff') = (actor_staff_id IS NOT NULL)),
    CONSTRAINT staff_lifecycle_events_note_other CHECK (reason_code IS DISTINCT FROM 'other' OR note IS NOT NULL)
);
CREATE INDEX idx_staff_lifecycle_events_staff ON staff_lifecycle_events (staff_user_id, created_at DESC);
CREATE INDEX idx_staff_lifecycle_events_tenant ON staff_lifecycle_events (tenant_id);

-- The actor's Person is forced from the actor row when it is visible (the s9.5(b) pair makes a platform actor's own row
-- visible inside a tenant transaction); a supplied value is never trusted.
CREATE FUNCTION sf_lifecycle_event_actor() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    NEW.actor_person_id := NULL;
    IF NEW.actor_staff_id IS NOT NULL THEN
        SELECT su.person_id INTO NEW.actor_person_id FROM public.staff_users su WHERE su.id = NEW.actor_staff_id;
    END IF;
    NEW.created_at := pg_catalog.clock_timestamp();
    RETURN NEW;
END
$$;

CREATE TRIGGER sf_event_tenant_forced BEFORE INSERT ON staff_lifecycle_events
    FOR EACH ROW EXECUTE FUNCTION sf_force_tenant_from_staff();
CREATE TRIGGER sf_event_actor_forced BEFORE INSERT ON staff_lifecycle_events
    FOR EACH ROW EXECUTE FUNCTION sf_lifecycle_event_actor();
CREATE TRIGGER sf_no_update BEFORE UPDATE ON staff_lifecycle_events
    FOR EACH ROW EXECUTE FUNCTION sf_deny_update();
CREATE TRIGGER sf_no_delete BEFORE DELETE ON staff_lifecycle_events
    FOR EACH ROW EXECUTE FUNCTION sf_deny_delete();
CREATE TRIGGER sf_no_truncate BEFORE TRUNCATE ON staff_lifecycle_events
    FOR EACH STATEMENT EXECUTE FUNCTION sf_deny_truncate();

ALTER TABLE staff_lifecycle_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_lifecycle_events FORCE ROW LEVEL SECURITY;
CREATE POLICY dual_scope_select ON staff_lifecycle_events FOR SELECT
    USING (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    );
CREATE POLICY dual_scope_insert ON staff_lifecycle_events FOR INSERT
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    );
-- The staff_users RESTRICTIVE fences in their strict 0137 form, plus the player and platform-service fences: no acting,
-- player or service session reads or writes a lifecycle event.
CREATE POLICY acting_fence_select ON staff_lifecycle_events AS RESTRICTIVE FOR SELECT USING (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_insert ON staff_lifecycle_events AS RESTRICTIVE FOR INSERT WITH CHECK (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_update ON staff_lifecycle_events AS RESTRICTIVE FOR UPDATE USING (NOT financial_acting_gucs_present()) WITH CHECK (NOT financial_acting_gucs_present());
CREATE POLICY acting_fence_delete ON staff_lifecycle_events AS RESTRICTIVE FOR DELETE USING (NOT financial_acting_gucs_present());
CREATE POLICY kyc_worker_fence_select ON staff_lifecycle_events AS RESTRICTIVE FOR SELECT
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_insert ON staff_lifecycle_events AS RESTRICTIVE FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_update ON staff_lifecycle_events AS RESTRICTIVE FOR UPDATE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_delete ON staff_lifecycle_events AS RESTRICTIVE FOR DELETE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY player_scope_fence ON staff_lifecycle_events AS RESTRICTIVE FOR ALL
    USING (NULLIF(current_setting('app.player_account_id', true), '') IS NULL)
    WITH CHECK (NULLIF(current_setting('app.player_account_id', true), '') IS NULL);
CREATE POLICY platform_service_fence ON staff_lifecycle_events AS RESTRICTIVE FOR ALL
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS NULL)
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS NULL);

-- =========================================================================
-- 7. The signed actor proof verifier: 0135's body plus staff_lifecycle:*
-- =========================================================================
CREATE OR REPLACE FUNCTION actor_proof_require(
    p_actor        uuid,
    p_scope        text,
    p_tenant       uuid,
    p_operation    text,
    p_target       text,
    p_payload_hash text
) RETURNS void AS $$
DECLARE
    v_token   text := NULLIF(pg_catalog.current_setting('app.actor_proof', true), '');
    v_parts   text[];
    v_secret  bytea;
    v_signed  text;
    v_mac     text;
    v_iat     bigint;
    v_exp     bigint;
    v_now     bigint := pg_catalog.floor(pg_catalog.date_part('epoch', pg_catalog.clock_timestamp()))::bigint;
    v_actor   uuid;
    v_tenant  uuid;
    v_n       int;
BEGIN
    IF v_token IS NULL OR p_actor IS NULL OR p_scope IS NULL OR p_operation IS NULL OR p_target IS NULL OR p_payload_hash IS NULL THEN
        RAISE EXCEPTION 'actor_proof: no proof presented' USING ERRCODE = 'AP001';
    END IF;
    IF p_scope NOT IN ('tenant', 'platform_acting', 'platform') THEN
        RAISE EXCEPTION 'actor_proof: scope is not provable' USING ERRCODE = 'AP004';
    END IF;
    -- ADR 0110 operation table (ADR 0111 M-10): withdrawal_hold_resolution:* is
    -- provable ONLY for scope platform_acting with a tenant.
    IF p_operation LIKE 'withdrawal\_hold\_resolution:%' AND (p_scope <> 'platform_acting' OR p_tenant IS NULL) THEN
        RAISE EXCEPTION 'actor_proof: this operation is provable only for a platform_acting scope with a tenant' USING ERRCODE = 'AP004';
    END IF;
    -- ADR 0112 6.4 (migration 0130): launch_authorisation:* is provable only for scope
    -- 'tenant' or 'platform' (never platform_acting), and a decision (approve / reject)
    -- only for scope 'platform'. The operation set is closed.
    IF p_operation LIKE 'launch\_authorisation:%' THEN
        IF p_operation NOT IN ('launch_authorisation:request', 'launch_authorisation:suspend', 'launch_authorisation:cancel',
                               'launch_authorisation:approve', 'launch_authorisation:reject')
           OR p_scope NOT IN ('platform', 'tenant')
           OR (p_operation IN ('launch_authorisation:approve', 'launch_authorisation:reject') AND p_scope <> 'platform') THEN
            RAISE EXCEPTION 'actor_proof: this launch_authorisation operation is not provable for this scope' USING ERRCODE = 'AP004';
        END IF;
    END IF;
    -- ADR 0115 S1 (migration 0135): rg_limit_config:* (the jurisdiction-floor four-eyes flow,
    -- section 2.1) is provable only for scope 'platform' (NULL tenant). The operation set is closed.
    IF p_operation LIKE 'rg\_limit\_config:%' THEN
        IF p_operation NOT IN ('rg_limit_config:request', 'rg_limit_config:approve', 'rg_limit_config:reject',
                               'rg_limit_config:cancel', 'rg_limit_config:apply')
           OR p_scope <> 'platform' THEN
            RAISE EXCEPTION 'actor_proof: this rg_limit_config operation is not provable for this scope' USING ERRCODE = 'AP004';
        END IF;
    END IF;
    -- ADR 0119 S5 + S6 (migration 0139): staff_lifecycle:* (the platform-staff four-eyes flow, section 10.4) is
    -- provable only for scope 'platform' (NULL tenant). The operation set is closed.
    IF p_operation LIKE 'staff\_lifecycle:%' THEN
        IF p_operation NOT IN ('staff_lifecycle:request', 'staff_lifecycle:approve', 'staff_lifecycle:reject',
                               'staff_lifecycle:cancel')
           OR p_scope <> 'platform' THEN
            RAISE EXCEPTION 'actor_proof: this staff_lifecycle operation is not provable for this scope' USING ERRCODE = 'AP004';
        END IF;
    END IF;
    -- The NULL-tenant encoding (an EMPTY tenant field) exists only for scope
    -- 'platform' and only for the K1 capability-grant, financial-policy-change,
    -- (ADR 0112, migration 0130) launch_authorisation, (ADR 0115) rg_limit_config and
    -- (ADR 0119, migration 0139) staff_lifecycle operations; every other scope and
    -- operation must carry a tenant.
    IF p_scope = 'platform' THEN
        IF p_tenant IS NOT NULL OR NOT (p_operation LIKE 'capability\_grant:%' OR p_operation LIKE 'financial\_policy\_change:%'
                                         OR p_operation LIKE 'launch\_authorisation:%'
                                         OR p_operation LIKE 'rg\_limit\_config:%'
                                         OR p_operation LIKE 'staff\_lifecycle:%') THEN
            RAISE EXCEPTION 'actor_proof: platform scope is not provable for this operation' USING ERRCODE = 'AP004';
        END IF;
    ELSIF p_tenant IS NULL THEN
        RAISE EXCEPTION 'actor_proof: no proof presented' USING ERRCODE = 'AP001';
    END IF;

    v_parts := pg_catalog.string_to_array(v_token, '|');
    IF pg_catalog.array_length(v_parts, 1) IS DISTINCT FROM 12 OR v_parts[1] <> 'v1' THEN
        RAISE EXCEPTION 'actor_proof: malformed proof' USING ERRCODE = 'AP001';
    END IF;
    IF v_parts[9] !~ '^[0-9]{1,12}$' OR v_parts[10] !~ '^[0-9]{1,12}$'
       OR v_parts[11] !~ '^[A-Za-z0-9_-]{16,64}$' OR v_parts[12] !~ '^[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'actor_proof: malformed proof' USING ERRCODE = 'AP001';
    END IF;
    BEGIN
        v_actor  := v_parts[3]::uuid;
        v_tenant := NULLIF(v_parts[5], '')::uuid;
    EXCEPTION WHEN OTHERS THEN
        RAISE EXCEPTION 'actor_proof: malformed proof' USING ERRCODE = 'AP001';
    END;
    v_iat := v_parts[9]::bigint;
    v_exp := v_parts[10]::bigint;

    -- Key lookup (active keys only) and MAC.
    SELECT k.secret INTO v_secret FROM public.actor_proof_keys k WHERE k.kid = v_parts[2] AND k.status = 'active';
    IF NOT FOUND THEN
        RAISE EXCEPTION 'actor_proof: proof rejected' USING ERRCODE = 'AP002';
    END IF;
    v_signed := pg_catalog.array_to_string(v_parts[1:11], '|');
    v_mac := pg_catalog.encode(public.hmac(pg_catalog.convert_to(v_signed, 'UTF8'), v_secret, 'sha256'), 'hex');
    -- Compare HMACs of both values under the same key, so the comparison is
    -- not a byte-wise early-exit on attacker-controlled input.
    IF public.hmac(pg_catalog.convert_to(v_mac, 'UTF8'), v_secret, 'sha256')
       IS DISTINCT FROM public.hmac(pg_catalog.convert_to(v_parts[12], 'UTF8'), v_secret, 'sha256') THEN
        RAISE EXCEPTION 'actor_proof: proof rejected' USING ERRCODE = 'AP002';
    END IF;

    -- Time window: not expired, not issued in the future (5 s skew), and the
    -- lifetime is at most 60 s.
    IF v_exp <= v_now OR v_iat > v_now + 5 OR v_exp <= v_iat OR v_exp - v_iat > 60 THEN
        RAISE EXCEPTION 'actor_proof: proof expired or not yet valid' USING ERRCODE = 'AP003';
    END IF;

    -- Binding: the proof must be for exactly this actor/scope/tenant and this
    -- operation/target/payload.
    IF v_actor IS DISTINCT FROM p_actor OR v_parts[4] <> p_scope OR v_tenant IS DISTINCT FROM p_tenant
       OR v_parts[6] <> p_operation OR v_parts[7] <> p_target OR v_parts[8] <> p_payload_hash THEN
        RAISE EXCEPTION 'actor_proof: proof does not match the actor/operation/target' USING ERRCODE = 'AP004';
    END IF;

    -- Replay protection: one proof, one consumption. The row commits or rolls
    -- back with the governed write, so a failed attempt does not burn it and a
    -- committed one can never be replayed (the UNIQUE nonce key serialises two
    -- concurrent consumers).
    INSERT INTO public.actor_proof_nonces
        (nonce, kid, actor, scope, tenant, operation, target, payload_hash, expires_at, consumed_txid)
    VALUES (v_parts[11], v_parts[2], v_actor, p_scope, v_tenant, p_operation, p_target, p_payload_hash,
            pg_catalog.to_timestamp(v_exp), pg_catalog.txid_current())
    ON CONFLICT (nonce) DO NOTHING;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    IF v_n = 0 THEN
        RAISE EXCEPTION 'actor_proof: proof already used' USING ERRCODE = 'AP005';
    END IF;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp;

-- =========================================================================
-- 8. Alert Kind staff.platform_break_glass_used (P1, platform, subjectless)
-- =========================================================================
ALTER TABLE alert_kinds DROP CONSTRAINT alert_kinds_check1;
ALTER TABLE alert_kinds ADD CONSTRAINT alert_kinds_check1 CHECK (
    requires_subject = true
    OR kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed',
                'staff.platform_mfa_integrity', 'staff.platform_session_integrity', 'staff.platform_mfa_hard_locked',
                'staff.platform_mfa_recovery_used', 'staff.platform_mfa_failure_pattern',
                'staff.platform_break_glass_used'));
DO $$
BEGIN
    CREATE POLICY alert_kinds_seed_0139 ON alert_kinds
        FOR INSERT TO CURRENT_USER
        WITH CHECK (kind = 'staff.platform_break_glass_used');
    INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject, in_tx_raisable_by_tenant, allowed_keys, raise_mode) VALUES
        ('staff.platform_break_glass_used', 'p1', 'platform', false, false, false,
         ARRAY['staff_id', 'operator', 'reason', 'force_additional'], 'post_commit');
    DROP POLICY alert_kinds_seed_0139 ON alert_kinds;
END
$$;

-- =========================================================================
-- 9. Grants to igaming_runtime (mirrored by deploy/init-app-role.sql)
-- =========================================================================
REVOKE ALL ON staff_lifecycle_requests, staff_lifecycle_approvals, staff_lifecycle_events FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE UPDATE ON staff_users FROM igaming_runtime';
        EXECUTE 'GRANT UPDATE (status, role, password_hash, status_changed_at, offboarded_at, person_id, display_name, updated_at) ON staff_users TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON staff_lifecycle_requests FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON staff_lifecycle_requests TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (state, decided_at, executing_txid, target_staff_id) ON staff_lifecycle_requests TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON staff_lifecycle_approvals FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON staff_lifecycle_approvals TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON staff_lifecycle_events FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON staff_lifecycle_events TO igaming_runtime';
    END IF;
END
$$;
