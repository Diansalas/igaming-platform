//go:build integration

// lic17 M-1 (ledger-finance review): hold_for_review is a PARK. The read API states it plainly and reports the tenant's open
// withdrawals parked by a WD056 refusal with the park age; a platform caller is told where to look instead (the decision rows
// are tenant-scoped). The parked funds stay on the player's withdrawal hold (ledger balanced). MOCK providers only.
package payments

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
	"github.com/Diansalas/igaming-platform/internal/withdrawalpolicy"
)

func TestLic17_M1_ParkedSummary_AndHoldForReviewSemantics(t *testing.T) {
	w := newF4World(t, "l17-parked")
	l11PaySuspendLicence(t, w.pool, w.f.tenantID)
	svc, err := withdrawalpolicy.New(withdrawalpolicy.Config{Pool: w.pool, LockTimeout: "3s"})
	if err != nil {
		t.Fatal(err)
	}
	staff := depositKillSwitchStaffPrincipal(t, w.pool, w.f.tenantID) // a tenant_admin of the tenant
	tenantActor := withdrawalpolicy.Actor{StaffID: staff, Scope: withdrawalpolicy.ScopeTenant, TenantID: w.f.tenantID, Role: string(auth.RoleTenantAdmin)}
	platformActor := withdrawalpolicy.Actor{StaffID: uuid.New(), Scope: withdrawalpolicy.ScopePlatform, Role: string(auth.RolePlatformAdmin)}
	read := func(a withdrawalpolicy.Actor) *withdrawalpolicy.EffectiveView {
		t.Helper()
		v, err := svc.Effective(context.Background(), a, w.f.tenantID)
		if err != nil {
			t.Fatalf("effective: %v", err)
		}
		return v
	}

	v := read(tenantActor)
	if !v.Parked.Visible || v.Parked.Count != 0 || v.Parked.OldestParkedSince != nil || v.Parked.OldestAgeSeconds != nil {
		t.Fatalf("nothing parked yet: %+v", v.Parked)
	}
	if len(v.ActionSemantics) != 3 {
		t.Fatalf("three action semantics, got %d", len(v.ActionSemantics))
	}
	for _, as := range v.ActionSemantics {
		if as.Action == withdrawalpolicy.ActionHoldForReview && (!strings.Contains(as.Meaning, "PARK, not a review queue") || !strings.Contains(as.Meaning, "NO per-withdrawal release")) {
			t.Fatalf("hold_for_review must be stated as a park without release: %q", as.Meaning)
		}
	}

	a := approvedWithdrawal(t, w.pool, w.f, 300, "l17-parked-a")
	b := approvedWithdrawal(t, w.pool, w.f, 200, "l17-parked-b")
	holdBefore := w.holdBalance(t)
	l11PayPolicy(t, w.pool, w.f.tenantID, "licence_suspended", "hold_for_review")
	for _, wr := range []withdrawal.WithdrawalRequest{a, b} {
		if _, err := w.claim(t, wr.ID); !errors.Is(err, ErrPayoutWithdrawalGateDenied) {
			t.Fatalf("parked: %v", err)
		}
	}
	v = read(tenantActor)
	if v.Parked.Count != 2 || v.Parked.OldestParkedSince == nil || v.Parked.OldestAgeSeconds == nil || *v.Parked.OldestAgeSeconds < 0 {
		t.Fatalf("two parked withdrawals with an age: %+v", v.Parked)
	}
	if w.holdBalance(t) != holdBefore {
		t.Fatal("the parked amounts stay on the withdrawal hold")
	}
	// A re-claim while the cause is still in force parks again: still counted once, the park start unchanged.
	since := *v.Parked.OldestParkedSince
	if _, err := w.claim(t, a.ID); !errors.Is(err, ErrPayoutWithdrawalGateDenied) {
		t.Fatalf("parked again: %v", err)
	}
	if v = read(tenantActor); v.Parked.Count != 2 || !v.Parked.OldestParkedSince.Equal(since) {
		t.Fatalf("a re-park keeps the count and the park start: %+v", v.Parked)
	}
	// The policy widened (owner path here; the governed path needs two approvers): one withdrawal claims and leaves the count.
	l11PayPolicy(t, w.pool, w.f.tenantID, "licence_suspended", "allow")
	if c, err := w.claim(t, b.ID); err != nil || c.Request.State != withdrawal.StateSubmitted {
		t.Fatalf("claims after the widening: %v", err)
	}
	if v = read(tenantActor); v.Parked.Count != 1 {
		t.Fatalf("one withdrawal still parked (its latest decision is a WD056 refusal), got %+v", v.Parked)
	}
	// Platform caller: not visible, told where to look.
	if p := read(platformActor).Parked; p.Visible || p.Count != 0 || p.Note != withdrawalpolicy.ParkedPlatformNote {
		t.Fatalf("platform caller: %+v", p)
	}
	loAssertBalanced(t, w.pool, w.f.tenantID)
	loAssertProjectionMatchesRebuild(t, w.pool, w.f.tenantID)
}
