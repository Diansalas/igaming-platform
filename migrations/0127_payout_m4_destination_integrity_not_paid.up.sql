-- GOV-R32 (owner decision 4, ADR 0095 section 48; ADR 0111 section 23): the
-- controlled CANCELLATION/RELEASE outcome for a `destination_integrity_failure`
-- park. The reason is admitted to the existing M4 NOT-PAID resolution
-- (m4_evidence_not_paid) ONLY. Nothing else changes.
--
-- MUST RUN INSIDE A SINGLE TRANSACTION (db.MigrateUp does this).
--
-- What this replaces: ONE function, payment_m4_in_scope (migration 0125), with
-- the 0125 body plus `destination_integrity_failure` beside `destination_mismatch`
-- in the not-paid-only arm. CREATE OR REPLACE keeps the function's OID, its
-- signature, IMMUTABLE and the pinned search_path, so both callers in
-- payment_manual_resolutions_guard() (the INSERT scope check, MR012, and the
-- `pending -> executing` re-check, MR012) pick it up unchanged.
--
-- Why this is the whole change (verified against the 0125/0126 bodies, nothing
-- else is reason-specific): payout_m4_evidence reads the attempt's merchant
-- reference, its bound provider reference when it holds one, and every typed Y,
-- whatever the reason; the DB-forced amount/asset (R-4), the reference pin
-- provider_reference_at_submission (C-6/R-3), the platform_acting approver floor
-- in the recount (S-6), the evidence re-check at `-> executing` (S-2), MR041
-- (withdrawal `failed`, `withdrawal_failed` keyed wr.id:failed, exactly the hold
-- debit and the player_cash credit on the withdrawal's OWN wallet), the
-- executing-only fences (g) and the acting K3 arms are all keyed on the KIND.
--
-- Owner decision 4 is not broadened:
--   * M4 PAID stays REFUSED for this reason (MR012): the paid arm still admits
--     only the unbound reasons with NO reference. Completing a payout to a
--     destination the platform cannot verify is never allowed.
--   * Not-paid moves the hold to the player's OWN cash only (never redirects
--     money, never touches the snapshot, never reassigns a beneficiary), and
--     only on the D-7 positive evidence payout_m4_evidence already enforces
--     (sealed eligible import, coverage, a declined line of equal amount/asset
--     after the last send, no succeeded/pending/reversed line on any matched
--     reference, declaration integrity), four-eyes with a platform_acting
--     requester and final approver (R19-1) and the DB platform floor.
--   * Nothing is automatic: the only writer is an EXECUTED four-eyes resolution
--     in the final approval's own transaction. Ambiguous evidence keeps the park.
--   * "Resume when the authoritative destination is positively established" is
--     NOT implemented here (DESIGN ONLY, ADR 0111 section 23.3): the snapshot is
--     write-once and no rebind or snapshot rewrite exists.
--
-- No SECURITY DEFINER. No new table, column, index, policy or SQLSTATE.
-- The down refuses (MR099) while any M4 row exists whose pinned dispute reason
-- is destination_integrity_failure, and otherwise restores the 0125 body byte
-- for byte.

-- Section 4.1 / A-12 / C-6 / M-10 + GOV-R32 (owner decision 4): a disputed
-- payout whose reason is invalid_provider_reference,
-- invalid_provider_reference:* or provider_reference_conflict WITH NO provider
-- reference (both kinds), or destination_mismatch / destination_integrity_failure
-- (not-paid only, with or without a bound reference). Anything else is outside M4.
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
