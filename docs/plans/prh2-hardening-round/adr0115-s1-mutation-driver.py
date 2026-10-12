"""ADR 0115 S1 mutation driver (migration 0135 + Go). Each mutant replaces ONE anchor (which must
occur exactly once), runs the suite, restores the file. KILLED = the suite failed. The integration
suite migrates throwaway scratch databases from the worktree's migration files on every run, so a
mutated migration is exercised as written. Run from the worktree root:
    python3 docs/plans/prh2-hardening-round/adr0115-s1-mutation-driver.py [name-prefix ...]
"""
import os
import subprocess
import sys

W = os.getcwd()
S = '/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad'
UP = 'migrations/0135_rg_limits_schema_and_index.up.sql'
DOWN = 'migrations/0135_rg_limits_schema_and_index.down.sql'
DB_TEST = ['env', 'PRIV_DB=adr0115s1', 'PRIV_SRC=' + W, 'bash', S + '/priv_test.sh', '-tags', 'integration', '-p', '1',
           '-count=1', '-run', 'TestRGLimitsS1|TestMigration0135', './internal/rg/']
UNIT = ['go', 'test', '-count=1', './internal/config/', './internal/db/', '-run', 'AppEnvironment|DropURLAppEnvironment']

M = [
    # --- environment anchor / synthetic guard (security condition 8) ---
    ('E1 GUC narrowing dropped (anchor alone)', UP,
     "       AND COALESCE(NULLIF(pg_catalog.current_setting('app.environment', true), '') IN ('development', 'test'), false)\n$$;",
     "\n$$;", DB_TEST),
    ('E2 anchor dropped (GUC alone, forgeable)', UP,
     "    SELECT public.rg_database_environment() IN ('development', 'test')\n       AND COALESCE",
     "    SELECT true\n       AND COALESCE", DB_TEST),
    ('E3 missing anchor defaults to test', UP, "WHERE e.singleton), 'production')", "WHERE e.singleton), 'test')", DB_TEST),
    ('E4 anchor insert guard: owner-membership check off (grant still denies)', UP,
     "    IF NOT pg_catalog.pg_has_role(current_user,", "    IF false AND NOT pg_catalog.pg_has_role(current_user,", DB_TEST),
    ('E5 near-synthetic attestation admitted', UP,
     "AND (NEW.attestation_ref <> 'synthetic' OR NOT public.rg_synthetic_config_allowed())",
     "AND (NOT public.rg_synthetic_config_allowed())", DB_TEST),
    ('E6 synthetic trigger off', UP,
     "    IF pg_catalog.strpos(pg_catalog.lower(NEW.attestation_ref), 'synthetic') > 0",
     "    IF false AND pg_catalog.strpos(pg_catalog.lower(NEW.attestation_ref), 'synthetic') > 0", DB_TEST),
    ('E7 compose ignores the synthetic filter', UP,
     "           AND (c.attestation_ref <> 'synthetic' OR public.rg_synthetic_config_allowed())",
     "           AND true", DB_TEST),
    ('E8 runtime granted INSERT on the anchor', UP,
     "'GRANT SELECT ON database_environment TO igaming_runtime'", "'GRANT SELECT, INSERT ON database_environment TO igaming_runtime'", DB_TEST),
    ('E9 Go: app.environment not the guarded environment', 'internal/config/config.go',
     "RuntimeParamAppEnvironment:               c.GuardEnvironment(),", "RuntimeParamAppEnvironment:               \"test\",", UNIT),
    ('E10 Go: URL app.environment kept', 'internal/db/app_environment.go', "\t\t\tdelete(rp, k)\n", "\t\t\t_ = k\n", UNIT),
    # --- backdating ---
    ('B1 backdate tolerance 5 h', UP, "NEW.effective_from < pg_catalog.clock_timestamp() - interval '5 seconds'",
     "NEW.effective_from < pg_catalog.clock_timestamp() - interval '5 hours'", DB_TEST),
    # --- floor four-eyes (security condition 9) ---
    ('F1 apply accepts a non-approved request', UP, "    IF NOT FOUND OR v_req.status <> 'approved' OR", "    IF NOT FOUND OR", DB_TEST),
    ('F2 apply skips the payload hash comparison', UP, "    IF v_hash IS DISTINCT FROM v_req.payload_hash THEN", "    IF false THEN", DB_TEST),
    ('F3 decision allows the requester Person', UP, "        IF v_actor.person_id = OLD.requested_by_person_id THEN", "        IF false THEN", DB_TEST),
    ('F4 apply proof never checked', UP, "'rg_limit_config:apply',\n        v_req.id::text, v_hash);",
     "'rg_limit_config:apply',\n        v_req.id::text, v_hash) WHERE false;", DB_TEST),
    ('F5 decision proof never checked', UP, "        OLD.id::text, OLD.payload_hash);", "        OLD.id::text, OLD.payload_hash) WHERE false;", DB_TEST),
    ('F6 request proof never checked', UP, "            public.rg_limit_config_request_digest(NEW));",
     "            public.rg_limit_config_request_digest(NEW)) WHERE false;", DB_TEST),
    ('F7 caller payload_hash trusted', UP, "        NEW.payload_hash := v_hash;\n", "        NEW.payload_hash := COALESCE(NEW.payload_hash, v_hash);\n", DB_TEST),
    ('F8 proposal scope not checked', UP,
     "            RAISE EXCEPTION 'rg_limit_config_change_requests: the proposed row is not a floor of this request''s scope' USING ERRCODE = 'RL007';",
     "            NULL;", DB_TEST),
    ('F9 floor needs no request', UP,
     "        RAISE EXCEPTION 'rg_limit_configs: a jurisdiction floor needs an approved change request' USING ERRCODE = 'RL006';",
     "        RETURN NEW;", DB_TEST),
    # --- operator tighten-only / widen-only (security condition 1) ---
    ('T1 ceiling may rise', UP, "(c.ceiling_none IS NOT FALSE OR c.ceiling > p.ceiling)", "(c.ceiling_none IS NOT FALSE OR c.ceiling > p.ceiling + 1000000)", DB_TEST),
    ('T2 ceiling may become none', UP, "(c.ceiling_none IS NOT FALSE OR c.ceiling > p.ceiling)", "(c.ceiling > p.ceiling)", DB_TEST),
    ('T3 cooldown may shorten', UP, "c.increase_cooldown < p.increase_cooldown)", "c.increase_cooldown < p.increase_cooldown - interval '1 hour')", DB_TEST),
    ('T4 enabled may turn off', UP, "WHEN p.enabled AND NOT c.enabled THEN 'enabled'", "WHEN false THEN 'enabled'", DB_TEST),
    ('T5 mandatory may turn off', UP, "WHEN p.mandatory AND NOT c.mandatory THEN 'mandatory'", "WHEN false THEN 'mandatory'", DB_TEST),
    ('T6 periods overlap instead of superset', UP, "NOT (c.allowed_periods @> p.allowed_periods)", "NOT (c.allowed_periods && p.allowed_periods)", DB_TEST),
    ('T7 durations narrowing allowed', UP, "NOT (c.allowed_durations @> p.allowed_durations)", "false", DB_TEST),
    ('T8 intervals narrowing allowed', UP, "NOT (c.allowed_intervals @> p.allowed_intervals)", "false", DB_TEST),
    ('T9 scope may narrow', UP, "WHEN public.rg_scope_rank(c.scope) < public.rg_scope_rank(p.scope) THEN 'scope'", "WHEN false THEN 'scope'", DB_TEST),
    ('T10 reconfirmation may turn off', UP, "WHEN p.increase_requires_reconfirmation IS TRUE AND c.increase_requires_reconfirmation IS NOT TRUE", "WHEN false", DB_TEST),
    ('T11 count_bonus_funded may turn off', UP, "WHEN p.count_bonus_funded IS TRUE AND c.count_bonus_funded IS NOT TRUE", "WHEN false", DB_TEST),
    ('T12 loss may go negative', UP, "WHEN p.control_kind = 'loss' AND p.loss_may_go_negative IS NOT TRUE AND c.loss_may_go_negative IS TRUE", "WHEN false", DB_TEST),
    ('T13 post_session_break may shorten', UP,
     "(c.post_session_break_none IS NOT FALSE OR c.post_session_break < p.post_session_break)", "(c.post_session_break < p.post_session_break - interval '1 hour')", DB_TEST),
    ('T14 chain aggregation may turn off', UP, "WHEN p.concurrent_chain_aggregation IS TRUE AND c.concurrent_chain_aggregation IS NOT TRUE", "WHEN false", DB_TEST),
    ('T15 pending TTL may shorten', UP, "c.pending_intent_ttl < p.pending_intent_ttl THEN", "false THEN", DB_TEST),
    ('T16 brand row ignores its tenant row', UP,
     "(NEW.brand_id IS NOT NULL AND c.tenant_id = NEW.tenant_id AND c.brand_id IS NULL)", "false", DB_TEST),
    ('T17 operator row needs no floor', UP, "    IF NOT v_floor THEN", "    IF false THEN", DB_TEST),
    # --- read-time composition (strictest / UNION / OR) ---
    ('C1 enabled AND instead of OR', UP, "(SELECT bool_or(r.enabled) FROM rows r)", "(SELECT bool_and(r.enabled) FROM rows r)", DB_TEST),
    ('C2 mandatory AND instead of OR', UP, "(SELECT bool_or(r.mandatory) FROM rows r)", "(SELECT bool_and(r.mandatory) FROM rows r)", DB_TEST),
    ('C3 ceiling MAX instead of MIN', UP, "(SELECT min(r.ceiling) FROM rows r WHERE r.ceiling_none IS FALSE)", "(SELECT max(r.ceiling) FROM rows r WHERE r.ceiling_none IS FALSE)", DB_TEST),
    ('C4 periods floor-only instead of UNION', UP,
     "(SELECT array_agg(DISTINCT x ORDER BY x) FROM rows r, unnest(r.allowed_periods) AS x)",
     "(SELECT array_agg(DISTINCT x ORDER BY x) FROM rows r, unnest(r.allowed_periods) AS x WHERE r.tenant_id IS NULL)", DB_TEST),
    ('C5 cooldown MIN instead of MAX', UP, "(SELECT max(r.increase_cooldown) FROM rows r)", "(SELECT min(r.increase_cooldown) FROM rows r)", DB_TEST),
    ('C6 another brand composes in', UP, "(c.tenant_id = p_tenant AND (c.brand_id IS NULL OR c.brand_id = p_brand))", "(c.tenant_id = p_tenant)", DB_TEST),
    ('C7 found without a floor', UP, "        SELECT i.* FROM inforce i WHERE EXISTS (SELECT 1 FROM inforce f WHERE f.tenant_id IS NULL)",
     "        SELECT i.* FROM inforce i WHERE true", DB_TEST),
    ('C8 reconfirmation AND instead of OR', UP, "(SELECT bool_or(r.increase_requires_reconfirmation) FROM rows r)", "(SELECT bool_and(r.increase_requires_reconfirmation) FROM rows r)", DB_TEST),
    # --- player changes: classification / realization (security condition 3) ---
    ('P1 increase effective immediately', UP, "    effective_at := p_at + v_cfg.increase_cooldown;", "    effective_at := p_at;", DB_TEST),
    ('P2 equal value classified decrease', UP, "    ELSIF p_value < v_cur.value THEN\n        change_kind := 'decrease';",
     "    ELSIF p_value <= v_cur.value THEN\n        change_kind := 'decrease';", DB_TEST),
    ('P3 realization <= becomes <', UP, "AND k.effective_at <= LEAST(p_at, k.next_requested_at)", "AND k.effective_at < LEAST(p_at, k.next_requested_at)", DB_TEST),
    ('P4 unconfirmed increase realized', UP, "         WHERE NOT k.requires_confirmation\n", "         WHERE true\n", DB_TEST),
    ('P5 superseded request realized', UP, "AND k.effective_at <= LEAST(p_at, k.next_requested_at)", "AND k.effective_at <= p_at", DB_TEST),
    ('P6 client times accepted', UP, "    IF NEW.requested_at IS NOT NULL OR NEW.effective_at IS NOT NULL THEN", "    IF false THEN", DB_TEST),
    ('P7 asserted change_kind not compared', UP, "    IF v_intent IS NOT NULL AND v_intent <> v_res.change_kind THEN", "    IF false THEN", DB_TEST),
    ('P8 L0.4 not exclusive (shared)', UP, "    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtext('player_restrictions')",
     "    PERFORM pg_catalog.pg_advisory_xact_lock_shared(pg_catalog.hashtext('player_restrictions')", DB_TEST),
    ('P9 requested_at before the lock', UP,
     "    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtext('player_restrictions'), pg_catalog.hashtext(NEW.person_id::text));\n    NEW.requested_at := pg_catalog.clock_timestamp();",
     "    NEW.requested_at := pg_catalog.clock_timestamp();", DB_TEST),
    ('P10 staff increase needs no confirm', UP,
     "    requires_confirmation := v_cfg.increase_requires_reconfirmation OR p_source = 'staff_on_player_request';",
     "    requires_confirmation := v_cfg.increase_requires_reconfirmation;", DB_TEST),
    ('P11 confirm before the cooldown', UP, "        IF p_at < v_latest.effective_at THEN", "        IF false THEN", DB_TEST),
    ('P12 cancel of a non-latest request', UP, "        IF NOT FOUND OR v_latest.id <> p_supersedes THEN", "        IF NOT FOUND THEN", DB_TEST),
    ('P13 isolation guard off', UP, "    IF current_setting('transaction_isolation') <> 'read committed' THEN", "    IF false THEN", DB_TEST),
    ('P14 staff self-target allowed', UP, "    IF v_staff_person IS NOT NULL AND v_staff_person = v_acct.person_id THEN", "    IF false THEN", DB_TEST),
    ('P15 player may write another account', UP,
     "        IF NEW.player_account_id IS DISTINCT FROM v_player::uuid OR NEW.source <> 'player_self_service' THEN",
     "        IF NEW.source <> 'player_self_service' THEN", DB_TEST),
    ('P16 increase without configuration', UP,
     "    IF p_jurisdiction IS NULL OR NOT v_cfg.found OR v_cfg.increase_cooldown IS NULL THEN",
     "    IF false THEN", DB_TEST),
    # --- RLS-blind / RLS-broad mutants ---
    ('R1 classification read blind (staff_read sees nothing)', UP,
     "        NULLIF(current_setting('app.player_account_id', true), '') IS NULL\n        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid\n    );\nCREATE POLICY player_self_read",
     "        NULLIF(current_setting('app.player_account_id', true), '') IS NULL\n        AND false\n    );\nCREATE POLICY player_self_read", DB_TEST),
    ('R2 trigger keeps the player visibility (no swap)', UP,
     "    PERFORM set_config('app.player_account_id', '', true);\n    SELECT j.code", "    SELECT j.code", DB_TEST),
    ('R3 staff_read without tenant predicate', UP,
     "        NULLIF(current_setting('app.player_account_id', true), '') IS NULL\n        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid\n    );\nCREATE POLICY player_self_read",
     "        NULLIF(current_setting('app.player_account_id', true), '') IS NULL\n        AND true\n    );\nCREATE POLICY player_self_read", DB_TEST),
    ('R4 player_self_read: any account', UP,
     "            player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid\n            OR (scope = 'tenant_person'",
     "            true\n            OR (scope = 'tenant_person'", DB_TEST),
    ('R5 operator rows readable by every tenant', UP,
     "    USING (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);",
     "    USING (true);", DB_TEST),
    ('R6 floor read blind', UP,
     "    USING (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);",
     "    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);", DB_TEST),
    ('R7 change requests readable by a tenant session', UP,
     "CREATE POLICY platform_only ON rg_limit_config_change_requests\n    FOR ALL\n    USING (\n        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL",
     "CREATE POLICY platform_only ON rg_limit_config_change_requests\n    FOR ALL\n    USING (\n        true", DB_TEST),
    # --- time_out ---
    ('O1 time_out not pinned to the session tenant', UP,
     "AND (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid))", "AND true)", DB_TEST),
    ('O2 indefinite time_out allowed', UP, "    CHECK (restriction_type <> 'time_out' OR ends_at IS NOT NULL);", "    CHECK (true);", DB_TEST),
    ('O3 brand-scoped player time_out allowed', UP, "(restriction_type = 'time_out' AND brand_id IS NULL", "(restriction_type = 'time_out'", DB_TEST),
    # --- migration mechanics ---
    ('X1 pre-flight accepts a differently defined index', UP,
     "               AND pg_catalog.pg_get_indexdef(i.indexrelid) = r.def;", "               ;", DB_TEST),
    ('X2 down: data refusal off', DOWN, "    IF v_cfg + v_req + v_chg + v_to + v_root > 0 THEN", "    IF false THEN", DB_TEST),
    ('X3 down: counts are RLS-blind (FORCE kept)', DOWN,
     "ALTER TABLE player_restrictions NO FORCE ROW LEVEL SECURITY;\nALTER TABLE casino_launch_sessions", "ALTER TABLE casino_launch_sessions", DB_TEST),
    ('X4 down does not restore 0037 player_self_insert', DOWN,
     "        tenant_id IS NULL\n        AND brand_id IS NULL\n        AND restriction_type = 'self_exclusion'",
     "        brand_id IS NULL\n        AND restriction_type = 'self_exclusion'", DB_TEST),
    ('X5 append-only deny trigger off (player changes)', UP,
     "CREATE TRIGGER player_rg_limit_changes_immutable BEFORE UPDATE OR DELETE ON player_rg_limit_changes",
     "CREATE TRIGGER player_rg_limit_changes_immutable BEFORE UPDATE ON player_rg_limit_changes", DB_TEST),
    # --- review round 1 (security C1-C4 / LOW, ledger-finance 1-6) ---
    ('S-C1a player seq not forced', UP, "    NEW.seq := pg_catalog.nextval('public.player_rg_limit_changes_seq_seq'::regclass);\n", "", DB_TEST),
    ('S-C1b player id not forced', UP, "    NEW.id := pg_catalog.gen_random_uuid();\n    IF current_setting('transaction_isolation')",
     "    IF current_setting('transaction_isolation')", DB_TEST),
    ('S-C2a session-tenant guard no-op', UP,
     "    IF p_tenant IS DISTINCT FROM NULLIF(pg_catalog.current_setting('app.tenant_id', true), '')::uuid THEN", "    IF false THEN", DB_TEST),
    ('S-C2b compose guard dropped', UP, "    SELECT public.rg_assert_session_tenant(p_tenant);\n    WITH inforce AS (", "    WITH inforce AS (", DB_TEST),
    ('S-C2c realized guard dropped', UP, "    SELECT public.rg_assert_session_tenant(p_tenant);\n    WITH k AS (", "    WITH k AS (", DB_TEST),
    ('S-C2d classify guard dropped', UP, "    PERFORM public.rg_assert_session_tenant(p_tenant);\n    IF p_supersedes IS NOT NULL THEN",
     "    IF p_supersedes IS NOT NULL THEN", DB_TEST),
    ('S-C4a time_out narrowing always true', UP, "        OR EXISTS (\n            SELECT 1 FROM player_accounts pa",
     "        OR true OR EXISTS (\n            SELECT 1 FROM player_accounts pa", DB_TEST),
    ('S-C4b narrowing ignores the tenant', UP,
     "               AND pa.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid\n        )\n    );", "        )\n    );", DB_TEST),
    ('S-LOW1 attestation ASCII CHECK dropped', UP, " AND attestation_ref ~ '^[ -~]+$'),", "),", DB_TEST),
    ('S-LOW2 Go: dev/test anchor accepted in production', 'internal/db/app_environment.go',
     '\tif anchor == "development" || anchor == "test" {', '\tif false {',
     ['go', 'test', '-count=1', './internal/db/', '-run', 'EnvironmentAnchor']),
    ('I1 an unconditional index statement (SHARE lock with a pre-built index)', UP,
     "-- 10. (The indexes are section 0b, at the top of this file.)",
     "CREATE INDEX IF NOT EXISTS idx_ledger_entries_tenant_player_asset_created ON ledger_entries (tenant_id, player_account_id, asset_code, created_at);\n--", DB_TEST),
    ('I2 large-table threshold off', UP, "            IF v_rows > 100000 OR v_bytes > 64 * 1024 * 1024 THEN", "            IF false THEN", DB_TEST),
    ('I3 no lock_timeout on the index step', UP, "    PERFORM pg_catalog.set_config('lock_timeout', '5s', true);\n", "", DB_TEST),
    ('I4 pre-flight accepts an INVALID index', UP, "WHERE i.indexrelid = ('public.' || r.name)::regclass AND i.indisvalid AND i.indisready",
     "WHERE i.indexrelid = ('public.' || r.name)::regclass", DB_TEST),
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
        r = subprocess.run(cmd, cwd=W, capture_output=True, text=True, timeout=1200)
        out = r.stdout + r.stderr
        verdict = 'KILLED' if r.returncode != 0 else 'SURVIVED'
        first = ''
        for line in out.splitlines():
            if ('--- FAIL' in line and '/' in line) or 'want SQLSTATE' in line or 'must' in line and '.go:' in line:
                first = line.strip()[:160]
                break
    finally:
        open(path, 'w').write(src)
    print('%s: %s%s' % (verdict, name, ('  [' + first + ']') if first else ''), flush=True)
