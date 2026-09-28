package identity

// A-14b (ADR 0099 §9, INV-CAP-6): static guard - no non-test Go package
// other than cmd/seed-admin inserts a staff_users row with role
// 'platform_admin'. internal/identity.CreateStaffUser takes role as a
// parameter and is the ONE shared insert path every staff-creation call
// site uses (HTTP admin routes, cmd/seed-admin) - so the actual
// mechanical signal for "who can produce a platform_admin row" is which
// non-test source file references the StaffRolePlatformAdmin constant at
// all: this file's own definition, and cmd/seed-admin/main.go, its one
// production caller. internal/httpserver's admin routes never pass this
// constant (its own HTTP role allowlist excludes "platform_admin" by
// construction - see admin_routes.go's own doc comment), and this guard
// pins that structurally: a future change that adds such a reference
// anywhere else fails the build immediately.
//
// The allow-listed test-support packages are internal/testsupport/* and
// internal/providercred/providercredtest, per the ADR text - the 23
// existing _test.go fixture files that insert platform_admin rows
// directly are excluded because this scan skips every "_test.go" file,
// full stop; the two named packages are the only allow-listed NON-test
// packages (already excluded from being test files) whose test-support
// helper functions are permitted to build a platform_admin fixture.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func a14bRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// a14bWalk mirrors internal/db's walkGoFilesSkippingHiddenAndTestdata:
// skips directories starting with "." or "_", and "testdata" - the same
// rule the go tool itself uses, and required here because
// .claude/worktrees/ (a "."-prefixed directory) holds other checkouts of
// this same repository.
func a14bWalk(root string, visit func(path string, src []byte) error) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		base := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") || base == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return visit(path, src)
	})
}

var a14bPlatformAdminRef = regexp.MustCompile(`StaffRolePlatformAdmin\b`)

func TestA14b_OnlySeedAdminReferencesPlatformAdminRole(t *testing.T) {
	root := a14bRepoRoot(t)
	allowed := map[string]bool{
		filepath.Join(root, "internal", "identity", "staff_user.go"): true,
		filepath.Join(root, "cmd", "seed-admin", "main.go"):          true,
	}
	// internal/testsupport/* and internal/providercred/providercredtest
	// are allow-listed NON-test packages (ADR 0099 §9) whose helpers may
	// build a platform_admin fixture for other packages' tests to use.
	allowedPrefixes := []string{
		filepath.Join(root, "internal", "testsupport") + string(filepath.Separator),
		filepath.Join(root, "internal", "providercred", "providercredtest") + string(filepath.Separator),
	}

	var violations []string
	scanned := 0
	if err := a14bWalk(root, func(path string, src []byte) error {
		scanned++
		if allowed[path] {
			return nil
		}
		for _, p := range allowedPrefixes {
			if strings.HasPrefix(path, p) {
				return nil
			}
		}
		if a14bPlatformAdminRef.Match(src) {
			violations = append(violations, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned == 0 {
		t.Fatal("this guard scanned zero .go files - the walk is broken")
	}
	if len(violations) > 0 {
		t.Fatalf("A-14b: StaffRolePlatformAdmin referenced outside the allow-listed files:\n  %s", strings.Join(violations, "\n  "))
	}
}

// TestA14b_GuardCatchesPlantedReference is the required negative control.
func TestA14b_GuardCatchesPlantedReference(t *testing.T) {
	planted := []byte(`package x
func f() { _ = identity.StaffRolePlatformAdmin }
`)
	if !a14bPlatformAdminRef.Match(planted) {
		t.Fatal("planted reference was not detected")
	}
	safe := []byte(`package x
func f() { _ = identity.StaffRoleTenantAdmin }
`)
	if a14bPlatformAdminRef.Match(safe) {
		t.Fatal("an unrelated role constant was falsely flagged")
	}
}
