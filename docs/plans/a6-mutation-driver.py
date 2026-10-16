#!/usr/bin/env python3
"""A6 mutation pass over the ADR 0115 S4 enforcement logic (hand-written mutants, no framework).

Each mutant replaces ONE exact source string, runs the named integration suites against a private
scratch database, and restores the file. A mutant is KILLED if any suite fails. A compile error is
reported as BUILD (not a kill). Equivalent mutants are justified in docs/plans/a6-rg-enforcement-record.md.

Usage: python3 -I a6-mutation-driver.py <worktree> <runner.sh> <results.tsv> [name ...]
The runner is `bash priv_test.sh` (PRIV_DB and PRIV_SRC exported by the caller).
"""
import subprocess, sys, os

WT, RUNNER, OUT = sys.argv[1], sys.argv[2], sys.argv[3]
ONLY = set(sys.argv[4:])

RG = ("./internal/rg/", r"^TestRGEnforceS4$")
CAS = ("./internal/casino/", r"^TestRGEnforceS4_Casino")
SPB = ("./internal/sportsbook/", r"^TestRGEnforceS4_Sportsbook")
PAY = ("./internal/payments/", r"^TestRGEnforceS4_Deposit")
WD = ("./internal/withdrawal/", r"^TestWDRG1")

ENF = "internal/rg/enforcement.go"
RGG = "internal/rg/rg.go"
SET = "SELECT set_config('lock_timeout', $1, true), set_config('statement_timeout', $2, true)"

M = [
    ("deposit_boundary_ge", ENF, "Cmp(d.value) > 0 {\n\t\t\treturn denial(dec, kindCode", "Cmp(d.value) >= 0 {\n\t\t\treturn denial(dec, kindCode", [RG]),
    ("stake_boundary_ge", ENF, "Cmp(d.value) > 0 {\n\t\t\tf := limitFigures", "Cmp(d.value) >= 0 {\n\t\t\tf := limitFigures", [RG]),
    ("switch_ignored", ENF, "\tif !on {\n\t\t// Limit enforcement is off", "\tif false && !on {\n\t\t// Limit enforcement is off", [RG]),
    ("session_scope_check_removed", ENF, "if session != tenantID.String() {", "if false {", [RG]),
    ("usage_guard_removed", ENF, "if err := usageGuard(ctx, tx, p.TenantID); err != nil {", "if err := error(nil); err != nil {", [RG]),
    ("config_missing_removed", ENF, "\t\tif !cfg.Found {", "\t\tif false {", [RG]),
    ("count_bonus_check_removed", ENF, "if p.Action.casino() && deref(cfg.CountBonusFunded) {", "if false {", [RG]),
    ("session_aggregation_check_removed", ENF, "if sess.Found && sess.Scope == LimitScopeTenantPerson &&", "if false && sess.Found &&", [RG]),
    ("mandatory_ignores_ceiling", ENF, "pl.cfg.Mandatory && !pl.hasValue && pl.cfg.Ceiling == nil", "pl.cfg.Mandatory && !pl.hasValue", [RG]),
    ("mandatory_never", ENF, "pl.cfg.Mandatory && !pl.hasValue && pl.cfg.Ceiling == nil", "false", [RG]),
    ("launch_early_return_removed", ENF, "\tif p.Action == ActionCasinoLaunch {\n\t\treturn dec, nil", "\tif false {\n\t\treturn dec, nil", [RG]),
    ("exclude_candidate_intent_removed", ENF, "ExcludeIntentID: p.CandidateRef,", "", [RG, PAY]),
    ("removal_treated_as_value", ENF, "if !st.HasValue || st.Value == nil {", "if !st.HasValue {", [RG]),
    ("ceiling_not_applied", ENF, 'consider(key{p, scope}, c, "ceiling")', "_, _ = p, c", [RG]),
    ("ceiling_only_for_valued_periods_off", ENF, "\t\tfor p := range periods {\n\t\t\tconsider(", "\t\tfor p := range map[PeriodKind]bool{} {\n\t\t\tconsider(", [RG]),
    ("loss_may_go_negative_forced_true", ENF, "req.LossMayGoNegative = deref(pl.cfg.LossMayGoNegative)", "req.LossMayGoNegative = true", [RG]),
    ("min_becomes_max", ENF, "!ok || v.Cmp(cur.value) < 0 {", "!ok || v.Cmp(cur.value) > 0 {", [RG]),
    ("timeouts_not_restored", ENF, "\t\t_, err := tx.Exec(ctx, `" + SET + "`, prevLock, prevStmt)\n\t\treturn err", "\t\treturn nil", [RG]),
    ("timeouts_not_applied", ENF, "\tif _, err := tx.Exec(ctx, `" + SET + "`,\n\t\tfmt.Sprintf", "\tif _, err := tx.Exec(ctx, `SELECT $1::text, $2::text`,\n\t\tfmt.Sprintf", [RG]),
    ("measure_scope_always_account", ENF, "\t\tif d.scope == LimitScopeTenantPerson {\n\t\t\treturn LimitScopeTenantPerson", "\t\tif d.scope == \"x\" {\n\t\t\treturn LimitScopeTenantPerson", [RG]),
    ("timeout_removed_from_restriction_list", RGG, "AND restriction_type IN ($2, $3)", "AND restriction_type IN ($2, $2)", [RG, CAS]),
    ("timeout_end_inclusive", RGG, "(ends_at IS NULL OR ends_at > clock_timestamp())", "(ends_at IS NULL OR ends_at >= clock_timestamp())", [RG]),
    ("withdrawal_mode_blocks_on_restriction", RGG, "\tif restrictionsBlock {", "\tif true {", [RG, WD]),
    ("wd_gate_denial_ignored", "internal/withdrawal/rggate.go", "\tif d.Allowed {\n\t\treturn d.RestrictionInForce, nil", "\tif true {\n\t\treturn d.RestrictionInForce, nil", [WD]),
    ("wd_gate_error_allowed", "internal/withdrawal/rggate.go", "\t\treturn \"\", fmt.Errorf(\"%w: %w\", ErrGateUnavailable, err)", "\t\treturn \"\", nil", [WD]),
    ("wd_nil_gate_allowed", "internal/withdrawal/rggate.go", "\tif p.Gate == nil {\n\t\treturn \"\", fmt.Errorf", "\tif p.Gate == nil {\n\t\treturn \"\", nil\n\t\treturn \"\", fmt.Errorf", [WD]),
    ("wd_risk_review_allowed", "internal/withdrawalgate/standard/standard.go", "case risk.OutcomeDeny, risk.OutcomeReview:", "case risk.OutcomeDeny:", [WD]),
    ("casino_bet_uses_eligibility_only", "internal/casino/orchestrator.go", "\tdecision, err := rg.EvaluateAction(ctx, tx, p)", "\tdecision, err := rg.EvaluateEligibility(ctx, tx, rg.EligibilityParams{TenantID: p.TenantID, BrandID: p.BrandID, PlayerAccountID: p.PlayerAccountID, WalletID: p.WalletID})", [CAS]),
    ("sportsbook_uses_eligibility_only", "internal/sportsbook/orchestrator.go", "\tdecision, err := rg.EvaluateAction(ctx, tx, p)", "\tdecision, err := rg.EvaluateEligibility(ctx, tx, rg.EligibilityParams{TenantID: p.TenantID, BrandID: p.BrandID, PlayerAccountID: p.PlayerAccountID, WalletID: p.WalletID})", [SPB]),
    ("deposit_t1_launch_shape", "internal/payments/deposit_v2.go", "Action: rg.ActionDeposit, AssetCode: intent.AssetCode, Amount: intent.Amount, CandidateRef: &intent.ID,", "Action: rg.ActionCasinoLaunch, AssetCode: intent.AssetCode, Amount: 0, CandidateRef: &intent.ID,", [PAY]),
    ("deposit_t2_launch_shape", "internal/payments/drive.go", "Action: rg.ActionDeposit, AssetCode: intent.AssetCode, Amount: intent.Amount, CandidateRef: &intent.ID,", "Action: rg.ActionCasinoLaunch, AssetCode: intent.AssetCode, Amount: 0, CandidateRef: &intent.ID,", [PAY]),
]


def run(pkg, pat):
    p = subprocess.run(["bash", RUNNER, "-tags", "integration", "-count=1", "-timeout", "5m", "-run", pat, pkg],
                       cwd=WT, capture_output=True, text=True)
    out = p.stdout + p.stderr
    if p.returncode == 0:
        return "PASS"
    if "build failed" in out:
        return "BUILD"
    return "FAIL"


with open(OUT, "a") as res:
    for name, f, old, new, suites in M:
        if ONLY and name not in ONLY:
            continue
        path = os.path.join(WT, f)
        src = open(path).read()
        if src.count(old) != 1:
            res.write(f"{name}\tNOMATCH({src.count(old)})\n"); res.flush(); continue
        verdict = "SURVIVED"
        try:
            open(path, "w").write(src.replace(old, new, 1))
            for pkg, pat in suites:
                r = run(pkg, pat)
                if r == "BUILD":
                    verdict = f"BUILD({pkg})"; break
                if r == "FAIL":
                    verdict = f"KILLED_BY({pkg})"; break
        finally:
            open(path, "w").write(src)
        res.write(f"{name}\t{verdict}\n"); res.flush()
