//go:build integration

package reconciliation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
)

// PRH-2 R3 security F-3: a failure to LIST the non-active tenants must be
// returned by both entry points (never swallowed), with the ordinary (active)
// outcomes still returned alongside it. Kills the mutant that swallows the
// listing error. The scratch database holds only this test's tenant, so the
// unscoped RunSweep is cheap.
func TestR3_NonActiveListingFailureIsReturnedNotSwallowed(t *testing.T) {
	pool := sbScratchPool(t)
	f := seedFixture(t, pool)
	errForced := errors.New("r3: forced non-active listing failure")
	orig := listNonActiveTenants
	listNonActiveTenants = func(context.Context, *db.Pool, []uuid.UUID) ([]nonActiveTenant, error) {
		return nil, errForced
	}
	t.Cleanup(func() { listNonActiveTenants = orig })

	now := time.Now()
	start := now.Add(-time.Hour)
	outs, err := RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{f.tenantID}, start, now,
		sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
	if !errors.Is(err, errForced) {
		t.Fatalf("RunSweepTenants must return the non-active listing error, got %v", err)
	}
	if o := findOutcome(t, outs, f); o.Err != nil || o.ObservationOnly {
		t.Fatalf("the active tenant's outcome must survive the listing failure: %+v", o)
	}

	outs, err = RunSweep(context.Background(), pool, nil, start, now,
		sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
	if !errors.Is(err, errForced) {
		t.Fatalf("RunSweep must return the non-active listing error, got %v", err)
	}
	if o := findOutcome(t, outs, f); o.Err != nil {
		t.Fatalf("the active tenant's outcome must survive the listing failure: %+v", o)
	}
}
