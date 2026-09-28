-- CAS-PLAY-BOOTSTRAP-1 (PRH-2 workstream B; ADR 0103, ACCEPTED). Adds the
-- vendor launch-token bootstrap/consume endpoint's two backing tables.
-- Placeholder number 0115 (orchestrator instruction): 0109-0114 are
-- allocated to other in-flight lanes not yet merged onto this branch; the
-- orchestrator renumbers at merge if needed (Rule 3). No change to
-- casino_launch_sessions or its trigger (workstream A owns both) except
-- one ADDITIVE supporting UNIQUE constraint ADR 0103 §5 explicitly calls
-- for, below.
--
-- Contents:
--   1. casino_launch_sessions gets a supporting UNIQUE (id, tenant_id) -
--      it did not have one, and casino_launch_bootstraps' own composite FK
--      needs it. Never touches the immutability trigger workstream A owns.
--   2. casino_provider_player_refs: the opaque, provider-scoped player
--      reference (ADR 0103 §3.5) - random, stable per (tenant, provider,
--      player), unlinkable across tenants or providers.
--   3. casino_launch_bootstraps: the idempotency record for one vendor
--      bootstrap call, keyed on (tenant_id, provider_id, request_id) and
--      bound to token_hash + request_digest (ADR 0103 §3.4/§5).
--
-- Both new tables share ADR 0103 §5's "common properties":
--   - FORCE ROW LEVEL SECURITY, tenant family, with app.player_account_id,
--     app.platform_admin_principal_id, app.platform_service_id,
--     app.acting_tenant_id and app.acting_platform_principal_id ALL unset
--     (the 0106 pattern extended - SB-7/C-103-5; the last two GUCs are not
--     set by any code yet - K-lane, ADRs 0099/0104 - this is defence in
--     depth against that future session family, exactly as instructed);
--   - append-only: INSERT and SELECT only, UPDATE/DELETE denied per row,
--     TRUNCATE denied per statement (ledger_deny_mutation(), migration
--     0021's shared function - the SAME binding control casino_callback_
--     rejections and payment_statement_imports already use).

-- 1. Supporting UNIQUE for casino_launch_bootstraps' composite FK below.
-- Additive only - the immutability trigger (migration 0036/0042/0108) is
-- untouched.
ALTER TABLE casino_launch_sessions
    ADD CONSTRAINT casino_launch_sessions_id_tenant_id_key UNIQUE (id, tenant_id);

-- 2. Opaque, provider-scoped player reference (ADR 0103 §3.5). Created on
-- first bootstrap (casino.BootstrapLaunch's own upsert-then-read, never an
-- UPDATE - see the append-only note above); stable thereafter. No
-- surrogate id: the natural key IS the row's own identity.
CREATE TABLE casino_provider_player_refs (
    tenant_id         UUID NOT NULL REFERENCES tenants (id),
    provider_id       TEXT NOT NULL CHECK (provider_id <> ''),
    player_account_id UUID NOT NULL,
    player_ref        UUID NOT NULL DEFAULT gen_random_uuid(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, provider_id, player_account_id),
    UNIQUE (tenant_id, provider_id, player_ref),
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id)
);

CREATE INDEX idx_casino_provider_player_refs_tenant ON casino_provider_player_refs (tenant_id);

ALTER TABLE casino_provider_player_refs ENABLE ROW LEVEL SECURITY;
ALTER TABLE casino_provider_player_refs FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_staff_select ON casino_provider_player_refs
    FOR SELECT
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

CREATE POLICY tenant_staff_insert ON casino_provider_player_refs
    FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

CREATE TRIGGER casino_provider_player_refs_immutable
    BEFORE UPDATE OR DELETE ON casino_provider_player_refs
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER casino_provider_player_refs_no_truncate
    BEFORE TRUNCATE ON casino_provider_player_refs
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- 3. The bootstrap idempotency record (ADR 0103 §3.2 step 6, §3.4, §5).
CREATE TABLE casino_launch_bootstraps (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants (id),
    provider_id       TEXT NOT NULL CHECK (provider_id <> ''),
    -- ADR 0103 §3.1's own request_id charset.
    request_id        TEXT NOT NULL CHECK (request_id ~ '^[A-Za-z0-9_.:-]{1,128}$'),
    -- The launch token's own hash (sha256 hex, matches internal/casino's
    -- hashLaunchToken) - never the raw token.
    token_hash        TEXT NOT NULL,
    -- SHA-256(canonical(provider_id, request_id, provider_game_id,
    -- asset_code, mode)) over the VERIFIED, parsed fields (ADR 0103 §3.4,
    -- SB-6) - carries no token material, so it is bound to the request's
    -- own shape separately from token_hash.
    request_digest    TEXT NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    launch_session_id UUID NOT NULL,
    player_ref        UUID NOT NULL,
    -- The exact 200 response body this bootstrap returned, replayed
    -- byte-identical on a matching retry (ADR 0103 §3.4). Bounded to 1 KiB
    -- and to the five keys §3.2 step 7's response can ever carry - never
    -- the raw token or its hash (BS-6).
    response          JSONB NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT casino_launch_bootstraps_once_per_request
        UNIQUE (tenant_id, provider_id, request_id),
    CONSTRAINT casino_launch_bootstraps_once_per_session
        UNIQUE (launch_session_id),
    CONSTRAINT casino_launch_bootstraps_response_bound
        CHECK (octet_length(response::text) <= 1024),
    -- No subquery in a CHECK constraint is possible (Postgres refuses it).
    -- Subtracting every allowed key must leave an empty object - the
    -- subset test without jsonb_object_keys() or EXISTS.
    CONSTRAINT casino_launch_bootstraps_response_keys_bound
        CHECK ((response - 'session_id' - 'player_ref' - 'provider_game_id' - 'asset_code' - 'mode') = '{}'::jsonb),
    FOREIGN KEY (launch_session_id, tenant_id) REFERENCES casino_launch_sessions (id, tenant_id),
    FOREIGN KEY (tenant_id, provider_id, player_ref) REFERENCES casino_provider_player_refs (tenant_id, provider_id, player_ref)
);

CREATE INDEX idx_casino_launch_bootstraps_tenant ON casino_launch_bootstraps (tenant_id);

ALTER TABLE casino_launch_bootstraps ENABLE ROW LEVEL SECURITY;
ALTER TABLE casino_launch_bootstraps FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_staff_select ON casino_launch_bootstraps
    FOR SELECT
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

CREATE POLICY tenant_staff_insert ON casino_launch_bootstraps
    FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

CREATE TRIGGER casino_launch_bootstraps_immutable
    BEFORE UPDATE OR DELETE ON casino_launch_bootstraps
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER casino_launch_bootstraps_no_truncate
    BEFORE TRUNCATE ON casino_launch_bootstraps
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- BEFORE INSERT trigger (ADR 0103 §5): the referenced session must be the
-- SAME tenant, the SAME provider, the SAME token_hash, and genuinely
-- 'consumed' - i.e. this row can only ever be inserted immediately after
-- (and consistently with) the CAS in casino.BootstrapLaunch's own
-- transaction, never as a detached/forged idempotency record.
CREATE FUNCTION casino_launch_bootstraps_insert_guard() RETURNS TRIGGER AS $$
DECLARE
    v_tenant_id   UUID;
    v_provider_id TEXT;
    v_token_hash  TEXT;
    v_status      TEXT;
BEGIN
    SELECT tenant_id, provider_id, token_hash, status
      INTO v_tenant_id, v_provider_id, v_token_hash, v_status
      FROM casino_launch_sessions
     WHERE id = NEW.launch_session_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'casino_launch_bootstraps: launch_session_id % does not exist', NEW.launch_session_id;
    END IF;
    IF v_tenant_id IS DISTINCT FROM NEW.tenant_id
        OR v_provider_id IS DISTINCT FROM NEW.provider_id
        OR v_token_hash IS DISTINCT FROM NEW.token_hash
        OR v_status <> 'consumed'
    THEN
        RAISE EXCEPTION 'casino_launch_bootstraps: launch_session_id % does not match this row''s tenant/provider/token_hash, or the session is not consumed (status=%)', NEW.launch_session_id, v_status;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER casino_launch_bootstraps_insert_guard
    BEFORE INSERT ON casino_launch_bootstraps
    FOR EACH ROW EXECUTE FUNCTION casino_launch_bootstraps_insert_guard();

-- Runtime role: defence in depth only, mirroring migration 0097's
-- identical guarded block (the deny triggers above are the binding
-- control). Guarded because the role may be provisioned after migrations
-- run.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE UPDATE, DELETE, TRUNCATE ON casino_provider_player_refs FROM igaming_runtime';
        EXECUTE 'REVOKE UPDATE, DELETE, TRUNCATE ON casino_launch_bootstraps FROM igaming_runtime';
    END IF;
END $$;
