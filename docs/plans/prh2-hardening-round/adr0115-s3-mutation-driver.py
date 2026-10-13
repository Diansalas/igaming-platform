"""ADR 0115 S3 mutation driver (usage measurement, internal/rg/limits_usage*.go). Each mutant replaces ONE anchor
(which must occur exactly once), runs the S3 suite (stops at the first failing test), restores the file.
KILLED = the suite failed. The integration suite builds throwaway scratch databases from the worktree's
migration files on every run, and runs every measurement as the RUNTIME role. Run from the worktree root:
    PRIV_DB=<name> python3 docs/plans/prh2-hardening-round/adr0115-s3-mutation-driver.py [name-prefix ...]
The performance/EXPLAIN test (TestRGUsageS3ExplainAndLatency) is excluded from the per-mutant suite (it is
slow and structural); mutants of the SQL shape that only it can catch are run with the PERF suite (marked P).
"""
import os
import subprocess
import sys

W = os.getcwd()
S = '/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad'
DB = os.environ.get('PRIV_DB', 'adr0115s3mut')
BASE = ['env', 'PRIV_DB=' + DB, 'PRIV_SRC=' + W, 'bash', S + '/priv_test.sh', '-tags', 'integration', '-p', '1', '-count=1', '-failfast']
SUITE = BASE + ['-run', 'TestRGUsageS3$|TestRGWindowStartsS3|TestRGDepositUsageS3|TestRGSessionUsageS3|TestRGEffectiveLimitInputsS3|TestS3', './internal/rg/']
PERF = BASE + ['-timeout', '20m', '-run', 'TestRGUsageS3ExplainAndLatency', './internal/rg/']

WIN = 'internal/rg/limits_usage_window.go'
USE = 'internal/rg/limits_usage.go'
SES = 'internal/rg/limits_usage_session.go'
INP = 'internal/rg/limits_usage_inputs.go'
CRE = 'internal/rg/limits_usage_creditcheck.go'

M = [
    # --- window starts (section 1.2) ---
    ('W1 rolling 24h is 23h', WIN, "a.t - interval '24 hours'", "a.t - interval '23 hours'", SUITE),
    ('W2 rolling 7d is 167h', WIN, "a.t - interval '168 hours'", "a.t - interval '167 hours'", SUITE),
    ('W3 rolling 30d is 719h', WIN, "a.t - interval '720 hours'", "a.t - interval '719 hours'", SUITE),
    ('W4 rolling uses a DST-sensitive day', WIN, "a.t - interval '24 hours'", "a.t - interval '1 day'", SUITE),
    ('W5 calendar day truncates to week', WIN, "WHEN 'calendar_day'   THEN date_trunc('day', loc.l)", "WHEN 'calendar_day'   THEN date_trunc('week', loc.l)", SUITE),
    ('W6 calendar month truncates to day', WIN, "WHEN 'calendar_month' THEN date_trunc('month', loc.l)", "WHEN 'calendar_month' THEN date_trunc('day', loc.l)", SUITE),
    ('W7 week uses dow (Sunday=0) not isodow', WIN, "extract(isodow FROM loc.l)", "extract(dow FROM loc.l)", SUITE),
    ('W8 week modulo dropped +7', WIN, "- w.ws + 7) %% 7)", "- w.ws) %% 7)", SUITE),
    ('W9 week start ignored (always Monday)', WIN, "- w.ws + 7) %% 7)", "- 1 + 7) %% 7)", SUITE),
    ('W10 DST overlap probe off (first occurrence missed)', WIN, "FROM generate_series(0, 4) k", "FROM generate_series(0, 0) k", SUITE),
    ('W11 DST probe picks the latest', WIN, "SELECT min(c) AS s FROM generate_series", "SELECT max(c) AS s FROM generate_series", SUITE),
    ('W12 DST probe boundary exclusive', WIN, "(x.c AT TIME ZONE w.tz) >= mid.m) cal)", "(x.c AT TIME ZONE w.tz) > mid.m) cal)", SUITE),
    ('W13 DST probe steps forward', WIN, "- k * interval '30 minutes'", "+ k * interval '30 minutes'", SUITE),
    ('W14 calendar timezone ignored (UTC)', WIN, "SELECT a.t AT TIME ZONE w.tz AS l", "SELECT a.t AT TIME ZONE 'UTC' AS l", SUITE),
    ('W15 local midnight converted in UTC', WIN, "SELECT min(c) AS s FROM generate_series(0, 4) k\n\t        CROSS JOIN LATERAL (SELECT (mid.m AT TIME ZONE w.tz)",
     "SELECT min(c) AS s FROM generate_series(0, 4) k\n\t        CROSS JOIN LATERAL (SELECT (mid.m AT TIME ZONE 'UTC')", SUITE),
    ('W16 windowArgs: rolling windows carry the caller tz', WIN, "tz, ws := \"UTC\", int32(1)\n\t\tif s.isCalendar() {\n\t\t\ttz = s.CalendarTZ\n\t\t}",
     "tz, ws := \"UTC\", int32(1)\n\t\ttz = s.CalendarTZ", SUITE),
    ('W17 calendar window validation off', WIN, "if w.CalendarTZ == \"\" || len(w.CalendarTZ) > 64 {", "if false {", SUITE),
    ('W18 week_start range check off', WIN, "if w.Period == PeriodCalendarWeek && (w.WeekStart < 1 || w.WeekStart > 7) {", "if false {", SUITE),
    ('W19 window count bound off', WIN, "if len(specs) == 0 || len(specs) > MaxUsageWindows {", "if len(specs) == 0 {", SUITE),
    # --- wager / loss (section 1.4, 3.4) ---
    ('U1 window start exclusive (wager)', USE, "SUM(sg) FILTER (WHERE wager_row AND eff >= %s AND acct = $2)::text AS aw%d", "SUM(sg) FILTER (WHERE wager_row AND eff > %s AND acct = $2)::text AS aw%d", SUITE),
    ('U2 window start exclusive (loss)', USE, "SUM(sg) FILTER (WHERE eff >= %s AND acct = $2)::text AS al%d", "SUM(sg) FILTER (WHERE eff > %s AND acct = $2)::text AS al%d", SUITE),
    ('U3 prefilter start exclusive', USE, "AND le.created_at >= (SELECT least(` + strings.Join(least, \", \") + `) FROM win)\n\t   AND le.created_at <= (SELECT t FROM asof)\n),\nscan AS",
     "AND le.created_at > (SELECT least(` + strings.Join(least, \", \") + `) FROM win)\n\t   AND le.created_at <= (SELECT t FROM asof)\n),\nscan AS", SUITE),
    ('U4 posting after the instant counted', USE, "AND le.created_at <= (SELECT t FROM asof)\n),\nscan AS", "AND true\n),\nscan AS", SUITE),
    ('U5 sign flipped', USE, "CASE WHEN direction = 'debit' THEN amount ELSE -amount END AS sg", "CASE WHEN direction = 'credit' THEN amount ELSE -amount END AS sg", SUITE),
    ('U6 reversal counted in its own window, not its original\'s', USE, "CASE WHEN is_rev THEN oc ELSE created_at END AS eff", "created_at AS eff", SUITE),
    ('U7 wager ignores reversals of stakes', USE, "(tt = ANY($11::text[]) OR (is_rev AND ot = ANY($11::text[]))) AS wager_row", "(tt = ANY($11::text[])) AS wager_row", SUITE),
    ('U8 wager nets every reversal (rollback of a win)', USE, "(tt = ANY($11::text[]) OR (is_rev AND ot = ANY($11::text[]))) AS wager_row", "(tt = ANY($11::text[]) OR is_rev) AS wager_row", SUITE),
    ('U9 sportsbook_void original not resolved (no correlation branch)', USE, "WHERE lt.transaction_type = 'sportsbook_void' AND lt.reverses_transaction_id IS NULL", "WHERE false AND lt.transaction_type = 'sportsbook_void' AND lt.reverses_transaction_id IS NULL", SUITE),
    ('U10 void original any type', USE, "AND o2.transaction_type = 'sportsbook_bet'\n\t        ) u", "AND true\n\t        ) u", SUITE),
    ('U11 explicit reverses_transaction_id original not resolved', USE, "WHERE lt.transaction_type = ANY($10::text[]) AND o1.id = lt.reverses_transaction_id", "WHERE false AND lt.transaction_type = ANY($10::text[]) AND o1.id = lt.reverses_transaction_id", SUITE),
    ('U12 account figure not restricted to the anchor (wager)', USE, "FILTER (WHERE wager_row AND eff >= %s AND acct = $2)", "FILTER (WHERE wager_row AND eff >= %s)", SUITE),
    ('U13 account figure not restricted to the anchor (loss)', USE, "FILTER (WHERE eff >= %s AND acct = $2)", "FILTER (WHERE eff >= %s)", SUITE),
    ('U14 Person set joined by account only', USE, "THEN a.person_id = (SELECT x.person_id FROM player_accounts x WHERE x.id = $2 AND x.tenant_id = $1)", "THEN a.id = $2", SUITE),
    ('U15 Person set ignores the tenant', USE, "WHERE a.tenant_id = $1\n\t   AND CASE WHEN $8", "WHERE true\n\t   AND CASE WHEN $8", SUITE),
    ('U16 asset filter dropped (entries)', USE, "AND le.asset_code = $3\n\t   AND le.created_at >= (SELECT least(` + strings.Join(least, \", \") + `) FROM win)\n\t   AND le.created_at <= (SELECT t FROM asof)\n),\nscan AS",
     "AND true\n\t   AND le.created_at >= (SELECT least(` + strings.Join(least, \", \") + `) FROM win)\n\t   AND le.created_at <= (SELECT t FROM asof)\n),\nscan AS", SUITE),
    ('U17 loss type set includes deposit-side types', USE, "string(ledger.TxSportsbookRollback),\n\t}\n\t// RGCashAccountTypes", "string(ledger.TxSportsbookRollback), string(ledger.TxBonusConversion),\n\t}\n\t// RGCashAccountTypes", SUITE),
    ('U18 loss type set loses casino_win', USE, "string(ledger.TxCasinoBet), string(ledger.TxCasinoWin), string(ledger.TxCasinoRollback),\n\t\tstring(ledger.TxSportsbookBet)", "string(ledger.TxCasinoBet), string(ledger.TxCasinoRollback),\n\t\tstring(ledger.TxSportsbookBet)", SUITE),
    ('U19 wager measured accounts ignore the flag (always bonus)', USE, "measuredWager := inSet(RGCashAccountTypes, *accountType) || (req.WagerCountsBonus && inSet(RGBonusAccountTypes, *accountType))",
     "measuredWager := inSet(RGCashAccountTypes, *accountType) || inSet(RGBonusAccountTypes, *accountType)", SUITE),
    ('U20 loss measured accounts ignore the flag (never bonus)', USE, "measuredLoss := inSet(RGCashAccountTypes, *accountType) || (req.LossCountsBonus && inSet(RGBonusAccountTypes, *accountType))",
     "measuredLoss := inSet(RGCashAccountTypes, *accountType)", SUITE),
    ('U21 wager flag read from the loss flag', USE, "(req.WagerCountsBonus && inSet(RGBonusAccountTypes", "(req.LossCountsBonus && inSet(RGBonusAccountTypes", SUITE),
    ('U22 locked accounts measured', USE, "RGIgnoredAccountTypes = []string{string(ledger.AccountPlayerLockedCash), string(ledger.AccountPlayerLockedBonus)}", "RGIgnoredAccountTypes = []string{}\n\tRGCashAccountTypes = append(RGCashAccountTypes, string(ledger.AccountPlayerLockedCash))", SUITE),
    ('U23 undeclared leg ignored', USE, "if !known {", "if false {", SUITE),
    ('U31 casino cash-lock shape ignored', USE, "if undeclaredShape != nil && *undeclaredShape > 0 {", "if false {", SUITE),
    ('U32 cash-lock shape check widened to sportsbook types', USE, "AND tt IN ('casino_bet', 'casino_win', 'casino_rollback')) AS undeclared_shape",
     "AND tt IN ('casino_bet', 'casino_win', 'casino_rollback', 'sportsbook_bet', 'sportsbook_settlement', 'sportsbook_void')) AS undeclared_shape", SUITE),
    ('U24 unresolved reversal ignored', USE, "if unresolved != nil && *unresolved > 0 {", "if false {", SUITE),
    ('U25 loss floor inverted', USE, "if !mayGoNegative && raw.Sign() < 0 {", "if mayGoNegative && raw.Sign() < 0 {", SUITE),
    ('U26 loss floor never applied', USE, "if !mayGoNegative && raw.Sign() < 0 {", "if false {", SUITE),
    ('U27 loss floor flag ignored (always attested)', USE, "w.Loss.Account = floorLoss(w.LossRaw.Account, req.LossMayGoNegative)", "w.Loss.Account = floorLoss(w.LossRaw.Account, true)", SUITE),
    ('U28 Person loss not floored', USE, "w.Loss.Person = floorLoss(w.LossRaw.Person, req.LossMayGoNegative)", "w.Loss.Person = new(big.Int).Set(w.LossRaw.Person)", SUITE),
    ('U29 float parse of minor units', USE, "n, ok := new(big.Int).SetString(strings.TrimSpace(*s), 10)", "f, _ := new(big.Float).SetString(strings.TrimSpace(*s))\n\tn, ok := new(big.Int), true\n\tf.Int(n)", SUITE),
    ('U30 empty account set is zero usage', USE, "if !seen || len(out.AccountIDs) == 0 {\n\t\treturn StakeUsage{}, ErrUsageAccountNotFound", "if !seen {\n\t\treturn StakeUsage{}, ErrUsageAccountNotFound", SUITE),
    # --- guards ---
    ('G1 isolation guard off', USE, "if iso != \"read committed\" {", "if false {", SUITE),
    ('G2 tenant mismatch allowed', USE, "if tenant != tenantID.String() || player != \"\" {", "if player != \"\" {", SUITE),
    ('G3 player scope allowed', USE, "if tenant != tenantID.String() || player != \"\" {", "if tenant != tenantID.String() {", SUITE),
    ('G4 request validation off', USE, "if r.Scope != LimitScopeAccount && r.Scope != LimitScopeTenantPerson {", "if false {", SUITE),
    # --- deposit (section 1.4) ---
    ('D1 in-flight includes declined', USE, "di.status IN ('pending', 'ambiguous')", "di.status IN ('pending', 'ambiguous', 'declined')", SUITE),
    ('D2 in-flight includes failed', USE, "di.status IN ('pending', 'ambiguous')", "di.status IN ('pending', 'ambiguous', 'failed')", SUITE),
    ('D3 in-flight drops ambiguous', USE, "di.status IN ('pending', 'ambiguous')", "di.status IN ('pending')", SUITE),
    ('D4 in-flight counts a posted intent again', USE, "AND di.ledger_transaction_id IS NULL\n", "AND true\n", SUITE),
    ('D5 candidate intent not excluded', USE, "AND ($9::uuid IS NULL OR di.id <> $9::uuid)", "AND true", SUITE),
    ('D6 ledger deposits not restricted to type deposit', USE, "WHERE lt.transaction_type = 'deposit'\n),", "WHERE true\n),", SUITE),
    ('D7 ledger credit window start exclusive', USE, "AND account_type = 'player_cash' AND created_at >= %s AND acct = $2)::text AS la%d", "AND account_type = 'player_cash' AND created_at > %s AND acct = $2)::text AS la%d", SUITE),
    ('D8 in-flight window start exclusive', USE, "SUM(amount) FILTER (WHERE created_at >= %s AND acct = $2)::text AS ia%d", "SUM(amount) FILTER (WHERE created_at > %s AND acct = $2)::text AS ia%d", SUITE),
    ('D9 in-flight account figure not restricted to the anchor', USE, "SUM(amount) FILTER (WHERE created_at >= %s AND acct = $2)::text AS ia%d", "SUM(amount) FILTER (WHERE created_at >= %s)::text AS ia%d", SUITE),
    ('D10 deposit account type check off', USE, "if accountType != nil && *accountType != string(ledger.AccountPlayerCash) {", "if false {", SUITE),
    ('D11 in-flight taken on every group row', USE, "if !inflightTaken {", "if true {", SUITE),
    ('D12 in-flight ignored in the total', USE, "w.Total.Account.Add(w.Credited.Account, w.InFlight.Account)", "w.Total.Account.Set(w.Credited.Account)", SUITE),
    ('D13 Person in-flight ignored', USE, "w.Total.Person.Add(w.Credited.Person, w.InFlight.Person)", "w.Total.Person.Set(w.Credited.Person)", SUITE),
    ('D14 intents of any tenant (tenant predicate dropped)', USE, "FROM deposit_intents di\n\t WHERE di.tenant_id = $1\n", "FROM deposit_intents di\n\t WHERE true\n", SUITE),
    # --- session (section 1.5) ---
    ('S1 root is the starting session', SES, "FROM up ORDER BY depth DESC LIMIT 1) r", "FROM up ORDER BY depth ASC LIMIT 1) r", SUITE),
    ('S2 revoked chains counted as open', SES, "AND s.replaced_by_session_id IS NULL AND s.revoked_at IS NULL AND s.expires_at > asof.t", "AND s.replaced_by_session_id IS NULL AND s.expires_at > asof.t", SUITE),
    ('S3 expired chains counted as open', SES, "AND s.replaced_by_session_id IS NULL AND s.revoked_at IS NULL AND s.expires_at > asof.t", "AND s.replaced_by_session_id IS NULL AND s.revoked_at IS NULL", SUITE),
    ('S4 aggregation keeps the later root', SES, "if earliest != nil && earliest.Before(agg) {", "if earliest != nil && earliest.After(agg) {", SUITE),
    ('S5 aggregation ignores the measured chain', SES, "agg := *root\n", "agg := time.Time{}\n\tif earliest == nil {\n\t\tagg = *root\n\t} else {\n\t\tagg = *earliest\n\t}\n", SUITE),
    ('S6 negative elapsed not clamped', SES, "if d < 0 {\n\t\treturn 0\n\t}\n", "", SUITE),
    ('S7 principal check off', SES, "if principal != req.PlayerAccountID.String() {", "if false {", SUITE),
    ('S8 missing chain is zero elapsed', SES, "if errors.Is(err, pgx.ErrNoRows) {\n\t\treturn SessionUsage{}, ErrSessionChainNotFound\n\t}", "if errors.Is(err, pgx.ErrNoRows) {\n\t\treturn SessionUsage{AsOf: time.Now()}, nil\n\t}", SUITE),
    ('S9 chain depth bound ignored', SES, "if *depth >= maxSessionChainDepth {", "if false {", SUITE),
    ('S10 elapsed rounds up', SES, "return int64(d / time.Second)", "return int64((d + time.Second - 1) / time.Second)", SUITE),
    ('S11 session of another tenant readable', SES, "WHERE s.id = $1 AND s.tenant_id = $2 AND s.principal_type = 'player' AND s.principal_id = $3", "WHERE s.id = $1 AND s.principal_type = 'player' AND s.principal_id = $3", SUITE),
    # --- inputs ---
    ('I1 window timezone dropped from the config windows', INP, "w.CalendarTZ = *c.CalendarTZ", "w.CalendarTZ = \"UTC\"", SUITE),
    ('I2 week start dropped from the config windows', INP, "w.WeekStart = *c.WeekStart", "w.WeekStart = 1", SUITE),
    ('I3 realized rows of other kinds leak', INP, "if st.Kind != kind {\n\t\t\tcontinue\n\t\t}", "", SUITE),
    ('I4 realized rows of other assets leak', INP, "if (st.AssetCode == nil) != (asset == nil) || (asset != nil && *st.AssetCode != *asset) {\n\t\t\tcontinue\n\t\t}", "", SUITE),
    ('I5 TTL none still reported', INP, "if !deref(ttlNone) {\n\t\tcfg.PendingIntentTTLSeconds = ttl\n\t}", "cfg.PendingIntentTTLSeconds = ttl", SUITE),
    ('I6 missing floor looks configured', INP, "if !cfg.Found {\n\t\treturn UsageConfig{Kind: kind, AssetCode: asset}, nil\n\t}", "cfg.Found = true", SUITE),
    ('I7 loss-may-go-negative defaults true', INP, "cfg.CountBonusFunded, cfg.LossMayGoNegative = countBonus, lossNeg", "cfg.CountBonusFunded, cfg.LossMayGoNegative = countBonus, func() *bool { b := true; return &b }()", SUITE),
    # --- credit check ---
    ('C1 succeeded without posting ignored', CRE, "if status == \"succeeded\" {\n\t\t\treturn MismatchSucceededWithoutPosting\n\t\t}", "if false {\n\t\t\treturn MismatchSucceededWithoutPosting\n\t\t}", SUITE),
    ('C2 posting on unsettled intent ignored', CRE, "case status != \"succeeded\":\n\t\treturn MismatchPostingOnUnsettledIntent", "case false:\n\t\treturn MismatchPostingOnUnsettledIntent", SUITE),
    ('C3 amount mismatch ignored', CRE, "if !ok || want.Cmp(got) != 0 {", "if !ok {", SUITE),
    ('C4 account mismatch ignored', CRE, "if len(accounts) != 1 || accounts[0] != player {", "if false {", SUITE),
    ('C5 asset mismatch ignored', CRE, "if len(assets) != 1 || assets[0] != asset {", "if false {", SUITE),
    ('C6 wrong posting type ignored', CRE, "case txType == nil || *txType != \"deposit\":", "case false:", SUITE),
    ('C7 credit check not tenant scoped', CRE, "WHERE di.tenant_id = $1 AND di.created_at >= $2", "WHERE $1::uuid IS NOT NULL AND di.created_at >= $2", SUITE),
    ('C8 range upper bound inclusive', CRE, "AND di.created_at < $3", "AND di.created_at <= $3", SUITE),
    # --- plan shape (only the performance test sees these) ---
    ('P1 entries scan not planned on its own (joined)', USE, "FROM ledger_entries le\n\t WHERE le.tenant_id = $1\n\t   AND le.player_account_id = ANY(ARRAY(SELECT id FROM accts))\n\t   AND le.asset_code = $3\n\t   AND le.created_at >= (SELECT least(` + strings.Join(least, \", \") + `) FROM win)\n\t   AND le.created_at <= (SELECT t FROM asof)\n),\nscan AS",
     "FROM ledger_entries le, ledger_accounts lax\n\t WHERE lax.id = le.ledger_account_id AND le.tenant_id = $1\n\t   AND le.player_account_id = ANY(ARRAY(SELECT id FROM accts))\n\t   AND le.asset_code = $3\n\t   AND le.created_at >= (SELECT least(` + strings.Join(least, \", \") + `) FROM win)\n\t   AND le.created_at <= (SELECT t FROM asof)\n),\nscan AS", PERF),
]

only = sys.argv[1:]
# A killed run (SIGKILL cannot be caught) leaves its mutant in place: refuse to start on a dirty
# mutation target, so a stale mutant can never be measured or committed as the baseline.
_targets = sorted({m[1] for m in M})
_dirty = subprocess.run(['git', 'status', '--porcelain', '--'] + _targets, cwd=W, capture_output=True, text=True).stdout.strip()
if _dirty:
    sys.exit('mutation targets are not clean (restore them from git first):\n' + _dirty)
for name, f, old, new, cmd in M:
    if only and not any(name.startswith(o) for o in only):
        continue
    path = os.path.join(W, f)
    src = open(path).read()
    if src.count(old) != 1:
        print('SKIP (anchor count %d): %s' % (src.count(old), name), flush=True)
        continue
    if os.environ.get('DRY'):
        continue
    open(path, 'w').write(src.replace(old, new))
    try:
        r = subprocess.run(cmd, cwd=W, capture_output=True, text=True, timeout=1800)
        out = r.stdout + r.stderr
        verdict = 'KILLED' if r.returncode != 0 else 'SURVIVED'
        if '[build failed]' in out or '[setup failed]' in out:
            verdict = 'INVALID(does not compile)'
        first = ''
        for line in out.splitlines():
            if ('--- FAIL' in line and '/' in line) or 'want SQLSTATE' in line or ('.go:' in line and ('want' in line or 'must' in line)) or 'build failed' in line or 'cannot use' in line or 'undefined' in line:
                first = line.strip()[:170]
                break
    finally:
        open(path, 'w').write(src)
    print('%s: %s%s' % (verdict, name, ('  [' + first + ']') if first else ''), flush=True)
