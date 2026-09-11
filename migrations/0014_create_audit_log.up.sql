-- Immutable audit trail. See docs/decisions/0013-audit-log-immutability-
-- and-dual-scope-rls.md for why the trigger, not just RLS/REVOKE, is
-- what actually guarantees append-only here.

CREATE TABLE audit_log (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID, -- NULL for platform-level events (service/platform-admin actions)
    actor_type  TEXT NOT NULL CHECK (actor_type IN ('player', 'staff', 'service', 'system')),
    actor_id    UUID, -- NULL only for actor_type = 'system'
    action      TEXT NOT NULL, -- e.g. "player.login_succeeded", "brand.updated"
    target_type TEXT,
    target_id   TEXT,
    outcome     TEXT NOT NULL CHECK (outcome IN ('success', 'failure', 'denied')),
    ip_address  INET,
    user_agent  TEXT,
    request_id  TEXT,
    -- Structured extra detail only - never secrets or authentication
    -- material (passwords, tokens, hashes). Enforced by code review and
    -- the callers in internal/audit, not by a database constraint, since
    -- "is this field secret-shaped" isn't mechanically checkable.
    metadata    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((actor_type = 'system') = (actor_id IS NULL))
);

CREATE INDEX idx_audit_log_tenant_time ON audit_log (tenant_id, created_at DESC);
CREATE INDEX idx_audit_log_actor ON audit_log (actor_type, actor_id);

ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE ROW LEVEL SECURITY;

CREATE POLICY dual_scope_isolation ON audit_log
    FOR ALL
    USING (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    )
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    );

CREATE FUNCTION audit_log_deny_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

-- Fires regardless of which role runs the query, including the table
-- owner - unlike REVOKE, a trigger is not bypassed by ownership.
CREATE TRIGGER audit_log_immutable
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_deny_mutation();
