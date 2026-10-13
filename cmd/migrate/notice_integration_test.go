//go:build integration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// ADR 0118 security review F2, end to end: the BUILT binary prints a migration's RAISE NOTICE to stderr
// ("migrate: NOTICE: <message>") while `up` applies it, on a throwaway scratch database. Before F2 the
// driver discarded every notice, so the 0136 class 4 report (non-zero balances on a closed brand) was
// invisible to the operator. Requires TEST_ADMIN_DATABASE_URL (scratchdb); a skip is NOT evidence.
func TestBinary_UpPrintsMigrationNotices(t *testing.T) {
	url := scratchdb.New(t, "migratenotice_")
	dir := t.TempDir()
	up := `DO $$ BEGIN RAISE NOTICE 'adr0118 f2 notice probe: hold=% cash=%', 1, 2; RAISE WARNING 'adr0118 f2 warning probe'; END $$;
CREATE TABLE f2_notice_probe (id int);`
	if err := os.WriteFile(filepath.Join(dir, "0001_f2_notice_probe.up.sql"), []byte(up), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0001_f2_notice_probe.down.sql"), []byte("DROP TABLE f2_notice_probe;"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "migrate-under-test")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build cmd/migrate: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "-dir="+dir, "up")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "DATABASE_URL=" + url}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("migrate up: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "migrate: applied 1 migration(s): [1]") {
		t.Fatalf("the migration must apply: %q", stdout.String())
	}
	for _, want := range []string{"migrate: NOTICE: adr0118 f2 notice probe: hold=1 cash=2\n", "migrate: WARNING: adr0118 f2 warning probe\n"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr must carry %q, got %q", want, stderr.String())
		}
	}
	if strings.Contains(stderr.String(), "PL/pgSQL function") {
		t.Fatalf("only severity and message are printed (no context field): %q", stderr.String())
	}
}
