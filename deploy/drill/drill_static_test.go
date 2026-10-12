package drill_test

// Static guards for the local backup/restore drill and the local smoke (OPS-READINESS-LOCAL). They pin
// the safety properties CLAUDE.md "Environment safety" requires, so a later edit cannot quietly turn
// the drill into something that alters roles, escalates privilege, weakens a control, or reaches a
// non-local host. The file set is a GLOB (deploy/drill/*.sh|sql, deploy/loadtest/*.sh), never a list,
// so a newly added script is covered automatically. Planted-offender tests prove the guards bite.

import (
	"os"
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

// guardedFiles globs every script and SQL file the drill and smoke ship.
func guardedFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, pat := range []string{"deploy/drill/*.sh", "deploy/drill/*.sql", "deploy/loadtest/*.sh"} {
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

func TestGlobCoversTheExpectedFiles(t *testing.T) {
	got := strings.Join(guardedFiles(t), " ")
	for _, want := range []string{"backup-restore-drill.sh", "lib-localdb.sh", "seed.sql", "verify.sql", "tamper.sql", "post-restore.sql", "local-smoke.sh"} {
		if !strings.Contains(got, want) {
			t.Errorf("glob does not cover %s (got %s)", want, got)
		}
	}
}

// stripComments removes full-line shell / SQL comments so a safety note that NAMES a forbidden command
// does not trip the guard.
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

// Tokens forbidden everywhere. Matched case-insensitively in .sql files; in shell files SQL keywords are
// matched upper-case only (shell variables are lower-case) while the identifier-style tokens stay
// case-insensitive.
var sqlTokens = []string{
	`ALTER\s+ROLE`, `ALTER\s+USER`, `CREATE\s+ROLE`, `CREATE\s+USER`, `DROP\s+ROLE`, `DROP\s+USER`,
	`SET\s+ROLE`, `SET\s+SESSION\s+AUTHORIZATION`, `PASSWORD`, `BYPASSRLS`, `SUPERUSER`,
	`DISABLE\s+ROW\s+LEVEL\s+SECURITY`, `NO\s+FORCE\s+ROW\s+LEVEL\s+SECURITY`, `DISABLE\s+TRIGGER`,
	`DROP\s+POLICY`, `DROP\s+TRIGGER`,
}
var identTokens = []string{`session_replication_role`, `row_security`, `pg_terminate_backend`, `pg_basebackup`, `sudo`, `pkill`, `killall`}

// tamperAllowed are the damaging tokens the negative-control file alone may contain.
var tamperAllowed = map[string]bool{`NO\s+FORCE\s+ROW\s+LEVEL\s+SECURITY`: true, `DROP\s+TRIGGER`: true}

func findForbidden(name, body string) []string {
	body = stripComments(body)
	isSQL := strings.HasSuffix(name, ".sql")
	isTamper := strings.HasSuffix(name, "tamper.sql")
	var hits []string
	for _, tok := range sqlTokens {
		if isTamper && tamperAllowed[tok] {
			continue
		}
		flags := ""
		if isSQL {
			flags = "(?i)"
		}
		if regexp.MustCompile(flags + `\b` + tok + `\b`).MatchString(body) {
			hits = append(hits, tok)
		}
	}
	for _, tok := range identTokens {
		if regexp.MustCompile(`(?i)\b` + tok + `\b`).MatchString(body) {
			hits = append(hits, tok)
		}
	}
	return hits
}

func TestNoForbiddenTokensInAnyDrillOrSmokeFile(t *testing.T) {
	for _, f := range guardedFiles(t) {
		if hits := findForbidden(f, read(t, f)); len(hits) > 0 {
			t.Errorf("%s contains forbidden token(s) %v (roles, passwords, RLS/trigger weakening, privilege escalation, process killing are out of bounds)", f, hits)
		}
	}
}

func TestPlantedOffendersAreCaught(t *testing.T) {
	cases := []struct{ name, body string }{
		{"x.sql", "ALTER ROLE igaming WITH PASSWORD 'x';"},
		{"x.sql", "alter table t disable row level security;"},
		{"x.sql", "ALTER TABLE t DISABLE TRIGGER ALL;"},
		{"x.sql", "SET session_replication_role = replica;"},
		{"x.sql", "SET row_security = off;"},
		{"x.sql", "DROP POLICY p ON t;"},
		{"x.sql", "DROP TRIGGER t1 ON t;"}, // forbidden outside tamper.sql
		{"x.sql", "ALTER TABLE t NO FORCE ROW LEVEL SECURITY;"},
		{"x.sql", "CREATE ROLE backup BYPASSRLS;"},
		{"x.sh", "psql -c 'CREATE ROLE x'"},
		{"x.sh", "sudo -u postgres psql"},
		{"x.sh", "pkill platform-api"},
		{"tamper.sql", "ALTER ROLE igaming SUPERUSER;"}, // tamper allow-list is narrow
		{"tamper.sql", "SET session_replication_role = replica;"},
	}
	for _, c := range cases {
		if len(findForbidden(c.name, c.body)) == 0 {
			t.Errorf("planted offender not caught in %s: %q", c.name, c.body)
		}
	}
	// the narrow allow-list for tamper.sql really is allowed, and only there
	if h := findForbidden("tamper.sql", "DROP TRIGGER a ON b; ALTER TABLE w NO FORCE ROW LEVEL SECURITY;"); len(h) != 0 {
		t.Errorf("tamper.sql allow-list too narrow: %v", h)
	}
	if h := findForbidden("seed.sql", "DROP TRIGGER a ON b;"); len(h) == 0 {
		t.Error("DROP TRIGGER must be forbidden outside tamper.sql")
	}
}

func TestNoCloudOrNetworkEgress(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)\b(aws|terraform|gcloud|ssh|scp|rsync)\s`)
	for _, f := range guardedFiles(t) {
		for _, line := range strings.Split(stripComments(read(t, f)), "\n") {
			if forbidden.MatchString(line) && !strings.Contains(line, "die ") && !strings.Contains(line, "echo ") {
				t.Errorf("%s: line reaches beyond the local machine: %q", f, strings.TrimSpace(line))
			}
		}
	}
}

func TestVerifyIsReadOnly(t *testing.T) {
	body := stripComments(read(t, "deploy/drill/verify.sql"))
	write := regexp.MustCompile(`(?i)\b(INSERT\s+INTO|UPDATE\s+\w+\s+SET|DELETE\s+FROM|DROP\s|ALTER\s|GRANT\s|REVOKE\s|TRUNCATE\s|CREATE\s+TABLE|CREATE\s+ROLE)`)
	if m := write.FindString(body); m != "" {
		t.Errorf("verify.sql must be read-only, found %q", m)
	}
	for _, m := range regexp.MustCompile(`(?i)CREATE\s+FUNCTION\s+(\S+)`).FindAllStringSubmatch(body, -1) {
		if !strings.HasPrefix(m[1], "pg_temp.") {
			t.Errorf("verify.sql creates a non-temporary function %s", m[1])
		}
	}
}

// tamperInvocations returns the non-comment lines of a shell body that mention tamper.sql.
func tamperInvocations(body string) []string {
	var out []string
	for _, l := range strings.Split(stripComments(body), "\n") {
		if strings.Contains(l, "tamper.sql") {
			out = append(out, l)
		}
	}
	return out
}

// checkTamperTarget: exactly one invocation, aimed only at $RST_URL.
func checkTamperTarget(all map[string]string) []string {
	var problems []string
	n := 0
	for name, body := range all {
		for _, l := range tamperInvocations(body) {
			n++
			if !regexp.MustCompile(`tamper\.sql"\s+"\$RST_URL"`).MatchString(l) {
				problems = append(problems, name+": tamper.sql not aimed at \"$RST_URL\": "+strings.TrimSpace(l))
			}
			if strings.Contains(l, "$SRC_URL") || strings.Contains(l, "$LGC_URL") || strings.Contains(l, "DATABASE_URL") {
				problems = append(problems, name+": tamper.sql aimed at a source/logical/shared database: "+strings.TrimSpace(l))
			}
		}
	}
	if n != 1 {
		problems = append(problems, "expected exactly one tamper.sql invocation across the shell files, found "+string(rune('0'+n)))
	}
	return problems
}

func shellFiles(t *testing.T) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, f := range guardedFiles(t) {
		if strings.HasSuffix(f, ".sh") {
			m[f] = read(t, f)
		}
	}
	return m
}

func TestTamperRunsOnceAndOnlyOnTheRestoredCopy(t *testing.T) {
	if p := checkTamperTarget(shellFiles(t)); len(p) > 0 {
		t.Errorf("%v", p)
	}
	good := `psql -f "${SCRIPT_DIR}/tamper.sql" "$RST_URL" >/dev/null`
	if p := checkTamperTarget(map[string]string{"a.sh": good}); len(p) != 0 {
		t.Errorf("the good invocation was rejected: %v", p)
	}
	for _, bad := range []map[string]string{
		{"a.sh": `psql -f "${SCRIPT_DIR}/tamper.sql" "$SRC_URL"`},
		{"a.sh": `psql -f "${SCRIPT_DIR}/tamper.sql" "$LGC_URL"`},
		{"a.sh": `psql -f "${SCRIPT_DIR}/tamper.sql" "$TEST_DATABASE_URL"`},
		{"a.sh": good + "\n" + good},          // two invocations
		{"a.sh": good, "b.sh": good},          // second file invokes it too
		{"a.sh": "echo no invocation at all"}, // zero
	} {
		if len(checkTamperTarget(bad)) == 0 {
			t.Errorf("planted tamper misuse not caught: %v", bad)
		}
	}
}

func TestOnlyTamperFileLiftsForceRLS(t *testing.T) {
	for _, f := range guardedFiles(t) {
		if strings.HasSuffix(f, "tamper.sql") {
			continue
		}
		if regexp.MustCompile(`(?i)NO\s+FORCE`).MatchString(stripComments(read(t, f))) {
			t.Errorf("%s lifts FORCE RLS; only tamper.sql may, and only on the restored scratch copy", f)
		}
	}
}

func TestDrillScriptSafetyProperties(t *testing.T) {
	s := read(t, "deploy/drill/backup-restore-drill.sh")
	lib := read(t, "deploy/drill/lib-localdb.sh")
	for _, want := range []string{
		"set -euo pipefail", "trap cleanup EXIT",
		`^drill_bk_[a-z0-9_]+$`, "--dry-run", `TEST_ADMIN_DATABASE_URL:?`, "WITH (FORCE)",
		"negative-control", "BACKUP-ROLE-1", "raw_restore_shows_database_acl_loss", "DRILL_STALE_MIN_AGE_SECONDS", "active connection",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("drill script lost required safety property %q", want)
		}
	}
	for _, want := range []string{
		"refusing a non-local database host", "inet_server_addr() IS NULL", "unset PGHOST PGHOSTADDR", "PGSERVICEFILE",
		"hostaddr", "passfile", "chmod 600", "chmod 700", "multiple hosts are refused",
	} {
		if !strings.Contains(lib, want) {
			t.Errorf("lib-localdb.sh lost required guard %q", want)
		}
	}
	for _, f := range []string{"deploy/drill/backup-restore-drill.sh", "deploy/loadtest/local-smoke.sh"} {
		if !strings.Contains(read(t, f), "lib-localdb.sh") {
			t.Errorf("%s must source lib-localdb.sh (the shared local-only guard)", f)
		}
	}
	// no DROP DATABASE on an unvalidated name, and no password-bearing URL is built
	for _, f := range []string{"deploy/drill/backup-restore-drill.sh", "deploy/loadtest/local-smoke.sh"} {
		for _, line := range strings.Split(stripComments(read(t, f)), "\n") {
			if strings.Contains(line, "DROP DATABASE") && !strings.Contains(line, `$d`) && !strings.Contains(line, "${DB}") && !strings.Contains(line, "$d\\") {
				t.Errorf("%s: DROP DATABASE on an unvalidated name: %q", f, strings.TrimSpace(line))
			}
		}
	}
}

func TestSmokeIsLoopbackOnlyAndUsesNoLiteralSecret(t *testing.T) {
	h := read(t, "deploy/loadtest/smoke/main.go")
	if !strings.Contains(h, "refusing non-loopback target") {
		t.Error("smoke harness must refuse non-loopback targets")
	}
	s := read(t, "deploy/loadtest/local-smoke.sh")
	for _, want := range []string{"APP_ENV=development", "drill_bk_smoke_", `kill "$API_PID"`, "/dev/urandom"} {
		if !strings.Contains(s, want) {
			t.Errorf("local-smoke.sh lost %q", want)
		}
	}
	if regexp.MustCompile(`JWT_SIGNING_SECRET="[A-Za-z0-9-]{16,}"`).MatchString(s) {
		t.Error("local-smoke.sh must not carry a literal JWT signing secret")
	}
}

func TestMakefileDrillTargetsQuoteCredentialURLs(t *testing.T) {
	mk := read(t, "Makefile")
	for _, line := range strings.Split(mk, "\n") {
		if strings.Contains(line, "deploy/drill/") || strings.Contains(line, "deploy/loadtest/") {
			if strings.Contains(line, "$(TEST_ADMIN_DATABASE_URL)") && !strings.Contains(line, "'$(TEST_ADMIN_DATABASE_URL)'") {
				t.Errorf("unquoted credential URL in Makefile: %q", line)
			}
			if strings.Contains(line, "$(DATABASE_URL)") && !strings.Contains(line, "'$(DATABASE_URL)'") {
				t.Errorf("unquoted credential URL in Makefile: %q", line)
			}
		}
	}
}
