-- Stage 10.3 W2b, CAS-RECON-1 (ADR 0092; ADR 0025 Stage 10.3 amendment
-- item 9; docs/plans/stage-10.3-planning/02-casino-financial-analysis.md
-- §2.4, §2.6, §2.8; gate 10.3-W1 ledger-finance C9 and its extension).
--
-- Contents:
--   1. casino_callback_rejections: the append-only, verified-only record
--      of a provider callback the platform REJECTED after it passed
--      signature verification (never anything before verification - the
--      writer is only ever handed a post-verification event). Input to the
--      casino_consistency reconciliation stream's C6/C7 checks.
--   2. reconciliation_mismatches.mismatch_kind widened for the
--      casino_consistency stream's seven checks (C1-C7). The
--      casino_statement stream's own kind (W3a, CAS-RECON-STMT-1) is NOT
--      added here; it ships with that stream.
--   3. Guarded REVOKE of UPDATE/DELETE/TRUNCATE on the new table from
--      igaming_runtime (defence in depth; the deny triggers bind every
--      role, owner included, and are the binding control).
--
-- No ledger, projection, round, session or capability table is touched.

-- 1. The rejection record.
CREATE TABLE casino_callback_rejections (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID NOT NULL REFERENCES tenants (id),
    provider_id             TEXT NOT NULL CHECK (provider_id <> ''),
    event_type              TEXT NOT NULL CHECK (event_type IN ('bet', 'win', 'rollback')),
    -- THIS callback's own provider reference (for a rollback: the
    -- rollback's own reference, never the original's).
    provider_tx_id          TEXT NOT NULL CHECK (provider_tx_id <> ''),
    -- Rollback only: the reference the rollback names.
    original_provider_tx_id TEXT,
    round_id                TEXT,
    asset_code              TEXT,
    -- Minor units as asserted by the provider. NULL for a rollback (a
    -- rollback's amount is never authoritative - the original's entries
    -- are). NUMERIC(38,0) to match ledger_entries.amount.
    amount                  NUMERIC(38, 0) CHECK (amount IS NULL OR amount > 0),
    reason_class            TEXT NOT NULL CHECK (reason_class IN (
        -- E3 (late bet after its tombstone) and E10 (late win after its
        -- tombstone).
        'original_tombstoned',
        -- The G-1 409 abort classes (ADR 0025 Stage 10.3 amendment item 7).
        'ambiguous_round', 'wallet_collision', 'mixed_funding',
        'lock_already_released', 'bonus_bet_not_locked',
        -- Other verified, terminal financial rejections.
        'bet_not_found', 'already_rolled_back', 'payload_mismatch',
        'round_ownership_conflict',
        -- E9 with a DIFFERENT reference: a second, distinct rollback
        -- reference naming an original that is already tombstoned
        -- (acknowledged 200, no ledger or audit record of its own).
        'rollback_of_tombstoned_original'
    )),
    -- The request id of the FIRST delivery that recorded this row, for
    -- joining to the http_request log line. Not PII.
    request_id              TEXT,
    first_seen_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT casino_callback_rejections_rollback_shape CHECK (
        (event_type = 'rollback') = (original_provider_tx_id IS NOT NULL)),
    CONSTRAINT casino_callback_rejections_rollback_amount CHECK (
        event_type <> 'rollback' OR amount IS NULL),
    -- Idempotency: a redelivered rejection never adds a row. Writers use
    -- INSERT ... ON CONFLICT DO NOTHING against exactly this key.
    CONSTRAINT casino_callback_rejections_once_per_key
        UNIQUE (tenant_id, provider_id, event_type, provider_tx_id, reason_class)
);

CREATE INDEX idx_casino_callback_rejections_provider_tx
    ON casino_callback_rejections (tenant_id, provider_id, provider_tx_id);
CREATE INDEX idx_casino_callback_rejections_seen
    ON casino_callback_rejections (tenant_id, first_seen_at DESC);

ALTER TABLE casino_callback_rejections ENABLE ROW LEVEL SECURITY;
ALTER TABLE casino_callback_rejections FORCE ROW LEVEL SECURITY;

-- Append-only, so no FOR ALL policy (the 0091 sportsbook_bet_settlements
-- shape): staff/system may read and insert under tenant scope, never
-- under a player-scoped connection (the 0028/0035 player-exclusion
-- guard). No UPDATE/DELETE policy exists; the deny triggers below bind
-- regardless.
CREATE POLICY tenant_staff_select ON casino_callback_rejections
    FOR SELECT
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY tenant_staff_insert ON casino_callback_rejections
    FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE TRIGGER casino_callback_rejections_immutable
    BEFORE UPDATE OR DELETE ON casino_callback_rejections
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER casino_callback_rejections_no_truncate
    BEFORE TRUNCATE ON casino_callback_rejections
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- 2. Mismatch kinds for the casino_consistency stream. Purely additive
-- (a strict superset of migration 0091's list); constraint validation
-- runs anyway and cannot be blinded by FORCE ROW LEVEL SECURITY.
ALTER TABLE reconciliation_mismatches DROP CONSTRAINT reconciliation_mismatches_mismatch_kind_check;
ALTER TABLE reconciliation_mismatches ADD CONSTRAINT reconciliation_mismatches_mismatch_kind_check
    CHECK (mismatch_kind IN ('missing_projection', 'balance_mismatch',
        'sb_locked_mismatch', 'sb_bet_net_mismatch', 'sb_orphan_ledger',
        'sb_orphan_history', 'sb_status_mismatch', 'sb_mock_statement_mismatch',
        'cas_round_binding_mismatch', 'cas_posting_shape_mismatch', 'cas_orphan_win',
        'cas_rollback_linkage_mismatch', 'cas_tombstone_conflict',
        'cas_unposted_provider_event', 'cas_tombstone_late_original'));

-- 3. Runtime role: defence in depth only. Guarded because the role may be
-- provisioned after migrations run (docs/security/runtime-role-
-- separation.md §6). NOTE: deploy/init-app-role.sql's blanket backfill
-- GRANT would re-grant these privileges if that script is re-run; the
-- deny triggers above still bind. Adding the matching guarded REVOKE to
-- that script is a deploy/ change, out of Stage 10.3 scope (ADR 0092) -
-- carried forward, not done here.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE UPDATE, DELETE, TRUNCATE ON casino_callback_rejections FROM igaming_runtime';
    END IF;
END $$;
