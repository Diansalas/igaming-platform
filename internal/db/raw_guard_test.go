package db

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestPoolRaw_NoNonTestCallersOutsideDB is security condition C6 (ADR
// 0094): a transaction or connection taken through (*db.Pool).Raw() is
// never txscope-marked, so it would bypass the INV-POOL guard. Raw() may
// be used only by tests and inside internal/db.
func TestPoolRaw_NoNonTestCallersOutsideDB(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	rawCall := regexp.MustCompile(`\.Raw\(\)`)
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			switch {
			case rel == ".git", rel == ".claude", rel == "node_modules", strings.HasPrefix(rel, "frontend"), rel == filepath.Join("internal", "db"):
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if rawCall.Match(b) {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("non-test code calls (*db.Pool).Raw() outside internal/db (bypasses the txscope guard): %v", offenders)
	}
}
