-- PRH-2 PAY-PAYOUT-UNBOUND-RESOLVE-1 (ADR 0111 revision 4 sections 4.1-4.6, 12-14;
-- owner decisions ADR 0095 section 44, decisions 9-12): the evidence-backed,
-- four-eyes manual resolution M4 of an UNBOUND payout park, on the existing K3
-- table, plus the sealed statement imports (ADR 0110 T10).
--
-- MUST RUN INSIDE A SINGLE TRANSACTION (db.MigrateUp does this).
--
-- Owner decisions 9-12 are NOT broadened: nothing here resolves automatically;
-- the park stays held until an executed four-eyes resolution (at least one
-- platform_acting approver, counted IN THE DATABASE) whose positive evidence the
-- database recomputes at request AND at `pending -> executing`; the money moves
-- only through the existing withdrawal.Complete / withdrawal.Fail postings of the
-- final approval's own transaction, behind the executing-only ledger fences.
-- Status: IMPLEMENTED against MOCK statement sources only. The evidence standard
-- itself (A-13) needs the owner's acknowledgement before any non-MOCK M4 (D-7).
--
-- What this adds / replaces (ADR 0111 section 4; built ON the 0124 bodies of the
-- four shared objects, section 7.2):
--   1.  payment_manual_resolutions: kinds m4_evidence_paid / m4_evidence_not_paid
--       (capability payment_force_resolve, D-9), the evidence columns, the M4
--       CHECKs, and the composite FK target on payment_statement_lines.
--   2.  payment_statement_imports: import_seal, seal_kid,
--       payout_lines_carry_merchant_reference, imported_by_service (S-3/S-5).
--   3.  Statement-table policies: the 0102 FOR ALL tenant_staff_scope keeps its
--       USING and gets a SYSTEM-SHAPE-ONLY WITH CHECK (the INSERT arm, S-5), plus
--       the acting SELECT policies on imports, lines and the typed reference
--       evidence (C-3 / M-5).
--   4.  payment_m4_in_scope(), payment_m4_scope_visible(), payout_m4_evidence()
--       (section 4.4; bounded 64 lines incl. the R lookups, S-4).
--   5.  payment_manual_resolution_execution_status(): the 0115 body + the
--       platform_acting approver floor for M4 (S-6), as a new OUT column
--       (DROP + CREATE: the OUT list changes).
--   6.  payment_manual_resolutions_guard(): the 0115 body + the M4 insert shape
--       (R-4 DB-forced amount/asset, C-6 pin, M-10 scope, evidence recompute) and
--       the M4 `-> executing` re-check (S-2, R-3, R-4) + the M4 governed-posting
--       exit refusal (MR042).
--   7.  payment_manual_resolutions_no_executing_commit(): MR041 per kind (C-7);
--       raises when the row is not visible at commit (as 0124 C-1).
--   8.  actor_proof_payment_manual_resolutions_guard(): the 0120 body; the K3
--       INSERT digest gains the trailing evidence_line_id field (L-1).
--   9.  The four shared objects (0124 text + the M4 additions only):
--       ledger_governed_fence_allows (+ (f), (g)), ledger_entries_governed_fence
--       (M4 lookups in the existing completed/failed shapes), the acting
--       ledger_accounts INSERT and the acting withdrawal_requests UPDATE (the M4
--       kinds added to the K3 arm ONLY; the HSEC arm and the 0124 trigger are
--       untouched - HN-4 policy/trigger split preserved).
--  10.  tenant_system_read_executed widened to the M4 kinds (section 4.6).
--
-- NOT touched (0125 builds on the 0124 bodies and leaves them):
-- financial_policy_required_approvals (M4 is the same payment_force_resolve
-- operation) and actor_proof_require (no new proof operation, section 4.5).
--
-- New SQLSTATEs in class MR:
--   MR060 the session cannot see the attempt or its whole statement scope
--         (an error, never a verdict; section 4.3)
--   MR061 the M4 evidence differs from the request / the pinned evidence
--   MR062 the M4 evidence verdict is not the kind's (insufficient/contradictory)
--   MR063 the M4 evidence read exceeds 64 lines (evidence_overflow, refused)
-- Reused: MR010 (precondition, incl. a client value for a DB-forced column),
-- MR012 (outside the section 4.1 scope), MR030, MR041, MR042, MR099, CG030.
--
-- No SECURITY DEFINER. No threshold value. No TEMP object. Every new function
-- pins search_path.

-- =========================================================================
-- 0. Pre-flight: the composite FK target on statement lines.
-- =========================================================================

ALTER TABLE payment_statement_lines ADD CONSTRAINT payment_statement_lines_id_tenant_key UNIQUE (id, tenant_id);

-- =========================================================================
-- 1. payment_manual_resolutions: kinds, evidence columns, M4 CHECKs.
-- =========================================================================

ALTER TABLE payment_manual_resolutions DROP CONSTRAINT payment_manual_resolutions_kind_check;
ALTER TABLE payment_manual_resolutions ADD CONSTRAINT payment_manual_resolutions_kind_check
    CHECK (kind IN ('m1_deposit_evidence', 'm2_declare_paid', 'm2_declare_not_paid', 'm4_evidence_paid', 'm4_evidence_not_paid'));

ALTER TABLE payment_manual_resolutions
    ADD COLUMN evidence_line_id UUID NULL,
    ADD COLUMN evidence_reference TEXT NULL,
    ADD COLUMN evidence_verdict TEXT NULL,
    ADD COLUMN evidence_import_ids UUID[] NULL,
    ADD COLUMN provider_reference_at_submission TEXT NULL;

ALTER TABLE payment_manual_resolutions
    ADD CONSTRAINT payment_manual_resolutions_evidence_line_fk
        FOREIGN KEY (evidence_line_id, tenant_id) REFERENCES payment_statement_lines (id, tenant_id),
    ADD CONSTRAINT payment_manual_resolutions_evidence_reference_check
        CHECK (evidence_reference IS NULL
               OR (octet_length(evidence_reference) BETWEEN 1 AND 255
                   AND evidence_reference !~ '[\x01-\x1F\x7F-\x9F]'
                   AND left(evidence_reference, 27) <> payment_reserved_ref_prefix())),
    -- ADR 0101 5.4 catalogue: every provider-supplied reference column carries
    -- the reserved-prefix CHECK (the pinned copy of the attempt's reference too).
    ADD CONSTRAINT payment_manual_resolutions_pinned_ref_no_reserved_prefix
        CHECK (provider_reference_at_submission IS NULL OR left(provider_reference_at_submission, 27) <> payment_reserved_ref_prefix()),
    ADD CONSTRAINT payment_manual_resolutions_evidence_verdict_check
        CHECK (evidence_verdict IS NULL OR evidence_verdict IN ('paid', 'not_paid')),
    -- M4 rows: a payout, no target, the out-of-band basis, no finding/context, no
    -- reserved id, every evidence column set (1..64 imports), the verdict the
    -- kind's own.
    ADD CONSTRAINT payment_manual_resolutions_m4_shape_check
        CHECK (kind NOT IN ('m4_evidence_paid', 'm4_evidence_not_paid')
               OR (operation = 'payout' AND target_state IS NULL AND provider_id IS NOT NULL
                   AND evidence_ref_hash IS NOT NULL AND evidence_line_id IS NOT NULL
                   AND evidence_verdict = CASE kind WHEN 'm4_evidence_paid' THEN 'paid' ELSE 'not_paid' END
                   AND evidence_import_ids IS NOT NULL
                   AND cardinality(evidence_import_ids) BETWEEN 1 AND 64
                   AND basis_code = 'provider_confirmed_out_of_band'
                   AND finding_code IS NULL AND context_code IS NULL
                   AND reserved_provider_tx_id IS NULL)),
    ADD CONSTRAINT payment_manual_resolutions_m4_reference_check
        CHECK ((kind = 'm4_evidence_paid') = (evidence_reference IS NOT NULL)),
    -- Every other kind carries no evidence column.
    ADD CONSTRAINT payment_manual_resolutions_non_m4_no_evidence_check
        CHECK (kind IN ('m4_evidence_paid', 'm4_evidence_not_paid')
               OR (evidence_line_id IS NULL AND evidence_reference IS NULL AND evidence_verdict IS NULL
                   AND evidence_import_ids IS NULL AND provider_reference_at_submission IS NULL));

-- =========================================================================
-- 2. payment_statement_imports: the Go import seal (S-5) and the source
--    declaration (S-3). Nullable seal: a legacy or unkeyed import stays
--    readable by reconciliation but is never eligible M4 evidence.
-- =========================================================================

ALTER TABLE payment_statement_imports
    ADD COLUMN import_seal TEXT NULL,
    ADD COLUMN seal_kid TEXT NULL,
    ADD COLUMN payout_lines_carry_merchant_reference BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN imported_by_service TEXT NULL;

ALTER TABLE payment_statement_imports
    ADD CONSTRAINT payment_statement_imports_import_seal_check
        CHECK (import_seal IS NULL OR import_seal ~ '^[0-9a-f]{64}$'),
    ADD CONSTRAINT payment_statement_imports_seal_kid_check
        CHECK (seal_kid IS NULL OR seal_kid ~ '^[A-Za-z0-9._-]{1,32}$'),
    ADD CONSTRAINT payment_statement_imports_seal_pair_check
        CHECK ((import_seal IS NULL) = (seal_kid IS NULL)),
    ADD CONSTRAINT payment_statement_imports_imported_by_service_check
        CHECK (imported_by_service IS NULL OR imported_by_service ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    ADD CONSTRAINT payment_statement_imports_sealed_service_check
        CHECK (import_seal IS NULL OR imported_by_service IS NOT NULL);

-- =========================================================================
-- 3. Statement-table policies.
--    (a) The 0102 FOR ALL tenant_staff_scope keeps its name and its USING (the
--        read, and the row visibility that lets the append-only triggers refuse
--        UPDATE/DELETE loudly); only its WITH CHECK - i.e. the INSERT arm - is
--        narrowed to the SYSTEM SHAPE (tenant GUC; principal, platform-admin,
--        player, platform-service and acting GUCs all NULL; S-5).
--    (b) Acting SELECT on imports, lines and the typed reference evidence
--        (reverses ADR 0101 6.4 for these three tables only; C-3, M-5).
-- =========================================================================

DROP POLICY tenant_staff_scope ON payment_statement_imports;
CREATE POLICY tenant_staff_scope ON payment_statement_imports
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    );
CREATE POLICY acting_read ON payment_statement_imports FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));

DROP POLICY tenant_staff_scope ON payment_statement_lines;
CREATE POLICY tenant_staff_scope ON payment_statement_lines
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NOT financial_acting_gucs_present()
    );
CREATE POLICY acting_read ON payment_statement_lines FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));

CREATE POLICY acting_read ON payment_attempt_reference_evidence FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));

-- =========================================================================
-- 4. The M4 scope and the deterministic evidence function (section 4.4).
-- =========================================================================

-- Section 4.1 / A-12 / C-6 / M-10: a disputed payout whose reason is
-- invalid_provider_reference, invalid_provider_reference:* or
-- provider_reference_conflict WITH NO provider reference (both kinds), or
-- destination_mismatch (not-paid only). Anything else is outside M4.
CREATE FUNCTION payment_m4_in_scope(p_kind text, p_state text, p_reason text, p_ref text) RETURNS boolean AS $$
    SELECT COALESCE(p_kind IN ('m4_evidence_paid', 'm4_evidence_not_paid')
        AND p_state = 'disputed'
        AND ((p_ref IS NULL
              AND (p_reason = 'invalid_provider_reference'
                   OR starts_with(p_reason, 'invalid_provider_reference:')
                   OR p_reason = 'provider_reference_conflict'))
             OR (p_kind = 'm4_evidence_not_paid' AND p_reason = 'destination_mismatch')), false);
$$ LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp;

-- True only in the two session shapes whose policies make EVERY input of the
-- verdict visible for p_tenant: a VALID acting session for p_tenant, or the
-- system shape (tenant GUC only). A tenant-staff session cannot see the typed
-- reference evidence (0115 R-4), so it gets MR060, never a verdict.
CREATE FUNCTION payment_m4_scope_visible(p_tenant uuid) RETURNS boolean AS $$
    SELECT p_tenant IS NOT NULL AND (
        (NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid IS NOT DISTINCT FROM p_tenant
         AND financial_acting_session_valid())
        OR (NULLIF(current_setting('app.tenant_id', true), '')::uuid IS NOT DISTINCT FROM p_tenant
            AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
            AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
            AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
            AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
            AND NOT financial_acting_gucs_present()));
$$ LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp;

-- payout_m4_evidence (section 4.4). Reads persisted kind = 'payout' lines of
-- this tenant and the attempt's provider: by the merchant reference, by the
-- bound provider reference (destination_mismatch), by every typed Y of the
-- attempt, and - for the paid verdict - by every succeeded line's reference R.
-- The 64-line bound covers EVERY line read, the R lookups included (S-4); the
-- 65th line is `evidence_overflow`, never a truncation. Eligible import = sealed
-- (import_seal present; the HMAC itself is verified in Go, the key never enters
-- the database) and (is_mock = false, or no is_mock = false import exists for
-- the tenant and provider). Unsealed lines never count as positive evidence but
-- still contradict. evidence_line_id is the earliest qualifying line by
-- (fetched_at, import_id, line_no) (L-1); import_ids = the imports of the lines
-- read, ascending.
CREATE FUNCTION payout_m4_evidence(p_tenant uuid, p_attempt uuid)
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

-- =========================================================================
-- 5. Counting at execution: the 0115 body + the M4 platform_acting floor
--    (S-6). The OUT list changes, so DROP + CREATE.
-- =========================================================================

DROP FUNCTION payment_manual_resolution_execution_status(uuid);
CREATE FUNCTION payment_manual_resolution_execution_status(p_resolution uuid,
    OUT required int, OUT counted int, OUT counted_approval_ids uuid[], OUT requester_valid boolean,
    OUT contributing_policy_ids uuid[], OUT tenant_status text, OUT enabled boolean,
    OUT platform_floor_met boolean
) AS $$
DECLARE
    r              payment_manual_resolutions%ROWTYPE;
    v_policy       RECORD;
    v_authors      uuid[];
    v_owner_person uuid;
    v_req_person   uuid;
BEGIN
    SELECT * INTO r FROM payment_manual_resolutions WHERE id = p_resolution;
    IF NOT FOUND THEN
        required := 1; counted := 0; counted_approval_ids := '{}'; requester_valid := false;
        contributing_policy_ids := '{}'; enabled := false; platform_floor_met := false;
        RETURN;
    END IF;
    SELECT * INTO v_policy FROM financial_policy_required_approvals('payment_force_resolve', r.tenant_id, r.brand_id, r.asset_code,
        CASE WHEN r.operation = 'payout' THEN r.amount ELSE NULL END, now());
    -- Never below the pinned value (as K2 3.6).
    required := GREATEST(r.required_at_submission, v_policy.required);
    contributing_policy_ids := v_policy.contributing_policy_ids;
    tenant_status := v_policy.tenant_status;
    enabled := v_policy.enabled;
    v_authors := financial_policy_author_persons(r.contributing_policy_ids || v_policy.contributing_policy_ids);

    IF r.operation = 'deposit' THEN
        SELECT pa.person_id INTO v_owner_person
          FROM deposit_intents i JOIN player_accounts pa ON pa.id = i.player_account_id AND pa.tenant_id = i.tenant_id
         WHERE i.id = r.deposit_intent_id AND i.tenant_id = r.tenant_id;
    ELSE
        SELECT pa.person_id INTO v_owner_person
          FROM withdrawal_requests w JOIN player_accounts pa ON pa.id = w.player_account_id AND pa.tenant_id = w.tenant_id
         WHERE w.id = r.withdrawal_request_id AND w.tenant_id = r.tenant_id;
    END IF;
    v_req_person := ledger_adjustment_live_person(r.requested_by, r.requested_by_person_id);

    requester_valid := v_req_person IS NOT NULL
        AND v_req_person = r.requested_by_person_id
        AND v_owner_person IS NOT NULL
        AND v_req_person <> v_owner_person
        AND NOT (v_req_person = ANY (v_authors))
        AND (tenant_status IS DISTINCT FROM 'closed' OR r.requested_by_scope = 'platform_acting')
        AND COALESCE(ledger_adjustment_eligible_grant(r.tenant_id, r.requested_by, 'payment_force_resolve:request'),
                     ledger_adjustment_invisible_platform_grant(r.tenant_id, r.requested_by, 'payment_force_resolve:request', r.requested_by_person_id)) IS NOT NULL;

    WITH cand AS (
        SELECT a.id, a.decided_at,
               ledger_adjustment_live_person(a.decided_by, a.decided_by_person_id) AS person
          FROM payment_manual_resolution_approvals a
         WHERE a.resolution_id = r.id
           AND a.decision = 'approve'
           AND a.payload_hash = r.payload_hash
           AND a.decided_by <> r.requested_by
           AND (tenant_status IS DISTINCT FROM 'closed' OR a.decided_by_scope = 'platform_acting')
           AND COALESCE(ledger_adjustment_eligible_grant(r.tenant_id, a.decided_by, 'payment_force_resolve:approve'),
                        ledger_adjustment_invisible_platform_grant(r.tenant_id, a.decided_by, 'payment_force_resolve:approve', a.decided_by_person_id)) IS NOT NULL
    ), qualified AS (
        SELECT DISTINCT ON (c.person) c.id
          FROM cand c
         WHERE c.person IS NOT NULL
           AND c.person IS DISTINCT FROM v_req_person
           AND c.person IS DISTINCT FROM v_owner_person
           AND NOT (c.person = ANY (v_authors))
         ORDER BY c.person, c.decided_at, c.id
    )
    SELECT count(*)::int, COALESCE(array_agg(q.id ORDER BY q.id), '{}') INTO counted, counted_approval_ids FROM qualified q;
    -- Never NULL: a NULL here would make the recount's NOT requester_valid test
    -- silently pass (the K2 initiator_valid lesson).
    requester_valid := COALESCE(requester_valid, false);
    -- ADR 0111 S-6 / A-14 (D-1): an M4 needs AT LEAST ONE COUNTED approval made in
    -- a platform_acting session. Every other kind: no floor (true).
    platform_floor_met := COALESCE(r.kind NOT IN ('m4_evidence_paid', 'm4_evidence_not_paid')
        OR EXISTS (SELECT 1 FROM payment_manual_resolution_approvals a
                    WHERE a.id = ANY (counted_approval_ids) AND a.decided_by_scope = 'platform_acting'), false);
END;
$$ LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp;

-- =========================================================================
-- 6. The resolution guard: the 0115 body + the M4 additions.
-- =========================================================================

CREATE OR REPLACE FUNCTION payment_manual_resolutions_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor        RECORD;
    v_att          RECORD;
    v_w            RECORD;
    v_intent       RECORD;
    v_policy       RECORD;
    v_exec         RECORD;
    v_ev           RECORD;
    v_code_type    text;
    v_found        boolean;
    v_m4           boolean;
BEGIN
    SELECT * INTO v_actor FROM financial_actor_session();

    IF TG_OP = 'INSERT' THEN
        v_m4 := NEW.kind IN ('m4_evidence_paid', 'm4_evidence_not_paid');
        -- (i) scope tenant or platform_acting only; the session tenant.
        IF v_actor.scope NOT IN ('tenant', 'platform_acting') THEN
            RAISE EXCEPTION 'payment_manual_resolutions: only a tenant or acting session may request (HD-PRH2-6)' USING ERRCODE = 'MR001';
        END IF;
        IF v_actor.person_id IS NULL THEN
            RAISE EXCEPTION 'payment_manual_resolutions: requester has no linked person_id' USING ERRCODE = 'MR002';
        END IF;
        IF NEW.tenant_id IS DISTINCT FROM v_actor.tenant THEN
            RAISE EXCEPTION 'payment_manual_resolutions: resolution tenant must be the session tenant' USING ERRCODE = 'MR001';
        END IF;
        -- (viii) server-forced id.
        IF NEW.id IS DISTINCT FROM '00000000-0000-0000-0000-000000000000'::uuid THEN
            RAISE EXCEPTION 'payment_manual_resolutions: id is server-forced and may not be supplied' USING ERRCODE = 'MR030';
        END IF;
        NEW.id := gen_random_uuid();
        -- (ii) the capability-specific grant (T-6).
        IF ledger_adjustment_eligible_grant(NEW.tenant_id, v_actor.actor, 'payment_force_resolve:request') IS NULL THEN
            RAISE EXCEPTION 'payment_manual_resolutions: requester lacks an eligible role or an in-force payment_force_resolve:request grant' USING ERRCODE = 'MR003';
        END IF;
        -- ADR 0111 R-4 / 4.5: on an M4 the amount, the asset and every evidence
        -- column but the requested line are DB-forced; a client value is refused.
        IF v_m4 AND (NEW.amount IS NOT NULL OR NEW.asset_code IS NOT NULL OR NEW.evidence_reference IS NOT NULL
                     OR NEW.evidence_verdict IS NOT NULL OR NEW.evidence_import_ids IS NOT NULL
                     OR NEW.provider_reference_at_submission IS NOT NULL) THEN
            RAISE EXCEPTION 'payment_manual_resolutions: an M4 amount, asset and evidence columns are DB-forced; a client value is refused (R-4)' USING ERRCODE = 'MR010';
        END IF;

        SELECT a.operation, a.state, a.terminal_reason, a.amount, a.asset_code, a.provider_id, a.provider_reference,
               a.ever_possibly_sent, a.deposit_intent_id, a.withdrawal_request_id INTO v_att
          FROM payment_attempts a WHERE a.id = NEW.attempt_id AND a.tenant_id = NEW.tenant_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'payment_manual_resolutions: attempt % not found in the session tenant', NEW.attempt_id USING ERRCODE = 'MR010';
        END IF;
        IF (NEW.kind = 'm1_deposit_evidence') <> (v_att.operation = 'deposit') THEN
            RAISE EXCEPTION 'payment_manual_resolutions: kind % does not fit a % attempt', NEW.kind, v_att.operation USING ERRCODE = 'MR010';
        END IF;
        NEW.operation := v_att.operation;
        NEW.amount := v_att.amount;
        NEW.asset_code := v_att.asset_code;
        NEW.attempt_state_at_submission := v_att.state;
        NEW.terminal_reason_at_submission := v_att.terminal_reason;
        NEW.ever_possibly_sent_at_submission := v_att.ever_possibly_sent;
        IF v_att.operation = 'deposit' THEN
            SELECT i.brand_id, i.player_account_id INTO v_intent
              FROM deposit_intents i WHERE i.id = v_att.deposit_intent_id AND i.tenant_id = NEW.tenant_id;
            IF NOT FOUND THEN
                RAISE EXCEPTION 'payment_manual_resolutions: deposit intent not found' USING ERRCODE = 'MR010';
            END IF;
            NEW.brand_id := v_intent.brand_id;
            NEW.deposit_intent_id := v_att.deposit_intent_id;
            NEW.withdrawal_request_id := NULL;
            NEW.provider_id := NULL;
        ELSE
            SELECT w.brand_id, w.state, w.amount, w.asset_code INTO v_w
              FROM withdrawal_requests w WHERE w.id = v_att.withdrawal_request_id AND w.tenant_id = NEW.tenant_id;
            IF NOT FOUND THEN
                RAISE EXCEPTION 'payment_manual_resolutions: withdrawal request not found' USING ERRCODE = 'MR010';
            END IF;
            NEW.brand_id := v_w.brand_id;
            NEW.withdrawal_request_id := v_att.withdrawal_request_id;
            NEW.deposit_intent_id := NULL;
            NEW.provider_id := v_att.provider_id;
        END IF;

        -- The code vocabulary (type checks by trigger).
        IF NEW.finding_code IS NOT NULL THEN
            SELECT c.code_type INTO v_code_type FROM payment_manual_resolution_codes c WHERE c.code = NEW.finding_code;
            IF v_code_type IS DISTINCT FROM 'finding' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: finding_code is not a finding code' USING ERRCODE = 'MR010';
            END IF;
        END IF;
        IF NEW.basis_code IS NOT NULL THEN
            SELECT c.code_type INTO v_code_type FROM payment_manual_resolution_codes c WHERE c.code = NEW.basis_code;
            IF v_code_type IS DISTINCT FROM 'basis' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: basis_code is not a basis code' USING ERRCODE = 'MR010';
            END IF;
        END IF;
        IF NEW.context_code IS NOT NULL THEN
            SELECT c.code_type INTO v_code_type FROM payment_manual_resolution_codes c WHERE c.code = NEW.context_code;
            IF v_code_type IS DISTINCT FROM 'context' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: context_code is not a context code' USING ERRCODE = 'MR010';
            END IF;
        END IF;

        -- The ADR 0101 5.1 preconditions (also re-run at -> executing below).
        IF NEW.kind = 'm1_deposit_evidence' THEN
            IF v_att.state <> 'disputed' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: M1 applies only to a disputed deposit (state %)', v_att.state USING ERRCODE = 'MR010';
            END IF;
        ELSIF NOT v_m4 THEN
            IF v_att.provider_id IS NULL THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the attempt has no provider' USING ERRCODE = 'MR010';
            END IF;
            -- F9: the closed allow-list (a literal set; C-5b/C-47 pin it).
            IF NOT COALESCE(v_att.state = 'ambiguous'
                            OR (v_att.state = 'disputed'
                                AND v_att.terminal_reason IN ('provider_reference_mismatch', 'success_for_never_sent_attempt')), false) THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the attempt state/dispute reason is not M2-resolvable' USING ERRCODE = 'MR012';
            END IF;
            IF v_w.state IS DISTINCT FROM 'submitted' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the withdrawal is not submitted (state %)', v_w.state USING ERRCODE = 'MR010';
            END IF;
            -- D-3: declare paid needs the reference the provider confirmed.
            IF NEW.kind = 'm2_declare_paid' AND v_att.provider_reference IS NULL THEN
                RAISE EXCEPTION 'payment_manual_resolutions: declare paid requires the attempt to hold a provider reference (D-3)' USING ERRCODE = 'MR010';
            END IF;
            -- L-3: declare not paid after a possible dispatch needs the out-of-band confirmation basis.
            IF NEW.kind = 'm2_declare_not_paid' AND v_att.ever_possibly_sent
               AND NEW.basis_code IS DISTINCT FROM 'provider_confirmed_out_of_band' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: declare not paid after a possible dispatch requires basis provider_confirmed_out_of_band (L-3)' USING ERRCODE = 'MR010';
            END IF;
        ELSE
            -- M4 (ADR 0111 4.1/4.2; C-6, M-10, R-4).
            IF v_att.provider_id IS NULL THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the attempt has no provider' USING ERRCODE = 'MR010';
            END IF;
            IF NOT payment_m4_in_scope(NEW.kind, v_att.state, v_att.terminal_reason, v_att.provider_reference) THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the attempt is outside the M4 scope (ADR 0111 4.1: unbound reason with no reference; destination_mismatch not-paid only)' USING ERRCODE = 'MR012';
            END IF;
            IF v_w.state IS DISTINCT FROM 'submitted' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the withdrawal is not submitted (state %)', v_w.state USING ERRCODE = 'MR010';
            END IF;
            IF v_w.amount IS DISTINCT FROM v_att.amount OR v_w.asset_code IS DISTINCT FROM v_att.asset_code THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the withdrawal amount/asset differ from the attempt''s (R-4)' USING ERRCODE = 'MR010';
            END IF;
            IF NEW.basis_code IS DISTINCT FROM 'provider_confirmed_out_of_band' OR NEW.context_code IS NOT NULL
               OR NEW.finding_code IS NOT NULL OR NEW.evidence_ref_hash IS NULL OR NEW.evidence_line_id IS NULL THEN
                RAISE EXCEPTION 'payment_manual_resolutions: an M4 needs basis provider_confirmed_out_of_band, the portal confirmation hash and the evidence line, and no finding/context' USING ERRCODE = 'MR010';
            END IF;
        END IF;

        NEW.requested_by := v_actor.actor;
        NEW.requested_by_scope := v_actor.scope;
        NEW.requested_by_person_id := v_actor.person_id;
        NEW.state := 'pending';
        NEW.created_at := now();
        NEW.closed_at := NULL;
        -- (vi) expires_at is DB-forced; never client-supplied.
        NEW.expires_at := now() + interval '24 hours';
        NEW.executed_txid := NULL;
        NEW.ledger_transaction_id := NULL;
        NEW.refusal_code := NULL;
        NEW.tenant_status_at_execution := NULL;
        NEW.required_at_execution := NULL;
        NEW.contributing_policy_ids_at_execution := NULL;
        NEW.target_state := CASE NEW.kind WHEN 'm2_declare_paid' THEN 'succeeded' WHEN 'm2_declare_not_paid' THEN 'declined' ELSE NULL END;
        NEW.reserved_provider_tx_id := CASE WHEN NEW.kind = 'm2_declare_paid' THEN payment_reserved_ref_prefix() || NEW.id::text ELSE NULL END;

        -- Policy (M2 by the attempt's amount and asset; M1 the base only).
        SELECT * INTO v_policy FROM financial_policy_required_approvals('payment_force_resolve', NEW.tenant_id, NEW.brand_id, NEW.asset_code,
            CASE WHEN NEW.operation = 'payout' THEN NEW.amount ELSE NULL END, now());
        NEW.tenant_status_at_submission := COALESCE(v_policy.tenant_status, 'unknown');
        IF NOT v_policy.enabled THEN
            RAISE EXCEPTION 'payment_manual_resolutions: payment_force_resolve is disabled for this tenant (no in-force platform baseline, HD-PRH2-3)' USING ERRCODE = 'MR014';
        END IF;
        -- R-5: a closed tenant admits only platform_acting actors.
        IF v_policy.tenant_status = 'closed' AND v_actor.scope <> 'platform_acting' THEN
            RAISE EXCEPTION 'payment_manual_resolutions: a closed tenant admits only platform_acting requesters (R-5)' USING ERRCODE = 'MR030';
        END IF;
        NEW.required_at_submission := v_policy.required;
        NEW.contributing_policy_ids := v_policy.contributing_policy_ids;
        -- (iii) S-2(iii).
        IF v_actor.person_id = ANY (financial_policy_author_persons(NEW.contributing_policy_ids)) THEN
            RAISE EXCEPTION 'payment_manual_resolutions: the requester authored or approved a contributing policy (S-2(iii))' USING ERRCODE = 'MR011';
        END IF;

        IF v_m4 THEN
            -- ADR 0111 4.5: the evidence is recomputed HERE (after every
            -- authorisation check above), and the requested line must be the
            -- deterministic one. C-6 / R-6: the reference is pinned.
            NEW.provider_reference_at_submission := v_att.provider_reference;
            SELECT e.verdict, e.line_id, e.reference, e.import_ids INTO v_ev FROM payout_m4_evidence(NEW.tenant_id, NEW.attempt_id) e;
            IF v_ev.verdict = 'evidence_overflow' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the M4 evidence read exceeds 64 statement lines (refused, never truncated)' USING ERRCODE = 'MR063';
            END IF;
            IF v_ev.verdict IS DISTINCT FROM (CASE NEW.kind WHEN 'm4_evidence_paid' THEN 'paid' ELSE 'not_paid' END) THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the M4 evidence verdict is % (the kind needs a positive verdict)', COALESCE(v_ev.verdict, '<none>') USING ERRCODE = 'MR062';
            END IF;
            IF v_ev.line_id IS DISTINCT FROM NEW.evidence_line_id THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the requested evidence line is not the deterministic evidence line' USING ERRCODE = 'MR061';
            END IF;
            NEW.evidence_verdict := v_ev.verdict;
            NEW.evidence_reference := v_ev.reference;
            NEW.evidence_import_ids := v_ev.import_ids;
            NEW.payload_hash := k2_sha256_hex(k2_canonical(
                NEW.tenant_id::text, NEW.attempt_id::text, NEW.kind, NEW.target_state, NEW.finding_code, NEW.basis_code,
                NEW.context_code, NEW.evidence_ref_hash, NEW.amount::text, NEW.asset_code, NEW.reason_code,
                NEW.attempt_state_at_submission, NEW.terminal_reason_at_submission,
                NEW.evidence_line_id::text, NEW.evidence_reference, NEW.evidence_verdict,
                array_to_string(NEW.evidence_import_ids, ','), NEW.provider_reference_at_submission));
            RETURN NEW;
        END IF;

        NEW.payload_hash := k2_sha256_hex(k2_canonical(
            NEW.tenant_id::text, NEW.attempt_id::text, NEW.kind, NEW.target_state, NEW.finding_code, NEW.basis_code,
            NEW.context_code, NEW.evidence_ref_hash, NEW.amount::text, NEW.asset_code, NEW.reason_code,
            NEW.attempt_state_at_submission, NEW.terminal_reason_at_submission));
        RETURN NEW;
    END IF;

    -- UPDATE ----------------------------------------------------------------
    v_m4 := OLD.kind IN ('m4_evidence_paid', 'm4_evidence_not_paid');
    IF OLD.state NOT IN ('pending', 'executing') THEN
        RAISE EXCEPTION 'payment_manual_resolutions: resolution % is terminal (%)', OLD.id, OLD.state USING ERRCODE = 'MR030';
    END IF;
    -- The payload, actor and pins are immutable (LF-10): only the state columns may change.
    IF (to_jsonb(NEW) - ARRAY['state', 'executed_txid', 'ledger_transaction_id', 'refusal_code', 'closed_at',
                              'tenant_status_at_execution', 'required_at_execution', 'contributing_policy_ids_at_execution'])
       IS DISTINCT FROM
       (to_jsonb(OLD) - ARRAY['state', 'executed_txid', 'ledger_transaction_id', 'refusal_code', 'closed_at',
                              'tenant_status_at_execution', 'required_at_execution', 'contributing_policy_ids_at_execution']) THEN
        RAISE EXCEPTION 'payment_manual_resolutions: the payload, actor and pinned policy are immutable' USING ERRCODE = 'MR030';
    END IF;
    IF NEW.payload_hash IS DISTINCT FROM (CASE WHEN v_m4 THEN k2_sha256_hex(k2_canonical(
            NEW.tenant_id::text, NEW.attempt_id::text, NEW.kind, NEW.target_state, NEW.finding_code, NEW.basis_code,
            NEW.context_code, NEW.evidence_ref_hash, NEW.amount::text, NEW.asset_code, NEW.reason_code,
            NEW.attempt_state_at_submission, NEW.terminal_reason_at_submission,
            NEW.evidence_line_id::text, NEW.evidence_reference, NEW.evidence_verdict,
            array_to_string(NEW.evidence_import_ids, ','), NEW.provider_reference_at_submission))
        ELSE k2_sha256_hex(k2_canonical(
            NEW.tenant_id::text, NEW.attempt_id::text, NEW.kind, NEW.target_state, NEW.finding_code, NEW.basis_code,
            NEW.context_code, NEW.evidence_ref_hash, NEW.amount::text, NEW.asset_code, NEW.reason_code,
            NEW.attempt_state_at_submission, NEW.terminal_reason_at_submission)) END) THEN
        RAISE EXCEPTION 'payment_manual_resolutions: payload_hash does not match the payload' USING ERRCODE = 'MR030';
    END IF;
    IF NEW.state IN ('executed', 'refused_at_execution', 'rejected', 'cancelled', 'expired') THEN
        NEW.closed_at := now();
    END IF;

    IF OLD.state = 'pending' AND NEW.state = 'cancelled' THEN
        -- (vii) only the requester may cancel. O-K1: no governed-posting check
        -- here (a wr.id:failed posting written by evidence after submission
        -- must not make a pending M2 uncancellable).
        IF v_actor.actor IS DISTINCT FROM OLD.requested_by THEN
            RAISE EXCEPTION 'payment_manual_resolutions: only the requester may cancel' USING ERRCODE = 'MR030';
        END IF;
        NEW.executed_txid := NULL; NEW.ledger_transaction_id := NULL; NEW.refusal_code := NULL;
        RETURN NEW;
    ELSIF OLD.state = 'pending' AND NEW.state = 'expired' THEN
        IF now() < OLD.expires_at THEN
            RAISE EXCEPTION 'payment_manual_resolutions: resolution has not expired' USING ERRCODE = 'MR030';
        END IF;
        NEW.executed_txid := NULL; NEW.ledger_transaction_id := NULL; NEW.refusal_code := NULL;
        RETURN NEW;
    ELSIF OLD.state = 'pending' AND NEW.state = 'rejected' THEN
        IF NOT EXISTS (SELECT 1 FROM payment_manual_resolution_approvals a
                        WHERE a.resolution_id = OLD.id AND a.decision = 'reject' AND a.decided_txid = txid_current()) THEN
            RAISE EXCEPTION 'payment_manual_resolutions: rejected only via a same-transaction reject decision' USING ERRCODE = 'MR030';
        END IF;
        NEW.executed_txid := NULL; NEW.ledger_transaction_id := NULL; NEW.refusal_code := NULL;
        RETURN NEW;
    ELSIF OLD.state = 'pending' AND NEW.state IN ('refused_at_execution', 'executing') THEN
        -- (vii) only in the final approval's own transaction (LF-13).
        IF NOT EXISTS (SELECT 1 FROM payment_manual_resolution_approvals a
                        WHERE a.resolution_id = OLD.id AND a.decision = 'approve' AND a.decided_txid = txid_current()) THEN
            RAISE EXCEPTION 'payment_manual_resolutions: execution happens only in the final approval''s transaction' USING ERRCODE = 'MR030';
        END IF;
        IF now() >= OLD.expires_at THEN
            RAISE EXCEPTION 'payment_manual_resolutions: resolution has expired' USING ERRCODE = 'MR030';
        END IF;
        IF NEW.state = 'refused_at_execution' THEN
            IF NEW.refusal_code IS NULL THEN
                RAISE EXCEPTION 'payment_manual_resolutions: refused_at_execution needs a refusal_code' USING ERRCODE = 'MR030';
            END IF;
            NEW.executed_txid := NULL; NEW.ledger_transaction_id := NULL;
            RETURN NEW;
        END IF;
        -- -> executing: the DB recounts with the SAME function the executor
        -- used (R-3 (iv); K3-S2) and re-runs the preconditions (R-6 pins).
        -- ADR 0111 S-6: the M4 platform_acting approver floor is part of the recount.
        SELECT * INTO v_exec FROM payment_manual_resolution_execution_status(OLD.id);
        IF NOT v_exec.enabled OR NOT v_exec.requester_valid OR v_exec.counted < v_exec.required
           OR NOT v_exec.platform_floor_met THEN
            RAISE EXCEPTION 'payment_manual_resolutions: fewer counted approvals (%) than required (%), the requester no longer qualifies, or no counted platform_acting approval on an M4 (K3-S2, S-6)',
                v_exec.counted, v_exec.required USING ERRCODE = 'MR030';
        END IF;
        SELECT a.operation, a.state, a.terminal_reason, a.provider_id, a.provider_reference, a.ever_possibly_sent,
               a.withdrawal_request_id, a.amount, a.asset_code INTO v_att
          FROM payment_attempts a WHERE a.id = OLD.attempt_id AND a.tenant_id = OLD.tenant_id;
        IF NOT FOUND
           OR v_att.state IS DISTINCT FROM OLD.attempt_state_at_submission
           OR v_att.terminal_reason IS DISTINCT FROM OLD.terminal_reason_at_submission THEN
            RAISE EXCEPTION 'payment_manual_resolutions: the attempt state or dispute reason changed since submission (R-6)' USING ERRCODE = 'MR030';
        END IF;
        IF OLD.kind = 'm1_deposit_evidence' THEN
            IF v_att.state <> 'disputed' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: M1 applies only to a disputed deposit' USING ERRCODE = 'MR010';
            END IF;
        ELSIF NOT v_m4 THEN
            IF v_att.provider_id IS DISTINCT FROM OLD.provider_id THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the attempt provider changed' USING ERRCODE = 'MR010';
            END IF;
            IF NOT COALESCE(v_att.state = 'ambiguous'
                            OR (v_att.state = 'disputed'
                                AND v_att.terminal_reason IN ('provider_reference_mismatch', 'success_for_never_sent_attempt')), false) THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the attempt state/dispute reason is not M2-resolvable' USING ERRCODE = 'MR012';
            END IF;
            SELECT w.state INTO v_w FROM withdrawal_requests w WHERE w.id = OLD.withdrawal_request_id AND w.tenant_id = OLD.tenant_id;
            IF NOT FOUND OR v_w.state <> 'submitted' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the withdrawal is not submitted' USING ERRCODE = 'MR010';
            END IF;
            IF OLD.kind = 'm2_declare_paid' AND v_att.provider_reference IS NULL THEN
                RAISE EXCEPTION 'payment_manual_resolutions: declare paid requires a provider reference (D-3)' USING ERRCODE = 'MR010';
            END IF;
            IF OLD.kind = 'm2_declare_not_paid' AND v_att.ever_possibly_sent
               AND OLD.basis_code IS DISTINCT FROM 'provider_confirmed_out_of_band' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: declare not paid after a possible dispatch requires provider_confirmed_out_of_band (L-3)' USING ERRCODE = 'MR010';
            END IF;
        ELSE
            -- M4 (ADR 0111 4.5 step 8; S-2, R-3, R-4): Go is the first check,
            -- not the only one.
            IF v_att.provider_id IS DISTINCT FROM OLD.provider_id THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the attempt provider changed' USING ERRCODE = 'MR010';
            END IF;
            IF NOT payment_m4_in_scope(OLD.kind, v_att.state, v_att.terminal_reason, v_att.provider_reference)
               OR v_att.provider_reference IS DISTINCT FROM OLD.provider_reference_at_submission THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the attempt left the M4 scope or its reference changed since submission (C-6, R-3)' USING ERRCODE = 'MR012';
            END IF;
            SELECT w.state, w.amount, w.asset_code INTO v_w FROM withdrawal_requests w WHERE w.id = OLD.withdrawal_request_id AND w.tenant_id = OLD.tenant_id;
            IF NOT FOUND OR v_w.state <> 'submitted' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the withdrawal is not submitted' USING ERRCODE = 'MR010';
            END IF;
            IF v_w.amount IS DISTINCT FROM OLD.amount OR v_w.asset_code IS DISTINCT FROM OLD.asset_code
               OR v_att.amount IS DISTINCT FROM OLD.amount OR v_att.asset_code IS DISTINCT FROM OLD.asset_code THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the withdrawal/attempt amount or asset differ from the resolution''s (R-4)' USING ERRCODE = 'MR010';
            END IF;
            SELECT e.verdict, e.line_id, e.reference, e.import_ids INTO v_ev FROM payout_m4_evidence(OLD.tenant_id, OLD.attempt_id) e;
            IF v_ev.verdict IS DISTINCT FROM OLD.evidence_verdict OR v_ev.line_id IS DISTINCT FROM OLD.evidence_line_id
               OR v_ev.reference IS DISTINCT FROM OLD.evidence_reference OR v_ev.import_ids IS DISTINCT FROM OLD.evidence_import_ids THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the M4 evidence changed since submission (S-2)' USING ERRCODE = 'MR061';
            END IF;
        END IF;
        NEW.required_at_execution := v_exec.required;
        NEW.contributing_policy_ids_at_execution := v_exec.contributing_policy_ids;
        NEW.tenant_status_at_execution := v_exec.tenant_status;
        NEW.executed_txid := txid_current();
        NEW.ledger_transaction_id := NULL;
        NEW.refusal_code := NULL;
        RETURN NEW;
    ELSIF OLD.state = 'executing' AND NEW.state = 'executed' THEN
        IF OLD.executed_txid IS DISTINCT FROM txid_current() OR NEW.executed_txid IS DISTINCT FROM OLD.executed_txid THEN
            RAISE EXCEPTION 'payment_manual_resolutions: executing -> executed only in the executing transaction' USING ERRCODE = 'MR030';
        END IF;
        IF OLD.operation = 'payout' AND NEW.ledger_transaction_id IS NULL THEN
            RAISE EXCEPTION 'payment_manual_resolutions: an executed M2 needs its ledger_transaction_id' USING ERRCODE = 'MR041';
        END IF;
        IF OLD.operation = 'deposit' AND NEW.ledger_transaction_id IS NOT NULL THEN
            RAISE EXCEPTION 'payment_manual_resolutions: M1 never links a ledger transaction (LF-1)' USING ERRCODE = 'MR041';
        END IF;
        RETURN NEW;
    ELSIF OLD.state = 'executing' AND NEW.state = 'refused_at_execution' THEN
        -- R-3 (v) / O-K1: a transition OUT OF executing other than executed is
        -- refused once a governed posting exists (the reserved-key posting, or
        -- the wr.id:failed posting of this withdrawal; for M4 the provider:R
        -- completion or the wr.id:failed posting).
        IF OLD.executed_txid IS DISTINCT FROM txid_current() THEN
            RAISE EXCEPTION 'payment_manual_resolutions: executing -> % only in the executing transaction', NEW.state USING ERRCODE = 'MR030';
        END IF;
        IF NEW.refusal_code IS NULL THEN
            RAISE EXCEPTION 'payment_manual_resolutions: refused_at_execution needs a refusal_code' USING ERRCODE = 'MR030';
        END IF;
        IF EXISTS (SELECT 1 FROM ledger_transactions t
                    WHERE t.tenant_id = OLD.tenant_id
                      AND ((OLD.kind = 'm2_declare_paid' AND t.idempotency_key = OLD.provider_id || ':' || OLD.reserved_provider_tx_id)
                        OR (OLD.kind = 'm4_evidence_paid' AND t.idempotency_key = OLD.provider_id || ':' || OLD.evidence_reference)
                        OR (OLD.kind IN ('m2_declare_not_paid', 'm4_evidence_not_paid') AND t.idempotency_key = OLD.withdrawal_request_id::text || ':failed'
                            AND t.correlation_id = OLD.withdrawal_request_id))) THEN
            RAISE EXCEPTION 'payment_manual_resolutions: % refused - a governed ledger transaction exists', NEW.state USING ERRCODE = 'MR042';
        END IF;
        NEW.ledger_transaction_id := NULL;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'payment_manual_resolutions: invalid transition % -> %', OLD.state, NEW.state USING ERRCODE = 'MR030';
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

-- =========================================================================
-- 7. MR041, per kind (C-7). The 0115 M1/M2 rules are unchanged; M4 adds its
--    own. A row the committing session cannot see now RAISES (as 0124 C-1).
-- =========================================================================

CREATE OR REPLACE FUNCTION payment_manual_resolutions_no_executing_commit() RETURNS TRIGGER AS $$
DECLARE
    r       payment_manual_resolutions%ROWTYPE;
    v_a     RECORD;
    v_w     RECORD;
    v_tx    RECORD;
    v_n     int;
    v_ok    int;
BEGIN
    SELECT * INTO r FROM payment_manual_resolutions WHERE id = NEW.id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'payment_manual_resolutions: resolution % is not visible to the committing session (fail closed)', NEW.id USING ERRCODE = 'MR041';
    END IF;
    IF r.state = 'executing' THEN
        RAISE EXCEPTION 'payment_manual_resolutions: resolution % may not commit in state executing', NEW.id USING ERRCODE = 'MR041';
    END IF;
    IF r.state = 'executed' AND r.operation = 'payout' THEN
        SELECT a.state, a.terminal_reason, a.provider_reference INTO v_a FROM payment_attempts a WHERE a.id = r.attempt_id AND a.tenant_id = r.tenant_id;
        SELECT w.id, w.state, w.wallet_id, w.release_ledger_transaction_id INTO v_w
          FROM withdrawal_requests w WHERE w.id = r.withdrawal_request_id AND w.tenant_id = r.tenant_id;
        IF r.kind IN ('m2_declare_paid', 'm2_declare_not_paid') THEN
            IF v_a.state IS DISTINCT FROM r.target_state
               OR v_w.state IS DISTINCT FROM (CASE r.kind WHEN 'm2_declare_paid' THEN 'completed' ELSE 'failed' END)
               OR v_w.release_ledger_transaction_id IS DISTINCT FROM r.ledger_transaction_id THEN
                RAISE EXCEPTION 'payment_manual_resolutions: executed M2 % does not match its attempt/withdrawal outcome', NEW.id USING ERRCODE = 'MR041';
            END IF;
        ELSE
            -- M4: the attempt stays exactly as pinned (A-15, R-3); the withdrawal
            -- is completed / failed and links this resolution's posting.
            IF v_a.state IS DISTINCT FROM r.attempt_state_at_submission
               OR v_a.terminal_reason IS DISTINCT FROM r.terminal_reason_at_submission
               OR v_a.provider_reference IS DISTINCT FROM r.provider_reference_at_submission
               OR v_w.state IS DISTINCT FROM (CASE r.kind WHEN 'm4_evidence_paid' THEN 'completed' ELSE 'failed' END)
               OR v_w.release_ledger_transaction_id IS DISTINCT FROM r.ledger_transaction_id THEN
                RAISE EXCEPTION 'payment_manual_resolutions: executed M4 % does not match its attempt/withdrawal outcome', NEW.id USING ERRCODE = 'MR041';
            END IF;
        END IF;
        SELECT t.transaction_type, t.idempotency_key, t.correlation_id, t.provider_id, t.provider_tx_id INTO v_tx
          FROM ledger_transactions t WHERE t.id = r.ledger_transaction_id AND t.tenant_id = r.tenant_id;
        IF NOT FOUND
           OR v_tx.correlation_id IS DISTINCT FROM r.withdrawal_request_id
           OR NOT COALESCE((r.kind = 'm2_declare_paid' AND v_tx.transaction_type = 'withdrawal_completed'
                    AND v_tx.idempotency_key = r.provider_id || ':' || r.reserved_provider_tx_id
                    AND v_tx.provider_id = r.provider_id AND v_tx.provider_tx_id = r.reserved_provider_tx_id)
                OR (r.kind = 'm2_declare_not_paid' AND v_tx.transaction_type = 'withdrawal_failed'
                    AND v_tx.idempotency_key = r.withdrawal_request_id::text || ':failed'
                    AND v_tx.provider_id IS NULL AND v_tx.provider_tx_id IS NULL)
                OR (r.kind = 'm4_evidence_paid' AND v_tx.transaction_type = 'withdrawal_completed'
                    AND v_tx.idempotency_key = r.provider_id || ':' || r.evidence_reference
                    AND v_tx.provider_id = r.provider_id AND v_tx.provider_tx_id = r.evidence_reference)
                OR (r.kind = 'm4_evidence_not_paid' AND v_tx.transaction_type = 'withdrawal_failed'
                    AND v_tx.idempotency_key = r.withdrawal_request_id::text || ':failed'
                    AND v_tx.provider_id IS NULL AND v_tx.provider_tx_id IS NULL), false) THEN
            RAISE EXCEPTION 'payment_manual_resolutions: executed resolution % links a ledger transaction that does not carry its keys', NEW.id USING ERRCODE = 'MR041';
        END IF;
        SELECT count(*) INTO v_n FROM ledger_entries e WHERE e.ledger_transaction_id = r.ledger_transaction_id;
        SELECT count(*) INTO v_ok
          FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
         WHERE e.ledger_transaction_id = r.ledger_transaction_id
           AND e.tenant_id = r.tenant_id AND e.asset_code = r.asset_code AND e.amount = r.amount
           AND ((la.account_type = 'player_withdrawal_hold' AND la.wallet_id = v_w.wallet_id AND e.direction = 'debit')
             OR (r.kind IN ('m2_declare_paid', 'm4_evidence_paid') AND la.account_type = 'psp_clearing' AND la.wallet_id IS NULL AND e.direction = 'credit')
             OR (r.kind IN ('m2_declare_not_paid', 'm4_evidence_not_paid') AND la.account_type = 'player_cash' AND la.wallet_id = v_w.wallet_id AND e.direction = 'credit'));
        IF v_n <> 2 OR v_ok <> 2 THEN
            RAISE EXCEPTION 'payment_manual_resolutions: executed resolution % entries are not exactly the approved two-leg shape', NEW.id USING ERRCODE = 'MR041';
        END IF;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

-- =========================================================================
-- 8. ADR 0110 K3 digest (L-1): the 0120 body; the INSERT digest gains the
--    trailing evidence_line_id ('~' for M1/M2). Go changes in lockstep.
-- =========================================================================

CREATE OR REPLACE FUNCTION actor_proof_payment_manual_resolutions_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor RECORD;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO v_actor FROM financial_actor_session();
        IF NEW.requested_by IS DISTINCT FROM v_actor.actor THEN
            RAISE EXCEPTION 'actor_proof: the requested_by column is not the proven actor' USING ERRCODE = 'AP004';
        END IF;
        -- The id is server-forced by the 0115 guard, so the target is the
        -- literal 'new'; the digest binds the caller-supplied payload columns
        -- (ADR 0111 L-1: + evidence_line_id, '~' when NULL).
        PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
            'payment_force_resolve:request', 'new',
            k2_sha256_hex(k2_canonical(NEW.tenant_id::text, NEW.attempt_id::text, NEW.kind, NEW.finding_code,
                NEW.basis_code, NEW.context_code, NEW.evidence_ref_hash, NEW.reason_code, NEW.evidence_line_id::text)));
    ELSIF OLD.state = 'pending' AND NEW.state = 'cancelled' THEN
        SELECT * INTO v_actor FROM financial_actor_session();
        IF v_actor.actor IS DISTINCT FROM OLD.requested_by THEN
            RAISE EXCEPTION 'actor_proof: only the requester may cancel' USING ERRCODE = 'AP004';
        END IF;
        PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
            'payment_force_resolve:cancel', OLD.id::text, OLD.payload_hash);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

-- =========================================================================
-- 9. THE FOUR SHARED OBJECTS, replaced in place: the 0124 text + the marked
--    M4 additions only. 0125's down restores the 0124 text byte for byte.
-- =========================================================================

-- (1/4) ledger_governed_fence_allows: 0124 (a)+(b)+(c)+(e) verbatim + (f), (g).
CREATE OR REPLACE FUNCTION ledger_governed_fence_allows(
    p_tenant uuid, p_type text, p_idempotency_key text, p_correlation uuid, p_provider_id text, p_provider_tx_id text
) RETURNS boolean AS $$
    SELECT (p_type = 'manual_adjustment' AND EXISTS (
        SELECT 1 FROM ledger_adjustment_requests r
         WHERE r.tenant_id = p_tenant
           AND p_idempotency_key = 'manual_adjustment:' || r.id::text
           AND p_correlation = r.id
           AND r.state = 'executing' AND r.executed_txid = txid_current()))
        -- (b) M2 declare paid
        OR (p_type = 'withdrawal_completed' AND EXISTS (
        SELECT 1 FROM payment_manual_resolutions m
         WHERE m.tenant_id = p_tenant AND m.kind = 'm2_declare_paid'
           AND m.state = 'executing' AND m.executed_txid = txid_current()
           AND p_correlation = m.withdrawal_request_id
           AND p_provider_id = m.provider_id
           AND p_provider_tx_id = m.reserved_provider_tx_id
           AND p_idempotency_key = m.provider_id || ':' || m.reserved_provider_tx_id))
        -- (c) M2 declare not paid
        OR (p_type = 'withdrawal_failed' AND EXISTS (
        SELECT 1 FROM payment_manual_resolutions m
         WHERE m.tenant_id = p_tenant AND m.kind = 'm2_declare_not_paid'
           AND m.state = 'executing' AND m.executed_txid = txid_current()
           AND p_correlation = m.withdrawal_request_id
           AND p_idempotency_key = m.withdrawal_request_id::text || ':failed'))
        -- (e) HSEC release_hold_to_player (ADR 0111 6.4): the exact key and correlation
        OR (p_type = 'withdrawal_rejected' AND EXISTS (
        SELECT 1 FROM withdrawal_hold_resolutions h
         WHERE h.tenant_id = p_tenant AND h.kind = 'release_hold_to_player'
           AND h.state = 'executing' AND h.executed_txid = txid_current()
           AND p_correlation = h.withdrawal_request_id
           AND p_idempotency_key = h.withdrawal_request_id::text || ':governed_hold_released'))
        -- (f) M4 evidence paid (ADR 0111 4.5, C-7): keyed by the evidenced PSP reference R
        OR (p_type = 'withdrawal_completed' AND EXISTS (
        SELECT 1 FROM payment_manual_resolutions m
         WHERE m.tenant_id = p_tenant AND m.kind = 'm4_evidence_paid'
           AND m.state = 'executing' AND m.executed_txid = txid_current()
           AND p_correlation = m.withdrawal_request_id
           AND p_provider_id = m.provider_id
           AND p_provider_tx_id = m.evidence_reference
           AND p_idempotency_key = m.provider_id || ':' || m.evidence_reference))
        -- (g) M4 evidence not paid
        OR (p_type = 'withdrawal_failed' AND EXISTS (
        SELECT 1 FROM payment_manual_resolutions m
         WHERE m.tenant_id = p_tenant AND m.kind = 'm4_evidence_not_paid'
           AND m.state = 'executing' AND m.executed_txid = txid_current()
           AND p_correlation = m.withdrawal_request_id
           AND p_idempotency_key = m.withdrawal_request_id::text || ':failed'));
$$ LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp;

-- (2/4) ledger_entries_governed_fence: the 0124 body; the completed/failed
-- lookup also admits the executing M4 of the same shape (keyed per kind).
CREATE OR REPLACE FUNCTION ledger_entries_governed_fence() RETURNS TRIGGER AS $$
DECLARE
    v_tx          RECORD;
    v_req         RECORD;
    v_m           RECORD;
    v_h           RECORD;
    v_acct        RECORD;
    v_player_dir  text;
    v_house_dir   text;
BEGIN
    IF financial_acting_gucs_present() THEN
        SELECT t.tenant_id, t.transaction_type, t.idempotency_key, t.correlation_id, t.provider_id, t.provider_tx_id INTO v_tx
          FROM ledger_transactions t WHERE t.id = NEW.ledger_transaction_id AND t.tenant_id = NEW.tenant_id;
        IF NOT FOUND OR NOT ledger_governed_fence_allows(v_tx.tenant_id, v_tx.transaction_type, v_tx.idempotency_key, v_tx.correlation_id,
                                                         v_tx.provider_id, v_tx.provider_tx_id) THEN
            RAISE EXCEPTION 'ledger_entries_governed_fence: an acting session may add entries only to a governed, executing request''s transaction' USING ERRCODE = 'CG030';
        END IF;
        IF v_tx.transaction_type = 'manual_adjustment' THEN
        SELECT r.wallet_id, r.asset_code, r.amount, r.direction INTO v_req
          FROM ledger_adjustment_requests r
         WHERE r.id = v_tx.correlation_id AND r.tenant_id = v_tx.tenant_id
           AND v_tx.idempotency_key = 'manual_adjustment:' || r.id::text;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'ledger_entries_governed_fence: governed request not found' USING ERRCODE = 'CG030';
        END IF;
        v_player_dir := CASE WHEN v_req.direction = 'credit_player' THEN 'credit' ELSE 'debit' END;
        v_house_dir  := CASE WHEN v_req.direction = 'credit_player' THEN 'debit' ELSE 'credit' END;
        SELECT la.account_type, la.wallet_id INTO v_acct
          FROM ledger_accounts la WHERE la.id = NEW.ledger_account_id AND la.tenant_id = NEW.tenant_id;
        IF NOT FOUND
           OR NEW.asset_code IS DISTINCT FROM v_req.asset_code
           OR NEW.amount IS DISTINCT FROM v_req.amount
           OR NOT ((v_acct.account_type = 'player_cash' AND v_acct.wallet_id = v_req.wallet_id AND NEW.direction = v_player_dir)
                OR (v_acct.account_type = 'manual_adjustment' AND v_acct.wallet_id IS NULL AND NEW.direction = v_house_dir)) THEN
            RAISE EXCEPTION 'ledger_entries_governed_fence: the entry is not a leg of the approved §4 shape (account, asset, amount or direction)' USING ERRCODE = 'CG030';
        END IF;
        ELSIF v_tx.transaction_type IN ('withdrawal_completed', 'withdrawal_failed') THEN
            -- (b) declare paid / (c) declare not paid: the resolution, the
            -- withdrawal and the entry, all in this transaction.
            -- (f) / (g) M4 (ADR 0111 4.5): the same two-leg shapes, the
            -- completion keyed by the evidenced reference R.
            SELECT m.amount AS m_amount, w.wallet_id, w.asset_code, w.amount AS w_amount INTO v_m
              FROM payment_manual_resolutions m
              JOIN withdrawal_requests w ON w.id = m.withdrawal_request_id AND w.tenant_id = m.tenant_id
             WHERE m.tenant_id = v_tx.tenant_id
               AND m.state = 'executing' AND m.executed_txid = txid_current()
               AND m.withdrawal_request_id = v_tx.correlation_id
               AND ((m.kind = CASE v_tx.transaction_type WHEN 'withdrawal_completed' THEN 'm2_declare_paid' ELSE 'm2_declare_not_paid' END
                     AND v_tx.idempotency_key = CASE v_tx.transaction_type
                             WHEN 'withdrawal_completed' THEN m.provider_id || ':' || m.reserved_provider_tx_id
                             ELSE m.withdrawal_request_id::text || ':failed' END)
                 OR (m.kind = 'm4_evidence_paid' AND v_tx.transaction_type = 'withdrawal_completed'
                     AND v_tx.provider_tx_id = m.evidence_reference
                     AND v_tx.idempotency_key = m.provider_id || ':' || m.evidence_reference)
                 OR (m.kind = 'm4_evidence_not_paid' AND v_tx.transaction_type = 'withdrawal_failed'
                     AND v_tx.idempotency_key = m.withdrawal_request_id::text || ':failed'));
            IF NOT FOUND THEN
                RAISE EXCEPTION 'ledger_entries_governed_fence: governed M2 resolution not found' USING ERRCODE = 'CG030';
            END IF;
            SELECT la.account_type, la.wallet_id INTO v_acct
              FROM ledger_accounts la WHERE la.id = NEW.ledger_account_id AND la.tenant_id = NEW.tenant_id;
            IF NOT FOUND
               OR NEW.asset_code IS DISTINCT FROM v_m.asset_code
               OR NEW.amount IS DISTINCT FROM v_m.w_amount
               OR NEW.amount IS DISTINCT FROM v_m.m_amount
               OR NOT COALESCE((v_acct.account_type = 'player_withdrawal_hold' AND v_acct.wallet_id = v_m.wallet_id AND NEW.direction = 'debit')
                    OR (v_tx.transaction_type = 'withdrawal_completed' AND v_acct.account_type = 'psp_clearing'
                        AND v_acct.wallet_id IS NULL AND NEW.direction = 'credit')
                    OR (v_tx.transaction_type = 'withdrawal_failed' AND v_acct.account_type = 'player_cash'
                        AND v_acct.wallet_id = v_m.wallet_id AND NEW.direction = 'credit'), false) THEN
                RAISE EXCEPTION 'ledger_entries_governed_fence: the entry is not a leg of the approved M2 shape (account, asset, amount or direction)' USING ERRCODE = 'CG030';
            END IF;
        ELSIF v_tx.transaction_type = 'withdrawal_rejected' THEN
            -- (e) HSEC release_hold_to_player: hold debit and player_cash credit,
            -- both on the withdrawal's wallet, in the resolution's asset and amount.
            SELECT h.amount AS h_amount, w.wallet_id, w.asset_code, w.amount AS w_amount INTO v_h
              FROM withdrawal_hold_resolutions h
              JOIN withdrawal_requests w ON w.id = h.withdrawal_request_id AND w.tenant_id = h.tenant_id
             WHERE h.tenant_id = v_tx.tenant_id AND h.kind = 'release_hold_to_player'
               AND h.state = 'executing' AND h.executed_txid = txid_current()
               AND h.withdrawal_request_id = v_tx.correlation_id
               AND v_tx.idempotency_key = h.withdrawal_request_id::text || ':governed_hold_released';
            IF NOT FOUND THEN
                RAISE EXCEPTION 'ledger_entries_governed_fence: governed hold resolution not found' USING ERRCODE = 'CG030';
            END IF;
            SELECT la.account_type, la.wallet_id INTO v_acct
              FROM ledger_accounts la WHERE la.id = NEW.ledger_account_id AND la.tenant_id = NEW.tenant_id;
            IF NOT FOUND
               OR NEW.asset_code IS DISTINCT FROM v_h.asset_code
               OR NEW.amount IS DISTINCT FROM v_h.w_amount
               OR NEW.amount IS DISTINCT FROM v_h.h_amount
               OR NOT COALESCE((v_acct.account_type = 'player_withdrawal_hold' AND v_acct.wallet_id = v_h.wallet_id AND NEW.direction = 'debit')
                    OR (v_acct.account_type = 'player_cash' AND v_acct.wallet_id = v_h.wallet_id AND NEW.direction = 'credit'), false) THEN
                RAISE EXCEPTION 'ledger_entries_governed_fence: the entry is not a leg of the approved hold-release shape (account, asset, amount or direction)' USING ERRCODE = 'CG030';
            END IF;
        ELSE
            RAISE EXCEPTION 'ledger_entries_governed_fence: no governed shape for transaction type %', v_tx.transaction_type USING ERRCODE = 'CG030';
        END IF;
        IF EXISTS (SELECT 1 FROM ledger_entries e
                    WHERE e.ledger_transaction_id = NEW.ledger_transaction_id
                      AND (e.direction = NEW.direction
                           OR (SELECT count(*) FROM ledger_entries e2 WHERE e2.ledger_transaction_id = NEW.ledger_transaction_id) >= 2)) THEN
            RAISE EXCEPTION 'ledger_entries_governed_fence: the linked transaction already holds this leg (at most two entries, one per direction)' USING ERRCODE = 'CG030';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

-- (3/4) the acting ledger_accounts INSERT policy: the 0124 text with the M4 kinds
-- added to the K3 hold / psp_clearing arms (the HSEC arm unchanged).
DROP POLICY acting_insert ON ledger_accounts;
CREATE POLICY acting_insert ON ledger_accounts FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
                AND (SELECT financial_acting_session_valid())
                AND (account_type IN ('player_cash', 'manual_adjustment')
                     OR (account_type = 'player_withdrawal_hold'
                         AND EXISTS (SELECT 1 FROM payment_manual_resolutions m
                                       JOIN withdrawal_requests w ON w.id = m.withdrawal_request_id AND w.tenant_id = m.tenant_id
                                      WHERE m.tenant_id = ledger_accounts.tenant_id
                                        AND m.kind IN ('m2_declare_paid', 'm2_declare_not_paid', 'm4_evidence_paid', 'm4_evidence_not_paid')
                                        AND m.state = 'executing' AND m.executed_txid = txid_current()
                                        AND w.wallet_id = ledger_accounts.wallet_id
                                        AND w.asset_code = ledger_accounts.asset_code))
                     OR (account_type = 'player_withdrawal_hold'
                         AND EXISTS (SELECT 1 FROM withdrawal_hold_resolutions h
                                       JOIN withdrawal_requests w ON w.id = h.withdrawal_request_id AND w.tenant_id = h.tenant_id
                                      WHERE h.tenant_id = ledger_accounts.tenant_id
                                        AND h.kind = 'release_hold_to_player'
                                        AND h.state = 'executing' AND h.executed_txid = txid_current()
                                        AND w.wallet_id = ledger_accounts.wallet_id
                                        AND w.asset_code = ledger_accounts.asset_code))
                     OR (account_type = 'psp_clearing' AND wallet_id IS NULL
                         AND EXISTS (SELECT 1 FROM payment_manual_resolutions m
                                       JOIN withdrawal_requests w ON w.id = m.withdrawal_request_id AND w.tenant_id = m.tenant_id
                                      WHERE m.tenant_id = ledger_accounts.tenant_id
                                        AND m.kind IN ('m2_declare_paid', 'm4_evidence_paid')
                                        AND m.state = 'executing' AND m.executed_txid = txid_current()
                                        AND w.asset_code = ledger_accounts.asset_code))));

-- (4/4) the acting withdrawal_requests UPDATE policy: the 0124 text with the M4
-- kinds added to the K3 arm ONLY. The HSEC arm and the 0124
-- withdrawal_requests_governed_release_guard trigger are untouched (HN-4: the
-- policy admits, the trigger pins the executing window).
DROP POLICY acting_update ON withdrawal_requests;
CREATE POLICY acting_update ON withdrawal_requests FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
                AND (SELECT financial_acting_session_valid())
                AND (EXISTS (SELECT 1 FROM payment_manual_resolutions m
                              WHERE m.withdrawal_request_id = withdrawal_requests.id AND m.tenant_id = withdrawal_requests.tenant_id
                                AND m.state = 'executing' AND m.executed_txid = txid_current()
                                AND m.kind IN ('m2_declare_paid', 'm2_declare_not_paid', 'm4_evidence_paid', 'm4_evidence_not_paid'))
                     OR (withdrawal_requests.state = 'rejected'
                         AND EXISTS (SELECT 1 FROM withdrawal_hold_resolutions h
                                      WHERE h.withdrawal_request_id = withdrawal_requests.id AND h.tenant_id = withdrawal_requests.tenant_id
                                        AND h.kind = 'release_hold_to_player'
                                        AND h.state = 'executing' AND h.executed_txid = txid_current()))));

-- =========================================================================
-- 10. Reconciliation read (section 4.6): the system shape reads EXECUTED M2
--     and M4 rows only (never M1, never pending payloads).
-- =========================================================================

DROP POLICY tenant_system_read_executed ON payment_manual_resolutions;
CREATE POLICY tenant_system_read_executed ON payment_manual_resolutions FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND state = 'executed'
           AND kind IN ('m2_declare_paid', 'm2_declare_not_paid', 'm4_evidence_paid', 'm4_evidence_not_paid')
           AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());

CREATE INDEX payment_manual_resolutions_evidence_line ON payment_manual_resolutions (evidence_line_id) WHERE evidence_line_id IS NOT NULL;
