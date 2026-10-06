-- 0119: MA020-SYNC-MISMATCH-1 (ledger-finance, Class-B item B4).
--
-- Widens the preventive MA020 check player_open_payment_exposure (0113 §7,
-- called by 0115's ledger_adjustment_payload_refusal and read for the audit
-- row by internal/adjustment/execute.go) from "a multiple_success_for_intent
-- park not cleared by a tombstone" to a per-attempt SQL port of the
-- reconciliation rule payMatcher.boundCapture() && payMatcher.capturedUnposted()
-- (internal/reconciliation/payment_statement.go, payment_statement_k3.go).
-- The signature (uuid, uuid) RETURNS boolean is unchanged, so no caller
-- changes. Ledger-finance rulings applied:
--
--   R-MA-1  Reasons: exactly those recon classifies bound or bound-if-
--           referenced (disputeReasonClasses). Unbound reasons
--           (invalid_provider_reference and its prefix family) and
--           reversal_tombstone_precedes_success (net zero) are excluded.
--           A parity test pins SQL set == Go set.
--   R-MA-2  Reference-less parks are excluded (D2F-1: never report what
--           cannot clear). This RELAXES a fail-closed case of 0113 (a
--           reference-less multiple_success_for_intent park used to block
--           every credit forever); B3 (PAY-CALLBACK-MISMATCH-BIND-1) makes new
--           parks carry their reference, and the detective control for the
--           remaining (legacy) reference-less parks is the reconciliation
--           standing finding (PAY-RECON-PARKED-CAPTURE-STANDING-1). Security
--           review required.
--   R-MA-3  Clearing equals recon's clearedRef: a tombstone on the reference,
--           OR an ELIGIBLE persisted deposit_reversal line naming it as its
--           original (RC-3: the import is not MOCK, or no non-MOCK import
--           exists for the tenant and provider) whose status says the reversal
--           COMPLETED (succeeded or reversed). A pending or declined reversal
--           never clears (B4 review C3, security + LF): it may still fail, or
--           the capture stands, and a manual credit plus the later refund would
--           double-credit. Recon's clearedRef applies the same status set.
--   Y       A poll_reference_mismatch park also clears on its typed returned
--           reference Y only when Y is attributable (yAttributable, incl. the
--           security F-1 shared-Y rule) and, when eligible succeeded lines
--           evidence BOTH X and Y, only when both are cleared (G-Y2).
--
-- F-VIS (fail closed, ledger-finance ruling at implementation): the typed Y
-- evidence (payment_attempt_reference_evidence) is readable only in the
-- system session shape (0115 R-4: app.tenant_id set, no principal). Both K2
-- sessions - the tenant staff session (WithPrincipalScope) and the platform
-- acting session (WithPlatformActingInTenant) - cannot see it, and the acting
-- session cannot see payment_statement_imports/lines either. Every input
-- below is therefore arranged so that an INVISIBLE row can only make the
-- result MORE open, never less:
--   * clearing inputs (tombstones, reversal lines) invisible -> fewer clears;
--   * imports and lines share one RLS policy, so RC-3 never sees lines
--     without their imports;
--   * a poll_reference_mismatch park with NO VISIBLE Y row is OPEN
--     (the G-Y2 rule could require Y to be cleared too, and an invisible Y
--     cannot be checked). In the K2 sessions this means every
--     poll_reference_mismatch park blocks credits until the evidence is made
--     visible to them (a security decision, not taken here).
-- The Y attribution absence tests (F-1, other holders, ledger keys) are only
-- reached when a Y row IS visible, i.e. in a session that sees them all.
--
-- All five functions: LANGUAGE sql STABLE, pinned search_path
-- (TRIGGER-SEARCH-PATH-1), NOT SECURITY DEFINER (they run as the caller,
-- under the caller's RLS), owned by the migration role, no TEMP objects.

CREATE FUNCTION payment_ref_cleared(p_tenant uuid, p_provider text, p_ref text) RETURNS boolean AS $$
    SELECT p_ref IS NOT NULL AND p_ref <> '' AND (
        EXISTS (SELECT 1 FROM ledger_transactions t
                 WHERE t.tenant_id = p_tenant AND t.provider_id = p_provider
                   AND t.transaction_type = 'tombstone' AND t.provider_tx_id = p_ref)
        OR EXISTS (SELECT 1 FROM payment_statement_lines l
                     JOIN payment_statement_imports i ON i.id = l.import_id AND i.tenant_id = l.tenant_id
                    WHERE l.tenant_id = p_tenant AND l.provider_id = p_provider
                      AND l.kind = 'deposit_reversal' AND l.original_provider_reference = p_ref
                      AND l.status IN ('succeeded', 'reversed')                       -- C3: completed only
                      AND (NOT i.is_mock
                           OR NOT EXISTS (SELECT 1 FROM payment_statement_imports r
                                           WHERE r.tenant_id = p_tenant AND r.provider_id = p_provider AND NOT r.is_mock))));
$$ LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp;

-- payMatcher.evidencedCapture: an ELIGIBLE succeeded deposit line names p_ref.
CREATE FUNCTION payment_ref_evidenced(p_tenant uuid, p_provider text, p_ref text) RETURNS boolean AS $$
    SELECT p_ref IS NOT NULL AND p_ref <> '' AND EXISTS (
        SELECT 1 FROM payment_statement_lines l
          JOIN payment_statement_imports i ON i.id = l.import_id AND i.tenant_id = l.tenant_id
         WHERE l.tenant_id = p_tenant AND l.provider_id = p_provider
           AND l.kind = 'deposit' AND l.status = 'succeeded' AND l.provider_reference = p_ref
           AND (NOT i.is_mock
                OR NOT EXISTS (SELECT 1 FROM payment_statement_imports r
                                WHERE r.tenant_id = p_tenant AND r.provider_id = p_provider AND NOT r.is_mock)));
$$ LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp;

-- payMatcher.yAttributable: Y may clear attempt p_attempt's park only if no one
-- else holds it.
CREATE FUNCTION payment_y_attributable(p_tenant uuid, p_provider text, p_attempt uuid, p_y text) RETURNS boolean AS $$
    SELECT p_y IS NOT NULL AND p_y <> ''
       -- security F-1: another park recorded the same returned reference
       AND NOT EXISTS (SELECT 1 FROM payment_attempt_reference_evidence e
                        WHERE e.tenant_id = p_tenant AND e.provider_id = p_provider
                          AND e.evidence_kind = 'poll_returned_reference'
                          AND e.reference = p_y AND e.attempt_id <> p_attempt)
       -- another deposit or payout attempt of this provider holds Y
       AND NOT EXISTS (SELECT 1 FROM payment_attempts o
                        WHERE o.tenant_id = p_tenant AND o.provider_id = p_provider
                          AND o.provider_reference = p_y AND o.id <> p_attempt
                          AND o.operation IN ('deposit', 'payout'))
       -- a payout's settlement reference (bySettlement)
       AND NOT EXISTS (SELECT 1 FROM payment_attempts o
                         JOIN withdrawal_requests wr ON wr.id = o.withdrawal_request_id AND wr.tenant_id = o.tenant_id
                         JOIN ledger_transactions rl ON rl.id = wr.release_ledger_transaction_id
                        WHERE o.tenant_id = p_tenant AND o.provider_id = p_provider AND o.id <> p_attempt
                          AND rl.transaction_type = 'withdrawal_completed' AND rl.provider_id = o.provider_id
                          AND rl.provider_tx_id = p_y)
       -- a posting keyed by Y (a tombstone on Y is allowed: G-Y3)
       AND NOT EXISTS (SELECT 1 FROM ledger_transactions t
                        WHERE t.tenant_id = p_tenant AND t.provider_id = p_provider AND t.provider_tx_id = p_y
                          AND t.transaction_type IN ('deposit', 'withdrawal_completed', 'deposit_reversal'));
$$ LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp;

-- Per attempt: boundCapture() && capturedUnposted() (+ D2F-1, + F-VIS).
-- 0115's payment_attempt_reference_evidence UNIQUE (tenant_id, attempt_id,
-- evidence_kind) makes the LEFT JOIN at most one row.
CREATE FUNCTION payment_attempt_open_exposure(p_tenant uuid, p_attempt uuid) RETURNS boolean AS $$
    SELECT COALESCE((
        SELECT CASE
                 WHEN a.terminal_reason = 'poll_reference_mismatch' AND y.reference IS NULL THEN true   -- F-VIS
                 WHEN y.reference IS NOT NULL AND payment_y_attributable(a.tenant_id, a.provider_id, a.id, y.reference) THEN
                     CASE WHEN payment_ref_evidenced(a.tenant_id, a.provider_id, a.provider_reference)
                               AND payment_ref_evidenced(a.tenant_id, a.provider_id, y.reference)          -- G-Y2
                          THEN NOT payment_ref_cleared(a.tenant_id, a.provider_id, a.provider_reference)
                               OR NOT payment_ref_cleared(a.tenant_id, a.provider_id, y.reference)
                          ELSE NOT payment_ref_cleared(a.tenant_id, a.provider_id, a.provider_reference)
                               AND NOT payment_ref_cleared(a.tenant_id, a.provider_id, y.reference)
                     END
                 ELSE NOT payment_ref_cleared(a.tenant_id, a.provider_id, a.provider_reference)
               END
          FROM payment_attempts a
          LEFT JOIN payment_attempt_reference_evidence y
                 ON y.tenant_id = a.tenant_id AND y.attempt_id = a.id AND y.provider_id = a.provider_id
                AND y.evidence_kind = 'poll_returned_reference'
         WHERE a.tenant_id = p_tenant AND a.id = p_attempt
           AND a.operation = 'deposit' AND a.state = 'disputed'
           AND a.provider_id IS NOT NULL
           AND a.provider_reference IS NOT NULL AND a.provider_reference <> ''              -- R-MA-2 (D2F-1)
           AND a.terminal_reason IN ('multiple_success_for_intent',                         -- R-MA-1
                                     'sync_amount_mismatch',
                                     'poll_amount_mismatch',
                                     'poll_reference_mismatch',
                                     'callback_amount_asset_mismatch',
                                     'provider_reference_conflict',
                                     'success_for_never_sent_attempt')), false);
$$ LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp;

CREATE OR REPLACE FUNCTION player_open_payment_exposure(p_tenant uuid, p_player uuid) RETURNS boolean AS $$
    SELECT EXISTS (
        SELECT 1
          FROM payment_attempts a
          JOIN deposit_intents i ON i.id = a.deposit_intent_id AND i.tenant_id = a.tenant_id
         WHERE a.tenant_id = p_tenant AND i.player_account_id = p_player
           AND a.operation = 'deposit' AND a.state = 'disputed'
           AND payment_attempt_open_exposure(p_tenant, a.id));
$$ LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp;
