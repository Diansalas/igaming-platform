-- GOV-R32 (owner decision 4, ADR 0095 section 48; ADR 0111 section 23): the
-- controlled CANCELLATION/RELEASE outcome for a `destination_integrity_failure`
-- park, plus the review-round refusal-direction fixes (ADR 0111 23.6).
--
-- MUST RUN INSIDE A SINGLE TRANSACTION (db.MigrateUp does this).
--
-- What this adds / replaces:
--   1. payout_destination_park_evidence (NEW, append-only, system-written only):
--      the durable record of what the provider reported when the platform parked
--      a payout on a destination reason (destination_mismatch /
--      destination_integrity_failure), and of every provider SUCCESS reported
--      later on such a parked attempt (sync phase C, QueryStatus poll,
--      callback/receipt, late evidence). Ledger-finance HIGH: before this, a
--      success-triggered park left no durable trace and M4 not-paid could
--      release a hold the PSP had paid out. Existing destination parks are
--      backfilled as 'unknown_pre_0127' (fail closed). Policies, guard,
--      immutability and grants mirror payment_attempt_reference_evidence (0115).
--   2. payout_m4_evidence: the 0125 body plus, in the NOT-PAID branch only,
--      (a) contradictory on a recorded success, insufficient on an
--      'unknown_pre_0127' record; (b) insufficient when any payout line on the
--      bound reference or on any matched reference names ANOTHER merchant
--      reference (security C-1 / LF Q-R32-2; tightens destination_mismatch and
--      every other M4 not-paid as well). Refusal direction only.
--   3. payment_m4_in_scope: the 0125 body plus destination_integrity_failure
--      beside destination_mismatch in the NOT-PAID-ONLY arm. M4 PAID stays
--      REFUSED for both destination reasons (MR012).
--
-- Owner decision 4 is not broadened: nothing is automatic; the only writer of
-- money is an EXECUTED four-eyes M4 not-paid (platform_acting requester and
-- final approver, the DB floor) that returns the hold to the player's OWN cash
-- on positive D-7 evidence. "Resume" is DESIGN ONLY (ADR 0111 23.3).
--
-- New SQLSTATE: MR064 (a park-evidence row that does not describe a parked
-- destination payout). No SECURITY DEFINER. The down refuses (MR099) while any
-- M4 row pinned to destination_integrity_failure or any park-evidence row
-- exists, and otherwise restores the 0125 bodies byte for byte.

-- =========================================================================
-- 1. payout_destination_park_evidence
-- =========================================================================

CREATE TABLE payout_destination_park_evidence (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL,
    attempt_id       UUID NOT NULL,
    terminal_reason  TEXT NOT NULL CHECK (terminal_reason IN ('destination_mismatch', 'destination_integrity_failure')),
    reported_outcome TEXT NOT NULL CHECK (reported_outcome IN ('succeeded', 'declined', 'pending', 'ambiguous', 'unknown_pre_0127')),
    evidence_kind    TEXT NOT NULL CHECK (evidence_kind IN ('sync', 'callback', 'query_status', 'sweeper', 'operator', 'platform', 'migration_0127')),
    recorded_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (attempt_id, tenant_id) REFERENCES payment_attempts (id, tenant_id),
    -- One row per (attempt, outcome, source): repeated polls/redeliveries add
    -- nothing (the writer uses ON CONFLICT DO NOTHING); the growth is bounded.
    UNIQUE (tenant_id, attempt_id, reported_outcome, evidence_kind),
    -- The backfill marker is written by this migration only.
    CHECK ((reported_outcome = 'unknown_pre_0127') = (evidence_kind = 'migration_0127'))
);

-- Backfill (fail closed): every destination park that exists now has an
-- unknown trigger. The owner reads payment_attempts with FORCE RLS lifted for
-- this one statement (as 0125's down does), then restores it.
DO $$
DECLARE
    v_forced boolean;
BEGIN
    SELECT relforcerowsecurity INTO v_forced FROM pg_class WHERE oid = 'payment_attempts'::regclass;
    IF v_forced THEN
        EXECUTE 'ALTER TABLE payment_attempts NO FORCE ROW LEVEL SECURITY';
    END IF;
    INSERT INTO payout_destination_park_evidence (tenant_id, attempt_id, terminal_reason, reported_outcome, evidence_kind)
    SELECT a.tenant_id, a.id, a.terminal_reason, 'unknown_pre_0127', 'migration_0127'
      FROM payment_attempts a
     WHERE a.operation = 'payout' AND a.state = 'disputed'
       AND a.terminal_reason IN ('destination_mismatch', 'destination_integrity_failure');
    IF v_forced THEN
        EXECUTE 'ALTER TABLE payment_attempts FORCE ROW LEVEL SECURITY';
    END IF;
END $$;

-- Only a parked destination payout of the same reason may be described, and
-- never with the backfill marker (O-5 style: the attempt row is locked so its
-- state cannot move under the check).
CREATE FUNCTION payout_destination_park_evidence_guard() RETURNS TRIGGER AS $$
DECLARE
    v_att RECORD;
BEGIN
    SELECT a.operation, a.state, a.terminal_reason INTO v_att
      FROM payment_attempts a WHERE a.id = NEW.attempt_id AND a.tenant_id = NEW.tenant_id FOR SHARE;
    IF NOT FOUND
       OR v_att.operation <> 'payout'
       OR v_att.state <> 'disputed'
       OR v_att.terminal_reason IS DISTINCT FROM NEW.terminal_reason
       OR NEW.reported_outcome = 'unknown_pre_0127' THEN
        RAISE EXCEPTION 'payout_destination_park_evidence: a row may only describe a payout parked on that destination reason' USING ERRCODE = 'MR064';
    END IF;
    NEW.recorded_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER payout_destination_park_evidence_guard
    BEFORE INSERT ON payout_destination_park_evidence
    FOR EACH ROW EXECUTE FUNCTION payout_destination_park_evidence_guard();
CREATE TRIGGER payout_destination_park_evidence_immutable
    BEFORE UPDATE OR DELETE ON payout_destination_park_evidence
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER payout_destination_park_evidence_no_truncate
    BEFORE TRUNCATE ON payout_destination_park_evidence
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- System shape writes and reads (the payout evidence paths and reconciliation);
-- a VALID acting session reads (payout_m4_evidence in the M4 request and
-- execution). No tenant-staff, player, platform or UPDATE/DELETE policy.
ALTER TABLE payout_destination_park_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE payout_destination_park_evidence FORCE ROW LEVEL SECURITY;
CREATE POLICY system_insert ON payout_destination_park_evidence FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY system_select ON payout_destination_park_evidence FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY acting_read ON payout_destination_park_evidence FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON payout_destination_park_evidence FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON payout_destination_park_evidence TO igaming_runtime';
    END IF;
END $$;

-- =========================================================================
-- 2. payout_m4_evidence: the 0125 body + the two not-paid refusals.
-- =========================================================================

CREATE OR REPLACE FUNCTION payout_m4_evidence(p_tenant uuid, p_attempt uuid)
    RETURNS TABLE (verdict text, line_id uuid, reference text, import_ids uuid[]) AS $$
#variable_conflict use_column
DECLARE
    v_a        RECORD;
    v_ys       text[];
    v_rs       text[];
    v_has_real boolean;
    v_n        int;
    v_ids      uuid[];
    v_groups   int;
    v_g        RECORD;
    v_line     uuid;
    v_paid     boolean;
BEGIN
    IF NOT payment_m4_scope_visible(p_tenant) THEN
        RAISE EXCEPTION 'payout_m4_evidence: this session cannot see the attempt''s whole statement scope (an error, never a verdict)' USING ERRCODE = 'MR060';
    END IF;
    SELECT a.operation, a.provider_id, a.provider_reference, a.merchant_reference, a.amount, a.asset_code,
           a.created_at, a.last_sent_at INTO v_a
      FROM payment_attempts a WHERE a.id = p_attempt AND a.tenant_id = p_tenant;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'payout_m4_evidence: attempt % is not visible in tenant %', p_attempt, p_tenant USING ERRCODE = 'MR060';
    END IF;
    IF v_a.operation <> 'payout' OR v_a.provider_id IS NULL THEN
        RETURN QUERY SELECT 'insufficient'::text, NULL::uuid, NULL::text, '{}'::uuid[];
        RETURN;
    END IF;

    v_ys := ARRAY(SELECT e.reference FROM payment_attempt_reference_evidence e
                   WHERE e.tenant_id = p_tenant AND e.attempt_id = p_attempt AND e.provider_id = v_a.provider_id
                   ORDER BY e.reference);
    v_has_real := EXISTS (SELECT 1 FROM payment_statement_imports i
                           WHERE i.tenant_id = p_tenant AND i.provider_id = v_a.provider_id AND NOT i.is_mock);

    -- Bound 1: the platform-keyed lookups (merchant, bound reference, Y).
    SELECT count(*) INTO v_n FROM (
        SELECT 1 FROM payment_statement_lines l
         WHERE l.tenant_id = p_tenant AND l.provider_id = v_a.provider_id AND l.kind = 'payout'
           AND (l.merchant_reference = v_a.merchant_reference OR l.provider_reference = v_a.provider_reference
                OR l.provider_reference = ANY (v_ys))
         LIMIT 65) s;
    IF v_n > 64 THEN
        RETURN QUERY SELECT 'evidence_overflow'::text, NULL::uuid, NULL::text, '{}'::uuid[];
        RETURN;
    END IF;
    -- The PSP references of EVERY platform-keyed line, any status (review
    -- amendment H-1/sec: the paid line's R and the declined line's D alike).
    -- Statement content: no budget of its own; their lines join the read set
    -- below and count against the same 64-line bound (S-4).
    v_rs := ARRAY(SELECT DISTINCT l.provider_reference FROM payment_statement_lines l
                   WHERE l.tenant_id = p_tenant AND l.provider_id = v_a.provider_id AND l.kind = 'payout'
                     AND (l.merchant_reference = v_a.merchant_reference OR l.provider_reference = v_a.provider_reference
                          OR l.provider_reference = ANY (v_ys))
                   ORDER BY 1);
    -- Bound 2: the whole read set S, the R lookups included (S-4).
    SELECT count(*) INTO v_n FROM (
        SELECT 1 FROM payment_statement_lines l
         WHERE l.tenant_id = p_tenant AND l.provider_id = v_a.provider_id AND l.kind = 'payout'
           AND (l.merchant_reference = v_a.merchant_reference OR l.provider_reference = v_a.provider_reference
                OR l.provider_reference = ANY (v_ys) OR l.provider_reference = ANY (v_rs))
         LIMIT 65) s;
    IF v_n > 64 THEN
        RETURN QUERY SELECT 'evidence_overflow'::text, NULL::uuid, NULL::text, '{}'::uuid[];
        RETURN;
    END IF;
    v_ids := ARRAY(SELECT DISTINCT l.import_id FROM payment_statement_lines l
                    WHERE l.tenant_id = p_tenant AND l.provider_id = v_a.provider_id AND l.kind = 'payout'
                      AND (l.merchant_reference = v_a.merchant_reference OR l.provider_reference = v_a.provider_reference
                           OR l.provider_reference = ANY (v_ys) OR l.provider_reference = ANY (v_rs))
                    ORDER BY 1);

    v_paid := EXISTS (SELECT 1 FROM payment_statement_lines l
                       WHERE l.tenant_id = p_tenant AND l.provider_id = v_a.provider_id AND l.kind = 'payout'
                         AND l.status = 'succeeded'
                         AND (l.merchant_reference = v_a.merchant_reference OR l.provider_reference = v_a.provider_reference
                              OR l.provider_reference = ANY (v_ys) OR l.provider_reference = ANY (v_rs)));

    IF v_paid THEN
        -- ---------------- the paid verdict (C-5) ----------------
        -- Exactly one succeeded line after cross-import dedupe on
        -- (provider_reference, status, amount, asset_code, occurred_at).
        SELECT count(*) INTO v_groups FROM (
            SELECT DISTINCT l.provider_reference, l.amount, l.asset_code, l.occurred_at
              FROM payment_statement_lines l
             WHERE l.tenant_id = p_tenant AND l.provider_id = v_a.provider_id AND l.kind = 'payout'
               AND l.status = 'succeeded'
               AND (l.merchant_reference = v_a.merchant_reference OR l.provider_reference = v_a.provider_reference
                    OR l.provider_reference = ANY (v_ys) OR l.provider_reference = ANY (v_rs))) g;
        IF v_groups <> 1 THEN
            RETURN QUERY SELECT 'contradictory'::text, NULL::uuid, NULL::text, v_ids;
            RETURN;
        END IF;
        SELECT DISTINCT l.provider_reference AS ref, l.amount, l.asset_code, l.occurred_at INTO v_g
          FROM payment_statement_lines l
         WHERE l.tenant_id = p_tenant AND l.provider_id = v_a.provider_id AND l.kind = 'payout'
           AND l.status = 'succeeded'
           AND (l.merchant_reference = v_a.merchant_reference OR l.provider_reference = v_a.provider_reference
                OR l.provider_reference = ANY (v_ys) OR l.provider_reference = ANY (v_rs));
        -- No declined, reversed or pending line on the merchant reference, the
        -- bound reference, R or any Y, in ANY import (MOCK and unsealed included).
        IF EXISTS (SELECT 1 FROM payment_statement_lines l
                    WHERE l.tenant_id = p_tenant AND l.provider_id = v_a.provider_id AND l.kind = 'payout'
                      AND l.status IN ('declined', 'reversed', 'pending')
                      AND (l.merchant_reference = v_a.merchant_reference OR l.provider_reference = v_a.provider_reference
                           OR l.provider_reference = ANY (v_ys) OR l.provider_reference = ANY (v_rs))) THEN
            RETURN QUERY SELECT 'contradictory'::text, NULL::uuid, NULL::text, v_ids;
            RETURN;
        END IF;
        -- Review amendment H-2/sec, C-1/LF: R must be unambiguously THIS
        -- attempt's - any payout line on R (any status, any import) naming
        -- another merchant reference makes the verdict contradictory.
        IF EXISTS (SELECT 1 FROM payment_statement_lines l
                    WHERE l.tenant_id = p_tenant AND l.provider_id = v_a.provider_id AND l.kind = 'payout'
                      AND l.provider_reference = v_g.ref
                      AND l.merchant_reference IS NOT NULL AND l.merchant_reference <> v_a.merchant_reference) THEN
            RETURN QUERY SELECT 'contradictory'::text, NULL::uuid, NULL::text, v_ids;
            RETURN;
        END IF;
        -- I-1: the evidenced amount and asset are the attempt's.
        IF v_g.amount IS DISTINCT FROM v_a.amount OR v_g.asset_code IS DISTINCT FROM v_a.asset_code THEN
            RETURN QUERY SELECT 'contradictory'::text, NULL::uuid, NULL::text, v_ids;
            RETURN;
        END IF;
        -- Attribution of R (C-5): no reserved prefix; payment_y_attributable (0119);
        -- no tombstone on (provider, R); no other attempt holds R as reference or
        -- typed Y; the attempt's own Y, if any, is R; no withdrawal holds R; no
        -- ledger row keyed by (provider, R) of any type; no idempotency key
        -- provider:R.
        IF left(v_g.ref, 27) = payment_reserved_ref_prefix()
           OR NOT payment_y_attributable(p_tenant, v_a.provider_id, p_attempt, v_g.ref)
           OR EXISTS (SELECT 1 FROM ledger_transactions t
                       WHERE t.tenant_id = p_tenant AND t.provider_id = v_a.provider_id
                         AND t.transaction_type = 'tombstone' AND t.provider_tx_id = v_g.ref)
           OR EXISTS (SELECT 1 FROM payment_attempts o
                       WHERE o.tenant_id = p_tenant AND o.provider_id = v_a.provider_id
                         AND o.provider_reference = v_g.ref AND o.id <> p_attempt)
           OR EXISTS (SELECT 1 FROM payment_attempt_reference_evidence e
                       WHERE e.tenant_id = p_tenant AND e.provider_id = v_a.provider_id
                         AND e.reference = v_g.ref AND e.attempt_id <> p_attempt)
           OR (cardinality(v_ys) > 0 AND v_ys IS DISTINCT FROM ARRAY[v_g.ref])
           OR EXISTS (SELECT 1 FROM withdrawal_requests w
                       WHERE w.tenant_id = p_tenant AND w.provider_reference = v_g.ref)
           OR EXISTS (SELECT 1 FROM ledger_transactions t
                       WHERE t.tenant_id = p_tenant AND t.provider_id = v_a.provider_id AND t.provider_tx_id = v_g.ref)
           OR EXISTS (SELECT 1 FROM ledger_transactions t
                       WHERE t.tenant_id = p_tenant AND t.idempotency_key = v_a.provider_id || ':' || v_g.ref) THEN
            RETURN QUERY SELECT 'contradictory'::text, NULL::uuid, NULL::text, v_ids;
            RETURN;
        END IF;
        -- The single succeeded line must come from an ELIGIBLE (sealed) import.
        SELECT l.id INTO v_line
          FROM payment_statement_lines l
          JOIN payment_statement_imports i ON i.id = l.import_id AND i.tenant_id = l.tenant_id
         WHERE l.tenant_id = p_tenant AND l.provider_id = v_a.provider_id AND l.kind = 'payout'
           AND l.status = 'succeeded'
           AND l.provider_reference = v_g.ref AND l.amount = v_g.amount AND l.asset_code = v_g.asset_code
           AND l.occurred_at = v_g.occurred_at
           AND i.import_seal IS NOT NULL
           AND (NOT i.is_mock OR NOT v_has_real)
         ORDER BY i.fetched_at, l.import_id, l.line_no
         LIMIT 1;
        IF v_line IS NULL THEN
            RETURN QUERY SELECT 'insufficient'::text, NULL::uuid, NULL::text, v_ids;
            RETURN;
        END IF;
        RETURN QUERY SELECT 'paid'::text, v_line, v_g.ref::text, v_ids;
        RETURN;
    END IF;

    -- ---------------- the not-paid verdict (H-1) ----------------
    -- GOV-R32 (migration 0127; ledger-finance HIGH): a provider SUCCESS the
    -- platform recorded when it parked this attempt on a destination reason (or
    -- later, on the parked attempt) contradicts "not paid", whatever the
    -- statement says. A destination park that predates 0127 carries the
    -- backfilled 'unknown_pre_0127' record: its trigger is unknown, so not-paid
    -- is insufficient (fail closed).
    IF EXISTS (SELECT 1 FROM payout_destination_park_evidence e
                WHERE e.tenant_id = p_tenant AND e.attempt_id = p_attempt AND e.reported_outcome = 'succeeded') THEN
        RETURN QUERY SELECT 'contradictory'::text, NULL::uuid, NULL::text, v_ids;
        RETURN;
    END IF;
    IF EXISTS (SELECT 1 FROM payout_destination_park_evidence e
                WHERE e.tenant_id = p_tenant AND e.attempt_id = p_attempt AND e.reported_outcome = 'unknown_pre_0127') THEN
        RETURN QUERY SELECT 'insufficient'::text, NULL::uuid, NULL::text, v_ids;
        RETURN;
    END IF;
    -- (iii) no succeeded, pending or reversed payout line on the merchant
    -- reference, the bound reference, any Y or - review amendment H-1/sec - the
    -- PSP reference D of any of those lines (the declined line's own reference
    -- included), in ANY import.
    IF EXISTS (SELECT 1 FROM payment_statement_lines l
                WHERE l.tenant_id = p_tenant AND l.provider_id = v_a.provider_id AND l.kind = 'payout'
                  AND l.status IN ('succeeded', 'pending', 'reversed')
                  AND (l.merchant_reference = v_a.merchant_reference OR l.provider_reference = v_a.provider_reference
                       OR l.provider_reference = ANY (v_ys) OR l.provider_reference = ANY (v_rs))) THEN
        RETURN QUERY SELECT 'insufficient'::text, NULL::uuid, NULL::text, v_ids;
        RETURN;
    END IF;
    -- GOV-R32 (migration 0127; security C-1, ledger-finance Q-R32-2): a payout
    -- line on the bound reference or on any matched reference (v_rs) that names
    -- ANOTHER merchant reference makes the attribution of those references
    -- ambiguous: no not-paid verdict (the paid branch has the H-2 rule). These
    -- lines are already inside the 64-line read set (S-4).
    IF EXISTS (SELECT 1 FROM payment_statement_lines l
                WHERE l.tenant_id = p_tenant AND l.provider_id = v_a.provider_id AND l.kind = 'payout'
                  AND (l.provider_reference = v_a.provider_reference OR l.provider_reference = ANY (v_rs))
                  AND l.merchant_reference IS NOT NULL AND l.merchant_reference <> v_a.merchant_reference) THEN
        RETURN QUERY SELECT 'insufficient'::text, NULL::uuid, NULL::text, v_ids;
        RETURN;
    END IF;
    -- Review amendment H-1/sec: an import that DECLARES its payout lines carry
    -- the merchant reference but holds a payout line without one contradicts
    -- its own declaration: no not-paid verdict while any such import is read.
    IF EXISTS (SELECT 1 FROM payment_statement_lines l
                 JOIN payment_statement_imports i ON i.id = l.import_id AND i.tenant_id = l.tenant_id
                WHERE l.tenant_id = p_tenant AND l.import_id = ANY (v_ids) AND l.kind = 'payout'
                  AND l.merchant_reference IS NULL AND i.payout_lines_carry_merchant_reference) THEN
        RETURN QUERY SELECT 'insufficient'::text, NULL::uuid, NULL::text, v_ids;
        RETURN;
    END IF;
    IF v_a.last_sent_at IS NULL THEN
        RETURN QUERY SELECT 'insufficient'::text, NULL::uuid, NULL::text, v_ids;
        RETURN;
    END IF;
    -- (i) coverage, (ii) the declined line, (iv) the source declaration - all of
    -- the SAME eligible import. The window is payments.DefaultSettlementWindow
    -- (24 h; a Go test pins equality).
    SELECT l.id INTO v_line
      FROM payment_statement_lines l
      JOIN payment_statement_imports i ON i.id = l.import_id AND i.tenant_id = l.tenant_id
     WHERE l.tenant_id = p_tenant AND l.provider_id = v_a.provider_id AND l.kind = 'payout'
       AND l.status = 'declined'
       AND l.amount = v_a.amount AND l.asset_code = v_a.asset_code
       AND l.occurred_at >= v_a.last_sent_at
       AND (l.merchant_reference = v_a.merchant_reference
            OR (v_a.provider_reference IS NOT NULL AND l.provider_reference = v_a.provider_reference))
       AND i.import_seal IS NOT NULL
       AND (NOT i.is_mock OR NOT v_has_real)
       AND i.payout_lines_carry_merchant_reference
       AND i.coverage_start <= v_a.created_at
       AND i.coverage_end >= v_a.last_sent_at + interval '24 hours'
     ORDER BY i.fetched_at, l.import_id, l.line_no
     LIMIT 1;
    IF v_line IS NULL THEN
        RETURN QUERY SELECT 'insufficient'::text, NULL::uuid, NULL::text, v_ids;
        RETURN;
    END IF;
    RETURN QUERY SELECT 'not_paid'::text, v_line, NULL::text, v_ids;
END;
$$ LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp;

-- =========================================================================
-- 3. payment_m4_in_scope: the 0125 body + destination_integrity_failure in
--    the not-paid-only arm (with or without a bound reference).
-- =========================================================================

CREATE OR REPLACE FUNCTION payment_m4_in_scope(p_kind text, p_state text, p_reason text, p_ref text) RETURNS boolean AS $$
    SELECT COALESCE(p_kind IN ('m4_evidence_paid', 'm4_evidence_not_paid')
        AND p_state = 'disputed'
        AND ((p_ref IS NULL
              AND (p_reason = 'invalid_provider_reference'
                   OR starts_with(p_reason, 'invalid_provider_reference:')
                   OR p_reason = 'provider_reference_conflict'))
             OR (p_kind = 'm4_evidence_not_paid'
                 AND (p_reason = 'destination_mismatch' OR p_reason = 'destination_integrity_failure'))), false);
$$ LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp;
