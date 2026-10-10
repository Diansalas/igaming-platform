//go:build integration

// ADR 0112 section 14.1, LF9: a governed brand closure racing a sportsbook bet placement.
//
// FLIPPED BY SLICE 2 (LF1, migration 0129). Before slice 2 this test documented the gap (the
// 0118/0121 posting gates read tenants.status only, so a bet racing a brand closure, and a bet
// after it, were admitted). Since slice 2 the governed brand status change takes the per-brand
// gameplay lock EXCLUSIVE inside zz_launch_status_governed and PlaceBet takes it SHARED before it
// reads brands.status, so a placement that races an in-flight closure WAITS for it and is then
// declined (RejectionBrandNotActive), and a placement after the closure is declined. Nothing is
// posted in either case. (LF9-SLICE2: both expectations flipped here.)
package sportsbook

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

func TestLF9_BrandClosureRacingBetPlacement_Slice2_BetIsRefused(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	f := seedFixture(t, owner)
	fundWallet(t, owner, f, 100_000)
	sel := seedSelection(t, owner, seedSelectionParams{})

	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	released := false
	releaseOnce := func() {
		if !released {
			released = true
			close(release)
		}
	}
	t.Cleanup(releaseOnce) // never leave the closure transaction held on a failed assertion
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
	txs0, entries0 := ledgerCounts(t, owner, f.tenantID)

	// LF9-SLICE2 (flipped): the placement WAITS for the in-flight closure (shared vs exclusive
	// per-brand lock) ...
	type out struct {
		res PlaceBetResult
		err error
	}
	placed := make(chan out, 1)
	go func() {
		r, err := placeBetRT(t, rt, f, sel, 1000, "lf9-during-"+uuid.NewString())
		placed <- out{r, err}
	}()
	select {
	case o := <-placed:
		t.Fatalf("LF9 (slice 2): a bet racing an in-flight brand closure must wait for it; finished early: accepted=%v category=%q err=%v",
			o.res.Accepted, o.res.RejectionCategory, o.err)
	case <-time.After(500 * time.Millisecond):
	}
	releaseOnce()
	if err := <-done; err != nil {
		t.Fatalf("brand closure: %v", err)
	}
	// ... and is then refused.
	if o := <-placed; o.err != nil || o.res.Accepted || o.res.RejectionCategory != RejectionBrandNotActive {
		t.Fatalf("LF9 (slice 2): a bet racing a brand closure must be refused brand_not_active; accepted=%v category=%q err=%v",
			o.res.Accepted, o.res.RejectionCategory, o.err)
	}

	// LF9-SLICE2 (flipped): a bet on the CLOSED brand is refused.
	res, err := placeBetRT(t, rt, f, sel, 1000, "lf9-after-"+uuid.NewString())
	if err != nil || res.Accepted || res.RejectionCategory != RejectionBrandNotActive {
		t.Fatalf("LF9 (slice 2): a bet on a closed brand must be refused brand_not_active; accepted=%v category=%q err=%v",
			res.Accepted, res.RejectionCategory, err)
	}
	if txs, entries := ledgerCounts(t, owner, f.tenantID); txs != txs0 || entries != entries0 {
		t.Fatalf("a refused bet posted: tx %d->%d entries %d->%d", txs0, txs, entries0, entries)
	}
	srInvariants(t, owner, f.tenantID)
}
