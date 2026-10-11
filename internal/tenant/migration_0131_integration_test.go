//go:build integration

// ADR 0112 slice 4, migration 0131 (the withdrawal-creation status guard) on a THROWAWAY scratch
// database: a whole-schema up/down/up snapshot (the down restores the 0130 schema byte for byte), and
// the new objects. The behaviour (raw runtime INSERT refusals, races in both orders) is in
// internal/launchgov/withdrawal_insert_guard_integration_test.go. Requires TEST_DATABASE_URL and
// TEST_ADMIN_DATABASE_URL (scratchdb); a skip is NOT evidence.
package tenant

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

const migration0131Version = 131

func TestMigration0131_UpDownUp_WholeSchema(t *testing.T) {
	ctx := context.Background()
	pool, _ := lgScratch(t, "m0131s_")
	if _, err := pool.MigrateUp(ctx, lgMigDir(t, migration0131Version-1)); err != nil {
		t.Fatal(err)
	}
	snap130 := lgSnapshot(t, pool)
	full := lgMigDir(t, migration0131Version)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	snap131 := lgSnapshot(t, pool)
	if snap130 == snap131 {
		t.Fatal("0131 changed nothing in the schema snapshot (vacuous)")
	}
	objects := func() int {
		var n int
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM pg_trigger WHERE tgname = 'withdrawal_requests_aa_subject_active_guard' AND tgrelid = 'withdrawal_requests'::regclass AND NOT tgisinternal)
				+ (SELECT count(*) FROM pg_proc WHERE proname = 'withdrawal_requests_subject_active_guard')`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := objects(); got != 2 {
		t.Fatalf("0131 objects present: %d (want 2)", got)
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := lgSnapshot(t, pool); got != snap130 {
		t.Fatalf("down did not restore the 0130 schema byte for byte:\n%s", lgSnapDiff(snap130, got))
	}
	if got := objects(); got != 0 {
		t.Fatalf("down left %d objects", got)
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	if got := lgSnapshot(t, pool); got != snap131 {
		t.Fatalf("up after down differs from the first up:\n%s", lgSnapDiff(snap131, got))
	}
}

// The guard body carries both refusals and shares the lock keys with the status writers (the
// behavioural proof is in internal/launchgov).
func TestMigration0131_GuardBodyPinned(t *testing.T) {
	ctx := context.Background()
	pool, _ := lgScratch(t, "m0131g_")
	full := lgMigDir(t, migration0131Version)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	var src string
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE proname = 'withdrawal_requests_subject_active_guard'`).Scan(&src)
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"WD010", "WD011", "tenant_status_gate_key", "brand_status_gate_key"} {
		found := false
		for i := 0; i+len(want) <= len(src); i++ {
			if src[i:i+len(want)] == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("guard body lacks %q", want)
		}
	}
}
