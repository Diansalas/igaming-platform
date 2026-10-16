package stagingrunner_test

// Static guards for the staging runner (STAGING-PREP-3). They are deliberately textual (no database, no AWS) and pin the
// properties the security review relies on, so a later edit cannot quietly put a secret on a command line, make the
// verification pack write, drop a denial probe, or bake a key into the image. The behavioural proof is the local harness run
// recorded in docs/plans/staging-prep-3-record.md. Planted-offender tests show the guards bite.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func root(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func glob(t *testing.T, pattern string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(root(t), pattern))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range m {
		rel, _ := filepath.Rel(root(t), f)
		out = append(out, filepath.ToSlash(rel))
	}
	sort.Strings(out)
	return out
}

func code(s string) string { // drop whole-line comments (#, --)
	var out []string
	for _, l := range strings.Split(s, "\n") {
		tl := strings.TrimSpace(l)
		if strings.HasPrefix(tl, "#") || strings.HasPrefix(tl, "--") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// runnerShell are the files that run inside the task (the local harness is a development tool and is excluded).
func runnerShell(t *testing.T) []string {
	t.Helper()
	fs := append(glob(t, "deploy/staging-runner/*.sh"), glob(t, "deploy/staging-runner/modes/*.sh")...)
	if len(fs) < 8 {
		t.Fatalf("vacuous: only %d runner shell files found", len(fs))
	}
	return fs
}

var modes = []string{"backup-restore-drill", "cleanup", "pitr-verify", "probe-backup-role", "record-anchor", "seed-actor-proof", "seed-market", "seed-world", "verify-db"}

func TestModesAreConsistentAcrossEntrypointFilesAndDeploySh(t *testing.T) {
	entry := read(t, "deploy/staging-runner/entrypoint.sh")
	deploy := read(t, "deploy/aws/scripts/deploy.sh")
	var files []string
	for _, f := range glob(t, "deploy/staging-runner/modes/*.sh") {
		files = append(files, strings.TrimSuffix(filepath.Base(f), ".sh"))
	}
	if strings.Join(files, ",") != strings.Join(modes, ",") {
		t.Fatalf("mode files %v differ from the expected modes %v", files, modes)
	}
	for _, m := range modes {
		if !strings.Contains(entry, m) {
			t.Errorf("entrypoint.sh does not list mode %s", m)
		}
		if !regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(m) + `\)\s+echo "`).MatchString(deploy) {
			t.Errorf("deploy.sh runner_mode_params has no entry for mode %s", m)
		}
		fn := "mode_" + strings.ReplaceAll(m, "-", "_") + "()"
		if !strings.Contains(read(t, "deploy/staging-runner/modes/"+m+".sh"), fn) {
			t.Errorf("modes/%s.sh does not define %s", m, fn)
		}
	}
	if !strings.Contains(entry, `"$#" -gt 1`) {
		t.Error("entrypoint.sh must refuse more than one command argument (parameters are environment overrides)")
	}
	if !strings.Contains(entry, "RUNNER|END") && !strings.Contains(read(t, "deploy/staging-runner/lib.sh"), "RUNNER|END") {
		t.Error("the runner must always print RUNNER|END (deploy.sh waits for it)")
	}
}

func TestNoSecretExposurePatternsInRunnerShell(t *testing.T) {
	bad := []struct {
		name string
		re   *regexp.Regexp
	}{
		{"shell tracing", regexp.MustCompile(`(?mi)(^|\s)set\s+-[a-z]*x|bash\s+-x|xtrace`)},
		{"PGPASSWORD", regexp.MustCompile(`PGPASSWORD`)},
		{"a password in a URL", regexp.MustCompile(`postgres(ql)?://[^\s"'@/]*:[^\s"'@/]+@`)},
		{"echo of a secret variable", regexp.MustCompile(`echo[^\n]*\$\{?(DB_MASTER_PASSWORD|DB_RUNTIME_PASSWORD|DB_OWNER_PASSWORD|ACTOR_PROOF_KEYS|pw|lpw|b64)\b`)},
		{"printf of a secret variable to the log", regexp.MustCompile(`printf[^\n|>]*\$\{?(DB_MASTER_PASSWORD|DB_RUNTIME_PASSWORD|DB_OWNER_PASSWORD|ACTOR_PROOF_KEYS|pw|lpw|b64)\b[^\n|>]*$`)},
		{"a password option on a command line", regexp.MustCompile(`(--password|-W\s|\s-P\s)`)},
		{"psql -c with the secret", regexp.MustCompile(`psql[^\n]*-c[^\n]*(ap_hex|ACTOR_PROOF|\$\{?pw\}?|\$\{?lpw\}?)`)},
		{"-v option carrying a secret", regexp.MustCompile(`-v\s+"?(ap_hex|[a-z_]*password|[a-z_]*secret)=`)},
	}
	for _, f := range runnerShell(t) {
		c := code(read(t, f))
		for _, b := range bad {
			// the only allowed PGPASSWORD is the explicit scrub `env -u PGPASSWORD`
			text := strings.ReplaceAll(c, "-u PGPASSWORD", "")
			if b.re.MatchString(text) {
				t.Errorf("%s: %s: %s", f, b.name, b.re.FindString(text))
			}
		}
	}
}

func TestGuardsBite_PlantedOffenders(t *testing.T) {
	re := regexp.MustCompile(`(?mi)(^|\s)set\s+-[a-z]*x|bash\s+-x|xtrace`)
	for _, s := range []string{"set -x", "set -euxo pipefail", "bash -x script", "export BASH_XTRACEFD=7"} {
		if !re.MatchString(s) {
			t.Errorf("tracing guard does not match %q", s)
		}
	}
	if re.MatchString("set -euo pipefail") {
		t.Error("tracing guard must not match set -euo pipefail")
	}
}

func TestVerificationPackIsReadOnly(t *testing.T) {
	stmt := regexp.MustCompile(`(?i)^\s*(INSERT|UPDATE|DELETE|CREATE|ALTER|DROP|TRUNCATE|GRANT|REVOKE|COPY|DO|CALL|SET|RESET|LOCK|VACUUM|ANALYZE|REINDEX|COMMENT|NOTIFY|LISTEN)\b`)
	for _, f := range []string{"deploy/staging-runner/sql/verify-db.sql", "deploy/staging-runner/sql/verify-db-runtime.sql"} {
		c := code(read(t, f))
		for i, l := range strings.Split(c, "\n") {
			if stmt.MatchString(l) {
				t.Errorf("%s line %d is not read-only: %s", f, i+1, strings.TrimSpace(l))
			}
		}
		// every statement starts with SELECT or WITH (or is a psql meta-command)
		for _, s := range strings.Split(c, ";\n") {
			ts := strings.TrimSpace(s)
			if ts == "" || strings.HasPrefix(ts, `\`) {
				continue
			}
			first := strings.Fields(ts)[0]
			if !strings.EqualFold(first, "SELECT") && !strings.EqualFold(first, "WITH") && !strings.HasPrefix(first, `\`) {
				t.Errorf("%s: statement does not start with SELECT/WITH: %.60s", f, ts)
			}
		}
	}
}

func TestActorProofSQLBindsTheSecretAndNeverInterpolatesIt(t *testing.T) {
	sql := read(t, "deploy/staging-runner/sql/seed-actor-proof.sql")
	c := code(sql)
	if !regexp.MustCompile("(?m)^\\\\set ap_hex `cat \"\\$RN_AP_HEX_FILE\"`$").MatchString(c) {
		t.Error("the secret must be read from the tmpfs file by backtick, exactly as operational-runbooks.md section 15 shows")
	}
	for i, l := range strings.Split(c, "\n") {
		if strings.Contains(l, "ap_hex") && !strings.Contains(l, `\bind`) && !strings.HasPrefix(strings.TrimSpace(l), `\set ap_hex`) {
			t.Errorf("line %d mentions ap_hex outside a \\bind parameter list: %s", i+1, l)
		}
		if strings.Contains(l, ":'ap_hex'") || strings.Contains(l, ":\"ap_hex\"") || strings.Contains(l, "decode(:") {
			t.Errorf("line %d interpolates the secret into SQL text: %s", i+1, l)
		}
	}
	if strings.Count(c, `\bind`) < 2 {
		t.Error("both the existence check and the insert must be parameter-bound")
	}
	sh := read(t, "deploy/staging-runner/modes/seed-actor-proof.sh")
	for _, want := range []string{`RN_S_ACTOR_KEYS=""`, `rm -f "$hexfile"`, "log_parameter_max_length_on_error", "RN_AP_HEX_FILE=", "conflict_different_secret_for_this_kid"} {
		if !strings.Contains(sh, want) {
			t.Errorf("seed-actor-proof.sh lacks %q", want)
		}
	}
	if strings.Contains(sh, "RN_SCRATCH}/ap") {
		t.Error("key material must live on tmpfs (RN_SHM), not in the scratch directory")
	}
}

func TestProbePackHasFifteenWritesAndNineAbusePrimitivesAndFailsClosed(t *testing.T) {
	src := read(t, "deploy/staging-runner/lib-probes.sh")
	count := func(arr string) int {
		m := regexp.MustCompile(`(?s)` + arr + `=\((.*?)\n\)`).FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("array %s not found", arr)
		}
		return len(regexp.MustCompile(`(?m)^\s+"[a-z_0-9]+\|`).FindAllString(m[1], -1))
	}
	if n := count("RN_WRITE_PROBES"); n != 15 {
		t.Errorf("expected the 15 write/escalation probes of drill.sh phase 2, found %d", n)
	}
	if n := count("RN_ABUSE_PROBES"); n != 9 {
		t.Errorf("expected the 9 abuse-primitive probes of ADR 0122 section 5b (8 + the two-key advisory lock), found %d", n)
	}
	// F1 (migration 0166, owner decision 5): the backup credential cannot read, lock or plant anything in the actor-proof key schema
	if n := count("RN_KEY_PROBES"); n != 3 {
		t.Errorf("expected the 3 actor-proof key probes of drill.sh phase 2 (F1), found %d", n)
	}
	for _, want := range []string{"SELECT secret FROM actorproof_private.actor_proof_keys", "LOCK TABLE actorproof_private.actor_proof_keys IN ACCESS SHARE MODE",
		"CREATE TABLE actorproof_private.zz_backup_probe", `rn_run_probe "$name" "$db" "$stmt" key "$hard"`, `[ "$class" = write ] || [ "$class" = key ]`} {
		if !strings.Contains(src, want) {
			t.Errorf("probe pack lacks the F1 key probe element %q", want)
		}
	}
	for _, want := range []string{"INSERT INTO tenants", "UPDATE ledger_entries", "DELETE FROM ledger_entries", "TRUNCATE ledger_entries", "CREATE TABLE", "CREATE TEMP TABLE", "DROP TABLE", "DISABLE TRIGGER ALL",
		"SET ROLE", "session_replication_role", "CREATE ROLE", "ALTER ROLE igaming_backup SUPERUSER", "COPY (SELECT 1) TO PROGRAM", "pg_read_file", "pg_reload_conf",
		"pg_advisory_lock(42, 43)", "pg_advisory_lock", "pg_try_advisory_lock", "pg_advisory_xact_lock", "pg_logical_emit_message", "lo_create", "pg_notify", "connect_postgres", "connect_template1"} {
		if !strings.Contains(src, want) {
			t.Errorf("probe pack lacks %q", want)
		}
	}
	// a probe that unexpectedly succeeds must be rolled back, never committed
	if !strings.Contains(src, "BEGIN READ WRITE; ${stmt}; ROLLBACK") || strings.Contains(code(src), "COMMIT") {
		t.Error("every probe must run inside BEGIN READ WRITE; ...; ROLLBACK")
	}
	// SRP3-2: fail closed. The verdict comes from the SQLSTATE; sessions are time-bounded; completeness is counted.
	c := code(src)
	for _, want := range []string{"VERBOSITY=sqlstate", "42501|0L000|0LP01", "statement_timeout", "lock_timeout", "probe_pack_complete", "RN_PROBE_RAN", "RN_PROBE_EXPECTED", "BLOCKED"} {
		if !strings.Contains(c, want) {
			t.Errorf("lib-probes.sh lacks %q", want)
		}
	}
	if strings.Contains(c, "RN_PRIV_RE") {
		t.Error("a PASS must not be derived from a message-text regex (only the connect probes read the server's CONNECT message)")
	}
}

// The classification itself is exercised by an offline shell test (needs bash only).
func TestProbeVerdictClassification(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := exec.Command("bash", filepath.Join(root(t), "deploy/staging-runner/tests/probe-verdict-test.sh")).CombinedOutput()
	if err != nil {
		t.Fatalf("probe-verdict-test.sh failed: %v\n%s", err, out)
	}
}

// SRP3-6: boot.sh and the entrypoint cannot be made to trace and keep secrets out of the environment and every argument list.
func TestBootAndEntrypointHardening(t *testing.T) {
	boot := read(t, "deploy/staging-runner/boot.sh")
	bc := code(boot)
	if !strings.HasPrefix(boot, "#!/bin/sh\n") {
		t.Error("boot.sh must be POSIX sh: it runs before any bash can be trusted")
	}
	for _, want := range []string{"set +x +v", "exec env -i", "bash --noprofile --norc", "RN_SECRETS_FD=3", "3<<EOF"} {
		if !strings.Contains(bc, want) {
			t.Errorf("boot.sh lacks %q", want)
		}
	}
	// the allow-list holds no secret and no shell-behaviour variable
	m := regexp.MustCompile(`(?s)names="(.*?)"`).FindStringSubmatch(bc)
	if m == nil {
		t.Fatal("boot.sh allow-list not found")
	}
	deny := regexp.MustCompile(`PASSWORD|_KEYS$|^(SHELLOPTS|BASHOPTS|BASH_ENV|ENV|PS4|IFS|BASH_XTRACEFD|PATH|LD_.*|BASH_FUNC.*)$`)
	for _, name := range strings.Fields(m[1]) {
		if deny.MatchString(name) {
			t.Errorf("boot.sh allow-list must not contain %s", name)
		}
	}
	if !strings.Contains(m[1], "TENANT_SLUG") || !strings.Contains(m[1], "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") {
		t.Error("boot.sh allow-list lost the runner parameters or the ECS credential variables")
	}
	// every parameter name deploy.sh accepts for any mode must survive the allow-list, or a mode would silently lose its parameter
	deploy := read(t, "deploy/aws/scripts/deploy.sh")
	allowed := map[string]bool{}
	for _, n := range strings.Fields(m[1]) {
		allowed[n] = true
	}
	n := 0
	for _, line := range regexp.MustCompile(`(?m)^\s+[a-z-]+(\|[a-z-]+)*\)\s+echo "([A-Z_ ]*)" ;;`).FindAllStringSubmatch(deploy, -1) {
		for _, name := range strings.Fields(line[2]) {
			n++
			if !allowed[name] {
				t.Errorf("deploy.sh accepts parameter %s but boot.sh's allow-list would drop it", name)
			}
		}
	}
	if n < 20 {
		t.Errorf("vacuous: only %d deploy.sh parameter names were checked", n)
	}
	// secrets reach the new shell only on fd 3: the here-document is the only place they are expanded
	for i, l := range strings.Split(bc, "\n") {
		if strings.Contains(l, "PASSWORD-") || strings.Contains(l, "_KEYS-") {
			if !regexp.MustCompile(`^\$\{[A-Z_]+-\}$`).MatchString(strings.TrimSpace(l)) {
				t.Errorf("boot.sh line %d expands a secret outside the here-document: %s", i+1, l)
			}
		}
	}
	entry := code(read(t, "deploy/staging-runner/entrypoint.sh"))
	iTrace, iRead, iFirst := strings.Index(entry, "set +x +v"), strings.Index(entry, "read -r -u 3"), strings.Index(entry, "RN_SELF=")
	if iTrace < 0 || iRead < 0 || iFirst < 0 || iTrace > iRead || iRead > iFirst {
		t.Error("entrypoint.sh must `set +x +v`, then read the secrets from fd 3, before it runs anything else")
	}
	for _, f := range runnerShell(t) {
		c := code(read(t, f))
		if regexp.MustCompile(`(export|declare\s+-x|typeset\s+-x)\s+(-[a-zA-Z]+\s+)?RN_S_`).MatchString(c) {
			t.Errorf("%s exports an RN_S_ secret variable", f)
		}
		if regexp.MustCompile(`env\s[^\n]*\$\{?RN_S_`).MatchString(c) {
			t.Errorf("%s puts an RN_S_ secret into an argument list", f)
		}
	}
	if !strings.Contains(code(read(t, "deploy/staging-runner/lib.sh")), `RN_S_MASTER=""; RN_S_OWNER=""; RN_S_RUNTIME=""`) {
		t.Error("rn_load_credentials must blank the captured passwords after writing the passfile")
	}
}

// SA-1 / SRP3-1: the seeding modes never print or pass credentials on a command line, and every seeding command checks the anchor.
func TestSeedWorldModeKeepsCredentialsOutOfOutputAndArguments(t *testing.T) {
	raw := read(t, "deploy/staging-runner/modes/seed-world.sh")
	c := code(raw)
	if !regexp.MustCompile(`--secret-string "file://`).MatchString(c) || strings.Contains(c, `--secret-string "$`) {
		t.Error("put-secret-value must take the value as file://<path>, never as an argument")
	}
	for i, l := range strings.Split(c, "\n") {
		if strings.Contains(l, "jq") && regexp.MustCompile(`password|bootstrap_token|\.actors\[\]|to_entries`).MatchString(l) {
			t.Errorf("seed-world.sh line %d reads credential values: %s", i+1, l)
		}
		if regexp.MustCompile(`\b(echo|printf)\b[^\n]*\$\{?(existing|plan|stored|applied|final)\b`).MatchString(l) && !strings.Contains(l, ">") {
			t.Errorf("seed-world.sh line %d prints a credentials file: %s", i+1, l)
		}
		if strings.Contains(l, "RN_S_MFA") && regexp.MustCompile(`\b(env|bash|aws)\b`).MatchString(l) {
			t.Errorf("seed-world.sh line %d may put a key on a command line: %s", i+1, l)
		}
	}
	for _, want := range []string{`RN_SW_SECRET_ID="igaming-staging/seed-world-passwords"`, "NOTHING was created", "rn_sw_merge", "DRY_RUN"} {
		if !strings.Contains(raw, want) {
			t.Errorf("seed-world.sh lacks %q", want)
		}
	}
	// the plan phase is stored BEFORE the apply phase runs
	iPlan, iPut, iApply := strings.Index(c, "rn_sw_run plan"), strings.Index(c, `rn_sw_put "$stored"`), strings.Index(c, "rn_sw_run apply")
	if iPlan < 0 || iPut < 0 || iApply < 0 || iPlan >= iPut || iPut >= iApply {
		t.Error("the planned passwords must be stored before the accounts are created")
	}
	// every seeding command goes through the anchor gate
	for _, f := range []string{"internal/stagingseed/market.go", "internal/stagingseed/world.go"} {
		if !strings.Contains(read(t, f), "RequireSyntheticAnchor") && !strings.Contains(read(t, f), "p.gate(") {
			t.Errorf("%s does not apply the anchor gate", f)
		}
	}
	// record-anchor never records production
	ra := read(t, "deploy/staging-runner/modes/record-anchor.sh") + read(t, "deploy/staging-runner/sql/record-anchor.sql")
	if !strings.Contains(ra, "(staging|development|test)") || !strings.Contains(ra, "IN ('staging', 'development', 'test')") {
		t.Error("record-anchor must accept staging, development and test only")
	}
}

func TestSeedWorldIamAndTerraformAreScopedToOneSecret(t *testing.T) {
	iam := read(t, "deploy/aws/modules/iam/main.tf")
	i := strings.Index(iam, `data "aws_iam_policy_document" "runner_seed_world"`)
	if i < 0 {
		t.Fatal("seed-world policy document not found")
	}
	body := iam[i : i+400]
	if !strings.Contains(body, `"secretsmanager:GetSecretValue", "secretsmanager:PutSecretValue"`) || !strings.Contains(body, "resources = [var.seed_world_secret_arn]") {
		t.Error("the seed-world task role must Get/PutSecretValue on exactly var.seed_world_secret_arn")
	}
	if strings.Contains(body, "rds-db") || strings.Contains(body, "*") {
		t.Error("the seed-world task role holds no rds-db permission and no wildcard")
	}
	boot := read(t, "deploy/aws/bootstrap/main.tf")
	if !strings.Contains(boot, "StagingRunnerSeedWorldSecretWrite") || !strings.Contains(boot, "secret:igaming-staging/seed-world-passwords-*") {
		t.Error("the permissions boundary must allow PutSecretValue on the seed-world secret (bootstrap credential)")
	}
	sec := read(t, "deploy/aws/modules/secrets/main.tf")
	if !strings.Contains(sec, `resource "aws_secretsmanager_secret" "seed_world"`) || strings.Contains(sec, `resource "aws_secretsmanager_secret_version" "seed_world"`) {
		t.Error("the seed-world secret must be an EMPTY container: no secret_version resource")
	}
	// the deployer-policy change (applied by an IAM administrator, not by the bootstrap credential) is its OWN policy file: adding the roles to the
	// edge/IAM/state policy would exceed the 6,144-character managed-policy limit
	dep := read(t, "deploy/aws/iam/staging-deployer-runner-roles-policy.json")
	for _, role := range []string{"igaming-staging-ecs-runner-task-role", "igaming-staging-ecs-seed-world-task-role"} {
		if n := strings.Count(dep, "role/"+role+`"`); n != 3 {
			t.Errorf("%s appears %d times in the runner-roles policy, want 3 (create, manage, pass)", role, n)
		}
	}
	for _, want := range []string{`"iam:PermissionsBoundary": "arn:aws:iam::765578795051:policy/igaming-staging-ecs-role-boundary"`, `"iam:PassedToService": "ecs-tasks.amazonaws.com"`} {
		if !strings.Contains(dep, want) {
			t.Errorf("runner-roles policy lacks the condition %s", want)
		}
	}
	if strings.Contains(dep, "*") || strings.Contains(dep, "Deny") {
		t.Error("the runner-roles policy holds exact ARNs only (no wildcard); the explicit Deny statements stay in the edge/IAM/state policy")
	}
	// every deployer policy fits the managed-policy limit (non-whitespace characters)
	for _, f := range glob(t, "deploy/aws/iam/staging-deployer-*.json") {
		var v any
		if err := json.Unmarshal([]byte(read(t, f)), &v); err != nil {
			t.Errorf("%s is not valid JSON: %v", f, err)
			continue
		}
		b, _ := json.Marshal(v)
		if n := len(strings.Join(strings.Fields(string(b)), "")); n > 6144 {
			t.Errorf("%s has %d non-whitespace characters, over the 6,144 managed-policy limit", f, n)
		}
	}
}

func TestDrillFollowsThePlanAndKeepsKeysOutOfTheBackupDirectory(t *testing.T) {
	d := read(t, "deploy/staging-runner/modes/backup-restore-drill.sh")
	for _, want := range []string{
		"BACKUP_ROLE_IAM_ONLY=1", "GRANT rds_iam TO igaming_backup", "harden-backup-surface.sql", "openssl genpkey -algorithm ED25519", "age-keygen",
		`RESTORE_VERIFY_PUBKEY_SHA256=`, "RESTORE_EXPECT_SET_SHA256=", "--phase provision", "--phase load", "create-restore-loader.sql", "DROP DATABASE",
		"PITR|T_utc=", "PITR|expected_digests=", "REPEATABLE READ READ ONLY", "backup.sh", "verify.sh",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("backup-restore-drill.sh lacks %q", want)
		}
	}
	lib := read(t, "deploy/staging-runner/lib.sh")
	if !strings.Contains(lib, "aws rds generate-db-auth-token") || !strings.Contains(lib, "verify-full") {
		t.Error("IAM token authentication and sslmode=verify-full must be the remote path")
	}
	if !strings.Contains(d, `keydir="${RN_SCRATCH}/keys"`) || !strings.Contains(d, `outdir="${RN_SCRATCH}/backups"`) {
		t.Error("the signing/age keys and the backup directory must be separate directories")
	}
	if strings.Contains(d, "BACKUP_ALLOW_UNENCRYPTED") || strings.Contains(d, "RESTORE_ALLOW_PRIVILEGED_DATA_ROLE") {
		t.Error("the drill must never use the synthetic-data / privileged-role overrides of the backup scripts")
	}
}

func TestLocalSwitchesAreLoopbackOnly(t *testing.T) {
	lib := read(t, "deploy/staging-runner/lib.sh")
	if !regexp.MustCompile(`local-test-harness`).MatchString(lib) {
		t.Fatal("the local switch is missing")
	}
	i := strings.Index(lib, "rn_validate_target()")
	if i < 0 {
		t.Fatal("rn_validate_target not found")
	}
	body := lib[i : i+1800]
	for _, want := range []string{`rn_is_loopback`, `5497|5498|5499`, `eu-central-1\.rds\.amazonaws\.com`, `"$port" = "5432"`} {
		if !strings.Contains(body, want) {
			t.Errorf("rn_validate_target lacks %q", want)
		}
	}
	// the production gate
	if !strings.Contains(lib, "production) rn_die") {
		t.Error("rn_init must refuse APP_ENV=production")
	}
}

func TestDockerfileBakesNoKeyAndHasTheToolchain(t *testing.T) {
	d := read(t, "deploy/docker/staging-runner.Dockerfile")
	c := code(d)
	for _, pkg := range []string{"bash", "postgresql16-client", "openssl", "age", "jq", "aws-cli", "curl"} {
		if !regexp.MustCompile(`apk add[^\n]*\b` + pkg + `\b`).MatchString(c) {
			t.Errorf("the image does not install %s", pkg)
		}
	}
	for _, want := range []string{"USER 10001", `ENTRYPOINT ["/opt/igaming/deploy/staging-runner/boot.sh"]`, `CMD ["usage"]`, "FROM golang:1.26.9-alpine", "FROM alpine:3.20@sha256:",
		"./cmd/seed-market", "./cmd/seed-staging-world", "deploy/staging-runner/boot.sh", "./deploy/backup/restorecheck", "./deploy/backup/scramhash", "./cmd/migrate", "deploy/drill/verify.sql"} {
		if !strings.Contains(c, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
	for _, bad := range []string{"ARG ", "ENV AWS_", "PASSWORD", ".pem", ".key", "sign.pem", "age.key"} {
		for i, l := range strings.Split(c, "\n") {
			if strings.Contains(l, bad) && !strings.HasPrefix(strings.TrimSpace(l), "COPY . .") {
				t.Errorf("Dockerfile line %d contains %q: %s", i+1, bad, l)
			}
		}
	}
	// the backup scripts shipped are exactly the ones the modes run
	for _, f := range []string{"backup.sh", "restore.sh", "verify.sh", "lib-common.sh", "lib-sign.sh", "lib-globals.sh", "baseline.sql", "init-backup-role.sql", "harden-backup-surface.sql", "post-restore-grants.sql"} {
		if !strings.Contains(c, "deploy/backup/"+f) {
			t.Errorf("the image does not ship deploy/backup/%s", f)
		}
	}
}

func TestTerraformGivesTheRunnerOnlyRdsDbConnectAsTheBackupUser(t *testing.T) {
	iam := read(t, "deploy/aws/modules/iam/main.tf")
	if strings.Count(code(iam), `"rds-db:connect"`) != 1 {
		t.Error("exactly one rds-db:connect statement is allowed in the IAM module")
	}
	if !strings.Contains(iam, `dbuser:${var.runner_db_resource_id}/${var.runner_db_user}`) {
		t.Error("the rds-db:connect ARN must name the exact DbiResourceId and the backup user")
	}
	if strings.Contains(code(iam), `dbuser:*`) || strings.Contains(code(iam), `dbuser:db-*`) {
		t.Error("the task role policy must not use a wildcard instance")
	}
	boot := read(t, "deploy/aws/bootstrap/main.tf")
	if !strings.Contains(boot, `dbuser:db-*/igaming_backup`) {
		t.Error("the permissions boundary must allow rds-db:connect for igaming_backup only")
	}
}
