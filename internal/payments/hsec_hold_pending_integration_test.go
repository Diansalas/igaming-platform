//go:build integration

package payments

import (
	"context"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// ADR 0112 gate matrix (ledger-finance L-2), 0124 hold-release admission: a pending_launch tenant
// or brand is NOT active, so the hold-release request is admitted exactly as for a suspended one
// (the admission test is "not active", never "suspended"), and the pending status is recorded
// verbatim. Nothing moves on request.
func TestHSEC_HoldRelease_PendingLaunchSubjects_AdmittedAsNonActive(t *testing.T) {
	for _, kind := range []string{"tenant", "brand"} {
		t.Run(kind, func(t *testing.T) {
			h := newHSR(t, 1)
			wr := h.hold(700)
			cash := h.walletBalance("player_cash")
			if kind == "tenant" {
				if err := launchfix.ForcePending(context.Background(), t, h.pool, h.f.tenantID, nil); err != nil {
					t.Fatal(err)
				}
			} else if err := launchfix.ForcePending(context.Background(), t, h.pool, h.f.tenantID, &h.f.brandID); err != nil {
				t.Fatal(err)
			}
			r := h.mustRequest(h.reqA, wr.ID)
			if kind == "tenant" && r.TenantStatusAtSubmission != "pending_launch" || kind == "brand" && r.BrandStatusAtSubmission != "pending_launch" {
				t.Fatalf("the pending status must be recorded verbatim: %+v", r)
			}
			if h.walletBalance("player_withdrawal_hold") != 700 || h.walletBalance("player_cash") != cash {
				t.Fatal("a request must move nothing")
			}
			h.assertInvariants()
		})
	}
}
