-- PRH-I5: ADR 0095 §12/§13.3 (docs/decisions/0095-provider-io-transaction-
-- boundary-and-payment-contract.md) - the payment_statement reconciliation
-- stream's append-only statement store, plus its eight mismatch kinds.
--
-- Migration number: ADR 0095 §13.3 names this "migration 0103". The
-- orchestrator swapped the allocation on 2026-09-27 (commit 07b354b): 0102
-- is payment statement reconciliation (this file) and 0103 is the kill
-- switch. The ADR's implementation record notes the swap.
--
-- What this migration does:
--   1. payment_statement_imports: one row per fetched statement (tenant,
--      provider, source label, coverage window, line count, content
--      digest). Idempotent on (tenant_id, provider_id, source_label,
--      coverage_start, coverage_end, content_digest): a re-fetch of the same
--      content stores nothing new.
--   2. payment_statement_lines: the statement's lines, verbatim. Duplicate
--      (provider_id, provider_reference, kind) lines are KEPT (distinct
--      line_no) so the stream can report pay_duplicate.
--   3. Size caps (S95-C11): line_count <= 1 000 000 per import; every text
--      column has an octet_length CHECK (provider references use migration
--      0099's 255-byte / no-control-character rule, merchant_reference
--      <= 64, asset_code <= 16, source_label <= 256). The line count is
--      bound to the stored rows: a statement-level trigger refuses any
--      INSERT that takes an import past its declared line_count, and a
--      deferred constraint trigger refuses a commit that leaves it short.
--   4. Both tables append-only (ledger_deny_mutation on UPDATE, DELETE and
--      TRUNCATE; binding on the owner too), FORCE ROW LEVEL SECURITY with
--      the staff-scope tenant policy used by migration 0101.
--   5. reconciliation_mismatches.mismatch_kind widened (strict superset of
--      migration 0098's list) with the eight pay_* kinds.
--   6. Guarded REVOKE/GRANT for igaming_runtime: SELECT and INSERT only
--      (re-asserted by deploy/init-app-role.sql on every run).
--
-- No ledger, projection, attempt, intent or receipt table is touched. The
-- stream reads those tables; it writes only reconciliation_runs /
-- reconciliation_mismatches (ADR 0095 INV-IO-12) and, in its separate
-- short ingest transaction, these two statement tables.

-- 1. Imports.
CREATE TABLE payment_statement_imports (
    id              UUID PRIMARY KEY,
    tenant_id       UUID NOT NULL REFERENCES tenants (id),
    provider_id     TEXT NOT NULL CHECK (
        octet_length(provider_id) BETWEEN 1 AND 255 AND provider_id !~ '[\x01-\x1F\x7F-\x9F]'
    ),
    source_label    TEXT NOT NULL CHECK (octet_length(source_label) BETWEEN 1 AND 256),
    is_mock         BOOLEAN NOT NULL,
    -- A synthetic (MOCK) source must say so in its label (the rule every
    -- statement source already follows).
    CHECK (NOT is_mock OR position('MOCK' IN source_label) > 0),
    coverage_start  TIMESTAMPTZ NOT NULL,
    coverage_end    TIMESTAMPTZ NOT NULL,
    CHECK (coverage_end > coverage_start),
    line_count      INT NOT NULL CHECK (line_count BETWEEN 0 AND 1000000),   -- S95-C11
    content_digest  BYTEA NOT NULL CHECK (octet_length(content_digest) = 32),  -- SHA-256
    fetched_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_id),
    UNIQUE (tenant_id, provider_id, source_label, coverage_start, coverage_end, content_digest)
);

CREATE INDEX payment_statement_imports_tenant ON payment_statement_imports (tenant_id, provider_id, fetched_at DESC);

-- 2. Lines.
CREATE TABLE payment_statement_lines (
    id                           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                    UUID NOT NULL,
    import_id                    UUID NOT NULL,
    FOREIGN KEY (import_id, tenant_id) REFERENCES payment_statement_imports (id, tenant_id),
    line_no                      INT NOT NULL CHECK (line_no >= 0 AND line_no < 1000000),
    provider_id                  TEXT NOT NULL CHECK (
        octet_length(provider_id) BETWEEN 1 AND 255 AND provider_id !~ '[\x01-\x1F\x7F-\x9F]'
    ),
    kind                         TEXT NOT NULL CHECK (kind IN ('deposit', 'deposit_reversal', 'payout')),
    provider_reference           TEXT NOT NULL CHECK (
        octet_length(provider_reference) BETWEEN 1 AND 255 AND provider_reference !~ '[\x01-\x1F\x7F-\x9F]'
    ),
    merchant_reference           TEXT NULL CHECK (
        merchant_reference IS NULL OR octet_length(merchant_reference) BETWEEN 1 AND 64
    ),
    original_provider_reference  TEXT NULL CHECK (
        original_provider_reference IS NULL
        OR (octet_length(original_provider_reference) BETWEEN 1 AND 255
            AND original_provider_reference !~ '[\x01-\x1F\x7F-\x9F]')
    ),
    settlement_reference         TEXT NULL CHECK (                                   -- LF95-C13
        settlement_reference IS NULL
        OR (octet_length(settlement_reference) BETWEEN 1 AND 255
            AND settlement_reference !~ '[\x01-\x1F\x7F-\x9F]')
    ),
    status                       TEXT NOT NULL CHECK (status IN ('pending', 'succeeded', 'declined', 'reversed')),
    amount                       NUMERIC(38, 0) NOT NULL CHECK (amount >= 0),
    asset_code                   TEXT NOT NULL CHECK (octet_length(asset_code) BETWEEN 1 AND 16),
    occurred_at                  TIMESTAMPTZ NOT NULL,
    UNIQUE (import_id, line_no)
);

CREATE INDEX payment_statement_lines_import ON payment_statement_lines (tenant_id, import_id, line_no);

-- 3. Line count bound to the stored rows.
--
-- (a) Statement-level, after every INSERT into lines: no import may hold
--     more lines than its declared line_count. Uses a transition table, so
--     one bulk COPY of a million lines fires this once, not per row.
CREATE FUNCTION payment_statement_lines_count_cap() RETURNS TRIGGER AS $$
DECLARE
    over RECORD;
BEGIN
    FOR over IN
        SELECT i.id, i.line_count, (SELECT count(*) FROM payment_statement_lines l WHERE l.import_id = i.id) AS stored
          FROM payment_statement_imports i
         WHERE i.id IN (SELECT DISTINCT import_id FROM new_lines)
    LOOP
        IF over.stored > over.line_count THEN
            RAISE EXCEPTION 'payment_statement_lines: import % declares % lines but % are stored', over.id, over.line_count, over.stored;
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER payment_statement_lines_count_cap
    AFTER INSERT ON payment_statement_lines
    REFERENCING NEW TABLE AS new_lines
    FOR EACH STATEMENT EXECUTE FUNCTION payment_statement_lines_count_cap();

-- (b) Deferred to commit, once per import row: the import's transaction
--     must have stored exactly line_count lines. Lines inserted by a LATER
--     transaction are refused by (a) because the import is already full.
CREATE FUNCTION payment_statement_imports_count_exact() RETURNS TRIGGER AS $$
DECLARE
    stored BIGINT;
BEGIN
    SELECT count(*) INTO stored FROM payment_statement_lines WHERE import_id = NEW.id;
    IF stored <> NEW.line_count THEN
        RAISE EXCEPTION 'payment_statement_imports: import % declares % lines but % were stored in its transaction', NEW.id, NEW.line_count, stored;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER payment_statement_imports_count_exact
    AFTER INSERT ON payment_statement_imports
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION payment_statement_imports_count_exact();

-- 4. Append-only, RLS.
CREATE TRIGGER payment_statement_imports_immutable
    BEFORE UPDATE OR DELETE ON payment_statement_imports
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER payment_statement_imports_no_truncate
    BEFORE TRUNCATE ON payment_statement_imports
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER payment_statement_lines_immutable
    BEFORE UPDATE OR DELETE ON payment_statement_lines
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER payment_statement_lines_no_truncate
    BEFORE TRUNCATE ON payment_statement_lines
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

ALTER TABLE payment_statement_imports ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_statement_imports FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_staff_scope ON payment_statement_imports
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

ALTER TABLE payment_statement_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_statement_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_staff_scope ON payment_statement_lines
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- 5. Mismatch kinds: strict superset of migration 0098's list.
ALTER TABLE reconciliation_mismatches DROP CONSTRAINT reconciliation_mismatches_mismatch_kind_check;
ALTER TABLE reconciliation_mismatches ADD CONSTRAINT reconciliation_mismatches_mismatch_kind_check
    CHECK (mismatch_kind IN ('missing_projection', 'balance_mismatch',
        'sb_locked_mismatch', 'sb_bet_net_mismatch', 'sb_orphan_ledger',
        'sb_orphan_history', 'sb_status_mismatch', 'sb_mock_statement_mismatch',
        'cas_round_binding_mismatch', 'cas_posting_shape_mismatch', 'cas_orphan_win',
        'cas_rollback_linkage_mismatch', 'cas_tombstone_conflict',
        'cas_unposted_provider_event', 'cas_tombstone_late_original',
        'cas_mock_statement_mismatch',
        'pay_missing_platform_record', 'pay_missing_provider_record', 'pay_amount_mismatch',
        'pay_asset_mismatch', 'pay_reference_mismatch', 'pay_status_mismatch',
        'pay_duplicate', 'pay_unresolved'));

-- 6. Runtime role: SELECT/INSERT only (defence in depth; the triggers above
--    are the binding control and bind every role, owner included).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON payment_statement_imports FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON payment_statement_imports TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON payment_statement_lines FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON payment_statement_lines TO igaming_runtime';
    END IF;
END $$;
