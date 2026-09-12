-- WARNING: this restores the exact cross-tenant/cross-principal session-
-- metadata read exposure migration 0018's up script exists to close (see
-- docs/decisions/0016-sessions-rls-hardening.md). Only ever intended as
-- the reversibility half of a dev/CI up-down-up verification cycle -
-- never as a production rollback path for this specific change without
-- an accompanying decision to accept that exposure again.

DROP POLICY IF EXISTS session_select_internal_op ON sessions;
DROP POLICY IF EXISTS session_select_own_principal ON sessions;
DROP POLICY IF EXISTS session_select_by_token_hash ON sessions;

CREATE POLICY session_read_by_token ON sessions
    FOR SELECT
    USING (true);
