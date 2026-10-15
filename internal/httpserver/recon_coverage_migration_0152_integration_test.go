//go:build integration

package httpserver

// A3 SR3B-3, migration 0152 on a scratch database: up adds EXACTLY one nullable column
// (reconciliation_runs.coverage_end) and changes no policy, trigger, constraint, RLS/FORCE flag or ACL; down returns
// the schema to 0151 exactly; up after down equals the first up; down REFUSES while any run recorded a coverage_end
// (the table is append-only evidence), and the refusal leaves FORCE ROW LEVEL SECURITY intact.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const m152Version = 152

func TestMigration0152_UpDownUp_WholeSchema(t *testing.T) {
	ctx := context.Background()
	pool, _ := m138Scratch(t, "m0152s_", m152Version-1)
	before := m138Snapshot(t, pool)
	full := m138MigDir(t, m152Version)
	if applied, err := pool.MigrateUp(ctx, full); err != nil || len(applied) != 1 || applied[0] != m152Version {
		t.Fatalf("up: %v %v", applied, err)
	}
	after := m138Snapshot(t, pool)
	removed, added := m138Diff(before, after)
	if len(removed) != 0 {
		t.Fatalf("0152 must not change or remove any object; removed: %v", removed)
	}
	if len(added) != 1 || !strings.HasPrefix(added[0], "column:reconciliation_runs:coverage_end:timestamp with time zone:YES:") {
		t.Fatalf("0152 must add exactly the nullable coverage_end column, got:\n%s", strings.Join(added, "\n"))
	}

	// Down refuses once a run recorded a coverage_end, and leaves RLS forced.
	tid := uuid.New()
	if err := pool.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO reconciliation_runs (tenant_id, stream, period_start, period_end, status, coverage_end)
			 VALUES ($1, 'casino_statement', now() - interval '1 hour', now(), 'clean', $2)`, tid, time.Now())
		return err
	}); err != nil {
		t.Fatalf("seed a run with coverage_end: %v", err)
	}
	if rolled, err := pool.MigrateDown(ctx, full, 1); err == nil {
		t.Fatalf("down must refuse while a coverage_end is recorded, rolled back %v", rolled)
	}
	if got := m138Snapshot(t, pool); got != after {
		r, a := m138Diff(after, got)
		t.Fatalf("a refused down must leave the schema untouched:\n- %v\n+ %v", r, a)
	}
}

func TestMigration0152_DownWithOnlyNullsRestoresThePreviousSchema(t *testing.T) {
	ctx := context.Background()
	pool, _ := m138Scratch(t, "m0152n_", m152Version-1)
	before := m138Snapshot(t, pool)
	full := m138MigDir(t, m152Version)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	after := m138Snapshot(t, pool)
	if rolled, err := pool.MigrateDown(ctx, full, 1); err != nil || len(rolled) != 1 || rolled[0] != m152Version {
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
