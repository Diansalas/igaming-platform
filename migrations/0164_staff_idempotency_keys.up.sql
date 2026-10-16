-- A10c (ADR 0114 G10): server-side Idempotency-Key for the K1 / K3 / HSEC staff mutating routes.
--
-- One small table, written only by the httpserver staffIdempotent wrapper (staff_idempotency.go). A key is
-- scoped to (tenant, acting staff, key); the row pins the request (a SHA-256 over method, path and body) so the
-- same key with a different request is refused, and stores the committed 2xx response so a retry replays it.
-- A route that did not return 2xx deletes its reservation (the client may retry the same key). The table
-- holds no authority: every state transition of the governed flows is still decided by the flows' own
-- database guards (distinct Person, proof, recount, state machine); this only de-duplicates a client retry.
--
-- Tenant-owned: tenant_id with FORCE row level security bound to app.tenant_id, staff sessions only.
CREATE TABLE staff_idempotency_keys (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants (id),
    actor_id        UUID NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 1 AND 120 AND idempotency_key ~ '^[\x20-\x7E]+$'),
    route           TEXT NOT NULL CHECK (octet_length(route) BETWEEN 1 AND 255),
    request_hash    BYTEA NOT NULL CHECK (octet_length(request_hash) = 32),
    state           TEXT NOT NULL CHECK (state IN ('in_progress', 'completed')),
    response_status INTEGER NULL CHECK (response_status BETWEEN 200 AND 299),
    response_body   BYTEA NULL CHECK (response_body IS NULL OR octet_length(response_body) <= 262144),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ NULL,
    UNIQUE (tenant_id, actor_id, idempotency_key),
    CHECK ((state = 'completed') = (response_status IS NOT NULL AND completed_at IS NOT NULL))
);

CREATE INDEX staff_idempotency_keys_created ON staff_idempotency_keys (created_at);

ALTER TABLE staff_idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_idempotency_keys FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_staff_scope ON staff_idempotency_keys
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

REVOKE ALL ON staff_idempotency_keys FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON staff_idempotency_keys FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON staff_idempotency_keys TO igaming_runtime';
    END IF;
END
$$;
