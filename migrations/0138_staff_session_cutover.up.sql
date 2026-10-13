-- ADR 0119 slice S3 (staff session assurance and cutover; ADR 0119 s7.2, s13.1a, s13.4, review C1). IDENTITY /
-- AUTHENTICATION INTEGRITY CONTROL: FOR `security` (live review), `code-reviewer`, `qa` REVIEW BEFORE MERGE (gate G).
--
-- NUMBERING: 0138 (gap-free after 0137_staff_mfa). 0139 is the staff lifecycle (S5 + S6); AML uses 0140+.
--
-- DEPLOY ORDER (ADR 0119 s13.1a item 4; docs/runbooks/staff-mfa-s3-cutover.md): the S3 code is deployed to EVERY pod
-- first, then this migration runs. A pre-S3 pod still running afterwards fails every staff login with SF040 (its
-- IssueSession writes no assurance columns), which is fail-closed. Every staff session that exists when this runs is
-- revoked here, so every staff member logs in again through the S3 MFA flow.
--
-- What it adds, and only this:
--   0. SET LOCAL lock_timeout = '5s' (0133 / 0136 / 0137 precedent), for the whole file.
--   1. Pre-flight (SF041): refuses unless sessions has exactly the twelve columns the column-level UPDATE grant below
--      enumerates, staff_session_cutover does not exist yet, staff_mfa_factors (0137) carries the counter columns the
--      SF008 rule reads, and no staff.* alert Kind exists. Nothing is read beyond the catalog; nothing is changed.
--   2. sessions: seven NULLABLE assurance columns (aal, amr, auth_time, mfa_at, absolute_expires_at, assurance_kid,
--      assurance_seal) and two CHECKs (player rows carry none of them; shape and ordering of a staff row's values).
--      The seal is a keyed HMAC computed in Go (staffmfa.Keys.SealSession, MAC family subkey
--      staff-session-assurance-v1); the database holds no key, so it cannot verify the seal - the refresh path does.
--   3. staff_session_cutover: a one-row OWNER table holding this migration's now() (review C1). The runtime role gets
--      SELECT only; UPDATE / DELETE / TRUNCATE are refused by trigger (SF099) for every role. A staff session with a
--      NULL seal created before `at` predates S3: its refresh is a plain not-found (no audit, no alert).
--   4. Revocation of every live staff session (revoked_at IS NULL), asserted equal to a pre-count (SF041 otherwise).
--      DEVIATION (ADR 0119 s13.1a item 3 names the per-row app.session_internal_op_id technique): under FORCE ROW
--      LEVEL SECURITY the owner cannot ENUMERATE the rows to revoke (every SELECT policy on sessions needs a token
--      hash, a principal or a row id), so a count would be blind. FORCE is therefore lifted inside THIS transaction
--      for the count and the UPDATE only, and restored before anything else runs (the 0137 down precedent); a
--      refusal rolls the lift back. A catalog test pins relforcerowsecurity after the migration.
--   5. sessions_staff_assurance_guard (SF040), BEFORE INSERT OR UPDATE: a staff INSERT must carry all seven assurance
--      columns; on a staff row aal, amr, auth_time, absolute_expires_at, expires_at and the identity columns (id,
--      principal_id, tenant_id, created_at, refresh_token_hash) are immutable, revoked_at and replaced_by_session_id
--      are WRITE-ONCE (NULL -> a value, never changed or cleared afterwards: security review R1, an un-revoke or a
--      reset chain link would revive a logged-out or rotated refresh token), mfa_at never moves backwards or to NULL,
--      and the seal / kid never return to NULL; principal_type never changes on any row. The trigger binds every role,
--      the owner included; the keyed seal defeats SQL forgery of the rest.
--   6. Least privilege on sessions: UPDATE becomes COLUMN-level (revoked_at, last_used_at, replaced_by_session_id),
--      exactly the columns the S3 code writes (review F9: mfa_at / assurance_kid / assurance_seal are granted by the
--      slice that first writes them, S4b step-up). The runtime role can no longer UPDATE aal, amr, auth_time, mfa_at,
--      absolute_expires_at, expires_at, the seal or any identity column (ADR 0119 s13.4). INSERT, SELECT and DELETE
--      are unchanged (mirrored by deploy/init-app-role.sql).
--   7. ADR 0119 S3 entry gate G2 (S2-R1), option "DB-clock rule" (no owner decision needed): sf_factor_lockout_clock
--      (SF008) on staff_mfa_factors - failed_24h_count may DECREASE only when its window start is at least 24 hours
--      old by the DATABASE clock (now()), and failed_24h_start never moves backwards. The runtime role (threat M5)
--      can therefore no longer reset the hard-lock budget: after 10 failures in a window the application sets the
--      write-once hard_locked_at. The S3 flows take their clock from the same transaction's now(), so the
--      application's own window reset can never be refused by this rule.
--   8. Ten alert Kinds (ADR 0119 s5.3, s6.1, s6.5, s7.2, S3 gate (e)): each ADR signal has a TENANT-staff Kind
--      (requires_subject, raised from the tenant session) and a PLATFORM-staff Kind (no subject, raised from the
--      platform staff member's own platform session). DEVIATION: ADR 0102 / migration 0110 require a subject tenant
--      for every business Kind and a Kind name with a dot, and a platform staff member has no tenant; the ADR's names
--      map as staff_mfa_integrity -> staff.mfa_integrity / staff.platform_mfa_integrity (and likewise). The
--      alert_kinds_check1 CHECK is widened by exactly the five named platform Kinds. Seeded under alert_kinds' FORCE
--      RLS exactly as 0114 / 0130 seed (a temporary literal FOR INSERT TO CURRENT_USER policy). Alert NOTIFICATION
--      is NOT IMPLEMENTED (no route or recipient is seeded; ALERT-DELIVERY-1).
--
-- Every new function is plain plpgsql (NOT SECURITY DEFINER) with a pinned search_path; messages name columns, never
-- values. The down reverses exactly this file and refuses (SF042) while alerts of the new Kinds exist or while a
-- live MFA-assured staff session exists. The revocation of step 4 is NOT reversed by the down (sessions stay revoked).

SET LOCAL lock_timeout = '5s';

-- =========================================================================
-- 1. Pre-flight (SF041)
-- =========================================================================
DO $$
DECLARE
    v_expected text[] := ARRAY['created_at', 'expires_at', 'id', 'ip_address', 'last_used_at', 'principal_id',
                               'principal_type', 'refresh_token_hash', 'replaced_by_session_id', 'revoked_at',
                               'tenant_id', 'user_agent'];
    v_found    text[];
    v_cutover  regclass;
    v_counters bigint;
    v_kinds    bigint;
BEGIN
    SELECT COALESCE(pg_catalog.array_agg(attname::text ORDER BY attname), ARRAY[]::text[]) INTO v_found
      FROM pg_catalog.pg_attribute
     WHERE attrelid = 'public.sessions'::pg_catalog.regclass AND attnum > 0 AND NOT attisdropped;
    v_cutover := pg_catalog.to_regclass('public.staff_session_cutover');
    SELECT pg_catalog.count(*) INTO v_counters FROM pg_catalog.pg_attribute
     WHERE attrelid = pg_catalog.to_regclass('public.staff_mfa_factors')
       AND attname IN ('failed_24h_count', 'failed_24h_start', 'hard_locked_at') AND NOT attisdropped;
    SELECT pg_catalog.count(*) INTO v_kinds FROM public.alert_kinds WHERE kind LIKE 'staff.%';
    IF v_found IS DISTINCT FROM v_expected OR v_cutover IS NOT NULL OR v_counters <> 3 OR v_kinds <> 0 THEN
        RAISE EXCEPTION 'migration 0138 pre-flight: the schema is not the one this migration was written against (ADR 0119 s13.1a): sessions_columns=%, staff_session_cutover_exists=%, staff_mfa_factors_counter_columns=%, staff_alert_kinds=%. Nothing was changed. A new sessions column must be added to the column-level UPDATE grant (or deliberately left out) in this migration first; do NOT loosen the check.',
            v_found, v_cutover IS NOT NULL, v_counters, v_kinds
            USING ERRCODE = 'SF041';
    END IF;
END
$$;

-- =========================================================================
-- 2. sessions: the assurance columns
-- =========================================================================
ALTER TABLE sessions
    ADD COLUMN aal                 SMALLINT NULL,
    ADD COLUMN amr                 TEXT[] NULL,
    ADD COLUMN auth_time           TIMESTAMPTZ NULL,
    ADD COLUMN mfa_at              TIMESTAMPTZ NULL,
    ADD COLUMN absolute_expires_at TIMESTAMPTZ NULL,
    ADD COLUMN assurance_kid       TEXT NULL,
    ADD COLUMN assurance_seal      BYTEA NULL;

COMMENT ON COLUMN sessions.assurance_seal IS 'ADR 0119 s7.2: keyed HMAC (MAC family subkey staff-session-assurance-v1 of assurance_kid) over id, principal_type, principal_id, tenant, aal, amr, auth_time, mfa_at, absolute_expires_at. Verified in Go on every staff refresh; the database holds no key.';

-- Player rows never carry assurance values; a staff row's values are well-formed and ordered.
ALTER TABLE sessions ADD CONSTRAINT sessions_assurance_staff_only CHECK (
    principal_type = 'staff'
    OR (aal IS NULL AND amr IS NULL AND auth_time IS NULL AND mfa_at IS NULL AND absolute_expires_at IS NULL
        AND assurance_kid IS NULL AND assurance_seal IS NULL));
ALTER TABLE sessions ADD CONSTRAINT sessions_assurance_shape CHECK (
    (aal IS NULL OR aal = 2)
    AND (amr IS NULL OR (pg_catalog.cardinality(amr) BETWEEN 1 AND 8 AND pg_catalog.array_position(amr, NULL) IS NULL))
    AND ((assurance_kid IS NULL) = (assurance_seal IS NULL))
    AND (assurance_kid IS NULL OR assurance_kid ~ '^[A-Za-z0-9._-]{1,32}$')
    AND (assurance_seal IS NULL OR pg_catalog.octet_length(assurance_seal) = 32)
    AND (mfa_at IS NULL OR auth_time IS NULL OR mfa_at >= auth_time)
    AND (absolute_expires_at IS NULL OR auth_time IS NULL OR absolute_expires_at > auth_time)
    AND (absolute_expires_at IS NULL OR expires_at <= absolute_expires_at));

-- =========================================================================
-- 3. The one-row cutover marker (owner table; runtime SELECT only)
-- =========================================================================
CREATE TABLE staff_session_cutover (
    singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    at        TIMESTAMPTZ NOT NULL
);
COMMENT ON TABLE staff_session_cutover IS 'ADR 0119 s7.2 / s13.1a (review C1): the instant migration 0138 ran. A staff session with a NULL assurance seal created before it predates S3 (plain not-found on refresh); a NULL seal at or after it is tampering (P1). Written once by the migration; immutable.';

INSERT INTO staff_session_cutover (singleton, at) VALUES (true, now());

CREATE FUNCTION sf_cutover_immutable() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION 'staff_session_cutover is immutable: % is not permitted (SF099)', TG_OP USING ERRCODE = 'SF099';
END
$$;
CREATE TRIGGER sf_cutover_immutable BEFORE UPDATE OR DELETE ON staff_session_cutover
    FOR EACH ROW EXECUTE FUNCTION sf_cutover_immutable();
CREATE TRIGGER sf_cutover_no_truncate BEFORE TRUNCATE ON staff_session_cutover
    FOR EACH STATEMENT EXECUTE FUNCTION sf_cutover_immutable();

ALTER TABLE staff_session_cutover ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_session_cutover FORCE ROW LEVEL SECURITY;
-- Readable by an ordinary (tenant, platform or session-lookup) transaction - the staff refresh path reads it inside
-- db.Pool.WithSessionLookup - and by no platform-service, acting or player-scoped session (no such session needs it;
-- the 0112 acting fence, the 0114 KYC-worker fence and the A-18 guard stay clean).
CREATE POLICY staff_session_cutover_read ON staff_session_cutover FOR SELECT USING (
    NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
    AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    AND NOT financial_acting_gucs_present());

REVOKE ALL ON staff_session_cutover FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON staff_session_cutover FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON staff_session_cutover TO igaming_runtime';
    END IF;
END
$$;

-- =========================================================================
-- 4. Revoke every live staff session (asserted against a pre-count)
-- =========================================================================
ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY;
DO $$
DECLARE
    v_pre  bigint;
    v_done bigint;
BEGIN
    SELECT pg_catalog.count(*) INTO v_pre FROM public.sessions WHERE principal_type = 'staff' AND revoked_at IS NULL;
    UPDATE public.sessions SET revoked_at = now() WHERE principal_type = 'staff' AND revoked_at IS NULL;
    GET DIAGNOSTICS v_done = ROW_COUNT;
    IF v_done <> v_pre THEN
        RAISE EXCEPTION 'migration 0138: revoked % staff session(s) but counted % beforehand; nothing was changed', v_done, v_pre
            USING ERRCODE = 'SF041';
    END IF;
    RAISE NOTICE 'migration 0138: revoked % live staff session(s) (ADR 0119 s13.1a)', v_done;
END
$$;
ALTER TABLE sessions FORCE ROW LEVEL SECURITY;

-- =========================================================================
-- 5. SF040: sessions_staff_assurance_guard
-- =========================================================================
CREATE FUNCTION sessions_staff_assurance_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_cols text[] := ARRAY[]::text[];
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.principal_type = 'staff' THEN
            IF NEW.aal IS NULL THEN v_cols := v_cols || 'aal'::text; END IF;
            IF NEW.amr IS NULL THEN v_cols := v_cols || 'amr'::text; END IF;
            IF NEW.auth_time IS NULL THEN v_cols := v_cols || 'auth_time'::text; END IF;
            IF NEW.mfa_at IS NULL THEN v_cols := v_cols || 'mfa_at'::text; END IF;
            IF NEW.absolute_expires_at IS NULL THEN v_cols := v_cols || 'absolute_expires_at'::text; END IF;
            IF NEW.assurance_kid IS NULL THEN v_cols := v_cols || 'assurance_kid'::text; END IF;
            IF NEW.assurance_seal IS NULL THEN v_cols := v_cols || 'assurance_seal'::text; END IF;
            IF pg_catalog.cardinality(v_cols) > 0 THEN
                RAISE EXCEPTION 'sessions: a staff session must carry its assurance columns; missing: % (SF040)',
                    pg_catalog.array_to_string(v_cols, ', ') USING ERRCODE = 'SF040';
            END IF;
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.principal_type IS DISTINCT FROM OLD.principal_type THEN
        RAISE EXCEPTION 'sessions: principal_type is immutable (SF040)' USING ERRCODE = 'SF040';
    END IF;
    IF OLD.principal_type = 'staff' THEN
        IF NEW.id IS DISTINCT FROM OLD.id THEN v_cols := v_cols || 'id'::text; END IF;
        IF NEW.principal_id IS DISTINCT FROM OLD.principal_id THEN v_cols := v_cols || 'principal_id'::text; END IF;
        IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id THEN v_cols := v_cols || 'tenant_id'::text; END IF;
        IF NEW.created_at IS DISTINCT FROM OLD.created_at THEN v_cols := v_cols || 'created_at'::text; END IF;
        IF NEW.refresh_token_hash IS DISTINCT FROM OLD.refresh_token_hash THEN v_cols := v_cols || 'refresh_token_hash'::text; END IF;
        IF NEW.aal IS DISTINCT FROM OLD.aal THEN v_cols := v_cols || 'aal'::text; END IF;
        IF NEW.amr IS DISTINCT FROM OLD.amr THEN v_cols := v_cols || 'amr'::text; END IF;
        IF NEW.auth_time IS DISTINCT FROM OLD.auth_time THEN v_cols := v_cols || 'auth_time'::text; END IF;
        IF NEW.absolute_expires_at IS DISTINCT FROM OLD.absolute_expires_at THEN v_cols := v_cols || 'absolute_expires_at'::text; END IF;
        IF NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN v_cols := v_cols || 'expires_at'::text; END IF;
        IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN v_cols := v_cols || 'revoked_at'::text; END IF;
        IF OLD.replaced_by_session_id IS NOT NULL AND NEW.replaced_by_session_id IS DISTINCT FROM OLD.replaced_by_session_id THEN
            v_cols := v_cols || 'replaced_by_session_id'::text;
        END IF;
        IF pg_catalog.cardinality(v_cols) > 0 THEN
            RAISE EXCEPTION 'sessions: immutable staff session column(s) changed: % (SF040)',
                pg_catalog.array_to_string(v_cols, ', ') USING ERRCODE = 'SF040';
        END IF;
        IF OLD.mfa_at IS NOT NULL AND (NEW.mfa_at IS NULL OR NEW.mfa_at < OLD.mfa_at) THEN
            RAISE EXCEPTION 'sessions: mfa_at never moves backwards or to NULL (SF040)' USING ERRCODE = 'SF040';
        END IF;
        IF (OLD.assurance_seal IS NOT NULL AND NEW.assurance_seal IS NULL)
           OR (OLD.assurance_kid IS NOT NULL AND NEW.assurance_kid IS NULL) THEN
            RAISE EXCEPTION 'sessions: the assurance seal never returns to NULL (SF040)' USING ERRCODE = 'SF040';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER sessions_staff_assurance_guard BEFORE INSERT OR UPDATE ON sessions
    FOR EACH ROW EXECUTE FUNCTION sessions_staff_assurance_guard();

-- =========================================================================
-- 6. Least privilege on sessions (mirrored by deploy/init-app-role.sql)
-- =========================================================================
-- Narrow, never widen: only a runtime role that holds table-level UPDATE today is narrowed (a database whose runtime
-- role holds no UPDATE on sessions is left as it is).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime')
       AND pg_catalog.has_table_privilege('igaming_runtime', 'public.sessions', 'UPDATE') THEN
        EXECUTE 'REVOKE UPDATE ON sessions FROM igaming_runtime';
        EXECUTE 'GRANT UPDATE (revoked_at, last_used_at, replaced_by_session_id) ON sessions TO igaming_runtime';
    END IF;
END
$$;

-- =========================================================================
-- 7. SF008: the database-clock rule on the hard-lock budget (S3 entry gate G2)
-- =========================================================================
CREATE FUNCTION sf_factor_lockout_clock() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF NEW.failed_24h_count < OLD.failed_24h_count
       AND NOT (OLD.failed_24h_start IS NOT NULL AND OLD.failed_24h_start <= now() - interval '24 hours') THEN
        RAISE EXCEPTION 'staff_mfa_factors: failed_24h_count may decrease only after its 24 hour window has closed by the database clock (SF008)'
            USING ERRCODE = 'SF008';
    END IF;
    IF OLD.failed_24h_start IS NOT NULL AND NEW.failed_24h_start IS NOT NULL AND NEW.failed_24h_start < OLD.failed_24h_start THEN
        RAISE EXCEPTION 'staff_mfa_factors: failed_24h_start must not move backwards (SF008)' USING ERRCODE = 'SF008';
    END IF;
    RETURN NEW;
END
$$;
-- Named so it fires after sf_factor_guard (the SF guards fire in the order of their names).
CREATE TRIGGER sf_factor_lockout_clock BEFORE UPDATE ON staff_mfa_factors
    FOR EACH ROW EXECUTE FUNCTION sf_factor_lockout_clock();

-- =========================================================================
-- 8. The staff alert Kinds
-- =========================================================================
ALTER TABLE alert_kinds DROP CONSTRAINT alert_kinds_check1;
ALTER TABLE alert_kinds ADD CONSTRAINT alert_kinds_check1 CHECK (
    requires_subject = true
    OR kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed',
                'staff.platform_mfa_integrity', 'staff.platform_session_integrity', 'staff.platform_mfa_hard_locked',
                'staff.platform_mfa_recovery_used', 'staff.platform_mfa_failure_pattern'));

DO $$
BEGIN
    CREATE POLICY alert_kinds_seed_0138 ON alert_kinds
        FOR INSERT TO CURRENT_USER
        WITH CHECK (kind IN ('staff.mfa_integrity', 'staff.session_integrity', 'staff.mfa_hard_locked',
                             'staff.mfa_recovery_used', 'staff.mfa_failure_pattern',
                             'staff.platform_mfa_integrity', 'staff.platform_session_integrity',
                             'staff.platform_mfa_hard_locked', 'staff.platform_mfa_recovery_used',
                             'staff.platform_mfa_failure_pattern'));
    INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject, in_tx_raisable_by_tenant, allowed_keys, raise_mode) VALUES
        ('staff.mfa_integrity',                 'p1', 'platform', false, true,  true,  ARRAY['staff_id', 'cause', 'request_id'],              'post_commit'),
        ('staff.session_integrity',             'p1', 'platform', false, true,  true,  ARRAY['staff_id', 'session_id', 'cause', 'request_id'], 'post_commit'),
        ('staff.mfa_hard_locked',               'p2', 'platform', false, true,  true,  ARRAY['staff_id', 'factor_id', 'request_id'],          'post_commit'),
        ('staff.mfa_recovery_used',             'p3', 'platform', false, true,  true,  ARRAY['staff_id', 'request_id'],                       'post_commit'),
        ('staff.mfa_failure_pattern',           'p2', 'platform', false, true,  true,  ARRAY['staff_id', 'distinct_days', 'request_id'],      'post_commit'),
        ('staff.platform_mfa_integrity',        'p1', 'platform', false, false, false, ARRAY['staff_id', 'cause', 'request_id'],              'post_commit'),
        ('staff.platform_session_integrity',    'p1', 'platform', false, false, false, ARRAY['staff_id', 'session_id', 'cause', 'request_id'], 'post_commit'),
        ('staff.platform_mfa_hard_locked',      'p2', 'platform', false, false, false, ARRAY['staff_id', 'factor_id', 'request_id'],          'post_commit'),
        ('staff.platform_mfa_recovery_used',    'p3', 'platform', false, false, false, ARRAY['staff_id', 'request_id'],                       'post_commit'),
        ('staff.platform_mfa_failure_pattern',  'p2', 'platform', false, false, false, ARRAY['staff_id', 'distinct_days', 'request_id'],      'post_commit');
    DROP POLICY alert_kinds_seed_0138 ON alert_kinds;
END
$$;
