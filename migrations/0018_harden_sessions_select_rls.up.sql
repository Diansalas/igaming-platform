-- Security hardening pass (pre-Stage-3 gate), closing the sessions
-- SELECT exposure flagged as the most significant remaining Stage 2
-- debt: migration 0012's `session_read_by_token` policy was
-- `FOR SELECT USING (true)` - necessary for the one case where a
-- refresh token carries no tenant hint (RotateSession's/logout's initial
-- lookup), but it meant ANY tenant-scoped connection could
-- `SELECT * FROM sessions` and see every OTHER tenant's, and every other
-- principal's, session metadata (principal_id, tenant_id, ip_address,
-- user_agent) - isolation depended entirely on application code
-- filtering by principal_id (internal/auth.ListActiveSessions/
-- RevokeSession), not on the database. See
-- docs/decisions/0016-sessions-rls-hardening.md for the full design
-- rationale and the alternatives considered (a new BYPASSRLS role, a
-- SECURITY DEFINER function) and why this narrower approach was chosen
-- instead.

DROP POLICY session_read_by_token ON sessions;

-- Phase 1 of refresh/logout: the caller presents a raw refresh token
-- with no tenant or principal context yet - token possession is the
-- only credential available at this point, exactly as migration 0012's
-- original comment argued. app.session_lookup_hash is set, per
-- transaction, ONLY by internal/db.Pool.WithSessionLookup, to the
-- SHA-256 hash of the actual token presented - this policy now grants
-- visibility to AT MOST the one row matching that exact hash, not every
-- row in the table.
CREATE POLICY session_select_by_token_hash ON sessions
    FOR SELECT
    USING (refresh_token_hash = NULLIF(current_setting('app.session_lookup_hash', true), ''));

-- Phase 2 / ordinary access (an authenticated principal listing their
-- own sessions): requires BOTH the caller's tenant scope (or platform
-- scope, matching every other dual-scope table's pattern) AND their own
-- principal_id, set together by internal/db.Pool.WithPrincipalScope. No
-- principal - even one correctly scoped to the right tenant - can read
-- another principal's rows through this table anymore.
CREATE POLICY session_select_own_principal ON sessions
    FOR SELECT
    USING (
        (
            (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
            OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
        )
        AND principal_id = NULLIF(current_setting('app.principal_id', true), '')::uuid
    );

-- Internal system operations that walk/touch a specific, already-
-- resolved session row (internal/auth's revokeChainFrom, revoking a
-- refresh-token-reuse chain one hop at a time): Postgres filters
-- UPDATE ... RETURNING output through the table's SELECT policies,
-- exactly as it would a plain SELECT, so a row can be legitimately
-- updated (passing the UPDATE policy) yet return nothing via RETURNING
-- if no SELECT policy matches it. app.session_internal_op_id is set,
-- per statement, ONLY by trusted internal code that already resolved
-- the row through a legitimate path (the phase-1 token-hash lookup, in
-- every current caller) - this grants RETURNING visibility to at most
-- that one specific row, never a blanket read.
CREATE POLICY session_select_internal_op ON sessions
    FOR SELECT
    USING (id = NULLIF(current_setting('app.session_internal_op_id', true), '')::uuid);

-- INSERT/UPDATE policies (session_scoped_insert, session_scoped_update)
-- are unchanged - they were already tenant-scoped, not the exposure this
-- migration closes.
