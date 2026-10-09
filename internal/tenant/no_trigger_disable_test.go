package tenant

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoTriggerDisableOutsideTests is a static guard (PRH-2 R5, security L1):
// production Go code must never disable a trigger or switch
// session_replication_role, since either would switch off the tenant-status
// gate, the closure guard or the append-only ledger. Only _test.go files (the
// R3 closed-tenant fixtures) and migrations may mention them.
func TestNoTriggerDisableOutsideTests(t *testing.T) {
	root := filepath.Join("..", "..")
	forbidden := regexp.MustCompile(`(?i)DISABLE\s+TRIGGER|session_replication_role`)
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// This file's own forbidden-word list is a _test.go file, skipped above.
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			// The one sanctioned exception: the B13 integration-only fixture support
			// (pitest.WithoutBindingGuard plants legacy-shaped NULL-binding rows). It
			// must carry the integration build tag, so it can never be part of a
			// production binary; any other file stays forbidden.
			if filepath.ToSlash(path) == "../../internal/payoutinstrument/pitest/pitest.go" {
				if !strings.HasPrefix(string(b), "//go:build integration") {
					t.Errorf("%s may mention a trigger disable only while it is integration-tag only", path)
				}
				return nil
			}
			if m := forbidden.Find(b); m != nil {
				t.Errorf("%s mentions %q: production code must not disable triggers", path, m)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
