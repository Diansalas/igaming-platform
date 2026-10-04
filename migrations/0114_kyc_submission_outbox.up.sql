-- PRH-2 E1 (KYC-SUBMIT-OUTBOX-1; ADR 0106 revision 2, ADR 0095 section 38;
-- HD-PRH2-10 and HD-PRH2-11 in ADR 0105).
--
-- What this adds:
--   1. kyc_submission_outbox: the durable create/submit outbox for the KYC
--      vendor calls. It stores ids, states and counters ONLY: no PII, no
--      document content, no storage reference, no vendor reference, no
--      credential, no raw error text. FORCE ROW LEVEL SECURITY with split
--      per-command policies (no FOR ALL, no DELETE policy, no NULL-tenant
--      arm, no USING (true)).
--   2. The guard trigger: insert key recompute, forced initial state, column
--      pinning, lease bounds, DB-validated deferral, one live create per
--      player account, terminal-row rules, and TRUNCATE/DELETE refusal.
--   3. The worker identity fence family: 36 AS RESTRICTIVE policies
--      (kyc_worker_fence_*) on the nine tables where a session with no tenant
--      GUC has read or write access beyond public reference data. The
--      predicate is true for every other session, so no other session changes.
--   4. The dedicated alert Kind kyc.submission_failed_terminal, seeded under
--      alert_kinds' FORCE RLS through a temporary, literal, single-Kind,
--      FOR INSERT TO CURRENT_USER policy created and dropped inside one DO
--      block (security Q-S2). FORCE RLS on alert_kinds is never toggled.
--
-- HQ-E1-2 (severity) is an OPEN human question: p2 is an engineering default.
-- Nothing here names a recipient, route, threshold or retention period.

-- =========================================================================
-- 1. Session-shape helper (never SECURITY DEFINER)
-- =========================================================================

CREATE FUNCTION kyc_submission_outbox_session() RETURNS TEXT AS $$
DECLARE
    v_tenant  TEXT := NULLIF(current_setting('app.tenant_id', true), '');
    v_service TEXT := NULLIF(current_setting('app.platform_service_id', true), '');
BEGIN
    IF NULLIF(current_setting('app.player_account_id', true), '') IS NOT NULL
       OR NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
       OR NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
       OR NULLIF(current_setting('app.acting_tenant_id', true), '') IS NOT NULL
       OR NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NOT NULL THEN
        RAISE EXCEPTION 'kyc_submission_outbox: session shape not permitted to write';
    END IF;
    IF v_service IS NOT NULL THEN
        IF v_service <> 'kyc_submission_worker' OR v_tenant IS NOT NULL THEN
            RAISE EXCEPTION 'kyc_submission_outbox: unknown or mixed platform-service session';
        END IF;
        RETURN 'worker';
    END IF;
    IF v_tenant IS NULL THEN
        RAISE EXCEPTION 'kyc_submission_outbox: no tenant session';
    END IF;
    RETURN 'tenant';
END;
$$ LANGUAGE plpgsql STABLE
-- Security L-1: a pinned search_path with pg_temp LAST, so a TEMP table (the
-- runtime role holds TEMP) can never shadow a table this function reads.
SET search_path = pg_catalog, public, pg_temp;

-- =========================================================================
-- 2. The outbox table
-- =========================================================================

CREATE TABLE kyc_submission_outbox (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL REFERENCES tenants (id),
    verification_id    UUID NOT NULL,
    -- Forced from the verification by the guard trigger (security F3 / IC C2).
    player_account_id  UUID NOT NULL,
    FOREIGN KEY (verification_id, tenant_id)   REFERENCES kyc_verifications (id, tenant_id),
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id),
    operation          TEXT NOT NULL CHECK (operation IN ('create', 'submit')),
    -- Security F5: charset-checked; the alert's provider_id attribute and
    -- discriminator copy it.
    provider_id        TEXT NOT NULL CHECK (provider_id ~ '^[A-Za-z0-9_.-]{1,64}$'),
    document_ids       UUID[] NOT NULL DEFAULT '{}',
    idempotency_key    TEXT NOT NULL CHECK (idempotency_key ~ '^(kv:[0-9a-f-]{36}|ks:[0-9a-f-]{36}:[0-9a-f]{64})$'),
    state              TEXT NOT NULL DEFAULT 'pending'
                           CHECK (state IN ('pending', 'claimed', 'sent', 'failed_terminal', 'cancelled')),
    claim_token        UUID NULL,
    claimed_by_service TEXT NULL CHECK (claimed_by_service IS NULL OR claimed_by_service = 'kyc_submission_worker'),
    claimed_at         TIMESTAMPTZ NULL,
    lease_expires_at   TIMESTAMPTZ NULL,
    claims             INT NOT NULL DEFAULT 0 CHECK (claims >= 0),
    failed_attempts    INT NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
    next_attempt_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error_class   TEXT NULL CHECK (last_error_class IN
                           ('ambiguous', 'not_sent', 'lease_expired', 'deferred_tenant_inactive',
                            'credential_binding_mismatch', 'apply_conflict')),
    cancel_reason      TEXT NULL CHECK (cancel_reason IN
                           ('superseded', 'verification_terminal', 'no_documents', 'document_set_changed',
                            'verification_not_submitted', 'provider_deconfigured', 'decided_concurrently')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    terminal_at        TIMESTAMPTZ NULL,

    CHECK ((state = 'claimed') = (lease_expires_at IS NOT NULL)),
    CHECK ((state = 'claimed') <= (claim_token IS NOT NULL)),
    CHECK ((state IN ('sent', 'failed_terminal', 'cancelled')) = (terminal_at IS NOT NULL)),
    CHECK ((state = 'cancelled') = (cancel_reason IS NOT NULL)),
    CHECK (state <> 'failed_terminal' OR last_error_class IS NOT NULL),
    CHECK ((operation = 'create') = (cardinality(document_ids) = 0)),
    CHECK (operation <> 'create' OR idempotency_key = 'kv:' || verification_id::text),
    CHECK (operation <> 'submit' OR idempotency_key LIKE 'ks:' || verification_id::text || ':%')
);

COMMENT ON TABLE kyc_submission_outbox IS
    'ADR 0106 KYC create/submit outbox. Ids, states and counters only: no PII, document content, storage reference, vendor reference, credential or raw error text. Never read by enforcement (INV-KYC-OB-5). Rows are the only durable record of worker claims: never deleted (retention HQ-E1-4 is an open human decision).';

-- A duplicate submit of the same content is a no-op while a live or sent row
-- exists; after failed_terminal or cancelled the same content may be enqueued
-- again.
CREATE UNIQUE INDEX kyc_submission_outbox_key_live ON kyc_submission_outbox (tenant_id, idempotency_key)
    WHERE state IN ('pending', 'claimed', 'sent');
-- A verification is created with the vendor at most once.
CREATE UNIQUE INDEX kyc_submission_outbox_one_create ON kyc_submission_outbox (tenant_id, verification_id)
    WHERE operation = 'create';
-- Security F3 / IC C2: at most one LIVE create per player account, DB-enforced
-- (not check-then-insert).
CREATE UNIQUE INDEX kyc_submission_outbox_one_live_create_per_player ON kyc_submission_outbox (tenant_id, player_account_id)
    WHERE operation = 'create' AND state IN ('pending', 'claimed');
CREATE INDEX kyc_submission_outbox_due      ON kyc_submission_outbox (next_attempt_at, created_at, id) WHERE state = 'pending';
CREATE INDEX kyc_submission_outbox_lease    ON kyc_submission_outbox (lease_expires_at) WHERE state = 'claimed';
CREATE INDEX kyc_submission_outbox_by_verif ON kyc_submission_outbox (tenant_id, verification_id, created_at, id);

-- =========================================================================
-- 3. The guard trigger (the backstop behind RLS and the Go claim-token CAS)
-- =========================================================================

CREATE FUNCTION kyc_submission_outbox_guard() RETURNS TRIGGER AS $$
DECLARE
    v_session       TEXT;
    v_ver           RECORD;
    v_expected_key  TEXT;
    v_i             INT;
    v_n             INT;
    v_tenant_status TEXT;
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'kyc_submission_outbox: TRUNCATE is not permitted';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'kyc_submission_outbox: DELETE is not permitted';
    END IF;

    v_session := kyc_submission_outbox_session();

    -- ---------------------------------------------------------------- INSERT
    IF TG_OP = 'INSERT' THEN
        IF v_session <> 'tenant' THEN
            RAISE EXCEPTION 'kyc_submission_outbox: only a tenant session may insert';
        END IF;
        IF NEW.tenant_id IS DISTINCT FROM NULLIF(current_setting('app.tenant_id', true), '')::uuid THEN
            RAISE EXCEPTION 'kyc_submission_outbox: tenant_id must equal the session tenant';
        END IF;

        SELECT id, status, provider_id, provider_reference, player_account_id INTO v_ver
          FROM kyc_verifications WHERE id = NEW.verification_id AND tenant_id = NEW.tenant_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'kyc_submission_outbox: verification not found in this tenant';
        END IF;
        IF NEW.provider_id IS DISTINCT FROM v_ver.provider_id THEN
            RAISE EXCEPTION 'kyc_submission_outbox: provider_id must equal the verification provider_id';
        END IF;
        -- Forced, never trusted: the player account is the verification's.
        NEW.player_account_id := v_ver.player_account_id;

        IF NEW.operation = 'create' THEN
            IF NOT (v_ver.status = 'unverified' AND v_ver.provider_reference IS NULL) THEN
                RAISE EXCEPTION 'kyc_submission_outbox: create requires the orphan-shaped verification';
            END IF;
            IF NEW.document_ids <> '{}'::uuid[] THEN
                RAISE EXCEPTION 'kyc_submission_outbox: create must not pin documents';
            END IF;
            IF NEW.idempotency_key IS DISTINCT FROM 'kv:' || NEW.verification_id::text THEN
                RAISE EXCEPTION 'kyc_submission_outbox: create idempotency key mismatch';
            END IF;
        ELSE
            v_n := cardinality(NEW.document_ids);
            IF v_n IS NULL OR v_n < 1 THEN
                RAISE EXCEPTION 'kyc_submission_outbox: submit requires at least one document';
            END IF;
            -- Distinct and in ascending text order (strictly ascending).
            FOR v_i IN 2..v_n LOOP
                IF NOT (NEW.document_ids[v_i]::text > NEW.document_ids[v_i - 1]::text) THEN
                    RAISE EXCEPTION 'kyc_submission_outbox: document_ids must be distinct and in ascending text order';
                END IF;
            END LOOP;
            IF EXISTS (SELECT 1 FROM unnest(NEW.document_ids) AS d
                        WHERE NOT EXISTS (SELECT 1 FROM kyc_documents k
                                           WHERE k.id = d AND k.tenant_id = NEW.tenant_id
                                             AND k.verification_id = NEW.verification_id
                                             AND k.status <> 'rejected')) THEN
                RAISE EXCEPTION 'kyc_submission_outbox: every pinned document must be a non-rejected document of this verification';
            END IF;
            -- Byte-identical to Go's submissionIdempotencyKey: recomputed, never trusted.
            v_expected_key := 'ks:' || NEW.verification_id::text || ':' || encode(sha256(
                (SELECT string_agg(convert_to(d::text, 'UTF8') || '\x00'::bytea, ''::bytea ORDER BY d::text)
                   FROM unnest(NEW.document_ids) AS d)), 'hex');
            IF NEW.idempotency_key IS DISTINCT FROM v_expected_key THEN
                RAISE EXCEPTION 'kyc_submission_outbox: submit idempotency key does not match the pinned document set';
            END IF;
        END IF;

        -- Forced initial shape.
        NEW.state := 'pending';
        NEW.claim_token := NULL;
        NEW.claimed_by_service := NULL;
        NEW.claimed_at := NULL;
        NEW.lease_expires_at := NULL;
        NEW.claims := 0;
        NEW.failed_attempts := 0;
        NEW.last_error_class := NULL;
        NEW.cancel_reason := NULL;
        NEW.terminal_at := NULL;
        NEW.created_at := now();
        NEW.updated_at := now();
        -- Security I-1: a new row is always due now, never at a caller-supplied
        -- time (a far-future value on a live create would block the player's
        -- future creates behind the per-player unique index).
        NEW.next_attempt_at := now();
        RETURN NEW;
    END IF;

    -- ---------------------------------------------------------------- UPDATE
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.verification_id IS DISTINCT FROM OLD.verification_id
       OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
       OR NEW.operation IS DISTINCT FROM OLD.operation
       OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
       OR NEW.document_ids IS DISTINCT FROM OLD.document_ids
       OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'kyc_submission_outbox: immutable column changed';
    END IF;
    IF OLD.state IN ('sent', 'failed_terminal', 'cancelled') THEN
        RAISE EXCEPTION 'kyc_submission_outbox: terminal rows are immutable';
    END IF;

    -- Worker: claim and re-claim only. Every pinned column is assigned from OLD
    -- explicitly (security F6): a column-by-column list, not "unchanged unless set".
    IF v_session = 'worker' THEN
        IF NEW.state IS DISTINCT FROM 'claimed' THEN
            RAISE EXCEPTION 'kyc_submission_outbox: the worker may only claim';
        END IF;
        IF OLD.state = 'pending' THEN
            IF OLD.next_attempt_at > now() THEN
                RAISE EXCEPTION 'kyc_submission_outbox: row is not due';
            END IF;
            IF OLD.operation = 'submit' AND EXISTS (
                   SELECT 1 FROM kyc_submission_outbox c
                    WHERE c.tenant_id = OLD.tenant_id AND c.verification_id = OLD.verification_id
                      AND c.operation = 'create' AND c.state IN ('pending', 'claimed')) THEN
                RAISE EXCEPTION 'kyc_submission_outbox: submit is not claimable while the create is live';
            END IF;
            NEW.failed_attempts := OLD.failed_attempts;
            NEW.last_error_class := OLD.last_error_class;
        ELSIF OLD.state = 'claimed' THEN
            IF OLD.lease_expires_at > now() THEN
                RAISE EXCEPTION 'kyc_submission_outbox: lease has not expired';
            END IF;
            -- The only two non-OLD values on a re-claim.
            NEW.failed_attempts := OLD.failed_attempts + 1;
            NEW.last_error_class := 'lease_expired';
        ELSE
            RAISE EXCEPTION 'kyc_submission_outbox: unexpected state for a claim';
        END IF;
        IF NEW.lease_expires_at IS NULL OR NEW.lease_expires_at <= now()
           OR NEW.lease_expires_at > now() + interval '10 minutes' THEN
            RAISE EXCEPTION 'kyc_submission_outbox: lease must be in (now, now + 10 minutes]';
        END IF;
        NEW.claim_token := gen_random_uuid();
        NEW.claimed_by_service := NULLIF(current_setting('app.platform_service_id', true), '');
        NEW.claimed_at := now();
        NEW.claims := OLD.claims + 1;
        NEW.next_attempt_at := OLD.next_attempt_at;
        NEW.cancel_reason := OLD.cancel_reason;
        NEW.terminal_at := OLD.terminal_at;
        NEW.updated_at := now();
        RETURN NEW;
    END IF;

    -- Tenant: every other transition. The claim columns never change here.
    NEW.claim_token := OLD.claim_token;
    NEW.claimed_by_service := OLD.claimed_by_service;
    NEW.claimed_at := OLD.claimed_at;
    NEW.claims := OLD.claims;
    NEW.updated_at := now();

    IF OLD.state = 'pending' THEN
        -- Only the validated pending -> cancelled / verification_not_submitted row.
        IF NEW.state IS DISTINCT FROM 'cancelled'
           OR NEW.cancel_reason IS DISTINCT FROM 'verification_not_submitted'
           OR OLD.operation <> 'submit' THEN
            RAISE EXCEPTION 'kyc_submission_outbox: a tenant may only cancel a pending submit as verification_not_submitted';
        END IF;
        IF NOT EXISTS (
               SELECT 1 FROM kyc_submission_outbox c
                WHERE c.tenant_id = OLD.tenant_id AND c.verification_id = OLD.verification_id
                  AND c.operation = 'create' AND c.state IN ('failed_terminal', 'cancelled')) THEN
            RAISE EXCEPTION 'kyc_submission_outbox: the create of this verification has not ended terminal';
        END IF;
        NEW.failed_attempts := OLD.failed_attempts;
        NEW.last_error_class := OLD.last_error_class;
        NEW.next_attempt_at := OLD.next_attempt_at;
        NEW.lease_expires_at := NULL;
        NEW.terminal_at := now();
        RETURN NEW;
    END IF;

    -- OLD.state = 'claimed'
    IF NEW.state = 'sent' THEN
        NEW.failed_attempts := OLD.failed_attempts;
        NEW.last_error_class := OLD.last_error_class;
        NEW.cancel_reason := NULL;
        NEW.next_attempt_at := OLD.next_attempt_at;
        NEW.lease_expires_at := NULL;
        NEW.terminal_at := now();
    ELSIF NEW.state = 'pending' THEN
        IF NEW.last_error_class IS NULL
           OR NEW.last_error_class NOT IN ('ambiguous', 'not_sent', 'deferred_tenant_inactive') THEN
            RAISE EXCEPTION 'kyc_submission_outbox: a retry needs class ambiguous, not_sent or deferred_tenant_inactive';
        END IF;
        IF NEW.next_attempt_at IS NULL OR NEW.next_attempt_at <= now()
           OR NEW.next_attempt_at > now() + interval '1 day' THEN
            RAISE EXCEPTION 'kyc_submission_outbox: next_attempt_at must be in (now, now + 1 day]';
        END IF;
        IF NEW.last_error_class = 'deferred_tenant_inactive' THEN
            -- DB-validated deferral (security F6): only while the tenant is not active.
            SELECT status INTO v_tenant_status FROM tenants WHERE id = NEW.tenant_id;
            IF NOT FOUND OR v_tenant_status IS NOT DISTINCT FROM 'active' THEN
                RAISE EXCEPTION 'kyc_submission_outbox: deferral is only valid for a non-active tenant';
            END IF;
            NEW.failed_attempts := OLD.failed_attempts;
        ELSE
            NEW.failed_attempts := OLD.failed_attempts + 1;
        END IF;
        NEW.cancel_reason := NULL;
        NEW.lease_expires_at := NULL;
        NEW.terminal_at := NULL;
    ELSIF NEW.state = 'failed_terminal' THEN
        IF NEW.last_error_class IS NULL
           OR NEW.last_error_class NOT IN ('ambiguous', 'not_sent', 'lease_expired',
                                            'credential_binding_mismatch', 'apply_conflict') THEN
            RAISE EXCEPTION 'kyc_submission_outbox: failed_terminal needs a terminal error class';
        END IF;
        IF NEW.last_error_class = 'lease_expired' THEN
            NEW.failed_attempts := OLD.failed_attempts;   -- already counted at re-claim
        ELSE
            NEW.failed_attempts := OLD.failed_attempts + 1;
        END IF;
        NEW.cancel_reason := NULL;
        NEW.next_attempt_at := OLD.next_attempt_at;
        NEW.lease_expires_at := NULL;
        NEW.terminal_at := now();
    ELSIF NEW.state = 'cancelled' THEN
        IF NEW.cancel_reason IS NULL THEN
            RAISE EXCEPTION 'kyc_submission_outbox: cancelled needs a cancel_reason';
        END IF;
        NEW.failed_attempts := OLD.failed_attempts;
        NEW.last_error_class := OLD.last_error_class;
        NEW.next_attempt_at := OLD.next_attempt_at;
        NEW.lease_expires_at := NULL;
        NEW.terminal_at := now();
    ELSE
        RAISE EXCEPTION 'kyc_submission_outbox: transition claimed -> % is not permitted', NEW.state;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
-- Security L-1: pinned search_path, pg_temp LAST. The trigger reads tenants,
-- kyc_verifications, kyc_documents and kyc_submission_outbox; a TEMP table of
-- the same name created by the runtime role must never decide a transition.
SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER kyc_submission_outbox_guard_row
    BEFORE INSERT OR UPDATE OR DELETE ON kyc_submission_outbox
    FOR EACH ROW EXECUTE FUNCTION kyc_submission_outbox_guard();
CREATE TRIGGER kyc_submission_outbox_guard_truncate
    BEFORE TRUNCATE ON kyc_submission_outbox
    FOR EACH STATEMENT EXECUTE FUNCTION kyc_submission_outbox_guard();

-- =========================================================================
-- 4. Row-level security (split policies; every predicate literal)
-- =========================================================================
--
-- X(tenant): player, platform-admin, platform-service, acting-tenant and
--            acting-platform GUCs all unset.
-- X(worker): app.platform_service_id = 'kyc_submission_worker' AND tenant,
--            player, platform-admin, principal, acting-tenant and
--            acting-platform GUCs all unset.

ALTER TABLE kyc_submission_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE kyc_submission_outbox FORCE ROW LEVEL SECURITY;

CREATE POLICY kso_tenant_select ON kyc_submission_outbox FOR SELECT
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

CREATE POLICY kso_tenant_insert ON kyc_submission_outbox FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
    );

-- USING admits 'pending' only for the trigger-validated pending -> cancelled /
-- verification_not_submitted row; every other tenant transition starts from
-- 'claimed'.
CREATE POLICY kso_tenant_update ON kyc_submission_outbox FOR UPDATE
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
        AND state IN ('claimed', 'pending')
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
        AND state IN ('pending', 'sent', 'failed_terminal', 'cancelled')
    );

CREATE POLICY kso_kyc_worker_select ON kyc_submission_outbox FOR SELECT
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'kyc_submission_worker'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

CREATE POLICY kso_kyc_worker_claim ON kyc_submission_outbox FOR UPDATE
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'kyc_submission_worker'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND state IN ('pending', 'claimed')
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'kyc_submission_worker'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND state = 'claimed'
    );

-- =========================================================================
-- 5. The worker fence family (security F1): 36 restrictive policies
-- =========================================================================
--
-- Derived from the LIVE catalogue (pg_policies) on a throwaway scratch
-- database at implementation time, not from the lexical replay alone (ADR 0106
-- section 3.3; the catalogue gate test keeps this true for every future
-- migration). The predicate is true for every session that is not the
-- kyc_submission_worker service (including a session with the GUC unset), so
-- no other session's access changes. Restrictive policies only narrow.
-- None of these tables is on the 0077 exact whitelist.

-- staff_users
CREATE POLICY kyc_worker_fence_select ON staff_users AS RESTRICTIVE FOR SELECT
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_insert ON staff_users AS RESTRICTIVE FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_update ON staff_users AS RESTRICTIVE FOR UPDATE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_delete ON staff_users AS RESTRICTIVE FOR DELETE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');

-- audit_log
CREATE POLICY kyc_worker_fence_select ON audit_log AS RESTRICTIVE FOR SELECT
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_insert ON audit_log AS RESTRICTIVE FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_update ON audit_log AS RESTRICTIVE FOR UPDATE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_delete ON audit_log AS RESTRICTIVE FOR DELETE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');

-- sessions
CREATE POLICY kyc_worker_fence_select ON sessions AS RESTRICTIVE FOR SELECT
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_insert ON sessions AS RESTRICTIVE FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_update ON sessions AS RESTRICTIVE FOR UPDATE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_delete ON sessions AS RESTRICTIVE FOR DELETE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');

-- login_attempts
CREATE POLICY kyc_worker_fence_select ON login_attempts AS RESTRICTIVE FOR SELECT
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_insert ON login_attempts AS RESTRICTIVE FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_update ON login_attempts AS RESTRICTIVE FOR UPDATE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_delete ON login_attempts AS RESTRICTIVE FOR DELETE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');

-- risk_rules
CREATE POLICY kyc_worker_fence_select ON risk_rules AS RESTRICTIVE FOR SELECT
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_insert ON risk_rules AS RESTRICTIVE FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_update ON risk_rules AS RESTRICTIVE FOR UPDATE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_delete ON risk_rules AS RESTRICTIVE FOR DELETE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');

-- player_restrictions
CREATE POLICY kyc_worker_fence_select ON player_restrictions AS RESTRICTIVE FOR SELECT
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_insert ON player_restrictions AS RESTRICTIVE FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_update ON player_restrictions AS RESTRICTIVE FOR UPDATE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_delete ON player_restrictions AS RESTRICTIVE FOR DELETE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');

-- persons
CREATE POLICY kyc_worker_fence_select ON persons AS RESTRICTIVE FOR SELECT
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_insert ON persons AS RESTRICTIVE FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_update ON persons AS RESTRICTIVE FOR UPDATE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_delete ON persons AS RESTRICTIVE FOR DELETE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');

-- asset_operation_eligibility
CREATE POLICY kyc_worker_fence_select ON asset_operation_eligibility AS RESTRICTIVE FOR SELECT
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_insert ON asset_operation_eligibility AS RESTRICTIVE FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_update ON asset_operation_eligibility AS RESTRICTIVE FOR UPDATE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_delete ON asset_operation_eligibility AS RESTRICTIVE FOR DELETE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');

-- open_bet_self_exclusion_policies
CREATE POLICY kyc_worker_fence_select ON open_bet_self_exclusion_policies AS RESTRICTIVE FOR SELECT
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_insert ON open_bet_self_exclusion_policies AS RESTRICTIVE FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_update ON open_bet_self_exclusion_policies AS RESTRICTIVE FOR UPDATE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker')
    WITH CHECK (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');
CREATE POLICY kyc_worker_fence_delete ON open_bet_self_exclusion_policies AS RESTRICTIVE FOR DELETE
    USING (NULLIF(current_setting('app.platform_service_id', true), '') IS DISTINCT FROM 'kyc_submission_worker');

-- =========================================================================
-- 6. Grants (0110 pattern; guarded so a database without the role migrates)
-- =========================================================================
--
-- SELECT, INSERT, UPDATE only: no DELETE, no TRUNCATE. No identity column
-- remains (the r1 global seq was removed, ADR 0106 F10), so there is no
-- sequence grant. Grants on the nine fenced tables are NOT changed (the fence
-- is RLS, not grants).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON kyc_submission_outbox FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE ON kyc_submission_outbox TO igaming_runtime';
    END IF;
END
$$;

-- =========================================================================
-- 7. Seed the KYC alert Kind (security Q-S2, APPROVED with conditions)
-- =========================================================================
--
-- alert_kinds is FORCE ROW LEVEL SECURITY with a SELECT-only policy, so a
-- plain owner INSERT is refused. The temporary policy below is literal SQL
-- (never EXECUTE), FOR INSERT only, TO CURRENT_USER, with a predicate that is
-- equality on exactly one Kind; CREATE, INSERT and DROP run inside this one DO
-- block (atomic: a failure after CREATE POLICY leaves neither the policy nor
-- the row). FORCE ROW LEVEL SECURITY is never toggled.
--
-- severity 'p2' is an ENGINEERING DEFAULT; HQ-E1-2 (human/compliance) is OPEN.
-- Alert NOTIFICATION is NOT IMPLEMENTED: no route, channel or recipient is
-- seeded or named (ALERT-DELIVERY-1 OPEN, HD-PRH2-4-OPS).
DO $$
BEGIN
    CREATE POLICY alert_kinds_seed_0114 ON alert_kinds
        FOR INSERT TO CURRENT_USER
        WITH CHECK (kind = 'kyc.submission_failed_terminal');
    INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject,
                             in_tx_raisable_by_tenant, allowed_keys, raise_mode)
    VALUES ('kyc.submission_failed_terminal', 'p2', 'platform', false, true, true,
            ARRAY['operation', 'provider_id', 'last_error_class', 'outbox_id'], 'in_tx');
    DROP POLICY alert_kinds_seed_0114 ON alert_kinds;
END
$$;
