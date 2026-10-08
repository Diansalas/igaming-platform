//go:build integration

package payments

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

const migration0122Version = 122

// PAY-RECEIPT-ANOMALY-APPLIED-1: migration 0122 up/down/up whole-schema test. The ONLY schema change is
// the body of payment_provider_events_guard: no table, column, constraint, index, policy, trigger or
// runtime grant is added or altered; down restores the N-1 schema exactly; re-up equals the first up.
func TestRR_Migration0122UpDownUp_WholeSchema_OnlyTheGuardFunctionChanges(t *testing.T) {
	pool, _ := scratchThrough(t, "rr0122_", migration0122Version-1)
	preSnap := schemaSnapshot15(t, pool)
	preNames := objectNames(t, pool)

	dir := migration0101Dir(t, migration0122Version)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("up: %v", err)
	}
	upSnap := schemaSnapshot15(t, pool)
	if upSnap == preSnap {
		t.Fatal("0122 changed nothing?")
	}
	diff := snapDiff(preSnap, upSnap)
	for _, l := range strings.Split(diff, "\n") {
		if !strings.Contains(l, "function:payment_provider_events_guard()") {
			t.Fatalf("0122 may change only payment_provider_events_guard, but the snapshot differs by: %s", l)
		}
	}
	if n := len(strings.Split(diff, "\n")); n != 2 {
		t.Fatalf("want exactly the old and new guard function lines, got %d:\n%s", n, diff)
	}
	// Security L-1: the replaced guard pins its search_path; down (the 0101 definition) carries none.
	proconfig := func() string {
		var cfg *string
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT array_to_string(proconfig, ',') FROM pg_proc WHERE proname = 'payment_provider_events_guard'`).Scan(&cfg)
		}); err != nil {
			t.Fatal(err)
		}
		if cfg == nil {
			return ""
		}
		return *cfg
	}
	if got := proconfig(); got != "search_path=pg_catalog, public, pg_temp" {
		t.Fatalf("guard proconfig after up = %q, want the pinned search_path", got)
	}
	upNames := objectNames(t, pool)
	for n := range upNames {
		if !preNames[n] {
			t.Errorf("0122 added an object: %s", n)
		}
	}
	for n := range preNames {
		if !upNames[n] {
			t.Errorf("0122 removed an object: %s", n)
		}
	}

	if _, err := pool.MigrateDown(context.Background(), dir, 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := proconfig(); got != "" {
		t.Fatalf("guard proconfig after down = %q, want none (0101 exactly)", got)
	}
	if got := schemaSnapshot15(t, pool); got != preSnap {
		t.Fatalf("0122 down did not restore the N-1 schema exactly:\n%s", snapDiff(preSnap, got))
	}
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if got := schemaSnapshot15(t, pool); got != upSnap {
		t.Fatalf("0122 re-up differs from the first up:\n%s", snapDiff(upSnap, got))
	}
}
