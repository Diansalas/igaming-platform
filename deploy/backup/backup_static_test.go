package backup_test

// Static guards for deploy/backup (A12, ADR 0122). They pin the safety properties CLAUDE.md "Environment safety" and the
// ADR require, so a later edit cannot quietly turn the backup tooling into something that weakens RLS, escalates
// privilege, reaches the shared 5432 cluster or AWS, or stores a secret. They are deliberately textual (cheap, no database
// needed); the behavioural proof is the drill itself (deploy/backup/drill.sh). Planted-offender tests show the guards bite.

import (
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

func files(t *testing.T, patterns ...string) []string {
	t.Helper()
	var out []string
	for _, pat := range patterns {
		m, err := filepath.Glob(filepath.Join(root(t), pat))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range m {
			rel, _ := filepath.Rel(root(t), f)
			out = append(out, filepath.ToSlash(rel))
		}
	}
	sort.Strings(out)
	return out
}

func stripComments(s string) string {
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

func base(rel string) string { return filepath.Base(rel) }

func TestGlobCoversTheScripts(t *testing.T) {
	got := strings.Join(files(t, "deploy/backup/*.sh", "deploy/backup/*.sql"), " ")
	for _, want := range []string{"backup.sh", "restore.sh", "verify.sh", "pitr-drill.sh", "drill.sh", "seed.sh", "lib-common.sh", "lib-cluster.sh", "lib-provision.sh", "baseline.sql", "init-backup-role.sql", "post-restore-grants.sql"} {
		if !strings.Contains(got, want) {
			t.Errorf("glob does not cover %s (got %s)", want, got)
		}
	}
}

// ---- forbidden tokens per file ------------------------------------------------------------------------------------

// Everywhere: privilege escalation to work around a failure, process killing, cloud/network egress.
var everywhere = []string{`\bsudo\b`, `\bpkill\b`, `\bkillall\b`, `\bsu\s+-`, `\bsetpriv\b`, `pg_terminate_backend`}

// Weakening of the controls under test. Allowed ONLY in the throwaway-cluster drill scripts (negative controls and the
// simulated operator error, which run as the drill's own superuser on its own cluster).
var weakening = []string{`session_replication_role`, `DISABLE\s+TRIGGER`, `NO\s+FORCE\s+ROW\s+LEVEL\s+SECURITY`, `DISABLE\s+ROW\s+LEVEL\s+SECURITY`, `\brow_security\b`, `--disable-triggers`}
var weakeningAllowed = map[string]bool{"drill.sh": true, "pitr-drill.sh": true}

// Role DDL. Allowed only where a role is provisioned on a throwaway cluster, or in the backup-role definition.
var roleDDL = []string{`CREATE\s+ROLE`, `ALTER\s+ROLE`, `DROP\s+ROLE`, `CREATE\s+USER`, `ALTER\s+USER`}

// restore.sh only replays CREATE/ALTER ROLE statements FILTERED from the backup's globals.sql for the three named project
// roles (--apply-globals); TestRestoreRoleFilter pins that filter.
var roleDDLAllowed = map[string]bool{"drill.sh": true, "lib-provision.sh": true, "init-backup-role.sql": true, "restore.sh": true, "lib-globals.sh": true}

func hits(name, body string, toks []string) []string {
	body = stripComments(body)
	var out []string
	for _, tok := range toks {
		if regexp.MustCompile(`(?i)` + tok).MatchString(body) {
			out = append(out, tok)
		}
	}
	return out
}

func findForbidden(name, body string) []string {
	h := hits(name, body, everywhere)
	if !weakeningAllowed[base(name)] {
		h = append(h, hits(name, body, weakening)...)
	}
	if !roleDDLAllowed[base(name)] {
		h = append(h, hits(name, body, roleDDL)...)
	}
	// a literal password in SQL or shell (psql variable substitution :'pw' is the allowed form)
	if regexp.MustCompile(`(?i)PASSWORD\s+'[^']`).MatchString(stripComments(body)) {
		h = append(h, "literal PASSWORD")
	}
	return h
}

func TestNoForbiddenTokens(t *testing.T) {
	for _, f := range files(t, "deploy/backup/*.sh", "deploy/backup/*.sql") {
		if h := findForbidden(f, read(t, f)); len(h) > 0 {
			t.Errorf("%s contains forbidden token(s) %v", f, h)
		}
	}
}

func TestPlantedOffendersAreCaught(t *testing.T) {
	cases := []struct{ name, body string }{
		{"backup.sh", "sudo -u postgres pg_dump"},
		{"restore.sh", "pkill postgres"},
		{"restore.sh", "psql -c 'SET session_replication_role = replica'"},
		{"restore.sh", "pg_restore --disable-triggers"},
		{"verify.sh", "psql -c 'ALTER TABLE t NO FORCE ROW LEVEL SECURITY'"},
		{"baseline.sql", "ALTER TABLE t DISABLE TRIGGER ALL;"},
		{"backup.sh", "psql -c 'CREATE ROLE x'"},
		{"restore.sh", "psql -c \"ALTER ROLE igaming WITH PASSWORD 'x'\""},
		{"seed.sh", "psql -c \"DROP ROLE igaming\""},
		{"init-backup-role.sql", "CREATE ROLE b PASSWORD 'hunter2hunter2';"},
		{"lib-common.sh", "su - postgres"},
	}
	for _, c := range cases {
		if len(findForbidden(c.name, c.body)) == 0 {
			t.Errorf("planted offender not caught in %s: %q", c.name, c.body)
		}
	}
	// the narrow allow-lists really allow what they should
	if h := findForbidden("drill.sh", "SET session_replication_role = replica; CREATE ROLE drill_restore LOGIN"); len(h) != 0 {
		t.Errorf("drill.sh allow-list too narrow: %v", h)
	}
	if h := findForbidden("init-backup-role.sql", "CREATE ROLE igaming_backup LOGIN NOSUPERUSER BYPASSRLS PASSWORD %L"); len(h) != 0 {
		t.Errorf("init-backup-role.sql allow-list too narrow: %v", h)
	}
}

func TestNoCloudOrNetworkEgress(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)(^|[;&|(\s])(aws|terraform|gcloud|ssh|scp|rsync|curl|wget)\s`)
	for _, f := range files(t, "deploy/backup/*.sh") {
		for _, line := range strings.Split(stripComments(read(t, f)), "\n") {
			if forbidden.MatchString(line) && !strings.Contains(line, "bk_die") && !strings.Contains(line, "echo ") && !strings.Contains(line, "res ") {
				t.Errorf("%s: line reaches beyond the local machine: %q", f, strings.TrimSpace(line))
			}
		}
	}
}

// ---- the target guard -----------------------------------------------------------------------------------------------

func TestTargetGuard(t *testing.T) {
	lib := read(t, "deploy/backup/lib-common.sh")
	for _, want := range []string{"5432", "SHARED local cluster", "BACKUP_DRILL_PORTS", "BACKUP_ALLOW_REMOTE_TARGET", "unset PGHOST PGHOSTADDR PGPORT", "must be mode 0600 or 0400", "credential file missing"} {
		if !strings.Contains(lib, want) {
			t.Errorf("lib-common.sh lost guard %q", want)
		}
	}
	// every script that talks to a database through BACKUP_* must go through bk_connect_env (which runs the guard)
	for _, f := range []string{"backup.sh", "restore.sh", "verify.sh"} {
		body := stripComments(read(t, "deploy/backup/"+f))
		if !strings.Contains(body, "bk_connect_env") {
			t.Errorf("%s does not call bk_connect_env (the target guard)", f)
		}
		if !strings.Contains(body, "lib-common.sh") {
			t.Errorf("%s does not source lib-common.sh", f)
		}
	}
	for _, f := range []string{"seed.sh", "pitr-drill.sh"} {
		if !strings.Contains(stripComments(read(t, "deploy/backup/"+f)), "bk_guard_target") {
			t.Errorf("%s does not call bk_guard_target", f)
		}
	}
	if !strings.Contains(read(t, "deploy/backup/lib-cluster.sh"), `/tmp/pgdrill-??????`) {
		t.Error("lib-cluster.sh must only ever remove a /tmp/pgdrill-XXXXXX root")
	}
	if !strings.Contains(read(t, "deploy/backup/lib-cluster.sh"), "refusing port 5432") {
		t.Error("lib-cluster.sh must refuse to create a cluster on 5432")
	}
	d := read(t, "deploy/backup/drill.sh")
	for _, want := range []string{"trap cl_teardown EXIT INT TERM", "set -euo pipefail"} {
		if !strings.Contains(d, want) {
			t.Errorf("drill.sh lost %q", want)
		}
	}
	if !strings.Contains(read(t, "deploy/backup/pitr-drill.sh"), "trap pitr_cleanup EXIT") {
		t.Error("pitr-drill.sh must stop its recovery instances on exit")
	}
	seed := read(t, "internal/httpserver/drillseed_integration_test.go")
	for _, want := range []string{"//go:build integration && drillseed", "DRILL_SEED_ALLOW", `u.Port() == "5432"`} {
		if !strings.Contains(seed, want) {
			t.Errorf("the seeder lost %q", want)
		}
	}
}

func TestDropDatabaseOnlyOnValidatedNames(t *testing.T) {
	for _, f := range files(t, "deploy/backup/*.sh") {
		for _, line := range strings.Split(stripComments(read(t, f)), "\n") {
			if !strings.Contains(strings.ToUpper(line), "DROP DATABASE") {
				continue
			}
			ok := strings.Contains(line, `${CREATED_DB}`) || strings.Contains(line, "igaming_restored") || strings.Contains(line, "${RST_DB}") // restore.sh's own rollback; drill's own copies
			if base(f) == "drill.sh" || base(f) == "restore.sh" {
				if !ok {
					t.Errorf("%s: DROP DATABASE on an unvalidated name: %q", f, strings.TrimSpace(line))
				}
			} else if !strings.Contains(line, "echo ") {
				t.Errorf("%s must not drop databases: %q", f, strings.TrimSpace(line))
			}
		}
	}
}

// ---- read-only SQL --------------------------------------------------------------------------------------------------

func TestBaselineIsReadOnlyAndCreatesNothing(t *testing.T) {
	body := stripComments(read(t, "deploy/backup/baseline.sql"))
	write := regexp.MustCompile(`(?i)\b(INSERT\s+INTO|UPDATE\s+\w+\s+SET|DELETE\s+FROM|DROP\s|ALTER\s|GRANT\s|REVOKE\s|TRUNCATE|CREATE\s)`)
	if m := write.FindString(body); m != "" {
		t.Errorf("baseline.sql must be read-only and create nothing (the backup role has no TEMP/CREATE), found %q", m)
	}
}

// ---- the role definition -----------------------------------------------------------------------------------------------

func TestBackupRoleDefinition(t *testing.T) {
	sql := stripComments(read(t, "deploy/backup/init-backup-role.sql"))
	for _, want := range []string{"NOSUPERUSER", "NOCREATEDB", "NOCREATEROLE", "NOREPLICATION", "BYPASSRLS", "NOINHERIT", "CONNECTION LIMIT 2", "VALID UNTIL",
		"default_transaction_read_only = on", "log_statement", `\getenv backup_password IGAMING_BACKUP_PASSWORD`, "must not be a member of any role"} {
		if !strings.Contains(sql, want) {
			t.Errorf("init-backup-role.sql lost %q", want)
		}
	}
	// only SELECT / USAGE / CONNECT may ever be granted to the backup role
	for _, m := range regexp.MustCompile(`(?is)GRANT\s+(.+?)\s+ON\s+.+?\s+TO\s+igaming_backup`).FindAllStringSubmatch(sql, -1) {
		priv := strings.ToUpper(strings.TrimSpace(m[1]))
		if priv != "SELECT" && priv != "USAGE" && priv != "CONNECT" {
			t.Errorf("init-backup-role.sql grants %q to igaming_backup (only SELECT, USAGE, CONNECT are allowed)", priv)
		}
	}
	if regexp.MustCompile(`(?i)GRANT\s+\w+\s+TO\s+igaming_backup`).MatchString(sql) {
		t.Error("the backup role must not be made a member of another role")
	}
	if regexp.MustCompile(`(?i)\bSUPERUSER\b`).FindAllString(strings.ReplaceAll(strings.ToUpper(sql), "NOSUPERUSER", ""), -1) != nil {
		t.Error("init-backup-role.sql mentions SUPERUSER other than NOSUPERUSER")
	}
}

func TestRestoreRoleFilter(t *testing.T) {
	r := stripComments(read(t, "deploy/backup/restore.sh"))
	for _, want := range []string{`PROJECT_ROLES="igaming igaming_runtime igaming_backup"`, "--apply-globals", `$0 == "CREATE ROLE " r ";"`, `index($0, "ALTER ROLE " r " ") == 1`, "WITHOUT a password"} {
		if !strings.Contains(r, want) {
			t.Errorf("restore.sh lost the globals filter element %q", want)
		}
	}
	if regexp.MustCompile(`psql[^\n]*-f\s+"?\$\{?GLOBALS`).MatchString(r) {
		t.Error("restore.sh must never replay globals.sql unfiltered")
	}
}

// TestGlobalsAllowList executes the real validator (lib-globals.sh) against genuine and tampered role statements: the
// checksum manifest is unsigned, so restore.sh must not replay an escalated globals.sql as the restore user.
func TestGlobalsAllowList(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	const norm = "NOSUPERUSER INHERIT NOCREATEROLE NOCREATEDB LOGIN NOREPLICATION NOBYPASSRLS"
	cases := []struct {
		role  string
		stmts []string
		ok    bool
	}{
		{"igaming", []string{"CREATE ROLE igaming;", "ALTER ROLE igaming WITH " + norm + ";"}, true},
		{"igaming_runtime", []string{"CREATE ROLE igaming_runtime;", "ALTER ROLE igaming_runtime WITH " + norm + ";"}, true},
		{"igaming_backup", []string{"CREATE ROLE igaming_backup;",
			"ALTER ROLE igaming_backup WITH NOSUPERUSER NOINHERIT NOCREATEROLE NOCREATEDB LOGIN NOREPLICATION BYPASSRLS CONNECTION LIMIT 2 VALID UNTIL '2026-10-16 17:11:32.849749+00';",
			"ALTER ROLE igaming_backup SET default_transaction_read_only TO 'on';", "ALTER ROLE igaming_backup SET log_statement TO 'all';"}, true},
		{"igaming", []string{"ALTER ROLE igaming WITH SUPERUSER INHERIT NOCREATEROLE NOCREATEDB LOGIN NOREPLICATION NOBYPASSRLS;"}, false},
		{"igaming", []string{"ALTER ROLE igaming WITH NOSUPERUSER INHERIT NOCREATEROLE NOCREATEDB LOGIN NOREPLICATION BYPASSRLS;"}, false},
		{"igaming_runtime", []string{"ALTER ROLE igaming_runtime WITH " + norm + " CREATEROLE;"}, false},
		{"igaming", []string{"ALTER ROLE igaming WITH NOSUPERUSER NOCREATEROLE NOCREATEDB NOREPLICATION NOBYPASSRLS PASSWORD 'x';"}, false},
		{"igaming", []string{"ALTER ROLE igaming WITH NOSUPERUSER NOCREATEROLE NOCREATEDB NOREPLICATION NOBYPASSRLS; DROP DATABASE x;"}, false},
		{"igaming", []string{"GRANT pg_execute_server_program TO igaming;"}, false},
		{"igaming", []string{"DROP ROLE igaming;"}, false},
		{"igaming_backup", []string{"ALTER ROLE igaming_backup SET session_replication_role TO 'replica';"}, false},
		{"igaming_backup", []string{"ALTER ROLE igaming_backup WITH NOSUPERUSER NOCREATEROLE NOCREATEDB NOREPLICATION NOBYPASSRLS;"}, false},
	}
	for i, c := range cases {
		f := filepath.Join(t.TempDir(), "globals.sql")
		if err := os.WriteFile(f, []byte(strings.Join(c.stmts, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		script := `. "$1"; bk_validate_role_stmts "$2" "$3"`
		cmd := exec.Command("bash", "-c", script, "x", filepath.Join(root(t), "deploy/backup/lib-globals.sh"), c.role, f)
		err := cmd.Run()
		if (err == nil) != c.ok {
			t.Errorf("case %d (%s %v): accepted=%v want %v", i, c.role, c.stmts, err == nil, c.ok)
		}
	}
	r := read(t, "deploy/backup/restore.sh")
	if !strings.Contains(r, "lib-globals.sh") || !strings.Contains(r, "bk_validate_role_stmts") {
		t.Error("restore.sh must validate the globals statements with lib-globals.sh before replaying them")
	}
}

func TestBackupScriptFailClosedProperties(t *testing.T) {
	b := read(t, "deploy/backup/backup.sh")
	for _, want := range []string{"does not have BYPASSRLS", "BACKUP_ALLOW_UNENCRYPTED", "BACKUP_ENCRYPT_AGE_RECIPIENT", "BACKUP_ENCRYPT_GPG_RECIPIENT", "pg_export_snapshot", "--snapshot=", "SHA256SUMS",
		"--no-role-passwords", "insufficient disk space", "credential EXPIRED", "refusing to back up as a SUPERUSER", "rm -rf \"$RUN_DIR\""} {
		if !strings.Contains(b, want) {
			t.Errorf("backup.sh lost %q", want)
		}
	}
	if regexp.MustCompile(`(?i)pg_dump[^\n]*--(enable-row-security|no-owner=)`).MatchString(b) {
		t.Error("backup.sh must not use --enable-row-security (silent incomplete dumps)")
	}
	r := read(t, "deploy/backup/restore.sh")
	for _, want := range []string{"--single-transaction", "--exit-on-error", "already exists: refusing to restore into an existing database", "CHECKSUM MISMATCH", "RESTORE_CONFIRM", "restoring as", "is refused", "TEMPLATE template0"} {
		if !strings.Contains(r, want) {
			t.Errorf("restore.sh lost %q", want)
		}
	}
	v := read(t, "deploy/backup/verify.sh")
	for _, want := range []string{"for kind in TABLE ROLE MEMBER DBACL SCHEMAACL ACL COLACL FUNCACL DEFACL TRG POL RLS FP", "equal_${kind}", "invariants_all_zero", "migrate_verify", "drill_verify_sql_owner", "RESTORECHECK_BIN", "fail-closed"} {
		if !strings.Contains(strings.ToLower(v), strings.ToLower(want)) {
			t.Errorf("verify.sh lost %q", want)
		}
	}
}

func TestNoKeyMaterialOrSecretsInTheDirectory(t *testing.T) {
	bad := regexp.MustCompile(`AGE-SECRET-KEY-|BEGIN (PGP|RSA|EC|OPENSSH) PRIVATE KEY|age1[a-z0-9]{50,}|AKIA[0-9A-Z]{16}`)
	for _, f := range files(t, "deploy/backup/*", "deploy/backup/*/*", "docs/runbooks/backup-restore-dr.md", "docs/runbooks/aws-staging-deploy-plan.md", "docs/decisions/0122-backup-role-and-pitr.md") {
		if st, err := os.Stat(filepath.Join(root(t), f)); err != nil || st.IsDir() || strings.HasSuffix(f, "_test.go") {
			continue
		}
		if bad.MatchString(read(t, f)) {
			t.Errorf("%s contains key-like material", f)
		}
	}
}

func TestScriptsParse(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	for _, f := range files(t, "deploy/backup/*.sh") {
		cmd := exec.Command("bash", "-n", filepath.Join(root(t), f))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s does not parse: %v\n%s", f, err, out)
		}
	}
	if _, err := exec.LookPath("shellcheck"); err == nil {
		for _, f := range files(t, "deploy/backup/*.sh") {
			cmd := exec.Command("shellcheck", "-S", "warning", "-e", "SC1091,SC2034,SC2155", filepath.Join(root(t), f))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("shellcheck %s: %v\n%s", f, err, out)
			}
		}
	}
}

// ---- Terraform (text level; terraform itself is not run here) -----------------------------------------------------------

func TestTerraformPITRSettings(t *testing.T) {
	mod := read(t, "deploy/aws/modules/database/main.tf")
	for _, want := range []string{"backup_retention_period", "backup_window", "deletion_protection", "skip_final_snapshot", "final_snapshot_identifier", "copy_tags_to_snapshot      = true",
		"enabled_cloudwatch_logs_exports", `"log_connections"`, `"log_disconnections"`, "shared_preload_libraries", "pgaudit", "storage_encrypted = true", "kms_key_arn"} {
		if !strings.Contains(mod, want) {
			t.Errorf("modules/database/main.tf lost %q", want)
		}
	}
	v := read(t, "deploy/aws/environments/staging/variables.tf")
	for _, want := range []string{`variable "db_backup_retention_days"`, "default     = 14", `variable "db_deletion_protection"`, `variable "backup_runner_secret_arn"`, "REFERENCE ONLY"} {
		if !strings.Contains(v, want) {
			t.Errorf("staging variables.tf lost %q", want)
		}
	}
	// the secret is a reference: no secret resource for the backup role and no ARN default
	all := read(t, "deploy/aws/environments/staging/main.tf") + read(t, "deploy/aws/modules/secrets/main.tf") + v
	if regexp.MustCompile(`resource\s+"aws_secretsmanager_secret(_version)?"\s+"[a-z_]*backup`).MatchString(all) {
		t.Error("Terraform must not create a backup-role secret (reference only)")
	}
	if regexp.MustCompile(`backup_runner_secret_arn"\s*{[^}]*default\s*=\s*"arn`).MatchString(v) {
		t.Error("backup_runner_secret_arn must not have an ARN default")
	}
}
