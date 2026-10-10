//go:build integration

package reconciliation

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// ADR 0112 gate matrix (ledger-finance L-2), reconciliation (LF6): a pending_launch tenant is
// OBSERVED like any non-active tenant (the observation sweep selects `status <> 'active'`), it is
// NOT skipped and NOT treated as an ordinary active tenant.
func TestR3_PendingLaunchTenant_IsObservedNotSkipped(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	active := seedFixture(t, pool)
	pending := seedFixture(t, pool)
	if err := launchfix.ForcePending(ctx, t, pool, pending.tenantID, nil); err != nil {
		t.Fatalf("force pending_launch: %v", err)
	}
	outcomes, err := RunSweepTenants(ctx, pool, nil, []uuid.UUID{active.tenantID, pending.tenantID},
		time.Now().Add(-time.Hour), time.Now(), sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
	if err != nil {
		t.Fatalf("RunSweepTenants: %v", err)
	}
	if o := findOutcome(t, outcomes, active); o.ObservationOnly || o.Err != nil {
		t.Fatalf("active outcome: %+v", o)
	}
	o := findOutcome(t, outcomes, pending)
	if !o.ObservationOnly || o.TenantStatus != "pending_launch" || o.Err != nil {
		t.Fatalf("a pending_launch tenant must be observed as non-active (observation-only, status recorded), got %+v", o)
	}
	// And it is not in the ordinary (active-only) enumeration.
	all, err := allTenantIDs(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range all {
		if id == pending.tenantID {
			t.Fatal("the unrestricted ordinary enumeration must not list a pending_launch tenant")
		}
	}
}
