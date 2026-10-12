package drill_test

// Static guards for the local backup/restore drill and the local smoke (OPS-READINESS-LOCAL). They
// pin the safety properties the task and CLAUDE.md "Environment safety" require, so a later edit
// cannot quietly turn the drill into something that alters roles, escalates privilege, reaches a
// non-local host or weakens a control. They do not run any database.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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

// stripComments removes full-line shell / SQL comments so a safety note that NAMES a forbidden command
// ("never uses sudo") does not trip the guard.
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

var files = []string{
	"deploy/drill/backup-restore-drill.sh",
	"deploy/drill/seed.sql",
	"deploy/drill/verify.sql",
	"deploy/drill/post-restore.sql",
	"deploy/drill/post-backup-write.sql",
	"deploy/drill/tamper.sql",
	"deploy/loadtest/local-smoke.sh",
}

func TestNoRoleOrPrivilegeEscalationOrProcessKilling(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)\b(ALTER\s+(ROLE|USER)|CREATE\s+(ROLE|USER)|DROP\s+(ROLE|USER)|SET\s+ROLE|SET\s+SESSION\s+AUTHORIZATION|sudo|pkill|killall|su\s+-|pg_terminate_backend|pg_basebackup|PASSWORD\s+')`)
	for _, f := range files {
		body := stripComments(read(t, f))
		if m := forbidden.FindString(body); m != "" {
			t.Errorf("%s contains forbidden token %q (roles/passwords/privilege escalation/process killing are out of bounds for the drill)", f, m)
		}
	}
}

func TestNoCloudOrNetworkEgress(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)\b(aws|terraform|gcloud|az\s|ssh|scp|rsync|curl\s+https?://[^1l])`)
	for _, f := range files {
		body := stripComments(read(t, f))
		for _, line := range strings.Split(body, "\n") {
			if forbidden.MatchString(line) && !strings.Contains(line, "127.0.0.1") && !strings.Contains(line, "die ") && !strings.Contains(line, "echo ") {
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
	// the only DDL allowed is session-local pg_temp helper functions
	for _, m := range regexp.MustCompile(`(?i)CREATE\s+FUNCTION\s+(\S+)`).FindAllStringSubmatch(body, -1) {
		if !strings.HasPrefix(m[1], "pg_temp.") {
			t.Errorf("verify.sql creates a non-temporary function %s", m[1])
		}
	}
}

func TestDrillScriptSafetyProperties(t *testing.T) {
	s := read(t, "deploy/drill/backup-restore-drill.sh")
	for _, want := range []string{
		"set -euo pipefail",
		"trap cleanup EXIT",
		`^drill_bk_[a-z0-9_]+$`,              // only drill-named databases are ever created or dropped
		"refusing a non-local database host", // local-only
		"--dry-run",                          // dry-run mode exists
		`TEST_ADMIN_DATABASE_URL:?`,          // credentials from the environment, never defaulted
		"WITH (FORCE)",                       // scratch drop
		"negative-control",                   // verifiers must be shown able to fail
		"BACKUP-ROLE-1",                      // the owner-dump refusal is recorded, not worked around
	} {
		if !strings.Contains(s, want) {
			t.Errorf("drill script lost required safety property %q", want)
		}
	}
	// every DROP DATABASE in the driver targets a validated drill name
	for _, line := range strings.Split(stripComments(s), "\n") {
		if strings.Contains(line, "DROP DATABASE") && !strings.Contains(line, `"$d"`) && !strings.Contains(line, `\"$d\"`) {
			t.Errorf("DROP DATABASE on an unvalidated name: %q", strings.TrimSpace(line))
		}
	}
	// FORCE RLS is never lifted by the driver itself
	if strings.Contains(stripComments(s), "NO FORCE") {
		t.Error("the driver must never lift FORCE RLS; only the negative-control tamper file may")
	}
}

func TestOnlyTamperTouchesForceRLSAndItRunsOnTheRestoredCopyOnly(t *testing.T) {
	for _, f := range files {
		if f == "deploy/drill/tamper.sql" {
			continue
		}
		if strings.Contains(strings.ToUpper(stripComments(read(t, f))), "NO FORCE ROW LEVEL SECURITY") {
			t.Errorf("%s lifts FORCE RLS; only the negative-control file may, and only on the restored scratch copy", f)
		}
	}
	s := read(t, "deploy/drill/backup-restore-drill.sh")
	if !regexp.MustCompile(`tamper\.sql"\s+"\$RST_URL"`).MatchString(s) {
		t.Error(`tamper.sql must be applied to "$RST_URL" (the restored scratch copy) and nothing else`)
	}
}

func TestSmokeIsLoopbackOnly(t *testing.T) {
	h := read(t, "deploy/loadtest/smoke/main.go")
	if !strings.Contains(h, "refusing non-loopback target") {
		t.Error("smoke harness must refuse non-loopback targets")
	}
	s := read(t, "deploy/loadtest/local-smoke.sh")
	for _, want := range []string{"APP_ENV=development", "drill_bk_smoke_", `kill "$API_PID"`, "refusing a non-local database host"} {
		if !strings.Contains(s, want) {
			t.Errorf("local-smoke.sh lost %q", want)
		}
	}
}
