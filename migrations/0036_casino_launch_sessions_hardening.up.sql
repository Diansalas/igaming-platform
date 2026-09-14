-- Stage 4A specialist review (architect/PostgreSQL-RLS) findings, closed
-- before the stage was declared complete:
--
-- 1. casino_launch_sessions had no immutability trigger, unlike every
--    sibling single-use-credential table this migration's own comment
--    claims to mirror (withdrawal_requests, migration 0026). Verified
--    exploitable: a tenant-staff-scoped UPDATE could resurrect a
--    'consumed' session back to 'active' with an unbounded new
--    expires_at, defeating single-use enforcement entirely.
-- 2. token_hash was a platform-GLOBAL UNIQUE constraint on a tenant-
--    partitioned, RLS-protected table - migration 0021's own stated rule
--    ("a platform-global unique key on a tenant-partitioned, RLS-
--    protected table is a cross-tenant collision and existence-oracle
--    risk, not merely a style preference") applies here identically.
--    ResolveLaunchToken already always runs inside a tenant-scoped
--    transaction, so scoping the uniqueness by tenant costs nothing.

-- 1. Immutability: freeze every identity/token/expiry column after
-- insert, and forbid any transition OUT of a terminal status
-- ('consumed'/'expired'/'revoked') - mirrors
-- withdrawal_requests_enforce_immutable_fields' shape (migration 0026)
-- exactly, adapted to this table's own terminal-status semantics (a
-- withdrawal request has no single "terminal means frozen forever"
-- state in the same sense; a launch session does).
CREATE FUNCTION casino_launch_sessions_enforce_immutable_fields() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
        OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
        OR NEW.wallet_id IS DISTINCT FROM OLD.wallet_id
        OR NEW.game_id IS DISTINCT FROM OLD.game_id
        OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
        OR NEW.provider_game_id IS DISTINCT FROM OLD.provider_game_id
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
        OR NEW.mode IS DISTINCT FROM OLD.mode
        OR NEW.token_hash IS DISTINCT FROM OLD.token_hash
        OR NEW.expires_at IS DISTINCT FROM OLD.expires_at
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'casino_launch_sessions: identity/token/expiry columns are immutable after insert';
    END IF;
    -- Once terminal, the row is frozen entirely (not merely its status) -
    -- the demonstrated exploit this closes combined a status revival with
    -- an unrelated column change (a new expires_at), so a check narrowed
    -- to "status changed" alone would still be too permissive if some
    -- future caller changed only consumed_at on an already-terminal row.
    IF OLD.status IN ('consumed', 'expired', 'revoked') THEN
        RAISE EXCEPTION 'casino_launch_sessions: row is immutable once consumed, expired, or revoked';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER casino_launch_sessions_immutable_fields
    BEFORE UPDATE ON casino_launch_sessions
    FOR EACH ROW EXECUTE FUNCTION casino_launch_sessions_enforce_immutable_fields();

-- 2. Tenant-scope the token_hash uniqueness. The inline column
-- constraint from migration 0035 auto-named itself
-- casino_launch_sessions_token_hash_key.
ALTER TABLE casino_launch_sessions DROP CONSTRAINT casino_launch_sessions_token_hash_key;
CREATE UNIQUE INDEX idx_casino_launch_sessions_tenant_token_hash
    ON casino_launch_sessions (tenant_id, token_hash);
