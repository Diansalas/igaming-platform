-- Refresh-token/session store, backing rotation, revocation, and
-- device/session tracking (Stage 2 instructions §3). Access tokens
-- (JWTs) are never stored - they're short-lived and self-verifying; only
-- the long-lived refresh token's hash is persisted, so a stolen database
-- row can't be replayed as a credential (same principle as password
-- hashing).

CREATE TABLE sessions (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    principal_type        TEXT NOT NULL CHECK (principal_type IN ('player', 'staff')),
    principal_id          UUID NOT NULL,
    tenant_id             UUID, -- mirrors the principal's tenant; NULL for platform-wide staff
    refresh_token_hash    TEXT NOT NULL UNIQUE,
    user_agent            TEXT,
    ip_address            INET,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at            TIMESTAMPTZ NOT NULL,
    revoked_at            TIMESTAMPTZ,
    -- Set when this session's refresh token is rotated forward, forming
    -- a chain. A presented token whose session already has
    -- replaced_by_session_id set is a reuse of an already-rotated token -
    -- a strong signal of theft - and revokes the whole chain (enforced in
    -- internal/auth/session.go, not by the schema).
    replaced_by_session_id UUID REFERENCES sessions(id),
    last_used_at          TIMESTAMPTZ
);

CREATE INDEX idx_sessions_principal ON sessions (principal_type, principal_id);
CREATE INDEX idx_sessions_tenant ON sessions (tenant_id);

ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions FORCE ROW LEVEL SECURITY;

-- SELECT is deliberately NOT tenant-scoped here, unlike every other
-- dual-scope table in this schema (staff_users, login_attempts,
-- audit_log). Rotating or validating a refresh token happens BEFORE the
-- caller has any tenant context - the token itself carries no tenant
-- hint (it is a random opaque value, not a slug like a brand or tenant
-- lookup uses), so there is no way to resolve a tenant to scope the
-- query by before reading the row that would tell us the tenant. This
-- table's real access control is the refresh_token_hash match itself:
-- finding a specific row requires already possessing the exact,
-- cryptographically random, unguessable token (see
-- internal/auth/session.go's hashRefreshToken). A bare, unfiltered read
-- of this table (`SELECT * FROM sessions`) only exposes metadata - which
-- principal/tenant has sessions, hashed (not usable) tokens, IPs, user
-- agents - never a credential that could itself be replayed.
CREATE POLICY session_read_by_token ON sessions
    FOR SELECT
    USING (true);

-- Mutations (creating, rotating, revoking a session) DO happen from an
-- already-resolved scope - the caller always knows, by that point,
-- whether it's issuing a tenant-scoped or platform-scoped session -
-- so these remain the standard dual-scope pattern.
CREATE POLICY session_scoped_insert ON sessions
    FOR INSERT
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    );

CREATE POLICY session_scoped_update ON sessions
    FOR UPDATE
    USING (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    )
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    );
