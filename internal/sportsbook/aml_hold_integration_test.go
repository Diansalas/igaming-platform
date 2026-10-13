//go:build integration

package sportsbook

import (
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// ADR 0116 s5.1 at the sportsbook gate: a NEW bet on a held player is rejected with the SHARED public result, identical to a
// KYC-pending rejection; settlement, void and rollback of bets that already exist are never gated.

func holdSBPlayer(t *testing.T, pool *db.Pool, f sbFixture, scope aml.HoldScope) {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	prooftest.Install(t, u)
	amlfix.HoldFor(t, launchfix.OwnerFor(t, pool), pool, f.tenantID, sbPersonID(t, pool, f), nil, scope)
}

func TestAMLHold_SportsbookBet_HeldAndKYCPending_SameRejection(t *testing.T) {
	pool := testPool(t)

	pf := seedFixture(t, pool)
	fundWallet(t, pool, pf, 10_000)
	mustActiveSportsbookPlayPolicy(t, pool, mustLicenseSBTenant(t, pool, pf))
	time.Sleep(10 * time.Millisecond)
	mustSeedSBVerification(t, pool, pf, sbPersonID(t, pool, pf), "pending")

	hf := seedFixture(t, pool)
	fundWallet(t, pool, hf, 10_000)
	holdSBPlayer(t, pool, hf, aml.ScopeAllValueMovement)

	sel := seedSelection(t, pool, seedSelectionParams{OddsNumerator: 250, OddsDenominator: 100})
	type view struct {
		Accepted bool
		Category string
		Code     string
		Message  string
	}
	got := map[string]view{}
	for name, f := range map[string]sbFixture{"pending": pf, "held": hf} {
		res, err := placeBet(t, pool, f, sel, 1_000, "aml-sb-"+name)
		if err != nil {
			t.Fatalf("%s: place bet: %v", name, err)
		}
		got[name] = view{res.Accepted, res.RejectionCategory, res.RejectionCode, res.RejectionMessage}
		if bal := cashBalance(t, pool, f); bal != 10_000 {
			t.Fatalf("%s: a rejected bet posts nothing: %d", name, bal)
		}
	}
	if got["pending"] != got["held"] || got["held"].Accepted || got["held"].Code != "account_review" || got["held"].Category != RejectionKYCDenied {
		t.Fatalf("the held and KYC-pending rejections must be identical and carry account_review: %+v", got)
	}
}

// LF: settlement of a bet placed BEFORE the hold is never gated, and neither is a void.
func TestAMLHold_SportsbookSettlementAndVoid_AreNeverGated(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	sel2 := seedSelection(t, pool, seedSelectionParams{})
	second, err := placeBet(t, pool, f, sel2, stdStake, "aml-void-bet-"+uuid.NewString())
	if err != nil || !second.Accepted {
		t.Fatalf("place the second (to be voided) bet: accepted=%v err=%v", second.Accepted, err)
	}
	voidBet := second.Bet.ID
	before := cashBalance(t, pool, f)
	holdSBPlayer(t, pool, f, aml.ScopeAllValueMovement)

	res := mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	if res.Result != SettlementResultApplied {
		t.Fatalf("a settlement must not be gated by a hold: %+v", res)
	}
	if got := cashBalance(t, pool, f); got-before != stdPayout {
		t.Fatalf("the payout credits the held player: %d -> %d (payout %d)", before, got, stdPayout)
	}
	vres := mustSimulate(t, pool, f.tenantID, SettlementEvent{
		TenantID: f.tenantID, BetID: voidBet, ActorStaffID: actor, EventType: SettlementEventVoid, VoidReason: "market_cancelled",
	})
	if vres.Result != SettlementResultApplied {
		t.Fatalf("a void must not be gated by a hold: %+v", vres)
	}
}
