-- Per-(tenant_id, consumer) ledger-derived sweep watermark (Stage 4H-B1
-- Wave 3 Phase 1, ledger-accounting-model.md §7.18.2 item 1; doc 29
-- §2.2's already-approved "a per-(tenant_id, consumer) watermark is the
-- durable backstop" naming; doc 29 §7's BC-24, "consumer watermark row -
-- OWED BY bonus-engine"). Schema only (Stage 4H-B1 Wave 3 Phase 2,
-- backend) - no scheduler, no Offer-matching logic, no scan query. All
-- of that is Phase 3 (bonus-engine), per ledger-accounting-model.md
-- §7.18.6's own ownership table.
--
-- Granularity, decided here with reasoning (the dispatch's own
-- question): per (tenant_id, consumer_name), a single TENANT-GLOBAL
-- cursor - never per-campaign. §7.18.2 item 1's own worked example scans
-- `ledger_transactions WHERE tenant_id = $1 AND transaction_type =
-- 'deposit' AND id > <this tenant's last-processed watermark>` - one
-- cursor over every deposit for the tenant, full stop. Campaign/Offer
-- resolution happens downstream, per already-selected deposit row,
-- independent of how far the sweep has scanned: the same ledger row can
-- match more than one campaign's Offer (or none), so "have I looked at
-- this ledger row yet" has nothing to do with which campaign, and a
-- per-campaign cursor would force N redundant full rescans of the same
-- deposit history for N active campaigns with no correctness or
-- performance benefit. consumer_name (mirroring reconciliation_runs.
-- stream's free-text, non-enum convention, migration 0027 - "e.g.
-- 'ledger_vs_projection', 'wallet_vs_psp'") is carried so a second,
-- future ledger-derived consumer (another BC-24 instance - e.g. a
-- generic ActivityIngestor, if one is ever built) gets its own
-- independent cursor from this same table without a second migration.
-- This is not speculative widening: doc 29 §2.2 already names the
-- concept generically ("per-(tenant_id, consumer)"), this migration
-- simply builds the one shape that naming already specifies rather than
-- a narrower "deposits only" table that would need replacing the moment
-- a second consumer is built. Today exactly one consumer exists
-- ('bonus_deposit_sweep', per §7.18.2's own lock-key example
-- `hashtextextended('bonus_deposit_sweep:' || tenant_id, 0)`) and Phase 3
-- is expected to use that literal string; this migration does not
-- hardcode it as a CHECK-constrained enum value for the same reason
-- reconciliation_runs.stream isn't one.
--
-- ORDERING CAVEAT - disclosed rather than silently assumed (found while
-- designing this schema, not previously stated this precisely anywhere):
-- ledger_transactions.id is gen_random_uuid() - a random UUIDv4, not a
-- sequential or time-ordered value. "id > watermark" over a random UUID
-- domain is still a valid TOTAL ORDER (safe for enumerating "rows not
-- yet marked seen" without gaps or duplicates, given a FIXED domain), but
-- it does NOT correspond to insertion/commit order. A newly-committed
-- deposit's random id can sort BELOW an already-advanced id watermark,
-- which would silently skip that deposit forever under a naive
-- "id > last_seen_id" cursor. last_processed_posted_at is stored
-- alongside last_processed_ledger_transaction_id specifically so Phase 3
-- can order/advance the cursor by (posted_at, id) - the standard keyset-
-- pagination shape, id only breaking ties within the same instant - and,
-- if it judges commit-order/timestamp-order skew a real risk for this
-- workload, can advance the timestamp component with a conservative
-- trailing safety margin behind clock_timestamp() rather than to the
-- literal maximum seen. This migration deliberately does not prescribe
-- which strategy Phase 3 picks (that is scan/scheduler business logic,
-- out of a schema-only phase's scope) - both fit this same column shape
-- without a further migration - and does not silently paper over the
-- finding by inventing scan logic here.
CREATE TABLE bonus_ledger_sweep_watermarks (
    id                                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                             UUID NOT NULL,
    consumer_name                         TEXT NOT NULL,
    -- Both null together (nothing processed yet) or both set together -
    -- never one without the other.
    last_processed_ledger_transaction_id  UUID,
    last_processed_posted_at              TIMESTAMPTZ,
    CHECK ((last_processed_ledger_transaction_id IS NULL) = (last_processed_posted_at IS NULL)),
    updated_at                            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, consumer_name),
    FOREIGN KEY (last_processed_ledger_transaction_id, tenant_id) REFERENCES ledger_transactions (id, tenant_id)
);

CREATE INDEX idx_bonus_ledger_sweep_watermarks_tenant ON bonus_ledger_sweep_watermarks (tenant_id);

-- The cursor columns (last_processed_*/updated_at) are exactly what this
-- table exists to UPDATE on every tick - unlike every append-only Bonus
-- table built so far, there is deliberately no blanket update-denial
-- trigger here. Only the row's own identity (which tenant, which
-- consumer) is frozen after insert.
CREATE FUNCTION bonus_ledger_sweep_watermarks_enforce_immutable_identity() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.consumer_name IS DISTINCT FROM OLD.consumer_name
    THEN
        RAISE EXCEPTION 'bonus_ledger_sweep_watermarks: tenant_id/consumer_name are immutable after insert - create a new watermark row for a new consumer instead';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER bonus_ledger_sweep_watermarks_immutable_identity
    BEFORE UPDATE ON bonus_ledger_sweep_watermarks
    FOR EACH ROW EXECUTE FUNCTION bonus_ledger_sweep_watermarks_enforce_immutable_identity();

ALTER TABLE bonus_ledger_sweep_watermarks ENABLE ROW LEVEL SECURITY;
ALTER TABLE bonus_ledger_sweep_watermarks FORCE ROW LEVEL SECURITY;

-- Staff/system-only (bulk_grant_jobs' precedent, migration 0060, "no
-- player-facing purpose exists for a sweep cursor"). No DELETE policy;
-- no player_self_scope at all.
CREATE POLICY tenant_isolation_read ON bonus_ledger_sweep_watermarks
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY tenant_isolation_insert ON bonus_ledger_sweep_watermarks
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY tenant_isolation_update ON bonus_ledger_sweep_watermarks
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE TRIGGER bonus_ledger_sweep_watermarks_no_truncate
    BEFORE TRUNCATE ON bonus_ledger_sweep_watermarks
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();
