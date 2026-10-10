package tenant

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ADR 0112 section 4.6 (LF2): the test-only fixture path is unreachable from a non-test build.
//
//  1. internal/testsupport/launchfix carries the `integration` build tag on every file and is
//     imported by no non-test file (only *_test.go files and other internal/testsupport
//     packages may import it), so no application binary can contain it.
//  2. The only non-test code that inserts into tenants / brands is identity.CreateTenant /
//     CreateBrand, which insert pending_launch explicitly; nothing outside it can provision an
//     active subject.
func TestLaunchFixture_UnreachableFromNonTestBuilds(t *testing.T) {
	root := repoRoot(t)
	const imp = "internal/testsupport/launchfix"

	// 1a. every launchfix source file is integration-tagged.
	fixDir := filepath.Join(root, "internal", "testsupport", "launchfix")
	entries, err := os.ReadDir(fixDir)
	if err != nil {
		t.Fatal(err)
	}
	var goFiles int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		goFiles++
		b, err := os.ReadFile(filepath.Join(fixDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(b), "//go:build integration") {
			t.Errorf("%s must start with //go:build integration", e.Name())
		}
	}
	if goFiles == 0 {
		t.Fatal("launchfix has no Go files; the pin would be vacuous")
	}

	// 1b. no non-test file outside internal/testsupport imports it.
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
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
			if strings.Contains(string(b), imp) {
				t.Errorf("%s (non-test) references %s: the fixture path must never reach a production build", rel, imp)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// 2. the only non-test INSERTs into tenants / brands.
	insert := regexp.MustCompile(`(?is)INSERT\s+INTO\s+(public\.)?(tenants|brands)\b`)
	allowed := map[string]bool{"internal/identity/tenant.go": true, "internal/identity/brand.go": true}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(rel, "internal/testsupport/") {
				return nil // integration-tagged test support (alertworld seeds through the owner pool)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if insert.Match(b) && !allowed[rel] {
				t.Errorf("%s (non-test) inserts into tenants / brands; only identity.CreateTenant / CreateBrand may, and they create pending_launch", rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for rel, want := range map[string]string{
		"internal/identity/tenant.go": `INSERT INTO tenants (id, name, slug, licensing_model, status) VALUES ($1, $2, $3, $4, $5)`,
		"internal/identity/brand.go":  `INSERT INTO brands (id, tenant_id, name, slug, status) VALUES ($1, $2, $3, $4, $5)`,
	} {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if !strings.Contains(src, want) || !strings.Contains(src, "StatusPendingLaunch") {
			t.Errorf("%s must insert the explicit StatusPendingLaunch (ADR 0112 7.4)", rel)
		}
	}
}
