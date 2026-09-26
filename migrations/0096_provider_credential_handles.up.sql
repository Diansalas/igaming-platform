-- Stage 10.3 W2a (ADR 0093 §1/§3 and its "Stage 10.3 W2a design review"
-- amendment; normative detail: docs/plans/stage-10.3-planning/
-- 07-w2a-design-review-security.md §1).
--
-- Three tables:
--
--   provider_credential_handles           tenant-owned; a HANDLE to a secret
--                                          held in an external store. No
--                                          secret value is ever stored here.
--   provider_credential_change_requests   platform-scoped four-eyes request
--                                          (hardened 0047/0089 shape), with a
--                                          narrow tenant "bridge" policy so the
--                                          tenant-scoped handle insert can
--                                          consume it.
--   provider_credential_change_approvals  platform-scoped, immutable
--                                          decisions; bridged read-only.
--
-- Rule: enabling a key takes two people (a DB-enforced, content-bound,
-- 24-hour approval by a different platform admin who is a different Person);
-- disabling takes one (verify_only, not_after shrink, revoke). No transition
-- ever leads back to 'active'. No function here is SECURITY DEFINER (B7), and
-- staff_users and provider_credential_handles get no platform/bridge policy.
--
-- Every RAISE carries a dedicated SQLSTATE (class 'PC') so the application
-- classifies errors by code and never by message text; the message text is
-- never returned to an API caller.
--
--   PC001 consume: activation request not found / not visible in scope
--   PC002 consume: request is not pending
--   PC003 consume: request targets another tenant
--   PC004 consume: a content column differs from the approved request
--   PC005 consume: recomputed content hash differs
--   PC006 consume: no unexpired approval by a different principal
--   PC007 consume: the request has a rejection
--   PC008 consume: predecessor disposition does not match
--   PC010 handle insert: status is not 'active'
--   PC011 handle insert: activation_request_id / created_by missing
--   PC012 handle insert: revoked_* set
--   PC013 handle insert: not_after already passed
--   PC020 transition: revoked is terminal
--   PC021 transition: an immutable column changed
--   PC022 transition: illegal status pair
--   PC023 transition: not_after extended, cleared or set in the past
--   PC024 transition: verify_only overlap exceeds 7 days
--   PC025 transition: revoke without revoked_by / valid revoke_reason
--   PC026 transition: revoked_* set on a non-revoked row
--   PC027 handles: DELETE / TRUNCATE refused
--   PC030 request: requesting principal is not eligible
--   PC031 request/approval: principal differs from the session principal
--   PC032 request: an immutable column changed
--   PC033 request: only pending -> applied is allowed
--   PC034 request: no handle row points back to this request (B4)
--   PC035 request: DELETE / TRUNCATE refused
--   PC040 approval: request not visible
--   PC041 approval: self-approval (same principal)
--   PC042 approval: approver not eligible
--   PC043 approval: requester has no Person linkage
--   PC044 approval: approver is the same Person as the requester
--   PC045 approval: request is not pending
--   PC046 approval: content hash differs from the request

-- ---------------------------------------------------------------------------
-- Pure helper functions (IMMUTABLE / STABLE, never SECURITY DEFINER).
-- ---------------------------------------------------------------------------

-- secret_ref namespace check (review §1.2, C1 defence in depth). After
-- stripping the fragment and the query string, the path must contain
-- '/provider-creds/' exactly once (an overlapping second occurrence is also
-- refused), and the tail must be exactly four '/'-separated segments:
-- tenant, domain, provider, name. It splits on segments and never builds a
-- regular expression from column values.
CREATE FUNCTION provider_credential_ref_in_namespace(p_ref TEXT, p_tenant UUID, p_domain TEXT, p_provider TEXT)
RETURNS BOOLEAN
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    WITH p AS (
        SELECT split_part(split_part(p_ref, '#', 1), '?', 1) AS path
    ), m AS (
        SELECT path, strpos(path, '/provider-creds/') AS pos FROM p
    ), t AS (
        SELECT path, pos,
               CASE WHEN pos > 0 THEN string_to_array(substr(path, pos + 16), '/') END AS segs
          FROM m
    )
    SELECT COALESCE(
        p_ref IS NOT NULL AND p_tenant IS NOT NULL AND p_domain IS NOT NULL AND p_provider IS NOT NULL
        AND pos > 0
        AND strpos(substr(path, pos + 1), '/provider-creds/') = 0
        AND array_length(segs, 1) = 4
        AND segs[1] = p_tenant::text
        AND segs[2] = p_domain
        AND segs[3] = p_provider
        AND segs[4] ~ '^[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}$',
        false)
      FROM t
$$;

-- One length-prefixed field of the content hash input: NULL is '-', any
-- value is '<char length>:<value>'. '-' is never a digit, so the encoding
-- is unambiguous.
CREATE FUNCTION provider_credential_hash_field(p TEXT) RETURNS TEXT
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT CASE WHEN p IS NULL THEN '-' ELSE length(p)::text || ':' || p END
$$;

-- Timestamps are rendered in UTC so the hash never depends on the session
-- TimeZone.
CREATE FUNCTION provider_credential_hash_ts(p TIMESTAMPTZ) RETURNS TEXT
LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT provider_credential_hash_field(to_char(p AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))
$$;

-- The content hash (review §1.2). Computed ONLY here; Go never computes it.
CREATE FUNCTION provider_credential_content_hash(
    p_tenant UUID, p_domain TEXT, p_provider_id TEXT, p_purpose TEXT, p_key_id TEXT,
    p_secret_ref TEXT, p_fingerprint TEXT, p_vendor_account_id TEXT,
    p_not_before TIMESTAMPTZ, p_not_after TIMESTAMPTZ,
    p_predecessor_handle_id UUID, p_predecessor_disposition TEXT, p_predecessor_not_after TIMESTAMPTZ
) RETURNS TEXT
LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT encode(sha256(convert_to(
        'pcr-v1|'
        || provider_credential_hash_field(p_tenant::text)
        || provider_credential_hash_field(p_domain)
        || provider_credential_hash_field(p_provider_id)
        || provider_credential_hash_field(p_purpose)
        || provider_credential_hash_field(p_key_id)
        || provider_credential_hash_field(p_secret_ref)
        || provider_credential_hash_field(p_fingerprint)
        || provider_credential_hash_field(p_vendor_account_id)
        || provider_credential_hash_ts(p_not_before)
        || provider_credential_hash_ts(p_not_after)
        || provider_credential_hash_field(p_predecessor_handle_id::text)
        || provider_credential_hash_field(p_predecessor_disposition)
        || provider_credential_hash_ts(p_predecessor_not_after),
        'UTF8')), 'hex')
$$;

-- ---------------------------------------------------------------------------
-- Tables. The two governance tables and the handle table reference each
-- other, so the cross-table foreign keys are added after all three exist.
-- ---------------------------------------------------------------------------

CREATE TABLE provider_credential_change_requests (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    target_tenant_id          UUID NOT NULL REFERENCES tenants (id),
    domain                    TEXT NOT NULL CHECK (domain IN ('payments', 'kyc', 'casino')),
    provider_id               TEXT NOT NULL CHECK (provider_id ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    purpose                   TEXT NOT NULL CHECK (purpose IN ('webhook_verify', 'outbound_api')),
    key_id                    TEXT NOT NULL CHECK (key_id ~ '^[A-Za-z0-9._-]{1,64}$'),
    secret_ref                TEXT NOT NULL,
    fingerprint               TEXT NOT NULL CHECK (fingerprint ~ '^fp1:[0-9a-f]{64}$'),
    vendor_account_id         TEXT NULL,
    not_before                TIMESTAMPTZ NOT NULL,
    not_after                 TIMESTAMPTZ NULL,
    predecessor_handle_id     UUID NULL,
    predecessor_disposition   TEXT NOT NULL CHECK (predecessor_disposition IN ('none', 'verify_only', 'revoked')),
    predecessor_not_after     TIMESTAMPTZ NULL,
    content_hash              TEXT NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    reason_code               TEXT NOT NULL CHECK (reason_code IN
                                  ('initial_registration', 'scheduled_rotation', 'compromise_replacement', 'vendor_migration')),
    requested_by_principal_id UUID NOT NULL,
    requested_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    state                     TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'applied')),
    applied_at                TIMESTAMPTZ NULL,
    applied_by_principal_id   UUID NULL,
    applied_handle_id         UUID NULL,

    CONSTRAINT pccr_secret_ref_shape CHECK (
        length(secret_ref) <= 512
        AND secret_ref !~ '[[:cntrl:][:space:]]'
        AND secret_ref ~ '^(awssm|devfile|memory)://'),
    CONSTRAINT pccr_secret_ref_namespace CHECK (
        provider_credential_ref_in_namespace(secret_ref, target_tenant_id, domain, provider_id)),
    CONSTRAINT pccr_awssm_pinned CHECK (
        secret_ref !~ '^awssm://' OR secret_ref ~ '^awssm://[^?#]+\?versionId=[A-Za-z0-9-]{32,64}(#[A-Za-z0-9._-]{1,64})?$'),
    CONSTRAINT pccr_devfile_pinned CHECK (
        secret_ref !~ '^devfile://' OR secret_ref ~ '^devfile://[^?#]+\?version=[A-Za-z0-9._-]{1,64}$'),
    CONSTRAINT pccr_vendor_account_id_shape CHECK (
        vendor_account_id IS NULL
        OR (vendor_account_id <> '' AND length(vendor_account_id) <= 128 AND vendor_account_id !~ '[[:cntrl:]]')),
    CONSTRAINT pccr_window CHECK (not_after IS NULL OR not_after > not_before),
    CONSTRAINT pccr_predecessor_none CHECK ((predecessor_disposition = 'none') = (predecessor_handle_id IS NULL)),
    CONSTRAINT pccr_predecessor_verify_only CHECK (
        (predecessor_disposition = 'verify_only') = (predecessor_not_after IS NOT NULL)),
    -- An outbound predecessor is revoked, never overlapped.
    CONSTRAINT pccr_predecessor_overlap_inbound_only CHECK (
        purpose = 'webhook_verify' OR predecessor_disposition <> 'verify_only'),
    CONSTRAINT pccr_applied_consistency CHECK (
        (state = 'applied') = (applied_at IS NOT NULL AND applied_by_principal_id IS NOT NULL AND applied_handle_id IS NOT NULL)),
    -- Target of the composite foreign key from provider_credential_handles.
    CONSTRAINT pccr_id_tenant_key UNIQUE (id, target_tenant_id)
);

CREATE INDEX idx_pccr_target_tenant ON provider_credential_change_requests (target_tenant_id, requested_at);

CREATE TABLE provider_credential_change_approvals (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id            UUID NOT NULL REFERENCES provider_credential_change_requests (id),
    approver_principal_id UUID NOT NULL,
    decision              TEXT NOT NULL CHECK (decision IN ('approve', 'reject')),
    content_hash          TEXT NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    reason_code           TEXT NULL CHECK (reason_code IS NULL OR reason_code ~ '^[a-z0-9_]{1,64}$'),
    decided_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pcca_reject_needs_reason CHECK (decision = 'approve' OR reason_code IS NOT NULL),
    CONSTRAINT pcca_one_decision_per_principal UNIQUE (request_id, approver_principal_id)
);

CREATE TABLE provider_credential_handles (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- No ON DELETE CASCADE: the history is evidence, so a tenant that has
    -- handles cannot be deleted.
    tenant_id             UUID NOT NULL REFERENCES tenants (id),
    domain                TEXT NOT NULL CHECK (domain IN ('payments', 'kyc', 'casino')),
    provider_id           TEXT NOT NULL CHECK (provider_id ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    purpose               TEXT NOT NULL CHECK (purpose IN ('webhook_verify', 'outbound_api')),
    key_id                TEXT NOT NULL CHECK (key_id ~ '^[A-Za-z0-9._-]{1,64}$'),
    secret_ref            TEXT NOT NULL,
    fingerprint           TEXT NOT NULL CHECK (fingerprint ~ '^fp1:[0-9a-f]{64}$'),
    vendor_account_id     TEXT NULL,
    status                TEXT NOT NULL CHECK (status IN ('active', 'verify_only', 'revoked')),
    status_changed_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    not_before            TIMESTAMPTZ NOT NULL,
    not_after             TIMESTAMPTZ NULL,
    -- B3: the approval that activated this key. Immutable (not in the
    -- runtime UPDATE column grant, and compared by the transition trigger).
    activation_request_id UUID NOT NULL UNIQUE,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by            UUID NOT NULL,
    revoked_at            TIMESTAMPTZ NULL,
    revoked_by            UUID NULL,
    revoke_reason         TEXT NULL CHECK (revoke_reason IS NULL OR revoke_reason IN
                              ('rotation_complete', 'suspected_compromise', 'confirmed_compromise',
                               'vendor_offboarded', 'misregistration', 'tenant_request')),

    CONSTRAINT pch_secret_ref_shape CHECK (
        length(secret_ref) <= 512
        AND secret_ref !~ '[[:cntrl:][:space:]]'
        AND secret_ref ~ '^(awssm|devfile|memory)://'),
    CONSTRAINT pch_secret_ref_namespace CHECK (
        provider_credential_ref_in_namespace(secret_ref, tenant_id, domain, provider_id)),
    CONSTRAINT pch_awssm_pinned CHECK (
        secret_ref !~ '^awssm://' OR secret_ref ~ '^awssm://[^?#]+\?versionId=[A-Za-z0-9-]{32,64}(#[A-Za-z0-9._-]{1,64})?$'),
    CONSTRAINT pch_devfile_pinned CHECK (
        secret_ref !~ '^devfile://' OR secret_ref ~ '^devfile://[^?#]+\?version=[A-Za-z0-9._-]{1,64}$'),
    CONSTRAINT pch_vendor_account_id_shape CHECK (
        vendor_account_id IS NULL
        OR (vendor_account_id <> '' AND length(vendor_account_id) <= 128 AND vendor_account_id !~ '[[:cntrl:]]')),
    CONSTRAINT pch_window CHECK (not_after IS NULL OR not_after > not_before),
    CONSTRAINT pch_verify_only_inbound_bounded CHECK (
        status <> 'verify_only' OR (purpose = 'webhook_verify' AND not_after IS NOT NULL)),
    CONSTRAINT pch_revoked_consistency CHECK (
        (status = 'revoked') = (revoked_at IS NOT NULL AND revoked_by IS NOT NULL AND revoke_reason IS NOT NULL)),
    CONSTRAINT pch_not_revoked_clean CHECK (
        status = 'revoked' OR (revoked_at IS NULL AND revoked_by IS NULL AND revoke_reason IS NULL)),
    CONSTRAINT pch_binding_key UNIQUE (tenant_id, domain, provider_id, purpose, key_id),
    -- R2/C1: across ALL tenants and ALL statuses. Constraints are not
    -- subject to RLS, so this stops one secret from being bound to two
    -- tenants and stops a revoked secret version from being registered
    -- again (the fingerprint HMAC therefore contains no tenant id).
    CONSTRAINT pch_global_fingerprint UNIQUE (domain, provider_id, purpose, fingerprint),
    -- Target of the composite foreign keys from the request table.
    CONSTRAINT pch_id_tenant_key UNIQUE (id, tenant_id)
);

CREATE UNIQUE INDEX uq_pch_one_active ON provider_credential_handles (tenant_id, domain, provider_id, purpose)
    WHERE status = 'active';
CREATE UNIQUE INDEX uq_pch_one_verify_only ON provider_credential_handles (tenant_id, domain, provider_id, purpose)
    WHERE status = 'verify_only';

-- Composite foreign keys: a handle's activation request must target the
-- handle's own tenant; a request's predecessor and applied handle must
-- belong to the request's target tenant. Referential-integrity checks
-- bypass RLS, which is intended here.
ALTER TABLE provider_credential_handles
    ADD CONSTRAINT pch_activation_request_fk
    FOREIGN KEY (activation_request_id, tenant_id)
    REFERENCES provider_credential_change_requests (id, target_tenant_id);
ALTER TABLE provider_credential_change_requests
    ADD CONSTRAINT pccr_predecessor_fk
    FOREIGN KEY (predecessor_handle_id, target_tenant_id)
    REFERENCES provider_credential_handles (id, tenant_id);
ALTER TABLE provider_credential_change_requests
    ADD CONSTRAINT pccr_applied_handle_fk
    FOREIGN KEY (applied_handle_id, target_tenant_id)
    REFERENCES provider_credential_handles (id, tenant_id);

CREATE INDEX idx_pcca_request ON provider_credential_change_approvals (request_id);
CREATE INDEX idx_pccr_predecessor ON provider_credential_change_requests (predecessor_handle_id)
    WHERE predecessor_handle_id IS NOT NULL;
CREATE INDEX idx_pccr_applied_handle ON provider_credential_change_requests (applied_handle_id)
    WHERE applied_handle_id IS NOT NULL;

COMMENT ON TABLE provider_credential_handles IS
    'ADR 0093: tenant-owned HANDLES to provider secrets held in an external store (never the secret value). FORCE RLS, tenant policies only, excluded from CDC.';
COMMENT ON TABLE provider_credential_change_requests IS
    'ADR 0093 §3: platform-scoped four-eyes activation requests; tenant bridge policy is SELECT + pending->applied UPDATE only.';
COMMENT ON TABLE provider_credential_change_approvals IS
    'ADR 0093 §3: immutable approve/reject decisions; tenant bridge policy is SELECT only.';

-- ---------------------------------------------------------------------------
-- Row-level security.
-- ---------------------------------------------------------------------------

ALTER TABLE provider_credential_handles ENABLE ROW LEVEL SECURITY;
ALTER TABLE provider_credential_handles FORCE ROW LEVEL SECURITY;
ALTER TABLE provider_credential_change_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE provider_credential_change_requests FORCE ROW LEVEL SECURITY;
ALTER TABLE provider_credential_change_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE provider_credential_change_approvals FORCE ROW LEVEL SECURITY;

-- Handles: tenant only, player scope excluded. No DELETE policy, no
-- platform policy, no dual-scope policy (ADR 0022 §2.2).
CREATE POLICY tenant_isolation_read ON provider_credential_handles
    FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL);
CREATE POLICY tenant_isolation_insert ON provider_credential_handles
    FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL);
CREATE POLICY tenant_isolation_update ON provider_credential_handles
    FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL);

-- Governance tables: the verbatim 0044/0086 platform predicate.
CREATE POLICY platform_admin_scope ON provider_credential_change_requests
    FOR ALL
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
CREATE POLICY platform_admin_scope ON provider_credential_change_approvals
    FOR ALL
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- Bridge policies (B1/B2): SELECT on both governance tables, plus the
-- pending -> applied UPDATE on requests only. Never FOR ALL, INSERT or
-- DELETE. No recursion: the request policies never reference approvals.
CREATE POLICY tenant_consume_read ON provider_credential_change_requests
    FOR SELECT
    USING (
        target_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL);

CREATE POLICY tenant_consume_apply ON provider_credential_change_requests
    FOR UPDATE
    USING (state = 'pending'
        AND target_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL)
    WITH CHECK (state = 'applied'
        AND target_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL);

CREATE POLICY tenant_consume_read ON provider_credential_change_approvals
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND EXISTS (SELECT 1 FROM provider_credential_change_requests r
                     WHERE r.id = request_id
                       AND r.target_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid));

-- ---------------------------------------------------------------------------
-- Requests: triggers.
-- ---------------------------------------------------------------------------

-- BEFORE INSERT: the verbatim hardened 0089 principal check, then every
-- server-owned field is forced (B5), then the content hash is computed.
CREATE FUNCTION provider_credential_change_requests_before_insert() RETURNS TRIGGER AS $$
DECLARE
    v_tenant_id UUID;
    v_person_id UUID;
    v_status    TEXT;
    v_session   UUID;
BEGIN
    SELECT su.tenant_id, su.person_id, su.status
      INTO v_tenant_id, v_person_id, v_status
      FROM staff_users su
     WHERE su.id = NEW.requested_by_principal_id;

    -- FOUND is checked immediately (0034/0044/0047/0089 idiom): with no
    -- row, every INTO target is NULL and would otherwise look platform-scoped.
    IF NOT FOUND THEN
        RAISE EXCEPTION 'provider_credential_change_requests: requesting principal cannot be resolved to a staff account'
            USING ERRCODE = 'PC030';
    END IF;
    IF v_tenant_id IS NOT NULL THEN
        RAISE EXCEPTION 'provider_credential_change_requests: requesting principal is not platform-scoped'
            USING ERRCODE = 'PC030';
    END IF;
    IF v_person_id IS NULL THEN
        RAISE EXCEPTION 'provider_credential_change_requests: requesting principal has no confirmed Person linkage'
            USING ERRCODE = 'PC030';
    END IF;
    IF v_status IS DISTINCT FROM 'active' THEN
        RAISE EXCEPTION 'provider_credential_change_requests: requesting staff account is not active'
            USING ERRCODE = 'PC030';
    END IF;

    -- The requester is the session's own platform principal: nobody files
    -- on another administrator's behalf.
    v_session := NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid;
    IF v_session IS DISTINCT FROM NEW.requested_by_principal_id THEN
        RAISE EXCEPTION 'provider_credential_change_requests: requesting principal differs from the session principal'
            USING ERRCODE = 'PC031';
    END IF;

    NEW.requested_at := now();
    NEW.state := 'pending';
    NEW.applied_at := NULL;
    NEW.applied_by_principal_id := NULL;
    NEW.applied_handle_id := NULL;
    NEW.content_hash := provider_credential_content_hash(
        NEW.target_tenant_id, NEW.domain, NEW.provider_id, NEW.purpose, NEW.key_id,
        NEW.secret_ref, NEW.fingerprint, NEW.vendor_account_id, NEW.not_before, NEW.not_after,
        NEW.predecessor_handle_id, NEW.predecessor_disposition, NEW.predecessor_not_after);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER provider_credential_change_requests_before_insert
    BEFORE INSERT ON provider_credential_change_requests
    FOR EACH ROW EXECUTE FUNCTION provider_credential_change_requests_before_insert();

-- BEFORE UPDATE / DELETE / TRUNCATE: every column but the applied_* fields
-- and state is immutable; only pending -> applied is allowed, and only when
-- a handle row of the same tenant points back to this request (B4). From
-- platform scope the handle table is invisible, so platform scope can never
-- mark a request applied.
CREATE FUNCTION provider_credential_change_requests_guard() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION 'provider_credential_change_requests is append-only: % is not permitted', TG_OP
            USING ERRCODE = 'PC035';
    END IF;

    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.target_tenant_id IS DISTINCT FROM OLD.target_tenant_id
        OR NEW.domain IS DISTINCT FROM OLD.domain
        OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
        OR NEW.purpose IS DISTINCT FROM OLD.purpose
        OR NEW.key_id IS DISTINCT FROM OLD.key_id
        OR NEW.secret_ref IS DISTINCT FROM OLD.secret_ref
        OR NEW.fingerprint IS DISTINCT FROM OLD.fingerprint
        OR NEW.vendor_account_id IS DISTINCT FROM OLD.vendor_account_id
        OR NEW.not_before IS DISTINCT FROM OLD.not_before
        OR NEW.not_after IS DISTINCT FROM OLD.not_after
        OR NEW.predecessor_handle_id IS DISTINCT FROM OLD.predecessor_handle_id
        OR NEW.predecessor_disposition IS DISTINCT FROM OLD.predecessor_disposition
        OR NEW.predecessor_not_after IS DISTINCT FROM OLD.predecessor_not_after
        OR NEW.content_hash IS DISTINCT FROM OLD.content_hash
        OR NEW.reason_code IS DISTINCT FROM OLD.reason_code
        OR NEW.requested_by_principal_id IS DISTINCT FROM OLD.requested_by_principal_id
        OR NEW.requested_at IS DISTINCT FROM OLD.requested_at
    THEN
        RAISE EXCEPTION 'provider_credential_change_requests: request content is immutable after insert'
            USING ERRCODE = 'PC032';
    END IF;

    IF OLD.state IS DISTINCT FROM 'pending' OR NEW.state IS DISTINCT FROM 'applied' THEN
        RAISE EXCEPTION 'provider_credential_change_requests: only pending -> applied is allowed'
            USING ERRCODE = 'PC033';
    END IF;

    NEW.applied_at := now();

    IF NEW.applied_handle_id IS NULL OR NEW.applied_by_principal_id IS NULL
        OR NOT EXISTS (SELECT 1 FROM provider_credential_handles h
                        WHERE h.id = NEW.applied_handle_id
                          AND h.activation_request_id = NEW.id
                          AND h.tenant_id = NEW.target_tenant_id)
    THEN
        RAISE EXCEPTION 'provider_credential_change_requests: no handle row points back to this request'
            USING ERRCODE = 'PC034';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER provider_credential_change_requests_guard_update
    BEFORE UPDATE ON provider_credential_change_requests
    FOR EACH ROW EXECUTE FUNCTION provider_credential_change_requests_guard();
CREATE TRIGGER provider_credential_change_requests_guard_delete
    BEFORE DELETE ON provider_credential_change_requests
    FOR EACH ROW EXECUTE FUNCTION provider_credential_change_requests_guard();
CREATE TRIGGER provider_credential_change_requests_no_truncate
    BEFORE TRUNCATE ON provider_credential_change_requests
    FOR EACH STATEMENT EXECUTE FUNCTION provider_credential_change_requests_guard();

-- ---------------------------------------------------------------------------
-- Approvals: triggers.
-- ---------------------------------------------------------------------------

-- BEFORE INSERT: the verbatim hardened 0089 self-approval body, plus the
-- request must be pending, the echoed content hash must equal the
-- request's, and decided_at is forced (B5).
CREATE FUNCTION provider_credential_change_approvals_before_insert() RETURNS TRIGGER AS $$
DECLARE
    v_requester_principal_id UUID;
    v_request_state          TEXT;
    v_request_hash           TEXT;
    v_requester_person_id    UUID;
    v_approver_person_id     UUID;
    v_approver_tenant_id     UUID;
    v_approver_status        TEXT;
    v_session                UUID;
BEGIN
    SELECT r.requested_by_principal_id, r.state, r.content_hash
      INTO v_requester_principal_id, v_request_state, v_request_hash
      FROM provider_credential_change_requests r
     WHERE r.id = NEW.request_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'provider_credential_change_approvals: request is not visible in this scope'
            USING ERRCODE = 'PC040';
    END IF;

    IF NEW.approver_principal_id = v_requester_principal_id THEN
        RAISE EXCEPTION 'provider_credential_change_approvals: the requesting principal may not decide its own request'
            USING ERRCODE = 'PC041';
    END IF;

    SELECT su.tenant_id, su.person_id, su.status
      INTO v_approver_tenant_id, v_approver_person_id, v_approver_status
      FROM staff_users su
     WHERE su.id = NEW.approver_principal_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'provider_credential_change_approvals: approver cannot be resolved to a staff account'
            USING ERRCODE = 'PC042';
    END IF;
    IF v_approver_tenant_id IS NOT NULL THEN
        RAISE EXCEPTION 'provider_credential_change_approvals: approver is not platform-scoped'
            USING ERRCODE = 'PC042';
    END IF;
    IF v_approver_person_id IS NULL THEN
        RAISE EXCEPTION 'provider_credential_change_approvals: approver has no confirmed Person linkage'
            USING ERRCODE = 'PC042';
    END IF;
    IF v_approver_status IS DISTINCT FROM 'active' THEN
        RAISE EXCEPTION 'provider_credential_change_approvals: approver staff account is not active'
            USING ERRCODE = 'PC042';
    END IF;

    v_session := NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid;
    IF v_session IS DISTINCT FROM NEW.approver_principal_id THEN
        RAISE EXCEPTION 'provider_credential_change_approvals: approver differs from the session principal'
            USING ERRCODE = 'PC031';
    END IF;

    SELECT su.person_id INTO v_requester_person_id
      FROM staff_users su
     WHERE su.id = v_requester_principal_id;
    IF v_requester_person_id IS NULL THEN
        RAISE EXCEPTION 'provider_credential_change_approvals: the requesting principal has no confirmed Person linkage'
            USING ERRCODE = 'PC043';
    END IF;
    IF v_requester_person_id = v_approver_person_id THEN
        RAISE EXCEPTION 'provider_credential_change_approvals: approver is the same Person as the requester'
            USING ERRCODE = 'PC044';
    END IF;

    IF v_request_state IS DISTINCT FROM 'pending' THEN
        RAISE EXCEPTION 'provider_credential_change_approvals: request is not pending'
            USING ERRCODE = 'PC045';
    END IF;
    IF NEW.content_hash IS DISTINCT FROM v_request_hash THEN
        RAISE EXCEPTION 'provider_credential_change_approvals: content hash does not match the request'
            USING ERRCODE = 'PC046';
    END IF;

    NEW.decided_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER provider_credential_change_approvals_before_insert
    BEFORE INSERT ON provider_credential_change_approvals
    FOR EACH ROW EXECUTE FUNCTION provider_credential_change_approvals_before_insert();
CREATE TRIGGER provider_credential_change_approvals_immutable
    BEFORE UPDATE OR DELETE ON provider_credential_change_approvals
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER provider_credential_change_approvals_no_truncate
    BEFORE TRUNCATE ON provider_credential_change_approvals
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- ---------------------------------------------------------------------------
-- Handles: insert, consume and transition triggers.
-- ---------------------------------------------------------------------------

-- BEFORE INSERT: a handle row is only ever inserted as 'active'.
CREATE FUNCTION provider_credential_handles_before_insert() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM 'active' THEN
        RAISE EXCEPTION 'provider_credential_handles: a handle is only ever inserted as active'
            USING ERRCODE = 'PC010';
    END IF;
    IF NEW.activation_request_id IS NULL OR NEW.created_by IS NULL THEN
        RAISE EXCEPTION 'provider_credential_handles: activation_request_id and created_by are required'
            USING ERRCODE = 'PC011';
    END IF;
    IF NEW.revoked_at IS NOT NULL OR NEW.revoked_by IS NOT NULL OR NEW.revoke_reason IS NOT NULL THEN
        RAISE EXCEPTION 'provider_credential_handles: revoked_* must be empty on insert'
            USING ERRCODE = 'PC012';
    END IF;
    NEW.created_at := now();
    NEW.status_changed_at := now();
    IF NEW.not_after IS NOT NULL AND NEW.not_after <= now() THEN
        RAISE EXCEPTION 'provider_credential_handles: not_after has already passed'
            USING ERRCODE = 'PC013';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER provider_credential_handles_before_insert
    BEFORE INSERT ON provider_credential_handles
    FOR EACH ROW EXECUTE FUNCTION provider_credential_handles_before_insert();

-- The consume step (review §1.4). Runs in the tenant-scoped insert and
-- reads the governance tables only through the bridge policies. It never
-- reads staff_users (B6) and selects the request by the explicit id on the
-- handle row, never by a content search (B3). Order of checks is binding.
CREATE FUNCTION provider_credential_consume() RETURNS TRIGGER AS $$
DECLARE
    r provider_credential_change_requests%ROWTYPE;
    p provider_credential_handles%ROWTYPE;
BEGIN
    -- 1. The request, by explicit id, locked. Another tenant's request is
    --    invisible, so it is NOT FOUND too.
    SELECT * INTO r
      FROM provider_credential_change_requests
     WHERE id = NEW.activation_request_id
     FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'provider_credential_handles: activation request not found in this scope'
            USING ERRCODE = 'PC001';
    END IF;

    -- 2. Pending, and for this tenant.
    IF r.state IS DISTINCT FROM 'pending' THEN
        RAISE EXCEPTION 'provider_credential_handles: activation request is not pending'
            USING ERRCODE = 'PC002';
    END IF;
    IF r.target_tenant_id IS DISTINCT FROM NEW.tenant_id THEN
        RAISE EXCEPTION 'provider_credential_handles: activation request targets another tenant'
            USING ERRCODE = 'PC003';
    END IF;

    -- 3. Every content column equals the approved request.
    IF NEW.domain IS DISTINCT FROM r.domain
        OR NEW.provider_id IS DISTINCT FROM r.provider_id
        OR NEW.purpose IS DISTINCT FROM r.purpose
        OR NEW.key_id IS DISTINCT FROM r.key_id
        OR NEW.secret_ref IS DISTINCT FROM r.secret_ref
        OR NEW.fingerprint IS DISTINCT FROM r.fingerprint
        OR NEW.vendor_account_id IS DISTINCT FROM r.vendor_account_id
        OR NEW.not_before IS DISTINCT FROM r.not_before
        OR NEW.not_after IS DISTINCT FROM r.not_after
    THEN
        RAISE EXCEPTION 'provider_credential_handles: handle content differs from the approved request'
            USING ERRCODE = 'PC004';
    END IF;

    -- 4. The recomputed hash equals the stored hash.
    IF provider_credential_content_hash(
            NEW.tenant_id, NEW.domain, NEW.provider_id, NEW.purpose, NEW.key_id,
            NEW.secret_ref, NEW.fingerprint, NEW.vendor_account_id, NEW.not_before, NEW.not_after,
            r.predecessor_handle_id, r.predecessor_disposition, r.predecessor_not_after)
        IS DISTINCT FROM r.content_hash
    THEN
        RAISE EXCEPTION 'provider_credential_handles: content hash does not match the approved request'
            USING ERRCODE = 'PC005';
    END IF;

    -- 5. An unexpired approval by a principal other than the requester,
    --    for exactly this content. 24 hours is a platform constant
    --    (changing it needs an ADR 0093 amendment).
    IF NOT EXISTS (
        SELECT 1 FROM provider_credential_change_approvals a
         WHERE a.request_id = r.id
           AND a.decision = 'approve'
           AND a.approver_principal_id <> r.requested_by_principal_id
           AND a.content_hash = r.content_hash
           AND a.decided_at <= now()
           AND a.decided_at > now() - interval '24 hours')
    THEN
        RAISE EXCEPTION 'provider_credential_handles: no unexpired approval by a different principal'
            USING ERRCODE = 'PC006';
    END IF;

    -- 6. Any rejection blocks the request.
    IF EXISTS (
        SELECT 1 FROM provider_credential_change_approvals a
         WHERE a.request_id = r.id AND a.decision = 'reject')
    THEN
        RAISE EXCEPTION 'provider_credential_handles: the activation request was rejected'
            USING ERRCODE = 'PC007';
    END IF;

    -- 7. The predecessor disposition.
    IF r.predecessor_disposition = 'none' THEN
        IF EXISTS (
            SELECT 1 FROM provider_credential_handles h
             WHERE h.tenant_id = NEW.tenant_id AND h.domain = NEW.domain
               AND h.provider_id = NEW.provider_id AND h.purpose = NEW.purpose
               AND h.status = 'active' AND h.id <> NEW.id)
        THEN
            RAISE EXCEPTION 'provider_credential_handles: another active handle exists (predecessor disposition none)'
                USING ERRCODE = 'PC008';
        END IF;
    ELSE
        SELECT * INTO p FROM provider_credential_handles WHERE id = r.predecessor_handle_id;
        IF NOT FOUND
            OR p.id = NEW.id
            OR p.tenant_id IS DISTINCT FROM NEW.tenant_id
            OR p.domain IS DISTINCT FROM NEW.domain
            OR p.provider_id IS DISTINCT FROM NEW.provider_id
            OR p.purpose IS DISTINCT FROM NEW.purpose
        THEN
            RAISE EXCEPTION 'provider_credential_handles: predecessor does not belong to this binding'
                USING ERRCODE = 'PC008';
        END IF;
        IF r.predecessor_disposition = 'verify_only' THEN
            IF p.status IS DISTINCT FROM 'verify_only'
                OR p.not_after IS DISTINCT FROM r.predecessor_not_after
                OR p.status_changed_at IS DISTINCT FROM now()
            THEN
                RAISE EXCEPTION 'provider_credential_handles: predecessor was not demoted to verify_only as approved in this transaction'
                    USING ERRCODE = 'PC008';
            END IF;
        ELSIF r.predecessor_disposition = 'revoked' THEN
            IF p.status IS DISTINCT FROM 'revoked' THEN
                RAISE EXCEPTION 'provider_credential_handles: predecessor is not revoked'
                    USING ERRCODE = 'PC008';
            END IF;
        ELSE
            RAISE EXCEPTION 'provider_credential_handles: unknown predecessor disposition'
                USING ERRCODE = 'PC008';
        END IF;
    END IF;

    -- 8. Consume: mark the request applied (the request's own trigger
    --    re-checks B4 and forces applied_at).
    UPDATE provider_credential_change_requests
       SET state = 'applied',
           applied_handle_id = NEW.id,
           applied_by_principal_id = NEW.created_by
     WHERE id = r.id;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER provider_credential_handles_consume
    AFTER INSERT ON provider_credential_handles
    FOR EACH ROW EXECUTE FUNCTION provider_credential_consume();

-- Handle transitions (review §1.5, single actor). Never back to 'active';
-- not_after may only shrink; the verify_only overlap is capped at 7 days
-- (a platform constant; changing it needs an ADR 0093 amendment).
CREATE FUNCTION provider_credential_handles_transition() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION 'provider_credential_handles: % is refused (the history is evidence)', TG_OP
            USING ERRCODE = 'PC027';
    END IF;

    IF OLD.status = 'revoked' THEN
        RAISE EXCEPTION 'provider_credential_handles: a revoked handle is terminal'
            USING ERRCODE = 'PC020';
    END IF;

    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.domain IS DISTINCT FROM OLD.domain
        OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
        OR NEW.purpose IS DISTINCT FROM OLD.purpose
        OR NEW.key_id IS DISTINCT FROM OLD.key_id
        OR NEW.secret_ref IS DISTINCT FROM OLD.secret_ref
        OR NEW.fingerprint IS DISTINCT FROM OLD.fingerprint
        OR NEW.vendor_account_id IS DISTINCT FROM OLD.vendor_account_id
        OR NEW.not_before IS DISTINCT FROM OLD.not_before
        OR NEW.activation_request_id IS DISTINCT FROM OLD.activation_request_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
        OR NEW.created_by IS DISTINCT FROM OLD.created_by
    THEN
        RAISE EXCEPTION 'provider_credential_handles: only status, not_after and revocation fields may change'
            USING ERRCODE = 'PC021';
    END IF;

    IF NOT (
        (OLD.status = 'active' AND NEW.status = 'active')
        OR (OLD.status = 'verify_only' AND NEW.status = 'verify_only')
        OR (OLD.status = 'active' AND NEW.status = 'verify_only')
        OR (OLD.status = 'active' AND NEW.status = 'revoked')
        OR (OLD.status = 'verify_only' AND NEW.status = 'revoked'))
    THEN
        RAISE EXCEPTION 'provider_credential_handles: illegal status transition'
            USING ERRCODE = 'PC022';
    END IF;
    IF OLD.status = 'active' AND NEW.status = 'verify_only'
        AND (NEW.purpose IS DISTINCT FROM 'webhook_verify' OR NEW.not_after IS NULL)
    THEN
        RAISE EXCEPTION 'provider_credential_handles: verify_only needs purpose webhook_verify and a not_after'
            USING ERRCODE = 'PC022';
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status THEN
        NEW.status_changed_at := now();
    ELSE
        NEW.status_changed_at := OLD.status_changed_at;
    END IF;

    IF NEW.not_after IS DISTINCT FROM OLD.not_after THEN
        IF NEW.not_after IS NULL
            OR (OLD.not_after IS NOT NULL AND NEW.not_after > OLD.not_after)
            OR NEW.not_after < now()
        THEN
            RAISE EXCEPTION 'provider_credential_handles: not_after may only shrink, to a time not in the past'
                USING ERRCODE = 'PC023';
        END IF;
    END IF;

    IF NEW.status = 'verify_only' AND NEW.not_after > NEW.status_changed_at + interval '7 days' THEN
        RAISE EXCEPTION 'provider_credential_handles: the verify_only overlap may not exceed 7 days'
            USING ERRCODE = 'PC024';
    END IF;

    IF NEW.status = 'revoked' THEN
        NEW.revoked_at := now();
        IF NEW.revoked_by IS NULL OR NEW.revoke_reason IS NULL OR NEW.revoke_reason NOT IN
            ('rotation_complete', 'suspected_compromise', 'confirmed_compromise',
             'vendor_offboarded', 'misregistration', 'tenant_request')
        THEN
            RAISE EXCEPTION 'provider_credential_handles: revoke needs revoked_by and a valid revoke_reason'
                USING ERRCODE = 'PC025';
        END IF;
    ELSIF NEW.revoked_at IS NOT NULL OR NEW.revoked_by IS NOT NULL OR NEW.revoke_reason IS NOT NULL THEN
        RAISE EXCEPTION 'provider_credential_handles: revoked_* must stay empty unless revoking'
            USING ERRCODE = 'PC026';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER provider_credential_handles_transition
    BEFORE UPDATE ON provider_credential_handles
    FOR EACH ROW EXECUTE FUNCTION provider_credential_handles_transition();
CREATE TRIGGER provider_credential_handles_deny_delete
    BEFORE DELETE ON provider_credential_handles
    FOR EACH ROW EXECUTE FUNCTION provider_credential_handles_transition();
CREATE TRIGGER provider_credential_handles_no_truncate
    BEFORE TRUNCATE ON provider_credential_handles
    FOR EACH STATEMENT EXECUTE FUNCTION provider_credential_handles_transition();

-- ---------------------------------------------------------------------------
-- Runtime role grants (ADR 0093 §1; review §1.2). Default privileges hand
-- every new table full DML to igaming_runtime; narrow them here. Guarded
-- because the role may be provisioned after migrations run
-- (docs/security/runtime-role-separation.md §6); deploy/init-app-role.sql
-- carries the matching guarded block so its backfill GRANT cannot restore
-- the removed privileges.
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON provider_credential_handles FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON provider_credential_handles TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (status, status_changed_at, not_after, revoked_at, revoked_by, revoke_reason) '
             || 'ON provider_credential_handles TO igaming_runtime';

        EXECUTE 'REVOKE ALL ON provider_credential_change_requests FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON provider_credential_change_requests TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (state, applied_at, applied_by_principal_id, applied_handle_id) '
             || 'ON provider_credential_change_requests TO igaming_runtime';

        EXECUTE 'REVOKE ALL ON provider_credential_change_approvals FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON provider_credential_change_approvals TO igaming_runtime';
    END IF;
END $$;
