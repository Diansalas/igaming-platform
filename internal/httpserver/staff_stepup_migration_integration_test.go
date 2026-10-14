//go:build integration

package httpserver

// ADR 0119 S4b, migration 0151 on a scratch database: up adds EXACTLY the three column-level UPDATE grants (mfa_at,
// assurance_kid, assurance_seal) and nothing else; down restores the 0142
// schema exactly; up after down equals the first up.

import (
	"context"
	"strings"
	"testing"
)

const m151Version = 151

func TestMigration0151_UpDownUp_WholeSchema(t *testing.T) {
	ctx := context.Background()
	pool, _ := m138Scratch(t, "m0151s_", m151Version-1)
	before := m138Snapshot(t, pool)
	full := m138MigDir(t, m151Version)
	if applied, err := pool.MigrateUp(ctx, full); err != nil || len(applied) != 1 || applied[0] != m151Version {
		t.Fatalf("up: %v %v", applied, err)
	}
	after := m138Snapshot(t, pool)
	removed, added := m138Diff(before, after)
	if len(removed) != 0 {
		t.Fatalf("0151 must not change or remove any object; removed: %v", removed)
	}
	var grants int
	for _, l := range added {
		switch {
		case strings.HasPrefix(l, "colacl:sessions:mfa_at:"), strings.HasPrefix(l, "colacl:sessions:assurance_kid:"),
			strings.HasPrefix(l, "colacl:sessions:assurance_seal:"):
			if !strings.Contains(l, "igaming_runtime=w/") {
				t.Fatalf("the column grant must be UPDATE only: %s", l)
			}
			grants++
		default:
			t.Fatalf("0151 added an object outside its scope: %s", l)
		}
	}
	if grants != 3 {
		t.Fatalf("want 3 column grants, got %d:\n%s", grants, strings.Join(added, "\n"))
	}
	if rolled, err := pool.MigrateDown(ctx, full, 1); err != nil || len(rolled) != 1 || rolled[0] != m151Version {
		t.Fatalf("down: %v %v", rolled, err)
	}
	if got := m138Snapshot(t, pool); got != before {
		r, a := m138Diff(before, got)
		t.Fatalf("down did not restore the previous schema exactly:\n- %v\n+ %v", r, a)
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	if got := m138Snapshot(t, pool); got != after {
		r, a := m138Diff(after, got)
		t.Fatalf("up after down differs from the first up:\n- %v\n+ %v", r, a)
	}
}
