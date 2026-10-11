//go:build integration

package sportsbook

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// ADR 0112 slice-3 LOW item. Since migration 0130 no governed path can close a brand that still has an
// open bet (LA024), so the closed-brand rows of TestBrandGate_Sportsbook_Matrix resolve their open bets
// first. The population the brand gate was designed for, though, includes a brand closed BEFORE 0130
// that still holds open bets: the terminal stake return (void) and the settlement of a bet the brand
// already accepted must keep working on it, a new wager is declined with the brand reason, a sibling
// brand is unaffected, and ledger invariants and reconciliation stay clean. launchfix.ForceClosed
// reproduces that subject (the sanctioned integration-only exception, no governed sequence).
func TestBrandGate_Sportsbook_ClosedPre0130_OpenBets_VoidAndSettleStillApply(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	f, actor, openBet := newStdBet(t, owner) // open bet A: to be voided (stake return)
	sel := seedSelection(t, owner, seedSelectionParams{})
	b, err := placeBet(t, owner, f, sel, stdStake, "bg-pre0130-b-"+uuid.NewString()) // open bet B: settled with payout
	if err != nil || !b.Accepted {
		t.Fatalf("bet B: %+v err=%v", b, err)
	}
	sib := seedSiblingBrandSB(t, owner, f)
	fundWallet(t, owner, sib, funded)

	// The governed closure is refused while the bets are open (the point of the backstop) ...
	if got := bgPgCode(launchfix.TrySetBrandStatus(context.Background(), f.tenantID, f.brandID, "closed")); got != "LA024" {
		t.Fatalf("the governed closure over open bets must be refused LA024, got %q", got)
	}
	// ... so the pre-0130 population is reproduced by the sanctioned fixture.
	if err := launchfix.ForceClosed(context.Background(), t, owner, f.tenantID, &f.brandID); err != nil {
		t.Fatalf("force closed: %v", err)
	}
	txs0, entries0 := ledgerCounts(t, owner, f.tenantID)

	res, err := placeBetRT(t, rt, f, sel, 100, "bg-pre0130-new-"+uuid.NewString())
	if err != nil || res.Accepted || res.RejectionCategory != RejectionBrandNotActive || res.RejectionCode != "brand_not_active" {
		t.Fatalf("a new bet on a brand closed before 0130 must be declined brand_not_active: %+v err=%v", res, err)
	}
	if txs, entries := ledgerCounts(t, owner, f.tenantID); txs != txs0 || entries != entries0 {
		t.Fatalf("a declined bet changed the ledger: tx %d->%d entries %d->%d", txs0, txs, entries0, entries)
	}

	// Terminal stake return of an OPEN bet on the closed brand.
	vres, err := simulateSettlement(t, rt, f.tenantID, voidEvent(openBet, actor, "market_cancelled"))
	if err != nil || vres.Rejected() || vres.Result != SettlementResultApplied || vres.BetStatus != BetStatusVoid {
		t.Fatalf("a void of an open bet must apply on a brand closed before 0130: %+v err=%v", vres, err)
	}
	// Settlement with payout of a bet the brand had already accepted.
	sres, err := simulateSettlement(t, rt, f.tenantID, settleEvent(b.Bet.ID, actor, 1, SettlementOutcomeWon, stdPayout))
	if err != nil || sres.Rejected() || sres.Result != SettlementResultApplied {
		t.Fatalf("settlement of an accepted bet must apply on a brand closed before 0130: %+v err=%v", sres, err)
	}
	// A sibling brand of the same tenant is unaffected.
	sb, err := placeBetRT(t, rt, sib, sel, 100, "bg-pre0130-sib-"+uuid.NewString())
	if err != nil || !sb.Accepted {
		t.Fatalf("a bet on the sibling (active) brand must be accepted: %+v err=%v", sb, err)
	}
	srInvariants(t, owner, f.tenantID)
	bgReconcileClean(t, owner, f.tenantID)
}
