-- Down of 0169 (F4). Refused (WD095) once any gate decision exists: a recorded WD-RG-1 decision at payout dispatch is
-- compliance history and is never discarded by a schema rollback. On an empty table it drops exactly what the up
-- created; no other object is touched.
DO $$
DECLARE
    v_any boolean;
BEGIN
    -- FORCE RLS would hide every row from a session without app.tenant_id: lift it for the owner's count only.
    ALTER TABLE withdrawal_gate_decisions NO FORCE ROW LEVEL SECURITY;
    SELECT EXISTS (SELECT 1 FROM withdrawal_gate_decisions) INTO v_any;
    ALTER TABLE withdrawal_gate_decisions FORCE ROW LEVEL SECURITY;
    IF v_any THEN
        RAISE EXCEPTION 'migration 0169 down refused: withdrawal gate decisions exist' USING ERRCODE = 'WD095';
    END IF;
END
$$;

DO $$
BEGIN
    ALTER TABLE alert_kinds DISABLE TRIGGER alert_kinds_deny_update_delete;
    CREATE POLICY alert_kinds_unseed_0169 ON alert_kinds
        FOR DELETE TO CURRENT_USER USING (kind = 'withdrawal.payout_gate_denied');
    DELETE FROM alert_kinds WHERE kind = 'withdrawal.payout_gate_denied';
    DROP POLICY alert_kinds_unseed_0169 ON alert_kinds;
    ALTER TABLE alert_kinds ENABLE TRIGGER alert_kinds_deny_update_delete;
EXCEPTION WHEN foreign_key_violation THEN
    RAISE EXCEPTION 'migration 0169 down refused: alerts of kind withdrawal.payout_gate_denied exist' USING ERRCODE = 'WD095';
END
$$;

DROP TRIGGER payment_attempts_zz_payout_redispatch_gate_guard ON payment_attempts;
DROP FUNCTION payment_attempts_payout_redispatch_gate_guard();
DROP TRIGGER withdrawal_requests_zz_dispatch_gate_guard ON withdrawal_requests;
DROP FUNCTION withdrawal_requests_dispatch_gate_guard();
DROP FUNCTION withdrawal_dispatch_decision_ok(uuid, uuid, text, uuid);
DROP FUNCTION withdrawal_dispatch_rg_violation(uuid, uuid, uuid);
DROP TABLE withdrawal_gate_decisions;
DROP FUNCTION withdrawal_gate_decisions_deny_mutation();
DROP FUNCTION withdrawal_gate_decisions_before_insert();
