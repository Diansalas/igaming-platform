#!/usr/bin/env python3
"""F4 mutation pass: single-site mutants of the dispatch gate (Go) and the 0169 backstop (SQL).
Usage: python3 f4_mutation_driver.py <worktree> <results.tsv> [mutant-name ...]"""
import os, subprocess, sys, time

WT = sys.argv[1]
OUT = sys.argv[2]
ONLY = set(sys.argv[3:])
S = '/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad'
MUTDB = 'f420261016221123p14545x02'
ENV = dict(os.environ, PRIV_DB=MUTDB, PRIV_SRC=WT)

def gotest(pkg, run):
    cmd = ['bash', S + '/priv_test.sh', '-tags', 'integration', '-count=1', '-p', '1', '-timeout', '25m', '-run', run, pkg]
    r = subprocess.run(cmd, env=ENV, capture_output=True, text=True, timeout=1800)
    out = r.stdout + r.stderr
    if 'build failed' in out or '[setup failed]' in out:
        return 'BUILD', out
    return ('PASS' if r.returncode == 0 else 'FAIL'), out

def rebuild():
    r = subprocess.run(['bash', S + '/priv_db.sh'], env=ENV, capture_output=True, text=True, timeout=1800)
    return r.returncode == 0, r.stdout + r.stderr

PAY = ('./internal/payments/', 'TestF4')
WD = ('./internal/withdrawal/', 'TestF4')
HTTP = ('./internal/httpserver/', 'TestSubmitWithdrawalHandler_F4')

GO = 'go'
SQL = 'sql'
M = 'migrations/0169_withdrawal_dispatch_gate_backstop.up.sql'
P = 'internal/payments/payout.go'
PS = 'internal/payments/payout_withdrawal_gate.go'
SW = 'internal/payments/payout_sweep.go'
DG = 'internal/withdrawal/dispatchgate.go'
OR = 'internal/payments/orchestrator.go'
H = 'internal/httpserver/withdrawal_handlers.go'

mutants = [
    # --- Go: T1p ---
    ('t1p_gate_skipped', GO, P, 'if pre.State == withdrawal.StateApproved {', 'if false {', [PAY]),
    ('t1p_always_evaluate_replay', GO, P, 'if pre.State == withdrawal.StateApproved {', 'if true {', [PAY]),
    ('t1p_deny_branch_off', GO, P, '		if !gateDecision.Allowed {\n			if err := withdrawal.RecordDispatchGateDecision', '		if false {\n			if err := withdrawal.RecordDispatchGateDecision', [PAY]),
    ('t1p_allow_row_not_recorded', GO, P, '		if err := withdrawal.RecordDispatchGateDecision(actx, tx, wr, withdrawal.DispatchStageDispatch, nil, gateDecision); err != nil {\n			return err\n		}\n\n		if err := withdrawal.MarkSubmittedPending', '		if false {\n			return nil\n		}\n\n		if err := withdrawal.MarkSubmittedPending', [PAY]),
    ('t1p_alert_not_raised', GO, P, '			return alerting.RaiseGuarded(actx, tx, WithdrawalGateDeniedAlert(tenantID, "withdrawal:"+requestID.String(), withdrawal.DispatchStageDispatch, gateDecision))', '			return nil', [PAY]),
    ('t1p_denial_not_returned', GO, P, '	if gateRefusal != nil {\n		// Returned only AFTER', '	if false {\n		// Returned only AFTER', [PAY]),
    ('t1p_unavailable_audit_dropped', GO, P, '			recordPayoutGateUnavailableAudit(ctx, pool, tenantID, requestID, actor)', '			_ = actor', [PAY]),
    ('t1p_not_evaluated_guard_off', GO, P, '		if !gateEvaluated {\n			// No gate decision', '		if false && !gateEvaluated {\n			// No gate decision', [PAY]),
    ('t1p_audit_gate_code_dropped', GO, P, '"denied_by_withdrawal_gate": true, "gate_code": d.GateCode,', '"denied_by_withdrawal_gate": true, "gate_code": "",', [PAY]),
    ('orch_default_gate_nil', GO, OR, '		withdrawalGate: standard.New(),', '		withdrawalGate: func() withdrawalgate.Gate { _ = standard.New(); return nil }(),', [PAY]),
    # --- Go: dispatchgate ---
    ('eval_error_is_allow', GO, DG, '		return withdrawalgate.Decision{}, fmt.Errorf("%w: %w", ErrGateUnavailable, err)\n	}\n	if !d.Allowed', '		return withdrawalgate.Decision{Allowed: true}, nil\n	}\n	if !d.Allowed', [WD, PAY]),
    ('eval_nil_gate_allows', GO, DG, '		return withdrawalgate.Decision{}, fmt.Errorf("%w: no gate was supplied at payout dispatch", ErrGateUnavailable)', '		return withdrawalgate.Decision{Allowed: true}, nil', [WD, PAY]),
    ('eval_unknown_code_accepted', GO, DG, '	if !d.Allowed && d.GateCode != GateCodeRG && d.GateCode != GateCodeRisk {', '	if false {', [WD]),
    ('record_restriction_dropped', GO, DG, '	case "self_exclusion", "time_out":\n		ri := d.RestrictionInForce', '	case "never":\n		ri := d.RestrictionInForce', [PAY]),
    ('translate_identity', GO, DG, '	if errors.As(err, &pgErr) && (pgErr.Code == SQLStateDispatchRGBackstop || pgErr.Code == SQLStateDispatchNoDecision) {', '	if false && errors.As(err, &pgErr) {', [WD]),
    # --- Go: T2 / T12 ---
    ('redispatch_deny_is_allow', GO, PS, '	if d.Allowed {\n		if err := withdrawal.RecordDispatchGateDecision', '	if true {\n		if err := withdrawal.RecordDispatchGateDecision', [PAY]),
    ('redispatch_escalated_branch_off', GO, PS, '	if attempt.EscalatedAt != nil {\n		return false, RescheduleNonTerminal', '	if false {\n		return false, RescheduleNonTerminal', [PAY]),
    ('redispatch_alert_dropped', GO, PS, '	return false, alerting.RaiseGuarded(ctx, tx, WithdrawalGateDeniedAlert(wr.TenantID, "payout_attempt:"+attempt.ID.String(), withdrawal.DispatchStageRedispatch, d))', '	return false, nil', [PAY]),
    ('t2_gate_not_applied', GO, SW, '		allowed, err = applyRedispatchGate(actx, tx, wr, attempt, gateDecision, nextPoll)\n		if err != nil || !allowed {\n			return err\n		}\n		// B13-B (ADR 0111 2.4 "T2 re-claim / T12 resend"): the destination gate and the tiering\n		// predicate on the exact adapter, next to the KYC gate. Failure escalates (T16): no\n		// resend, no release. The destination is never re-resolved.\n		allowed, err = s.destinationGateAndEscalate(actx, tx, wr, attempt, nextPoll)\n		if err != nil || !allowed {\n			return err\n		}\n		allowed, err = s.gateAndEscalateOnDeny(actx, tx, wr, attempt, nextPoll)\n		if err != nil || !allowed {\n			return err\n		}\n		claimToken := uuid.New()\n		if err := ClaimCreatedForSubmission', '		_ = gateDecision\n		// B13-B (ADR 0111 2.4 "T2 re-claim / T12 resend"): the destination gate and the tiering\n		// predicate on the exact adapter, next to the KYC gate. Failure escalates (T16): no\n		// resend, no release. The destination is never re-resolved.\n		allowed, err = s.destinationGateAndEscalate(actx, tx, wr, attempt, nextPoll)\n		if err != nil || !allowed {\n			return err\n		}\n		allowed, err = s.gateAndEscalateOnDeny(actx, tx, wr, attempt, nextPoll)\n		if err != nil || !allowed {\n			return err\n		}\n		claimToken := uuid.New()\n		if err := ClaimCreatedForSubmission', [PAY]),
    ('redispatch_eval_skipped', GO, PS, '	return withdrawal.EvaluateDispatchGate(ctx, tx, s.withdrawalGate(), pre)', '	_ = pre\n	return withdrawalgate.Decision{Allowed: true, Outcome: "allow"}, nil', [PAY]),
    # --- Go: HTTP mapping ---
    ('http_409_mapping_dropped', GO, H, '		if errors.Is(err, payments.ErrPayoutWithdrawalGateDenied) {', '		if false && errors.Is(err, payments.ErrPayoutWithdrawalGateDenied) {', [HTTP]),
    # --- SQL: 0169 ---
    ('sql_same_xact_dropped', SQL, M, "                      AND d.decided_xact = pg_catalog.pg_current_xact_id() AND d.allowed)", "                      AND d.allowed)", [WD]),
    ('sql_deny_veto_dropped', SQL, M, "AND d.decided_xact = pg_catalog.pg_current_xact_id() AND NOT d.allowed)", "AND d.decided_xact = pg_catalog.pg_current_xact_id() AND NOT d.allowed AND false)", [WD]),
    ('sql_account_check_dropped', SQL, M, "    IF v_status IS DISTINCT FROM 'active' THEN\n        RETURN 'player_account_not_active';", "    IF false THEN\n        RETURN 'player_account_not_active';", [WD, PAY]),
    ('sql_wallet_check_dropped', SQL, M, "    IF v_status IS DISTINCT FROM 'active' THEN\n        RETURN 'wallet_not_active';", "    IF false THEN\n        RETURN 'wallet_not_active';", [WD]),
    ('sql_e7_rg_not_called', SQL, M, "    v_rg := public.withdrawal_dispatch_rg_violation(NEW.tenant_id, NEW.player_account_id, NEW.wallet_id);", "    v_rg := NULL;", [WD]),
    ('sql_e7_decision_not_checked', SQL, M, "    IF NOT public.withdrawal_dispatch_decision_ok(NEW.tenant_id, NEW.id, 'dispatch', NULL) THEN", "    IF false THEN", [WD]),
    ('sql_redispatch_decision_not_checked', SQL, M, "    IF NOT public.withdrawal_dispatch_decision_ok(NEW.tenant_id, NEW.withdrawal_request_id, 'redispatch', NEW.id) THEN", "    IF false THEN", [PAY]),
    ('sql_redispatch_rg_not_called', SQL, M, "    v_rg := public.withdrawal_dispatch_rg_violation(NEW.tenant_id, v_account, v_wallet);", "    v_rg := NULL;", [PAY]),
    ('sql_insert_dispatch_state_unchecked', SQL, M, "    IF NEW.stage = 'dispatch' AND v_state IS DISTINCT FROM 'approved' THEN", "    IF false THEN", [WD]),
    ('sql_insert_xact_not_forced', SQL, M, "    NEW.decided_xact := pg_catalog.pg_current_xact_id();", "    NEW.decided_xact := COALESCE(NEW.decided_xact, pg_catalog.pg_current_xact_id());", [WD]),
    ('sql_insert_id_not_forced', SQL, M, "    NEW.id := pg_catalog.gen_random_uuid();", "    NEW.id := COALESCE(NEW.id, pg_catalog.gen_random_uuid());", [WD]),
    ('sql_append_only_off', SQL, M, "CREATE TRIGGER withdrawal_gate_decisions_immutable BEFORE UPDATE OR DELETE ON withdrawal_gate_decisions", "CREATE TRIGGER withdrawal_gate_decisions_immutable BEFORE TRUNCATE ON withdrawal_gate_decisions", [WD]),
    ('sql_e7_when_wrong_edge', SQL, M, "    WHEN (OLD.state = 'approved' AND NEW.state = 'submitted')", "    WHEN (OLD.state = 'submitted' AND NEW.state = 'completed')", [WD]),
]

results = []
for name, kind, rel, old, new, tests in mutants:
    if ONLY and name not in ONLY:
        continue
    path = os.path.join(WT, rel)
    src = open(path).read()
    n = src.count(old)
    if n != 1:
        results.append((name, 'BAD_PATTERN(%d)' % n, ''))
        print(name, 'BAD_PATTERN', n, flush=True)
        continue
    open(path, 'w').write(src.replace(old, new))
    verdict, detail = 'SURVIVED', ''
    try:
        if kind == SQL and any(t[0] == './internal/withdrawal/' or t[0] == './internal/httpserver/' for t in tests):
            ok, out = rebuild()
            if not ok:
                verdict, detail = 'KILLED', 'migration failed to apply'
        if verdict == 'SURVIVED':
            for pkg, run in tests:
                res, out = gotest(pkg, run)
                if res == 'BUILD':
                    verdict, detail = 'BUILD_ERROR', pkg
                    break
                if res == 'FAIL':
                    fails = [l.strip() for l in out.splitlines() if l.startswith('--- FAIL')]
                    verdict, detail = 'KILLED', pkg + ' ' + ';'.join(fails[:3])
                    break
    finally:
        open(path, 'w').write(src)
    results.append((name, verdict, detail))
    print(name, verdict, detail, flush=True)
    with open(OUT, 'a') as f:
        f.write('%s\t%s\t%s\n' % (name, verdict, detail))
# restore the mutation database to the clean head
rebuild()
