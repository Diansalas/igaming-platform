//go:build integration

package launchgov_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/wdpath"
)

// DB BACKSTOP (security C1 / ledger-finance condition 2): the trigger itself refuses a closure
// with a hold-bearing withdrawal (LA023), for every one of the four hold states, for a brand and
// for a tenant, on the key-holder path that bypasses the Go service. Non-hold states do not block,
// and another brand's hold does not block a brand closure.
func TestClosureBackstop_HoldBearingWithdrawal_RefusedByDatabase_LA023(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	for _, state := range []string{"requested", "pending_review", "approved", "submitted"} {
		for _, kind := range []string{"brand", "tenant"} {
			state, kind := state, kind
			t.Run(kind+"/"+state, func(t *testing.T) {
				w.t = t
				tid := w.activeTenant()
				b := w.activeBrand(tid)
				p, wl := w.playerWithWallet(tid, b)
				w.withdrawal(tid, b, p, wl, state)
				var err error
				if kind == "brand" {
					err = launchfix.TrySetBrandStatus(ctx, tid, b, "closed")
				} else {
					err = launchfix.TrySetTenantStatus(ctx, tid, "closed")
				}
				if got := pgCode(err); got != "LA023" {
					t.Fatalf("%s closure with a %s withdrawal must be refused LA023 by the database, got %v", kind, state, err)
				}
				var pe *pgconn.PgError
				if errors.As(err, &pe) && pe.Detail != "hold_bearing_withdrawals=1" {
					t.Fatalf("detail must carry the count only: %q", pe.Detail)
				}
				sub := launchgov.Subject{TenantID: tid}
				if kind == "brand" {
					sub.BrandID = &b
				}
				if st := w.status(sub); st != "active" {
					t.Fatalf("the refused closure must leave the subject active, got %s", st)
				}
			})
		}
	}
	w.t = t
	for _, state := range []string{"rejected", "completed", "failed", "cancelled", "reversed"} {
		if state == "reversed" && wdpath.OnPersistentDatabase(t, w.owner) {
			t.Log("reversed: owner tamper plant (permanent row) not run on a persistent database (ADR 0113 review)")
			continue
		}
		tid := w.activeTenant()
		b := w.activeBrand(tid)
		p, wl := w.playerWithWallet(tid, b)
		w.withdrawal(tid, b, p, wl, state)
		if err := launchfix.TrySetBrandStatus(ctx, tid, b, "closed"); err != nil {
			t.Fatalf("a %s withdrawal holds nothing; the brand closure must succeed: %v", state, err)
		}
	}
	// Another brand's hold does not block this brand, but blocks the tenant.
	tid := w.activeTenant()
	b1, b2 := w.activeBrand(tid), w.activeBrand(tid)
	p2, w2 := w.playerWithWallet(tid, b2)
	w.withdrawal(tid, b2, p2, w2, "requested")
	if err := launchfix.TrySetBrandStatus(ctx, tid, b1, "closed"); err != nil {
		t.Fatalf("another brand's hold must not block: %v", err)
	}
	if got := pgCode(launchfix.TrySetBrandStatus(ctx, tid, b2, "closed")); got != "LA023" {
		t.Fatalf("brand 2 holds: %q", got)
	}
	if got := pgCode(launchfix.TrySetTenantStatus(ctx, tid, "closed")); got != "LA023" {
		t.Fatalf("the tenant closure must see brand 2's hold: %q", got)
	}
}
