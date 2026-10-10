//go:build integration

// ADR 0112 section 14.1, LF9: a governed brand closure racing a sportsbook bet placement.
//
// DOCUMENTING TEST (slice 1). Today nothing gates new gameplay on brands.status (the 0118/0121
// posting gates read tenants.status only; ADR 0112 decision LF1 adds the per-brand gate in
// SLICE 2). Until then a closure that is in flight (its status UPDATE has run, uncommitted) does
// NOT block a bet placement of that brand, and after the closure commits a bet is STILL admitted.
// The assertions below pin that gap so it cannot be forgotten: when slice 2 lands, both
// expectations flip (the placement must wait for the exclusive per-brand lock and then be
// refused with RejectionTenantNotActive / the brand equivalent), and this test is rewritten as the
// slice-2 race test. Search for "LF9-SLICE2" when doing so.
package sportsbook

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

func TestLF9_BrandClosureRacingBetPlacement_BeforeSlice2_BetIsAdmitted(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	f := seedFixture(t, owner)
	fundWallet(t, owner, f, 100_000)
	sel := seedSelection(t, owner, seedSelectionParams{})

	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- launchfix.TrySetBrandStatusHolding(context.Background(), f.tenantID, f.brandID, "closed", func() {
			close(held)
			<-release
		})
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("closure ended before the hold: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("closure never reached the hold")
	}

	// LF9-SLICE2: after slice 2 this placement must WAIT for the in-flight closure (shared vs
	// exclusive per-brand lock) and then be refused; today it is admitted immediately.
	res, err := placeBetRT(t, rt, f, sel, 1000, "lf9-during-"+uuid.NewString())
	if err != nil || !res.Accepted {
		close(release)
		t.Fatalf("LF1 gap documented in ADR 0112 14.1 (LF9): before slice 2 a bet racing a brand closure is admitted; accepted=%v err=%v", res.Accepted, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("brand closure: %v", err)
	}

	// LF9-SLICE2: after slice 2 a bet on the CLOSED brand is refused; today it is admitted.
	res, err = placeBetRT(t, rt, f, sel, 1000, "lf9-after-"+uuid.NewString())
	if err != nil || !res.Accepted {
		t.Fatalf("LF1 gap documented in ADR 0112 14.1 (LF9): before slice 2 a bet on a closed brand is admitted; accepted=%v err=%v", res.Accepted, err)
	}
}
