//go:build integration

package licensingchange

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// 0183 up / down / up on a throwaway database: the down restores the 0182 (or, without it, the 0181) schema byte for byte (the 0180 guard, conflict function and
// read policies; no hardening objects left), the re-up equals the first up. Migration 0182 belongs to a parallel slice and may be
// absent: the helper copies every file <= the target.
func TestMigration0183_UpDownUp_Snapshot(t *testing.T) {
	ctx := context.Background()
	u := scratchdb.New(t, "m0183_")
	pool, err := db.Connect(ctx, u, 8, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	full, _ := migrationsThrough(t, 183)
	base, _ := migrationsThrough(t, 182)
	if _, err := pool.MigrateUp(ctx, base); err != nil {
		t.Fatalf("migrate to 0182: %v", err)
	}
	before := schemaSnapshot(t, pool)
	if applied, err := pool.MigrateUp(ctx, full); err != nil || len(applied) == 0 || applied[len(applied)-1] != 183 {
		t.Fatalf("0183 up: %v %v", applied, err)
	}
	up := schemaSnapshot(t, pool)
	for _, want := range []string{"operating_bases_close_requires_successor", "tenants_licence_basis_exclusive",
		"operating_bases_ref_charset", "operating_bases_no_long_digit_runs"} {
		if !strings.Contains(up, want) {
			t.Fatalf("0183 up must create %q", want)
		}
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("0183 down: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != before {
		t.Fatalf("0183 down must restore the 0181 schema byte for byte; %s", firstDiff(before, got))
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("0183 up again: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != up {
		t.Fatalf("0183 up again must equal the first up; %s", firstDiff(up, got))
	}
}
