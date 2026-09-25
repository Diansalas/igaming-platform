//go:build integration

// OB-1 (ADR 0088 §2.5, OPEN): rolling back a won settlement debits CASH by
// the full payout. If the player already spent or withdrew it,
// player_cash goes negative - and that posting MUST still succeed (no
// balance check, no RG/eligibility gate on rollback). LOCKED must never go
// negative.
package sportsbook

import "testing"

func TestOB1_RollbackOfWonSettlementCanDriveCashNegative(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)

	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	if got := cashBalance(t, pool, f); got <= 0 {
		t.Fatalf("sanity: cash should be positive right after a won settlement, got %d", got)
	}

	// The player spends/withdraws everything, including the winnings just
	// credited (funded - stake + payout).
	spend := funded - stdStake + stdPayout
	debitCash(t, pool, f, spend)
	if got := cashBalance(t, pool, f); got != 0 {
		t.Fatalf("sanity: cash should be exactly 0 after spending it all, got %d", got)
	}

	res, err := simulateSettlement(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))
	if err != nil {
		t.Fatalf("rollback after the player spent the payout must still succeed, got error: %v", err)
	}
	if res.Rejected() {
		t.Fatalf("rollback after the player spent the payout must never be rejected (no balance/RG gate), got %q", res.RejectionCode)
	}
	if res.BetStatus != BetStatusOpen {
		t.Fatalf("bet status = %q, want open", res.BetStatus)
	}

	if got := cashBalance(t, pool, f); got != -stdPayout {
		t.Fatalf("player_cash after rollback = %d, want %d (negative; OB-1's receivable)", got, -stdPayout)
	}
	if got := lockedCashBalance(t, pool, f); got != stdStake {
		t.Fatalf("player_locked_cash after rollback = %d, want %d", got, stdStake)
	}
	if got := lockedCashBalance(t, pool, f); got < 0 {
		t.Fatalf("player_locked_cash must never go negative, got %d", got)
	}
}
