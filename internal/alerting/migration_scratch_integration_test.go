//go:build integration

package alerting_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// scratchThroughVersion migrates a throwaway database through the given migration version only and returns
// the owner pool plus the directory holding exactly those files (for MigrateDown/MigrateUp round trips).
func scratchThroughVersion(t *testing.T, prefix string, version int64) (*db.Pool, string) {
	t.Helper()
	src := "../../migrations"
	dir := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		n, perr := strconv.ParseInt(e.Name()[:4], 10, 64)
		if perr != nil || n > version {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(src, e.Name()))
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := db.Connect(context.Background(), scratchdb.New(t, prefix), 10, 5_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate through %d: %v", version, err)
	}
	if applied[len(applied)-1] != version {
		t.Fatalf("last applied %v, want %d", applied, version)
	}
	return pool, dir
}
