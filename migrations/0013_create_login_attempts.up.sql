-- Backing store for login rate-limiting/lockout (Stage 2 instructions
-- §10). Deliberately in Postgres, not Redis or in-memory: Stage 1's tech
-- baseline earmarks Redis for this, but introducing a new infrastructure
-- dependency for a single-instance foundation stage with no concrete
-- scale requirement yet would violate the explicit "do not introduce
-- infrastructure merely because it's on a list" instruction. Revisit if
-- login volume ever makes a Postgres query the bottleneck.

CREATE TABLE login_attempts (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID, -- NULL for platform-wide staff login attempts
    principal_type TEXT NOT NULL CHECK (principal_type IN ('player', 'staff')),
    identifier     TEXT NOT NULL, -- the email that was attempted, lowercased
    ip_address     INET,
    succeeded      BOOLEAN NOT NULL,
    attempted_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Supports "count recent failures for this identifier" efficiently,
-- which is the only query this table serves.
CREATE INDEX idx_login_attempts_identifier_time ON login_attempts (identifier, attempted_at DESC);

ALTER TABLE login_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE login_attempts FORCE ROW LEVEL SECURITY;

CREATE POLICY dual_scope_isolation ON login_attempts
    FOR ALL
    USING (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    )
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    );
