//go:build integration

// A5 / owner decision 13: a casino bet on a wallet whose cash balance is negative is declined with the
// RG decline reason wallet_negative_balance, with no ledger effect and an audit record. (Wins, rollbacks,
// settlements and voids are never RG-gated, ADR 0115 A6; this gate does not change that.)
package casino

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/testsupport/negbal"
)

func TestA5Neg_BetDeclinedOnNegativeWallet_NoLedgerEffect(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 1000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider)).WithUngatedMarketOpenForTests()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := negbal.Chargeback(ctx, tx, f.tenantID, f.walletID, "EUR", 1500) // -500
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := cashBalance(t, pool, f)
	if before != -500 {
		t.Fatalf("setup: cash %d, want -500", before)
	}
	debitsBefore, _ := sumDebitsCredits(t, pool, f.tenantID)

	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-a5-neg", "", "round-a5-neg", "game-1", 100, "EUR", OutcomeSucceeded, "", f.playerRef, sessionID)
	var result ReceiveCallbackResult
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	}); err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}
	if result.Outcome != OutcomeDeclined || result.DeclineReason != rg.CodeWalletNegativeBalance {
		t.Fatalf("want a declined bet %q, got %+v", rg.CodeWalletNegativeBalance, result)
	}
	if got := cashBalance(t, pool, f); got != before {
		t.Fatalf("a declined bet posts nothing: %d -> %d", before, got)
	}
	if d, c := sumDebitsCredits(t, pool, f.tenantID); d != debitsBefore || d != c {
		t.Fatalf("no ledger effect and balanced: debits %d -> %d, credits %d", debitsBefore, d, c)
	}
	if !auditActionExists(t, pool, f.tenantID, "casino_bet.denied_by_rg_policy") {
		t.Fatal("the denial must be audited")
	}
}
