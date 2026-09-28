package db

// ADR 0099 §6.1 (C-99-8) / A-16: WithPlatformActingInTenant, in this
// file's own package (tenant_rls.go), is the SOLE sanctioned way to set
// the "app.acting_tenant_id"/"app.acting_platform_principal_id" GUCs. This
// is a permanent static regression guard, in the style of
// internal/txscope's own INV-IO-1(c) test and internal/ledger's
// lockorder_static_test.go: it walks the actual repository source (not a
// fixture copy) so it fails the moment a future change violates the
// invariant, and it separately proves the guard catches a planted
// violation so a silently-broken scan can never pass by doing nothing.
//
// Two things are pinned:
//  1. no raw `set_config('app.acting_...` call exists anywhere outside
//     this package's own tenant_rls.go;
//  2. only internal/adjustment and internal/payments/manual_resolution.go
//     call db.Pool.WithPlatformActingInTenant.
//
// Deliberately a plain substring/regex scan over source text (matching
// this codebase's other static guards' stated preference for simplicity
// over a full type-checked call-graph) - a call written through
// reflection or string-built SQL would not be caught, but no code in this
// codebase builds SQL that way.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRootForActingSetterTest mirrors every other static guard's own
// go.mod-search helper.
func repoRootForActingSetterTest(t *testing.T) string {
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

// walkGoFilesSkippingHiddenAndTestdata visits every ".go" file under root,
// skipping any directory whose base name starts with "." or "_", or is
// named "testdata" - exactly the directories the "go" tool itself ignores
// during package discovery. This matters here specifically because
// .claude/worktrees/ (a "." -prefixed directory) holds OTHER checkouts of
// this same repository; without this skip, a scan from the repo root
// would double-count (or falsely flag) source that lives in a sibling
// worktree, not this one.
func walkGoFilesSkippingHiddenAndTestdata(root string, visit func(path string, src []byte) error) error {
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
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return visit(path, src)
	})
}

var actingSetConfigPattern = regexp.MustCompile(`set_config\(\s*'app\.acting_`)

// TestA16_NoRawSetConfigForActingGUCsOutsideTheSetter is A-16's first
// half.
func TestA16_NoRawSetConfigForActingGUCsOutsideTheSetter(t *testing.T) {
	root := repoRootForActingSetterTest(t)
	allowedFile := filepath.Join(root, "internal", "db", "tenant_rls.go")

	var violations []string
	scanned := 0
	if err := walkGoFilesSkippingHiddenAndTestdata(root, func(path string, src []byte) error {
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		if path == allowedFile {
			return nil
		}
		if actingSetConfigPattern.Match(src) {
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
		t.Fatalf("A-16: raw set_config('app.acting_...') found outside internal/db/tenant_rls.go:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// TestA16_GuardCatchesPlantedRawSetConfig is the required negative
// control: a planted raw set_config call for an acting GUC, outside the
// allowed file, must be caught by the exact regex the real test uses.
func TestA16_GuardCatchesPlantedRawSetConfig(t *testing.T) {
	planted := []byte(`package x
func f() { tx.Exec(ctx, "SELECT set_config('app.acting_tenant_id', $1, true)") }
`)
	if !actingSetConfigPattern.Match(planted) {
		t.Fatal("planted violation was not detected by actingSetConfigPattern")
	}
	safe := []byte(`package x
func f() { tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)") }
`)
	if actingSetConfigPattern.Match(safe) {
		t.Fatal("a set_config call for a DIFFERENT (non-acting) GUC was falsely flagged")
	}
}

var actingSetterCallPattern = regexp.MustCompile(`\.WithPlatformActingInTenant\s*\(`)

// TestA16_OnlyTwoPackagesCallTheActingSetter is A-16's second half. The
// closed allow-list is internal/adjustment (K2, ADR 0100) and
// internal/payments/manual_resolution.go (K3, ADR 0101) - neither exists
// yet, so this test passes vacuously until one does; it still pins that
// the ONLY two places ever allowed to gain such a call site are these, so
// a future change adding a call anywhere else fails the build
// immediately.
func TestA16_OnlyTwoPackagesCallTheActingSetter(t *testing.T) {
	root := repoRootForActingSetterTest(t)
	allowedDir := filepath.Join(root, "internal", "adjustment")
	allowedFile := filepath.Join(root, "internal", "payments", "manual_resolution.go")

	var violations []string
	if err := walkGoFilesSkippingHiddenAndTestdata(root, func(path string, src []byte) error {
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if !actingSetterCallPattern.Match(src) {
			return nil
		}
		if strings.HasPrefix(path, allowedDir+string(filepath.Separator)) || path == allowedFile {
			return nil
		}
		// The setter's own definition file legitimately contains its own
		// name in a doc comment reference, but not a call of the shape
		// "X.WithPlatformActingInTenant(" (which requires a receiver) -
		// tenant_rls.go defines the method, it does not call it.
		violations = append(violations, path)
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("A-16: WithPlatformActingInTenant called outside the allow-listed K2/K3 call sites:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// TestA16_GuardCatchesPlantedSetterCall is the required negative control
// for the second half.
func TestA16_GuardCatchesPlantedSetterCall(t *testing.T) {
	planted := []byte(`package x
func f(pool *Pool) { pool.WithPlatformActingInTenant(ctx, p, t, fn) }
`)
	if !actingSetterCallPattern.Match(planted) {
		t.Fatal("planted call site was not detected by actingSetterCallPattern")
	}
	safe := []byte(`package x
func f(pool *Pool) { pool.WithPlatformAdmin(ctx, p, fn) }
`)
	if actingSetterCallPattern.Match(safe) {
		t.Fatal("an unrelated setter call was falsely flagged")
	}
}
