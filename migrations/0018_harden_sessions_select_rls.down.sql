DROP POLICY IF EXISTS session_select_internal_op ON sessions;
DROP POLICY IF EXISTS session_select_own_principal ON sessions;
DROP POLICY IF EXISTS session_select_by_token_hash ON sessions;

CREATE POLICY session_read_by_token ON sessions
    FOR SELECT
    USING (true);
