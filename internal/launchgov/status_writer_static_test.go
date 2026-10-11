package launchgov

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ADR 0112 7.1 (slice 3): internal/launchgov is the ONLY non-test writer of tenants.status and
// brands.status. The database refuses any status change without a same-transaction governed
// decision (zz_launch_status_governed), and this pin makes the Go side single-sourced: the only
// statements that UPDATE either column live in execute.go's updateSubjectStatus, and
// tenant.ChangeStatus (the former unauthorised writer, TENANT-STATUS-AUTHZ-1) no longer exists.
// A heuristic over source text, the same family as internal/tenant/status_readers_static_test.go:
// it does not parse SQL, and a status write built by string concatenation would escape it.

var (
	rawStringRE = regexp.MustCompile("(?s)`[^`]*`")
	// UPDATE <tenants|brands> ... SET ... status = : in a raw string, with the table named
	// directly after UPDATE (public. optional) and a status assignment before the statement ends.
	statusUpdateRE = regexp.MustCompile(`(?is)\bupdate\s+(?:only\s+)?(?:public\.)?(?:tenants|brands)\b[^;]*?\bset\b[^;]*?\bstatus\s*=`)
	// A dynamic table name: "UPDATE " + table / fmt.Sprintf("UPDATE %s ...") with a status SET.
	dynamicUpdateRE = regexp.MustCompile(`(?is)\bupdate\s+(?:%s|%q|%v)\b[^;]*?\bset\b[^;]*?\bstatus\s*=`)
	changeStatusRE  = regexp.MustCompile(`\bfunc\s+(?:\([^)]*\)\s*)?ChangeStatus\b`)
)

// statusWriters returns the repo-relative non-test Go files under root (outside the integration-
// tag-only internal/testsupport tree, pinned unreachable by internal/tenant/launch_fixture_pin_test.go)
// whose source contains a statement that UPDATEs tenants.status or brands.status, with the number
// of such statements.
func statusWriters(t *testing.T, root string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(rel, "internal/testsupport/") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			n := 0
			for _, raw := range rawStringRE.FindAllString(string(b), -1) {
				if statusUpdateRE.MatchString(raw) || dynamicUpdateRE.MatchString(raw) {
					n++
				}
			}
			for _, line := range strings.Split(string(b), "\n") {
				tl := strings.TrimSpace(line)
				if strings.HasPrefix(tl, "//") {
					continue
				}
				if (statusUpdateRE.MatchString(tl) || dynamicUpdateRE.MatchString(tl)) && !strings.Contains(tl, "`") {
					n++ // a double-quoted one-line SQL string
				}
			}
			if n > 0 {
				out[rel] = n
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func repoRootOf(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestStatic_LaunchgovIsTheOnlyNonTestWriterOfSubjectStatus(t *testing.T) {
	got := statusWriters(t, repoRootOf(t))
	want := map[string]int{"internal/launchgov/execute.go": 2} // updateSubjectStatus: tenants + brands
	if len(got) != len(want) {
		t.Fatalf("the non-test writers of tenants.status / brands.status must be exactly %v, got %v", want, got)
	}
	for f, n := range want {
		if got[f] != n {
			t.Fatalf("the non-test writers of tenants.status / brands.status must be exactly %v, got %v", want, got)
		}
	}
}

func TestStatic_TenantChangeStatusIsGone(t *testing.T) {
	root := repoRootOf(t)
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if changeStatusRE.Match(b) {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s declares ChangeStatus: the status write is the launchgov executor's alone (ADR 0112 7.1)", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Non-vacuity: the scan flags a planted writer of each shape and ignores the shapes that are not
// a status write.
func TestStatic_StatusWriterScan_IsNotVacuous(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/x/tenants.go", "package x\nvar q = `UPDATE tenants SET status = $1, updated_at = now() WHERE id = $2`\n")
	write("internal/x/brands.go", "package x\nvar q = `\n\tUPDATE public.brands\n\t   SET name = $1, status = $2\n\t WHERE id = $3`\n")
	write("internal/x/dyn.go", "package x\nvar q = fmt.Sprintf(`UPDATE %s SET status = $1 WHERE id = $2`, tbl)\n")
	write("internal/x/oneline.go", "package x\nvar q = \"UPDATE brands SET status = $1 WHERE id = $2\"\n")
	// Not status writes:
	write("internal/y/ok1.go", "package y\nvar q = `UPDATE tenants SET name = $1 WHERE id = $2`\n")
	write("internal/y/ok2.go", "package y\nvar q = `UPDATE player_accounts SET status = $1 WHERE id = $2`\n")
	write("internal/y/ok3.go", "package y\n// UPDATE tenants SET status = 'x' in a comment\nvar q = `SELECT status FROM tenants`\n")
	write("internal/testsupport/z/fixture.go", "package z\nvar q = `UPDATE tenants SET status = $1 WHERE id = $2`\n")
	write("internal/y/ok_test.go", "package y\nvar q = `UPDATE tenants SET status = $1 WHERE id = $2`\n")
	got := statusWriters(t, root)
	want := []string{"internal/x/tenants.go", "internal/x/brands.go", "internal/x/dyn.go", "internal/x/oneline.go"}
	if len(got) != len(want) {
		t.Fatalf("want exactly %v flagged, got %v", want, got)
	}
	for _, f := range want {
		if got[f] != 1 {
			t.Fatalf("%s must be flagged once, got %v", f, got)
		}
	}
}
