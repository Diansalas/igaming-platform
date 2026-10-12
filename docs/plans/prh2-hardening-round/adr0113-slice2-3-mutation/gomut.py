"""ADR 0113 slices 2-3: Go source mutants (and 0133 file mutants run by the scratch-database tests).

usage: python3 -I gomut.py <worktree> <results file> [id-prefix-filter]
Each mutant: back up ONE file, apply ONE textual change (asserted unique), run the named tests of one
package against the PRIVATE mutation database priv_lf_adr0113s23_mut (priv_test.sh), record KILLED /
SURVIVED, restore the file byte for byte. Never runs two mutants at once.
"""
import os, subprocess, sys

W, OUT = sys.argv[1], sys.argv[2]
FILTER = sys.argv[3] if len(sys.argv) > 3 else ''
S = '/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad'
ENV = dict(os.environ, PRIV_DB='priv_lf_adr0113s23_mut', PRIV_SRC=W)

LG = ('launchgov', 'TestLA025|TestClosure_HoldBearingWithdrawals')
RD = ('reconciliation', 'TestADR0113D')
MIG = ('tenant', 'TestMigration0133')

MUTANTS = [
    # --- slice 2, Go arm
    ('G2M01_executor_hold_balance_check_off', 'internal/launchgov/execute.go',
     '		if holdAccounts > 0 {\n', '		if false && holdAccounts > 0 {\n', LG),
    ('G2M02_request_hold_balance_check_off', 'internal/launchgov/hard.go',
     '			if h > 0 {\n', '			if false && h > 0 {\n', LG),
    ('G2M03_go_read_brand_filter_dropped', 'internal/launchgov/readiness.go',
     "AND ($2::uuid IS NULL OR EXISTS (", "AND ($2::uuid IS NULL OR true OR EXISTS (", LG),
    ('G2M04_go_read_totals_filter_dropped', 'internal/launchgov/readiness.go',
     "AND p.credit_total <> p.debit_total\n		  AND ($2::uuid", "\n		  AND ($2::uuid", LG),
    ('G2M05_closure_notice_dropped', 'internal/launchgov/readiness.go',
     '			r.Notices = append(r.Notices, NoticeFundsHoldBlocksClosure)\n', '', LG),
    # --- slice 2, migration file (scratch databases)
    ('F2M01_index_with_totals_predicate', 'migrations/0133_launch_closure_withdrawal_hold_balance_arm.up.sql',
     "    WHERE account_type = 'player_withdrawal_hold';",
     "    WHERE account_type = 'player_withdrawal_hold' AND credit_total <> debit_total;", MIG),
    ('F2M02_down_body_not_verbatim', 'migrations/0133_launch_closure_withdrawal_hold_balance_arm.down.sql',
     "        RAISE EXCEPTION '% status: closed is terminal', TG_TABLE_NAME USING ERRCODE = 'LA020';",
     "        RAISE EXCEPTION '% status: closed is terminal.', TG_TABLE_NAME USING ERRCODE = 'LA020';", MIG),
    ('F2M03_down_keeps_index', 'migrations/0133_launch_closure_withdrawal_hold_balance_arm.down.sql',
     'DROP INDEX idx_wallet_balance_projection_withdrawal_hold;\n', '', MIG),
    # --- slice 3, check D
    ('D3M01_check_d_not_called', 'internal/reconciliation/reconciliation.go',
     '	holds, err := withdrawalHoldFindings(ctx, tx, tenantID)\n', '	holds, err := []Mismatch(nil), error(nil)\n', RD),
    ('D3M02_blind_check_off', 'internal/reconciliation/withdrawal_hold.go',
     '	if reason := vis.blind(); reason != "" {\n', '	if reason := vis.blind(); false && reason != "" {\n', RD),
    ('D3M03_session_probe_off', 'internal/reconciliation/withdrawal_hold.go',
     '	case !v.SessionOK:\n', '	case false && !v.SessionOK:\n', RD),
    ('D3M04_blind_requests_rule_off', 'internal/reconciliation/withdrawal_hold.go',
     '	case v.Requests == 0 && v.E0bTxs > 0:\n', '	case false:\n', RD),
    ('D3M05_blind_hold_tx_rule_off', 'internal/reconciliation/withdrawal_hold.go',
     '	case v.Requests > 0 && v.E0bTxs == 0:\n', '	case false:\n', RD),
    ('D3M06_blind_entries_rule_off', 'internal/reconciliation/withdrawal_hold.go',
     '	case v.HoldEntries == 0 && v.HoldProjectionRowsWithPostings > 0:\n', '	case false:\n', RD),
    ('D3M07_open_mismatch_off', 'internal/reconciliation/withdrawal_hold.go',
     '     WHERE COALESCE(b.open_amount, 0) <> COALESCE(o.open_sum, 0)\n', '     WHERE false\n', RD),
    ('D3M08_link_rules_off', 'internal/reconciliation/withdrawal_hold.go',
     '     WHERE c.code IS NOT NULL\n', '     WHERE false\n', RD),
    ('D3M09_wd034_off', 'internal/reconciliation/withdrawal_hold.go',
     "          WHEN cs.s <> CASE WHEN r.term THEN 0 ELSE r.amount END THEN 'WD034'\n", '', RD),
    ('D3M10_wd033_release_shape_off', 'internal/reconciliation/withdrawal_hold.go',
     "          WHEN r.term AND NOT COALESCE(rt.correlation_id = r.id AND CASE r.state\n", "          WHEN false AND r.term AND NOT COALESCE(rt.correlation_id = r.id AND CASE r.state\n", RD),
    ('D3M11_rebuilt_from_projection', 'internal/reconciliation/withdrawal_hold.go',
     "    SELECT la.id, la.wallet_id, la.asset_code, count(e.id) AS n_entries,\n"
     "           COALESCE(sum(CASE e.direction WHEN 'credit' THEN e.amount ELSE -e.amount END), 0) AS open_amount\n"
     "      FROM ledger_accounts la\n"
     "      LEFT JOIN ledger_entries e ON e.ledger_account_id = la.id AND e.tenant_id = la.tenant_id\n",
     "    SELECT la.id, la.wallet_id, la.asset_code, (SELECT count(*) FROM ledger_entries x WHERE x.ledger_account_id = la.id) AS n_entries,\n"
     "           COALESCE(max(e.credit_total - e.debit_total), 0) AS open_amount\n"
     "      FROM ledger_accounts la\n"
     "      LEFT JOIN wallet_balance_projection e ON e.ledger_account_id = la.id AND e.tenant_id = la.tenant_id\n", RD),
    ('D3M12_submitted_treated_terminal', 'internal/reconciliation/withdrawal_hold.go',
     "           w.state IN ('rejected', 'cancelled', 'failed', 'completed', 'reversed') AS term\n",
     "           w.state IN ('rejected', 'cancelled', 'failed', 'completed', 'reversed', 'submitted') AS term\n", RD),
    ('D3M13_alert_not_raised', 'internal/reconciliation/alerts.go',
     '	if holds > 0 {\n', '	if false && holds > 0 {\n', RD),
    ('D3M14_hold_findings_counted_as_drift', 'internal/reconciliation/alerts.go',
     '		case MismatchKindWithdrawalHoldOpenMismatch, MismatchKindWithdrawalRequestLedgerLinkInvalid:\n',
     '		case "never":\n', RD),
    ('D3M15_compare_two_statements_again', 'internal/reconciliation/reconciliation.go',
     '		rebuilt, projected, err := rebuildAndProject(ctx, tx, accountID)\n',
     '		rebuilt, _, err := rebuildAndProject(ctx, tx, accountID)\n'
     '		var projected accountBalance\n'
     '		if err == nil {\n'
     '			_, projected, err = rebuildAndProject(ctx, tx, accountID)\n'
     '		}\n', RD),
]

out = open(OUT, 'a')
for mid, path, old, new, (pkg, run) in MUTANTS:
    if FILTER and not mid.startswith(FILTER):
        continue
    fp = os.path.join(W, path)
    orig = open(fp, 'rb').read()
    s = orig.decode()
    assert s.count(old) == 1, (mid, s.count(old))
    open(fp, 'w').write(s.replace(old, new))
    try:
        r = subprocess.run(['bash', S + '/priv_test.sh', '-tags', 'integration', '-count=1', '-p', '1', '-timeout', '1200s',
                            '-run', run, './internal/' + pkg + '/'], env=ENV, capture_output=True, text=True)
        log = r.stdout + r.stderr
        open(os.path.join(S, 's23mut', mid + '.log'), 'w').write(log)
        killers = [l.split()[2] for l in log.splitlines() if l.startswith('--- FAIL')]
        if r.returncode != 0:
            line = '%s KILLED by: %s' % (mid, ' '.join(killers) or '(build/vet failure: ' + next((l for l in log.splitlines() if l.strip()), '') + ')')
        else:
            line = '%s SURVIVED' % mid
    finally:
        open(fp, 'wb').write(orig)
    print(line)
    out.write(line + '\n')
    out.flush()
