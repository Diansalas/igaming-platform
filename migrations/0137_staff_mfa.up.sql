-- ADR 0119 slice S2 (staff MFA tables). IDENTITY / AUTHENTICATION INTEGRITY CONTROL: FOR `security` (live review),
-- `code-reviewer`, `qa` REVIEW BEFORE MERGE (ADR 0119 s16 gate G).
--
-- NUMBERING: 0137 (gap-free after 0136_player_brand_binding_immutable). 0138 is the staff-session cutover (merges with
-- S3) and 0139 is the staff lifecycle (merges with S6); the orchestrator allocates numbers - confirm before merge.
--
-- What it adds, and only this (ADR 0119 s13.1, S2):
--   0. SET LOCAL lock_timeout = '5s' (0133 / 0136 precedent), for the whole file.
--   1. Pre-flight (SF098): refuses unless the staff_users RLS posture this file replicates is exactly the one found
--      (FORCE RLS, the dual_scope_isolation policy and the eight RESTRICTIVE fences of migrations 0112 / 0114). A
--      fence added to staff_users later would otherwise be silently missing from the new tables. No row is read or
--      changed, so no FORCE ROW LEVEL SECURITY is lifted.
--   2. Four tables: staff_mfa_factors, staff_mfa_recovery_codes, staff_auth_challenges, staff_enrolment_tokens.
--      tenant_id is NULL for platform staff. Secrets: the factor secret is AES-256-GCM ciphertext (the key is never in
--      the database); recovery codes, challenge tokens and enrolment tokens are stored ONLY as keyed HMACs.
--   3. Guard triggers (plain plpgsql, NOT SECURITY DEFINER, search_path pinned, schema-qualified; messages name the
--      table and the column set, never a row value):
--        SF001 the tenant of a child row is not the staff member's tenant (forced on INSERT, a differing value refused;
--              a composite FK cannot cover the NULL platform arm: MATCH SIMPLE skips rows with a NULL column)
--        SF002 immutable factor column changed, or an illegal factor status transition
--        SF003 last_used_step decreased (the replay-protection burn is monotonic)
--        SF004 recovery-code mutation other than consume or revoke
--        SF005 challenge mutation other than an attempt decrement or consume
--        SF006 enrolment-token mutation other than consume or revoke
--        SF097 down refused: MFA data exists (migration 0137 down)
--        SF098 up pre-flight refused (staff_users fence drift)
--        SF099 append-only: DELETE or TRUNCATE
--      The SF guards fire in the order of their names (sf_*_tenant_forced, sf_*_guard, sf_no_*).
--   4. FORCE ROW LEVEL SECURITY on every table: the dual-scope policy of migration 0011 (tenant session: own tenant's
--      rows; platform session with no app.tenant_id: platform rows only) plus the pre-tenant single-row lookup-by-MAC
--      SELECT policies on the challenge and enrolment tables (ADR 0016 / migration 0040 pattern), plus all eight
--      RESTRICTIVE fences of staff_users (acting_fence_*, kyc_worker_fence_*), in the strict form "no acting session
--      and no KYC worker reaches a staff member's MFA material at all".
--   5. Least-privilege grants to igaming_runtime (mirrored by deploy/init-app-role.sql): SELECT, INSERT and COLUMN-level
--      UPDATE only; no DELETE, TRUNCATE, REFERENCES or TRIGGER.
--
-- NOT in this migration, deliberately (ADR 0119 s20 C1/C5, review-binding): the sessions assurance columns and the seal,
-- the SF040 trigger, the staff-session revocation and the cutover marker (all 0138, merged with S3); every staff_users
-- trigger and column (SF010-SF014, SF020, SF021, staff_version: 0139); SF007 and SF030 (S-H1, owner-deferred). Staff
-- login on main is unchanged: nothing reads or writes these tables until S3.
--
-- No existing function, policy, constraint, grant or row is changed. The down drops exactly what this file creates and
-- refuses (SF097) while any MFA row exists (whole-schema snapshot test: internal/staffmfa/store/migration_0137_integration_test.go).

SET LOCAL lock_timeout = '5s';

-- =========================================================================
-- 1. Pre-flight (SF098): the staff_users posture replicated below must be the one that exists.
-- =========================================================================
DO $$
DECLARE
    v_expected text[] := ARRAY[
        'acting_fence_delete', 'acting_fence_insert', 'acting_fence_select', 'acting_fence_update',
        'kyc_worker_fence_delete', 'kyc_worker_fence_insert', 'kyc_worker_fence_select', 'kyc_worker_fence_update'];
    v_found    text[];
    v_missing  text[];
    v_extra    text[];
    v_forced   boolean;
    v_dual     bigint;
    v_fn       bigint;
BEGIN
    SELECT relforcerowsecurity INTO v_forced FROM pg_catalog.pg_class
     WHERE oid = 'public.staff_users'::pg_catalog.regclass;
    SELECT pg_catalog.count(*) INTO v_dual FROM pg_catalog.pg_policies
     WHERE schemaname = 'public' AND tablename = 'staff_users' AND policyname = 'dual_scope_isolation'
       AND permissive = 'PERMISSIVE' AND cmd = 'ALL';
    SELECT COALESCE(pg_catalog.array_agg(policyname::text ORDER BY policyname), ARRAY[]::text[]) INTO v_found
      FROM pg_catalog.pg_policies
     WHERE schemaname = 'public' AND tablename = 'staff_users' AND permissive = 'RESTRICTIVE';
    SELECT COALESCE(pg_catalog.array_agg(x ORDER BY x), ARRAY[]::text[]) INTO v_missing
      FROM pg_catalog.unnest(v_expected) x WHERE NOT (x = ANY (v_found));
    SELECT COALESCE(pg_catalog.array_agg(x ORDER BY x), ARRAY[]::text[]) INTO v_extra
      FROM pg_catalog.unnest(v_found) x WHERE NOT (x = ANY (v_expected));
    SELECT pg_catalog.count(*) INTO v_fn FROM pg_catalog.pg_proc
     WHERE pronamespace = 'public'::pg_catalog.regnamespace AND proname = 'financial_acting_gucs_present';
    IF v_forced IS DISTINCT FROM true OR v_dual <> 1 OR pg_catalog.cardinality(v_missing) > 0
       OR pg_catalog.cardinality(v_extra) > 0 OR v_fn <> 1 THEN
        RAISE EXCEPTION 'migration 0137 pre-flight: the staff_users RLS posture is not the one the staff MFA tables replicate (ADR 0119 s13.3, review N3): force_rls=%, dual_scope_isolation=%, missing_restrictive_fences=%, unexpected_restrictive_fences=%, financial_acting_gucs_present=%. Nothing was changed. Replicate the new fence on the four tables in this migration, then update this list; do NOT loosen the check.',
            v_forced, v_dual, v_missing, v_extra, v_fn
            USING ERRCODE = 'SF098';
    END IF;
END
$$;

-- =========================================================================
-- 2. Tables.
-- =========================================================================

-- 2.1 staff_mfa_factors (ADR 0119 s13.1 item 1).
CREATE TABLE staff_mfa_factors (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NULL REFERENCES tenants(id),
    staff_user_id       UUID NOT NULL REFERENCES staff_users(id),
    factor_type         TEXT NOT NULL CHECK (factor_type IN ('totp')),
    status              TEXT NOT NULL CHECK (status IN ('pending', 'active', 'revoked')),
    secret_kid          TEXT NOT NULL CHECK (secret_kid <> ''),
    secret_nonce        BYTEA NOT NULL CHECK (octet_length(secret_nonce) = 12),
    secret_ct           BYTEA NOT NULL CHECK (octet_length(secret_ct) > 0),
    last_used_step      BIGINT NOT NULL DEFAULT 0 CHECK (last_used_step >= 0),
    failed_count        INT NOT NULL DEFAULT 0,
    failed_window_start TIMESTAMPTZ NULL,
    locked_until        TIMESTAMPTZ NULL,
    failed_24h_count    INT NOT NULL DEFAULT 0,
    failed_24h_start    TIMESTAMPTZ NULL,
    hard_locked_at      TIMESTAMPTZ NULL,
    pending_expires_at  TIMESTAMPTZ NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    activated_at        TIMESTAMPTZ NULL,
    revoked_at          TIMESTAMPTZ NULL,
    revoked_reason      TEXT NULL CHECK (revoked_reason IN
        ('replaced', 'reset', 'suspended_compromise', 'offboarded', 'pending_superseded', 'pending_expired')),
    CONSTRAINT staff_mfa_factors_pending_expiry CHECK ((status = 'pending') = (pending_expires_at IS NOT NULL)),
    -- The status columns agree with the status (a revoked row has a revocation, an active one an activation).
    CONSTRAINT staff_mfa_factors_activation CHECK (
        (status = 'pending' AND activated_at IS NULL) OR status = 'revoked' OR (status = 'active' AND activated_at IS NOT NULL)),
    CONSTRAINT staff_mfa_factors_revocation CHECK ((status = 'revoked') = (revoked_at IS NOT NULL) AND (status = 'revoked') = (revoked_reason IS NOT NULL)),
    -- Security review C4 of the S1 library: the counters mirror staffmfa.Counters.valid(). A 24 hour count at the hard-lock
    -- threshold without hard_locked_at is an inconsistent row (ADR 0119 s21.6 C4); the library treats it as locked and the
    -- database refuses to store it.
    CONSTRAINT staff_mfa_factors_hard_lock_consistent CHECK (hard_locked_at IS NOT NULL OR failed_24h_count < 10),
    CONSTRAINT staff_mfa_factors_failed_count_range CHECK (failed_count >= 0 AND failed_count < 5),
    CONSTRAINT staff_mfa_factors_failed_24h_range CHECK (failed_24h_count >= 0 AND failed_24h_count <= 10),
    CONSTRAINT staff_mfa_factors_window_agrees CHECK ((failed_count = 0) = (failed_window_start IS NULL)),
    CONSTRAINT staff_mfa_factors_window_24h_agrees CHECK ((failed_24h_count = 0) = (failed_24h_start IS NULL))
);

CREATE UNIQUE INDEX staff_mfa_factors_one_active
    ON staff_mfa_factors (staff_user_id, factor_type) WHERE status = 'active';
CREATE UNIQUE INDEX staff_mfa_factors_one_pending
    ON staff_mfa_factors (staff_user_id, factor_type) WHERE status = 'pending';
CREATE INDEX idx_staff_mfa_factors_tenant ON staff_mfa_factors (tenant_id);
CREATE INDEX idx_staff_mfa_factors_staff ON staff_mfa_factors (staff_user_id);

-- 2.2 staff_mfa_recovery_codes (s13.1 item 2).
CREATE TABLE staff_mfa_recovery_codes (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NULL REFERENCES tenants(id),
    staff_user_id UUID NOT NULL REFERENCES staff_users(id),
    batch_id      UUID NOT NULL,
    code_kid      TEXT NOT NULL CHECK (code_kid <> ''),
    code_mac      BYTEA NOT NULL UNIQUE CHECK (octet_length(code_mac) = 32),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    consumed_at   TIMESTAMPTZ NULL,
    revoked_at    TIMESTAMPTZ NULL,
    CONSTRAINT staff_mfa_recovery_codes_consumed_xor_revoked CHECK (consumed_at IS NULL OR revoked_at IS NULL)
);

CREATE INDEX idx_staff_mfa_recovery_codes_live
    ON staff_mfa_recovery_codes (staff_user_id) WHERE consumed_at IS NULL AND revoked_at IS NULL;
CREATE INDEX idx_staff_mfa_recovery_codes_tenant ON staff_mfa_recovery_codes (tenant_id);

-- 2.3 staff_auth_challenges (s13.1 item 3).
CREATE TABLE staff_auth_challenges (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NULL REFERENCES tenants(id),
    staff_user_id         UUID NOT NULL REFERENCES staff_users(id),
    purpose               TEXT NOT NULL CHECK (purpose IN ('mfa', 'enrol')),
    token_kid             TEXT NOT NULL CHECK (token_kid <> ''),
    token_mac             BYTEA NOT NULL UNIQUE CHECK (octet_length(token_mac) = 32),
    staff_version_at_issue BIGINT NOT NULL CHECK (staff_version_at_issue >= 0),
    attempts_left         SMALLINT NOT NULL CHECK (attempts_left BETWEEN 0 AND 5),
    expires_at            TIMESTAMPTZ NOT NULL,
    consumed_at           TIMESTAMPTZ NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    via                   TEXT NOT NULL CHECK (via IN ('login', 'recovery_code', 'invite', 'mfa_reset', 'bootstrap')),
    CONSTRAINT staff_auth_challenges_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX idx_staff_auth_challenges_staff ON staff_auth_challenges (staff_user_id);
CREATE INDEX idx_staff_auth_challenges_tenant ON staff_auth_challenges (tenant_id);

-- 2.4 staff_enrolment_tokens (s13.1 item 4). SF007 (bootstrap tokens on the owner path only) is reserved for S-H1.
CREATE TABLE staff_enrolment_tokens (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NULL REFERENCES tenants(id),
    staff_user_id        UUID NOT NULL REFERENCES staff_users(id),
    purpose              TEXT NOT NULL CHECK (purpose IN ('invite', 'mfa_reset', 'password_reset', 'bootstrap')),
    token_kid            TEXT NOT NULL CHECK (token_kid <> ''),
    token_mac            BYTEA NOT NULL UNIQUE CHECK (octet_length(token_mac) = 32),
    expires_at           TIMESTAMPTZ NOT NULL,
    consumed_at          TIMESTAMPTZ NULL,
    revoked_at           TIMESTAMPTZ NULL,
    -- issued_by names a staff member (NULL for a system / bootstrap issue). lifecycle_request_id has no FK: its table
    -- is created by 0139.
    issued_by            UUID NULL REFERENCES staff_users(id),
    lifecycle_request_id UUID NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT staff_enrolment_tokens_consumed_xor_revoked CHECK (consumed_at IS NULL OR revoked_at IS NULL),
    CONSTRAINT staff_enrolment_tokens_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX staff_enrolment_tokens_one_live
    ON staff_enrolment_tokens (staff_user_id, purpose) WHERE consumed_at IS NULL AND revoked_at IS NULL;
CREATE INDEX idx_staff_enrolment_tokens_staff ON staff_enrolment_tokens (staff_user_id);
CREATE INDEX idx_staff_enrolment_tokens_tenant ON staff_enrolment_tokens (tenant_id);

-- =========================================================================
-- 3. Guard functions and triggers.
-- =========================================================================

-- SF001: the tenant of a child row is the staff member's tenant, forced on INSERT. The staff row is read under the
-- caller's own RLS scope: a staff member the session cannot see (another tenant's, a platform row from a tenant
-- session, a tenant row from a platform session without WithTenant) is refused.
CREATE FUNCTION sf_force_tenant_from_staff() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_tenant uuid;
BEGIN
    SELECT s.tenant_id INTO v_tenant FROM public.staff_users s WHERE s.id = NEW.staff_user_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION '%: the staff member is not visible in this scope (SF001)', TG_TABLE_NAME
            USING ERRCODE = 'SF001';
    END IF;
    IF NEW.tenant_id IS NOT NULL AND NEW.tenant_id IS DISTINCT FROM v_tenant THEN
        RAISE EXCEPTION '%: tenant_id must be the staff member''s tenant (SF001)', TG_TABLE_NAME
            USING ERRCODE = 'SF001';
    END IF;
    NEW.tenant_id := v_tenant;
    RETURN NEW;
END
$$;

-- SF099: append-only (no DELETE, no TRUNCATE).
CREATE FUNCTION sf_deny_delete() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION '%: rows are never deleted (ADR 0119, SF099)', TG_TABLE_NAME USING ERRCODE = 'SF099';
END
$$;

CREATE FUNCTION sf_deny_truncate() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION '%: TRUNCATE is refused (ADR 0119, SF099)', TG_TABLE_NAME USING ERRCODE = 'SF099';
END
$$;

-- SF002 / SF003: the factor guard.
CREATE FUNCTION sf_factor_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_changed text[] := ARRAY[]::text[];
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id THEN v_changed := v_changed || 'id'::text; END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id THEN v_changed := v_changed || 'tenant_id'::text; END IF;
    IF NEW.staff_user_id IS DISTINCT FROM OLD.staff_user_id THEN v_changed := v_changed || 'staff_user_id'::text; END IF;
    IF NEW.factor_type IS DISTINCT FROM OLD.factor_type THEN v_changed := v_changed || 'factor_type'::text; END IF;
    IF NEW.secret_kid IS DISTINCT FROM OLD.secret_kid THEN v_changed := v_changed || 'secret_kid'::text; END IF;
    IF NEW.secret_nonce IS DISTINCT FROM OLD.secret_nonce THEN v_changed := v_changed || 'secret_nonce'::text; END IF;
    IF NEW.secret_ct IS DISTINCT FROM OLD.secret_ct THEN v_changed := v_changed || 'secret_ct'::text; END IF;
    IF NEW.created_at IS DISTINCT FROM OLD.created_at THEN v_changed := v_changed || 'created_at'::text; END IF;
    IF pg_catalog.cardinality(v_changed) > 0 THEN
        RAISE EXCEPTION 'staff_mfa_factors: immutable column(s) changed: % (SF002)',
            pg_catalog.array_to_string(v_changed, ', ') USING ERRCODE = 'SF002';
    END IF;

    -- last_used_step never decreases (the replay-protection burn is monotonic).
    IF NEW.last_used_step < OLD.last_used_step THEN
        RAISE EXCEPTION 'staff_mfa_factors: last_used_step must not decrease (SF003)' USING ERRCODE = 'SF003';
    END IF;

    -- Status: pending -> active | revoked, active -> revoked. A revoked factor is terminal and frozen.
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NOT ((OLD.status = 'pending' AND NEW.status IN ('active', 'revoked'))
             OR (OLD.status = 'active' AND NEW.status = 'revoked')) THEN
            RAISE EXCEPTION 'staff_mfa_factors: illegal status transition % -> % (SF002)', OLD.status, NEW.status
                USING ERRCODE = 'SF002';
        END IF;
    ELSIF OLD.status = 'revoked' AND ROW(NEW.*) IS DISTINCT FROM ROW(OLD.*) THEN
        RAISE EXCEPTION 'staff_mfa_factors: a revoked factor is frozen (SF002)' USING ERRCODE = 'SF002';
    END IF;

    -- Write-once columns: the activation instant, the hard lock and the revocation stay as first written; the
    -- pending expiry may only be cleared (the status CHECK ties it to 'pending').
    IF OLD.activated_at IS NOT NULL AND NEW.activated_at IS DISTINCT FROM OLD.activated_at THEN
        RAISE EXCEPTION 'staff_mfa_factors: activated_at is write-once (SF002)' USING ERRCODE = 'SF002';
    END IF;
    IF OLD.hard_locked_at IS NOT NULL AND NEW.hard_locked_at IS DISTINCT FROM OLD.hard_locked_at THEN
        RAISE EXCEPTION 'staff_mfa_factors: hard_locked_at is write-once; only an MFA reset (a new factor) clears a hard lock (SF002)'
            USING ERRCODE = 'SF002';
    END IF;
    IF OLD.revoked_at IS NOT NULL AND (NEW.revoked_at IS DISTINCT FROM OLD.revoked_at OR NEW.revoked_reason IS DISTINCT FROM OLD.revoked_reason) THEN
        RAISE EXCEPTION 'staff_mfa_factors: the revocation is write-once (SF002)' USING ERRCODE = 'SF002';
    END IF;
    IF NEW.pending_expires_at IS NOT NULL AND NEW.pending_expires_at IS DISTINCT FROM OLD.pending_expires_at THEN
        RAISE EXCEPTION 'staff_mfa_factors: pending_expires_at may only be cleared (SF002)' USING ERRCODE = 'SF002';
    END IF;
    RETURN NEW;
END
$$;

-- SF004: recovery code - only consumed_at / revoked_at move, NULL -> value, never both.
CREATE FUNCTION sf_recovery_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.staff_user_id IS DISTINCT FROM OLD.staff_user_id OR NEW.batch_id IS DISTINCT FROM OLD.batch_id
       OR NEW.code_kid IS DISTINCT FROM OLD.code_kid OR NEW.code_mac IS DISTINCT FROM OLD.code_mac
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'staff_mfa_recovery_codes: only consumed_at and revoked_at may change (SF004)' USING ERRCODE = 'SF004';
    END IF;
    IF (OLD.consumed_at IS NOT NULL AND NEW.consumed_at IS DISTINCT FROM OLD.consumed_at)
       OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at) THEN
        RAISE EXCEPTION 'staff_mfa_recovery_codes: consumed_at and revoked_at are write-once (SF004)' USING ERRCODE = 'SF004';
    END IF;
    IF NEW.consumed_at IS NOT NULL AND NEW.revoked_at IS NOT NULL THEN
        RAISE EXCEPTION 'staff_mfa_recovery_codes: a code is consumed or revoked, never both (SF004)' USING ERRCODE = 'SF004';
    END IF;
    RETURN NEW;
END
$$;

-- SF005: challenge - only an attempts_left decrement and consumed_at NULL -> value. A consumed challenge is frozen.
CREATE FUNCTION sf_challenge_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.staff_user_id IS DISTINCT FROM OLD.staff_user_id OR NEW.purpose IS DISTINCT FROM OLD.purpose
       OR NEW.token_kid IS DISTINCT FROM OLD.token_kid OR NEW.token_mac IS DISTINCT FROM OLD.token_mac
       OR NEW.staff_version_at_issue IS DISTINCT FROM OLD.staff_version_at_issue
       OR NEW.expires_at IS DISTINCT FROM OLD.expires_at OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR NEW.via IS DISTINCT FROM OLD.via THEN
        RAISE EXCEPTION 'staff_auth_challenges: only attempts_left (decrement) and consumed_at may change (SF005)' USING ERRCODE = 'SF005';
    END IF;
    IF NEW.attempts_left > OLD.attempts_left THEN
        RAISE EXCEPTION 'staff_auth_challenges: attempts_left only decreases (SF005)' USING ERRCODE = 'SF005';
    END IF;
    IF OLD.consumed_at IS NOT NULL AND ROW(NEW.*) IS DISTINCT FROM ROW(OLD.*) THEN
        RAISE EXCEPTION 'staff_auth_challenges: a consumed challenge is frozen (SF005)' USING ERRCODE = 'SF005';
    END IF;
    RETURN NEW;
END
$$;

-- SF006: enrolment token - only consumed_at / revoked_at move, NULL -> value, never both.
CREATE FUNCTION sf_enrolment_token_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.staff_user_id IS DISTINCT FROM OLD.staff_user_id OR NEW.purpose IS DISTINCT FROM OLD.purpose
       OR NEW.token_kid IS DISTINCT FROM OLD.token_kid OR NEW.token_mac IS DISTINCT FROM OLD.token_mac
       OR NEW.expires_at IS DISTINCT FROM OLD.expires_at OR NEW.issued_by IS DISTINCT FROM OLD.issued_by
       OR NEW.lifecycle_request_id IS DISTINCT FROM OLD.lifecycle_request_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'staff_enrolment_tokens: only consumed_at and revoked_at may change (SF006)' USING ERRCODE = 'SF006';
    END IF;
    IF (OLD.consumed_at IS NOT NULL AND NEW.consumed_at IS DISTINCT FROM OLD.consumed_at)
       OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at) THEN
        RAISE EXCEPTION 'staff_enrolment_tokens: consumed_at and revoked_at are write-once (SF006)' USING ERRCODE = 'SF006';
    END IF;
    IF NEW.consumed_at IS NOT NULL AND NEW.revoked_at IS NOT NULL THEN
        RAISE EXCEPTION 'staff_enrolment_tokens: a token is consumed or revoked, never both (SF006)' USING ERRCODE = 'SF006';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER sf_factor_tenant_forced BEFORE INSERT ON staff_mfa_factors
    FOR EACH ROW EXECUTE FUNCTION sf_force_tenant_from_staff();
CREATE TRIGGER sf_factor_guard BEFORE UPDATE ON staff_mfa_factors
    FOR EACH ROW EXECUTE FUNCTION sf_factor_guard();
CREATE TRIGGER sf_no_delete BEFORE DELETE ON staff_mfa_factors
    FOR EACH ROW EXECUTE FUNCTION sf_deny_delete();
CREATE TRIGGER sf_no_truncate BEFORE TRUNCATE ON staff_mfa_factors
    FOR EACH STATEMENT EXECUTE FUNCTION sf_deny_truncate();

CREATE TRIGGER sf_recovery_tenant_forced BEFORE INSERT ON staff_mfa_recovery_codes
    FOR EACH ROW EXECUTE FUNCTION sf_force_tenant_from_staff();
CREATE TRIGGER sf_recovery_guard BEFORE UPDATE ON staff_mfa_recovery_codes
    FOR EACH ROW EXECUTE FUNCTION sf_recovery_guard();
CREATE TRIGGER sf_no_delete BEFORE DELETE ON staff_mfa_recovery_codes
    FOR EACH ROW EXECUTE FUNCTION sf_deny_delete();
CREATE TRIGGER sf_no_truncate BEFORE TRUNCATE ON staff_mfa_recovery_codes
    FOR EACH STATEMENT EXECUTE FUNCTION sf_deny_truncate();

CREATE TRIGGER sf_challenge_tenant_forced BEFORE INSERT ON staff_auth_challenges
    FOR EACH ROW EXECUTE FUNCTION sf_force_tenant_from_staff();
CREATE TRIGGER sf_challenge_guard BEFORE UPDATE ON staff_auth_challenges
    FOR EACH ROW EXECUTE FUNCTION sf_challenge_guard();
CREATE TRIGGER sf_no_delete BEFORE DELETE ON staff_auth_challenges
    FOR EACH ROW EXECUTE FUNCTION sf_deny_delete();
CREATE TRIGGER sf_no_truncate BEFORE TRUNCATE ON staff_auth_challenges
    FOR EACH STATEMENT EXECUTE FUNCTION sf_deny_truncate();

CREATE TRIGGER sf_enrolment_token_tenant_forced BEFORE INSERT ON staff_enrolment_tokens
    FOR EACH ROW EXECUTE FUNCTION sf_force_tenant_from_staff();
CREATE TRIGGER sf_enrolment_token_guard BEFORE UPDATE ON staff_enrolment_tokens
    FOR EACH ROW EXECUTE FUNCTION sf_enrolment_token_guard();
CREATE TRIGGER sf_no_delete BEFORE DELETE ON staff_enrolment_tokens
    FOR EACH ROW EXECUTE FUNCTION sf_deny_delete();
CREATE TRIGGER sf_no_truncate BEFORE TRUNCATE ON staff_enrolment_tokens
    FOR EACH STATEMENT EXECUTE FUNCTION sf_deny_truncate();

COMMENT ON TRIGGER sf_factor_guard ON staff_mfa_factors IS 'ADR 0119 s13.1: SF002 immutable secret_* / identity columns, status graph, SF003 monotonic last_used_step';
COMMENT ON TRIGGER sf_recovery_guard ON staff_mfa_recovery_codes IS 'ADR 0119 s13.1: SF004 consume or revoke only';
COMMENT ON TRIGGER sf_challenge_guard ON staff_auth_challenges IS 'ADR 0119 s13.1: SF005 attempt decrement or consume only';
COMMENT ON TRIGGER sf_enrolment_token_guard ON staff_enrolment_tokens IS 'ADR 0119 s13.1: SF006 consume or revoke only';

-- =========================================================================
-- 4. Row level security (ADR 0119 s13.3).
-- =========================================================================
ALTER TABLE staff_mfa_factors ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_mfa_factors FORCE ROW LEVEL SECURITY;
ALTER TABLE staff_mfa_recovery_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_mfa_recovery_codes FORCE ROW LEVEL SECURITY;
ALTER TABLE staff_auth_challenges ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_auth_challenges FORCE ROW LEVEL SECURITY;
ALTER TABLE staff_enrolment_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_enrolment_tokens FORCE ROW LEVEL SECURITY;

-- The dual-scope policy of migration 0011, identical on every table: a tenant-scoped connection sees only that
-- tenant's rows; a connection with no tenant context (WithoutTenant / WithPlatformAdmin) sees only platform rows.
CREATE POLICY dual_scope_isolation ON staff_mfa_factors FOR ALL
    USING (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    )
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    );
CREATE POLICY dual_scope_isolation ON staff_mfa_recovery_codes FOR ALL
    USING (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    )
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    );
CREATE POLICY dual_scope_isolation ON staff_auth_challenges FOR ALL
    USING (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    )
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    );
CREATE POLICY dual_scope_isolation ON staff_enrolment_tokens FOR ALL
    USING (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    )
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    );

-- The pre-tenant lookup (ADR 0016 / migration 0040 pattern, ADR 0119 s13.1 item 6): a presented challenge or
-- enrolment token carries no tenant hint, so there is no tenant scope to query with before reading the one row that
-- reveals it. SELECT-only, at most the single row whose keyed MAC equals the value the caller already holds; the
-- trailing "no tenant is set" conjunct is migration 0040's own (a caller that combined a tenant scope with this GUC
-- would otherwise see a different tenant's row by knowing its MAC). Every mutation, including FOR UPDATE, uses the
-- two-phase pattern: look up by MAC to learn the scope, then operate in the resolved tenant or platform scope.
CREATE POLICY staff_challenge_select_by_mac ON staff_auth_challenges FOR SELECT
    USING (
        token_mac = decode(NULLIF(current_setting('app.staff_challenge_lookup_mac', true), ''), 'hex')
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
    );
CREATE POLICY staff_enrolment_select_by_mac ON staff_enrolment_tokens FOR SELECT
    USING (
        token_mac = decode(NULLIF(current_setting('app.staff_enrolment_lookup_mac', true), ''), 'hex')
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
    );

-- The RESTRICTIVE fences of staff_users (0112 acting fence, 0114 KYC-worker fence), replicated by the SAME names on
-- each new table (a catalog test asserts the parity, review N3). Strict form: staff_users lets an acting session
-- see its own platform row and the acted tenant's staff rows; nothing in an acting session or a worker needs a staff
-- member's MFA material, so here the acting fence admits no acting session at all.
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['staff_mfa_factors', 'staff_mfa_recovery_codes', 'staff_auth_challenges', 'staff_enrolment_tokens'] LOOP
        EXECUTE format('CREATE POLICY acting_fence_select ON %I AS RESTRICTIVE FOR SELECT USING (NOT financial_acting_gucs_present())', t);
        EXECUTE format('CREATE POLICY acting_fence_insert ON %I AS RESTRICTIVE FOR INSERT WITH CHECK (NOT financial_acting_gucs_present())', t);
        EXECUTE format('CREATE POLICY acting_fence_update ON %I AS RESTRICTIVE FOR UPDATE USING (NOT financial_acting_gucs_present()) WITH CHECK (NOT financial_acting_gucs_present())', t);
        EXECUTE format('CREATE POLICY acting_fence_delete ON %I AS RESTRICTIVE FOR DELETE USING (NOT financial_acting_gucs_present())', t);
        EXECUTE format($f$CREATE POLICY kyc_worker_fence_select ON %I AS RESTRICTIVE FOR SELECT USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')$f$, t);
        EXECUTE format($f$CREATE POLICY kyc_worker_fence_insert ON %I AS RESTRICTIVE FOR INSERT WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')$f$, t);
        EXECUTE format($f$CREATE POLICY kyc_worker_fence_update ON %I AS RESTRICTIVE FOR UPDATE USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker') WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')$f$, t);
        EXECUTE format($f$CREATE POLICY kyc_worker_fence_delete ON %I AS RESTRICTIVE FOR DELETE USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')$f$, t);
        -- Security review F1 (D14): ADR 0119 s13.3 gives player, service and acting sessions NO access. staff_users is
        -- readable by those sessions (and fenced only for acting / the KYC worker), so replicating its fences is not
        -- enough: a player-scoped session or ANY platform-service session (alert_dispatcher, sportsbook_catalogue_sync,
        -- ...) satisfied the tenant arm. These two fences apply to every command and require the GUC to be unset.
        EXECUTE format($f$CREATE POLICY player_scope_fence ON %I AS RESTRICTIVE FOR ALL USING (NULLIF(current_setting('app.player_account_id', true), '') IS NULL) WITH CHECK (NULLIF(current_setting('app.player_account_id', true), '') IS NULL)$f$, t);
        EXECUTE format($f$CREATE POLICY platform_service_fence ON %I AS RESTRICTIVE FOR ALL USING (NULLIF(current_setting('app.platform_service_id', true), '') IS NULL) WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS NULL)$f$, t);
    END LOOP;
END
$$;

-- =========================================================================
-- 5. Grants to igaming_runtime (mirrored by deploy/init-app-role.sql). RLS + triggers bind.
--    SELECT and INSERT; UPDATE is column-level only; no DELETE, TRUNCATE, REFERENCES or TRIGGER.
-- =========================================================================
REVOKE ALL ON staff_mfa_factors, staff_mfa_recovery_codes, staff_auth_challenges, staff_enrolment_tokens FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON staff_mfa_factors FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON staff_mfa_factors TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (status, last_used_step, failed_count, failed_window_start, locked_until, failed_24h_count, failed_24h_start, hard_locked_at, pending_expires_at, activated_at, revoked_at, revoked_reason) ON staff_mfa_factors TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON staff_mfa_recovery_codes FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON staff_mfa_recovery_codes TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (consumed_at, revoked_at) ON staff_mfa_recovery_codes TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON staff_auth_challenges FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON staff_auth_challenges TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (attempts_left, consumed_at) ON staff_auth_challenges TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON staff_enrolment_tokens FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON staff_enrolment_tokens TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (consumed_at, revoked_at) ON staff_enrolment_tokens TO igaming_runtime';
    END IF;
END
$$;
