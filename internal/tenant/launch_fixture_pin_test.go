package tenant

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ADR 0112 section 4.6 (LF2): the test-only fixture paths are unreachable from a non-test build.
//
//  1. internal/testsupport/launchfix AND internal/testsupport/launchdrive (each a signer of actor
//     proofs for the launch authorisation tables, see internal/actorproof/launch_scope_test.go)
//     carry the `integration` build tag on every file and are imported by no non-test file
//     outside internal/testsupport, so no application binary can contain either.
//  2. The only non-test code that inserts into tenants / brands is identity.CreateTenant /
//     CreateBrand, which insert pending_launch explicitly; nothing outside it can provision an
//     active subject.

// fixturePackages are the test-support packages that can write governed launch state.
// wdpath (ADR 0113 slice 1) plants and walks withdrawal requests (money-path fixtures).
// rgfix (ADR 0115 S1) seeds SYNTHETIC-NOT-LEGAL RG floors and writes the test environment anchor.
var fixturePackages = []string{"launchfix", "launchdrive", "wdpath", "rgfix"}

// untaggedFixtureFiles returns the .go files of the package directory that do not start with the
// integration build constraint, and the number of Go files inspected.
func untaggedFixtureFiles(t *testing.T, dir string) (untagged []string, goFiles int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		goFiles++
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(b), "//go:build integration") {
			untagged = append(untagged, e.Name())
		}
	}
	return untagged, goFiles
}

// nonTestImporters returns the repo-relative non-test Go files (outside internal/testsupport)
// that mention the import path.
func nonTestImporters(t *testing.T, root, imp string) []string {
	t.Helper()
	var out []string
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
			if strings.Contains(string(b), imp) {
				out = append(out, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestLaunchFixture_UnreachableFromNonTestBuilds(t *testing.T) {
	root := repoRoot(t)

	for _, pkg := range fixturePackages {
		imp := "internal/testsupport/" + pkg
		// 1a. every source file of the fixture package is integration-tagged.
		untagged, n := untaggedFixtureFiles(t, filepath.Join(root, "internal", "testsupport", pkg))
		if n == 0 {
			t.Fatalf("%s has no Go files; the pin would be vacuous", pkg)
		}
		for _, f := range untagged {
			t.Errorf("%s/%s must start with //go:build integration", pkg, f)
		}
		// 1b. no non-test file outside internal/testsupport imports it.
		for _, rel := range nonTestImporters(t, root, imp) {
			t.Errorf("%s (non-test) references %s: the fixture path must never reach a production build", rel, imp)
		}
	}

	// 2. the only non-test INSERTs into tenants / brands.
	insert := regexp.MustCompile(`(?is)INSERT\s+INTO\s+(public\.)?(tenants|brands)\b`)
	allowed := map[string]bool{"internal/identity/tenant.go": true, "internal/identity/brand.go": true,
		// STAGING-PREP-3 / SA-1: the staging seeding library creates the SYNTHETIC tenant and brand directly as 'active' (an
		// owner-provisioned launch transition, ADR 0112) behind APP_ENV in {development, staging} AND the database_environment
		// anchor in {staging, development, test}; it is imported only by cmd/seed-staging-world and cmd/seed-market
		// (internal/stagingseed TestStagingSeedImportedOnlyByItsCommands) and never by platform-api. Security review:
		// docs/plans/staging-prep-3-record.md sections 11-15.
		"internal/stagingseed/world.go": true}
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

// Non-vacuity (security L4): removing the build tag from a fixture package, or importing it from
// a non-test file, is detected by the same helpers the pin uses.
func TestLaunchFixture_Pin_IsNotVacuous(t *testing.T) {
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
	write("internal/testsupport/launchdrive/tagged.go", "//go:build integration\n\npackage launchdrive\n")
	write("internal/testsupport/launchdrive/untagged.go", "package launchdrive\n")
	untagged, n := untaggedFixtureFiles(t, filepath.Join(root, "internal", "testsupport", "launchdrive"))
	if n != 2 || len(untagged) != 1 || untagged[0] != "untagged.go" {
		t.Fatalf("a file without the build tag must be reported: n=%d untagged=%v", n, untagged)
	}

	write("internal/app/uses_drive.go", "package app\n\nimport _ \"github.com/Diansalas/igaming-platform/internal/testsupport/launchdrive\"\n")
	write("internal/app/uses_fix.go", "package app\n\nimport _ \"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix\"\n")
	write("internal/app/clean.go", "package app\n")
	write("internal/app/uses_in_test_test.go", "package app\n\nimport _ \"github.com/Diansalas/igaming-platform/internal/testsupport/launchdrive\"\n")
	write("internal/testsupport/other/uses.go", "//go:build integration\n\npackage other\n\nimport _ \"github.com/Diansalas/igaming-platform/internal/testsupport/launchdrive\"\n")
	write("cmd/svc/main.go", "package main\n\nimport _ \"github.com/Diansalas/igaming-platform/internal/testsupport/launchdrive\"\n")
	got := nonTestImporters(t, root, "internal/testsupport/launchdrive")
	if len(got) != 2 || got[0] != "cmd/svc/main.go" && got[1] != "cmd/svc/main.go" {
		t.Fatalf("exactly the two non-test importers must be reported, got %v", got)
	}
	for _, g := range got {
		if g != "cmd/svc/main.go" && g != "internal/app/uses_drive.go" {
			t.Fatalf("unexpected importer reported: %s", g)
		}
	}
	if fix := nonTestImporters(t, root, "internal/testsupport/launchfix"); len(fix) != 1 || fix[0] != "internal/app/uses_fix.go" {
		t.Fatalf("launchfix importers: %v", fix)
	}
}
