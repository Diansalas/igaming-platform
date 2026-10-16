package stagingseed

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStagingSeedImportedOnlyByItsCommands pins the boundary the launch-fixture, platform-admin and status-reader static
// pins rely on when they allow-list this package: it may be imported only by cmd/seed-market and cmd/seed-staging-world
// (and test files), never by platform-api or any other production package.
func TestStagingSeedImportedOnlyByItsCommands(t *testing.T) {
	root := filepath.Join("..", "..")
	allowed := map[string]bool{"cmd/seed-market": true, "cmd/seed-staging-world": true}
	const imp = `"github.com/Diansalas/igaming-platform/internal/stagingseed"`
	found := 0
	for _, top := range []string{"cmd", "internal", "deploy"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
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
			if !strings.Contains(string(b), imp) {
				return nil
			}
			rel, _ := filepath.Rel(root, filepath.Dir(path))
			rel = filepath.ToSlash(rel)
			found++
			if !allowed[rel] {
				t.Errorf("%s imports internal/stagingseed; only cmd/seed-market and cmd/seed-staging-world may", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if found == 0 {
		t.Fatal("no importer found: the scan is broken or the commands moved")
	}
}
