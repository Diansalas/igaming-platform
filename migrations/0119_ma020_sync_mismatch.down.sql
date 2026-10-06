-- Reverses 0119: restores the exact 0113 body of player_open_payment_exposure
-- (multiple_success_for_intent only, tombstone-only clearing, NULL reference
-- open, no search_path pin), then drops the four helpers in dependency order.
-- No data is touched.
CREATE OR REPLACE FUNCTION player_open_payment_exposure(p_tenant uuid, p_player uuid) RETURNS boolean AS $$
    SELECT EXISTS (
        SELECT 1
          FROM payment_attempts a
          JOIN deposit_intents i ON i.id = a.deposit_intent_id AND i.tenant_id = a.tenant_id
         WHERE a.tenant_id = p_tenant AND i.player_account_id = p_player
           AND a.operation = 'deposit' AND a.state = 'disputed'
           AND a.terminal_reason = 'multiple_success_for_intent'
           AND NOT EXISTS (
               SELECT 1 FROM ledger_transactions t
                WHERE t.tenant_id = a.tenant_id
                  AND t.transaction_type = 'tombstone'
                  AND t.provider_id = a.provider_id
                  AND t.provider_tx_id = a.provider_reference));
$$ LANGUAGE sql STABLE;

ALTER FUNCTION player_open_payment_exposure(uuid, uuid) RESET ALL;

DROP FUNCTION payment_attempt_open_exposure(uuid, uuid);
DROP FUNCTION payment_y_attributable(uuid, text, uuid, text);
DROP FUNCTION payment_ref_evidenced(uuid, text, text);
DROP FUNCTION payment_ref_cleared(uuid, text, text);
