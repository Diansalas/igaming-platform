-- MUST RUN INSIDE A SINGLE TRANSACTION (db.MigrateDown does this). By hand, use
-- `psql --single-transaction -v ON_ERROR_STOP=1 -f <this file>`: an MR099 refusal
-- below would otherwise leave FORCE ROW LEVEL SECURITY lifted on the checked tables.
--
-- Reverses 0127 (GOV-R32, ADR 0111 section 23). REFUSES (MR099) while ANY M4 row
-- (any kind, any state) exists whose pinned dispute reason is
-- destination_integrity_failure (under the restored 0125 scope such a row would be
-- outside M4), or while ANY payout_destination_park_evidence row exists (dropping
-- it would discard a recorded provider success and reopen M4 not-paid on a
-- success-parked destination_mismatch park). Otherwise it drops the evidence table
-- and its guard and restores the 0125 bodies of payout_m4_evidence and
-- payment_m4_in_scope BYTE FOR BYTE (the whole-schema snapshot test verifies it).

ALTER TABLE payment_manual_resolutions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE payout_destination_park_evidence NO FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM payment_manual_resolutions
                WHERE kind IN ('m4_evidence_paid', 'm4_evidence_not_paid')
                  AND terminal_reason_at_submission = 'destination_integrity_failure')
       OR EXISTS (SELECT 1 FROM payout_destination_park_evidence) THEN
        RAISE EXCEPTION '0127 down refused: M4 rows on a destination_integrity_failure park or destination park evidence exist' USING ERRCODE = 'MR099';
    END IF;
END $$;

ALTER TABLE payment_manual_resolutions FORCE ROW LEVEL SECURITY;

CREATE OR REPLACE FUNCTION payment_m4_in_scope(p_kind text, p_state text, p_reason text, p_ref text) RETURNS boolean AS $$
    SELECT COALESCE(p_kind IN ('m4_evidence_paid', 'm4_evidence_not_paid')
        AND p_state = 'disputed'
        AND ((p_ref IS NULL
              AND (p_reason = 'invalid_provider_reference'
                   OR starts_with(p_reason, 'invalid_provider_reference:')
                   OR p_reason = 'provider_reference_conflict'))
             OR (p_kind = 'm4_evidence_not_paid' AND p_reason = 'destination_mismatch')), false);
$$ LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp;

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

DROP TABLE payout_destination_park_evidence;
DROP FUNCTION payout_destination_park_evidence_guard();
