-- F4 (platform-freeze work list 2026-10-16; A6 SR6 I-2 / residual 4): the database backstop of the WD-RG-1
-- withdrawal RG/risk gate at PAYOUT DISPATCH.
-- MONEY-PATH TRIGGERS: FOR `ledger-finance`, `security`, `code-reviewer`, `qa` REVIEW BEFORE MERGE.
--
-- NUMBERING: 0169 (allocated by the orchestrator; 0166-0168 are held by other in-flight work, 0159 and 0161
-- stay reserved and unused).
--
-- SQLSTATE class "WD" (0131 WD010-012, 0132 WD020-043/WD098, 0162/0165 WD026-028/WD035/WD044/WD096-099). WD050
-- and WD051 are the APPLICATION gate codes of WD-RG-1 (internal/withdrawalgate); they are not raised here. New:
--   WD052 RG backstop: the withdrawing player account is not 'active' or the wallet is not 'active' (or one of
--         those rows is not visible to the session: fail closed). The STATUS part of the RG half of WD-RG-1
--         (rg.EvaluateWithdrawalEligibility with restrictions NOT blocking: a self-exclusion / time-out does not
--         refuse a withdrawal, ADR 0115). NOT mirrored here: the negative-wallet-balance refusal (owner decision
--         13), which the application gate applies at dispatch; see docs/plans/f4-withdrawal-gate-backstop-record.md
--         (residual R2) for why.
--   WD053 no ALLOWING dispatch gate decision was recorded for this request (and, for a re-dispatch, this
--         attempt) in THIS transaction, or a DENYING one was.
--   WD054 a withdrawal_gate_decisions row does not match its request / attempt / tenant / stage.
--   WD055 withdrawal_gate_decisions is append-only (UPDATE, DELETE, TRUNCATE).
--   WD095 down refused: gate decisions exist (compliance history).
--
-- What it adds:
--   1. withdrawal_gate_decisions: one row per WD-RG-1 evaluation at a payout dispatch point (stage 'dispatch'
--      = T1p, approved -> submitted; stage 'redispatch' = the sweeper's T2 re-claim / T12 resend of a payout
--      attempt). Append-only, FORCE RLS, tenant staff/system scope (never a player session), runtime role
--      SELECT + INSERT only. id, decided_at and decided_xact are set by the trigger, never by the caller.
--   2. withdrawal_requests_zz_dispatch_gate_guard: BEFORE UPDATE, WHEN approved -> submitted (ADR 0113 E7).
--   3. payment_attempts_zz_payout_redispatch_gate_guard: BEFORE UPDATE, WHEN a PAYOUT attempt moves
--      created -> submitting (T2) or ambiguous -> submitting (T12).
--   4. The alert Kind withdrawal.payout_gate_denied (raised in-tx by internal/payments on a dispatch denial).
--   Both guards refuse WD052 (the RG half, re-read in SQL at the instant of the transition) and WD053 (the
--   recorded decision). The risk half cannot be evaluated by the database (a rule engine with cumulative
--   advisory locks): WD053 proves that the dispatch code path evaluated the gate and recorded an ALLOW in the
--   SAME transaction as the transition (decided_xact = pg_current_xact_id()), so a path that bypasses the
--   evaluation is refused. It does NOT prove what a deliberately forged insert by the runtime credential says
--   (the C-WD023 caveat of 0132: a record exists, not that its content is true). Recorded residual.
--
-- Trigger names carry "zz" so they fire AFTER every existing BEFORE ROW trigger on the two tables (the 0132
-- state guard, 0124 hold freeze / governed release, 0026 immutable fields, 0126 binding guard; the 0101/0105
-- attempt guards): every refusal an existing guard gives keeps its code and precedence; these guards only add a
-- refusal to an update all of them admit. Nothing takes a lock (the writers hold L1 / the attempt row; the
-- reads are plain RLS-bound reads), so the ADR 0082 lock order is unchanged. Completion (E9), failure (E10),
-- rejection, cancellation, reversal and every terminal attempt transition are NOT gated: once money may have
-- left, the platform must record the truth, never refuse it.
--
-- Every function pins search_path (ADR 0108); none is SECURITY DEFINER. No role is exempt (owner included,
-- while the triggers are enabled: ADR 0110 9.4 residual). No existing function, policy, trigger or row changes.

-- =========================================================================
-- 1. withdrawal_gate_decisions
-- =========================================================================

CREATE TABLE withdrawal_gate_decisions (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    withdrawal_request_id UUID NOT NULL,
    payment_attempt_id    UUID NULL REFERENCES payment_attempts (id) ON DELETE RESTRICT,
    stage                 TEXT NOT NULL CHECK (stage IN ('dispatch', 'redispatch')),
    allowed               BOOLEAN NOT NULL,
    gate_code             TEXT NULL,
    reason_code           TEXT NULL CHECK (reason_code IS NULL OR (octet_length(reason_code) BETWEEN 1 AND 64 AND reason_code ~ '^[a-z0-9_.:-]+$')),
    risk_outcome          TEXT NULL CHECK (risk_outcome IS NULL OR risk_outcome IN ('allow', 'deny', 'review')),
    restriction_in_force  TEXT NULL CHECK (restriction_in_force IS NULL OR restriction_in_force IN ('self_exclusion', 'time_out')),
    decided_at            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    decided_xact          XID8 NOT NULL DEFAULT pg_current_xact_id(),
    FOREIGN KEY (withdrawal_request_id, tenant_id) REFERENCES withdrawal_requests (id, tenant_id) ON DELETE RESTRICT,
    CONSTRAINT withdrawal_gate_decisions_outcome_shape CHECK (
        (allowed AND gate_code IS NULL)
        OR (NOT allowed AND gate_code IN ('WD050', 'WD051') AND reason_code IS NOT NULL)),
    CONSTRAINT withdrawal_gate_decisions_stage_attempt CHECK (
        (stage = 'dispatch' AND payment_attempt_id IS NULL)
        OR (stage = 'redispatch' AND payment_attempt_id IS NOT NULL))
);

CREATE INDEX withdrawal_gate_decisions_request ON withdrawal_gate_decisions (withdrawal_request_id, decided_xact);
CREATE INDEX withdrawal_gate_decisions_attempt ON withdrawal_gate_decisions (payment_attempt_id, decided_xact)
    WHERE payment_attempt_id IS NOT NULL;

CREATE FUNCTION withdrawal_gate_decisions_before_insert() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_state text;
    v_ok    boolean;
BEGIN
    -- Never a caller value: the identity, the instant and the transaction that recorded the decision.
    NEW.id := pg_catalog.gen_random_uuid();
    NEW.decided_at := pg_catalog.clock_timestamp();
    NEW.decided_xact := pg_catalog.pg_current_xact_id();

    SELECT r.state INTO v_state FROM public.withdrawal_requests r
     WHERE r.id = NEW.withdrawal_request_id AND r.tenant_id = NEW.tenant_id;
    IF v_state IS NULL THEN
        RAISE EXCEPTION 'withdrawal_gate_decisions: the withdrawal request is not visible in this tenant'
            USING ERRCODE = 'WD054';
    END IF;
    IF NEW.stage = 'dispatch' AND v_state IS DISTINCT FROM 'approved' THEN
        RAISE EXCEPTION 'withdrawal_gate_decisions: a dispatch decision is recorded only for an approved request (state %)', v_state
            USING ERRCODE = 'WD054';
    END IF;
    IF NEW.stage = 'redispatch' THEN
        IF v_state IS DISTINCT FROM 'submitted' THEN
            RAISE EXCEPTION 'withdrawal_gate_decisions: a re-dispatch decision is recorded only for a submitted request (state %)', v_state
                USING ERRCODE = 'WD054';
        END IF;
        SELECT EXISTS (SELECT 1 FROM public.payment_attempts a
                        WHERE a.id = NEW.payment_attempt_id AND a.tenant_id = NEW.tenant_id
                          AND a.operation = 'payout' AND a.withdrawal_request_id = NEW.withdrawal_request_id)
          INTO v_ok;
        IF NOT v_ok THEN
            RAISE EXCEPTION 'withdrawal_gate_decisions: the attempt is not a payout attempt of this request in this tenant'
                USING ERRCODE = 'WD054';
        END IF;
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER withdrawal_gate_decisions_before_insert BEFORE INSERT ON withdrawal_gate_decisions
    FOR EACH ROW EXECUTE FUNCTION withdrawal_gate_decisions_before_insert();

CREATE FUNCTION withdrawal_gate_decisions_deny_mutation() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION 'withdrawal_gate_decisions: rows are append-only (no update, delete or truncate)'
        USING ERRCODE = 'WD055';
END
$$;

CREATE TRIGGER withdrawal_gate_decisions_immutable BEFORE UPDATE OR DELETE ON withdrawal_gate_decisions
    FOR EACH ROW EXECUTE FUNCTION withdrawal_gate_decisions_deny_mutation();
CREATE TRIGGER withdrawal_gate_decisions_no_truncate BEFORE TRUNCATE ON withdrawal_gate_decisions
    FOR EACH STATEMENT EXECUTE FUNCTION withdrawal_gate_decisions_deny_mutation();

ALTER TABLE withdrawal_gate_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE withdrawal_gate_decisions FORCE ROW LEVEL SECURITY;
-- Staff/system scope only (the 0026 / 0101 tenant_staff_scope pattern): a player session neither reads nor writes.
CREATE POLICY tenant_staff_scope ON withdrawal_gate_decisions
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- =========================================================================
-- 2. The shared checks.
-- =========================================================================

-- withdrawal_dispatch_rg_violation: the status part of the RG half of WD-RG-1 for (tenant, account, wallet), read
-- at the instant of the transition. NULL when it passes; otherwise a closed reason word. A row the session cannot
-- see is a violation (fail closed).
CREATE FUNCTION withdrawal_dispatch_rg_violation(p_tenant uuid, p_account uuid, p_wallet uuid) RETURNS text
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
BEGIN
    SELECT pa.status INTO v_status FROM public.player_accounts pa WHERE pa.id = p_account AND pa.tenant_id = p_tenant;
    IF v_status IS DISTINCT FROM 'active' THEN
        RETURN 'player_account_not_active';
    END IF;
    v_status := NULL;
    SELECT w.status INTO v_status FROM public.wallets w
     WHERE w.id = p_wallet AND w.tenant_id = p_tenant AND w.player_account_id = p_account;
    IF v_status IS DISTINCT FROM 'active' THEN
        RETURN 'wallet_not_active';
    END IF;
    RETURN NULL;
END
$$;

-- withdrawal_dispatch_decision_ok: an ALLOWING decision of the stage (and attempt) recorded in THIS transaction,
-- and no DENYING one in this transaction.
CREATE FUNCTION withdrawal_dispatch_decision_ok(p_tenant uuid, p_request uuid, p_stage text, p_attempt uuid) RETURNS boolean
    LANGUAGE sql
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT EXISTS (SELECT 1 FROM public.withdrawal_gate_decisions d
                    WHERE d.tenant_id = p_tenant AND d.withdrawal_request_id = p_request AND d.stage = p_stage
                      AND d.payment_attempt_id IS NOT DISTINCT FROM p_attempt
                      AND d.decided_xact = pg_catalog.pg_current_xact_id() AND d.allowed)
       AND NOT EXISTS (SELECT 1 FROM public.withdrawal_gate_decisions d
                        WHERE d.tenant_id = p_tenant AND d.withdrawal_request_id = p_request AND d.stage = p_stage
                          AND d.payment_attempt_id IS NOT DISTINCT FROM p_attempt
                          AND d.decided_xact = pg_catalog.pg_current_xact_id() AND NOT d.allowed)
$$;

-- =========================================================================
-- 3. E7 (approved -> submitted): the T1p claim.
-- =========================================================================

CREATE FUNCTION withdrawal_requests_dispatch_gate_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_rg text;
BEGIN
    v_rg := public.withdrawal_dispatch_rg_violation(NEW.tenant_id, NEW.player_account_id, NEW.wallet_id);
    IF v_rg IS NOT NULL THEN
        RAISE EXCEPTION 'withdrawal_requests: approved -> submitted refused by the RG backstop (%)', v_rg
            USING ERRCODE = 'WD052';
    END IF;
    IF NOT public.withdrawal_dispatch_decision_ok(NEW.tenant_id, NEW.id, 'dispatch', NULL) THEN
        RAISE EXCEPTION 'withdrawal_requests: approved -> submitted needs an allowing dispatch gate decision recorded in this transaction'
            USING ERRCODE = 'WD053';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER withdrawal_requests_zz_dispatch_gate_guard
    BEFORE UPDATE ON withdrawal_requests
    FOR EACH ROW
    WHEN (OLD.state = 'approved' AND NEW.state = 'submitted')
    EXECUTE FUNCTION withdrawal_requests_dispatch_gate_guard();

-- =========================================================================
-- 4. T2 (created -> submitting) and T12 (ambiguous -> submitting) of a PAYOUT attempt: a new Withdraw call.
-- =========================================================================

CREATE FUNCTION payment_attempts_payout_redispatch_gate_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_account uuid;
    v_wallet  uuid;
    v_rg      text;
BEGIN
    SELECT r.player_account_id, r.wallet_id INTO v_account, v_wallet FROM public.withdrawal_requests r
     WHERE r.id = NEW.withdrawal_request_id AND r.tenant_id = NEW.tenant_id;
    IF v_account IS NULL THEN
        RAISE EXCEPTION 'payment_attempts: the payout attempt''s withdrawal request is not visible (RG backstop)'
            USING ERRCODE = 'WD052';
    END IF;
    v_rg := public.withdrawal_dispatch_rg_violation(NEW.tenant_id, v_account, v_wallet);
    IF v_rg IS NOT NULL THEN
        RAISE EXCEPTION 'payment_attempts: payout % -> submitting refused by the RG backstop (%)', OLD.state, v_rg
            USING ERRCODE = 'WD052';
    END IF;
    IF NOT public.withdrawal_dispatch_decision_ok(NEW.tenant_id, NEW.withdrawal_request_id, 'redispatch', NEW.id) THEN
        RAISE EXCEPTION 'payment_attempts: payout % -> submitting needs an allowing re-dispatch gate decision recorded in this transaction', OLD.state
            USING ERRCODE = 'WD053';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER payment_attempts_zz_payout_redispatch_gate_guard
    BEFORE UPDATE ON payment_attempts
    FOR EACH ROW
    WHEN (NEW.operation = 'payout' AND NEW.state = 'submitting' AND OLD.state IN ('created', 'ambiguous'))
    EXECUTE FUNCTION payment_attempts_payout_redispatch_gate_guard();

-- =========================================================================
-- 5. The alert Kind withdrawal.payout_gate_denied (the 0141 pattern: a temporary literal single-Kind FOR INSERT
--    policy, created and dropped in one DO block). p2 is an ENGINEERING DEFAULT (ALERT-DELIVERY-1 OPEN).
-- =========================================================================
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.alert_kinds WHERE kind = 'withdrawal.payout_gate_denied') THEN
        RAISE EXCEPTION 'migration 0169 pre-flight: alert kind withdrawal.payout_gate_denied already exists' USING ERRCODE = 'WD098';
    END IF;
    CREATE POLICY alert_kinds_seed_0169 ON alert_kinds
        FOR INSERT TO CURRENT_USER
        WITH CHECK (kind = 'withdrawal.payout_gate_denied');
    INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject,
                             in_tx_raisable_by_tenant, allowed_keys, raise_mode)
    VALUES ('withdrawal.payout_gate_denied', 'p2', 'platform', false, true, true,
            ARRAY['gate_code', 'reason_code', 'stage'], 'in_tx');
    DROP POLICY alert_kinds_seed_0169 ON alert_kinds;
END
$$;

-- =========================================================================
-- 6. Grants to igaming_runtime (mirrored by deploy/init-app-role.sql). RLS + triggers bind.
-- =========================================================================
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON withdrawal_gate_decisions FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON withdrawal_gate_decisions TO igaming_runtime';
    END IF;
END $$;

COMMENT ON TABLE withdrawal_gate_decisions IS 'F4: append-only WD-RG-1 decisions at payout dispatch (T1p) and re-dispatch (T2/T12); the E7 and payout T2/T12 guards require an allowing row of the same transaction.';
