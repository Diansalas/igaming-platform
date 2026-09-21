//go:build integration

// ADR 0082 §6 test 2 - finding LOCK-1b, the cycle this package contains
// with no internal/casino code involved at all.
package payments

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

func loPaymentsAccount(t *testing.T, pool *db.Pool, f orchFixture, walletID *uuid.UUID, at ledger.AccountType) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, walletID, at, "EUR")
		return err
	})
	if err != nil {
		t.Fatalf("resolve %s account: %v", at, err)
	}
	return id
}

// loConfirmedDeposit initiates a deposit and delivers its success
// callback, returning the provider reference the reversal will name.
func loConfirmedDeposit(t *testing.T, pool *db.Pool, f orchFixture, orch *Orchestrator, provider *MockProvider, amount int64) string {
	t.Helper()
	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = orch.InitiateDeposit(ctx, tx, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: amount, PaymentMethod: "card",
			IdempotencyKey: "lockorder-dep-" + uuid.NewString(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	if intent.ProviderReference == nil {
		t.Fatal("intent has no provider reference")
	}
	ref := *intent.ProviderReference
	payload := provider.CallbackPayload(CallbackEventDeposit, ref, "", OutcomeSucceeded, amount, "EUR", "", false)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-psp", payload)
		return err
	}); err != nil {
		t.Fatalf("confirm deposit: %v", err)
	}
	return ref
}

// loPendingDeposit initiates a deposit WITHOUT confirming it, returning
// the success-callback payload to be delivered later.
func loPendingDeposit(t *testing.T, pool *db.Pool, f orchFixture, orch *Orchestrator, provider *MockProvider, amount int64) []byte {
	t.Helper()
	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = orch.InitiateDeposit(ctx, tx, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: amount, PaymentMethod: "card",
			IdempotencyKey: "lockorder-dep-" + uuid.NewString(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	if intent.ProviderReference == nil {
		t.Fatal("intent has no provider reference")
	}
	return provider.CallbackPayload(CallbackEventDeposit, *intent.ProviderReference, "", OutcomeSucceeded, amount, "EUR", "", false)
}

// TestLockOrder_ConcurrentDepositAndDepositReversal_NoDeadlock is finding
// LOCK-1b (ADR 0082 §1.6), which that inventory recorded for the first
// time and which nothing in this repository previously tested:
//
//	postDepositSuccess posts [Dr psp_clearing, Cr player_cash] - so it
//	takes psp_clearing's projection lock and then player_cash's, via
//	migration 0023's AFTER INSERT trigger, in entry-slice order. The
//	deposit-reversal path posts [Dr player_cash, Cr psp_clearing] - the
//	same two rows in the OPPOSITE order. Neither path takes any lock
//	outside ledger.Post, so this is LOCK-1's defect with no casino code
//	anywhere near it.
//
// The two callbacks name DIFFERENT deposits on the SAME wallet+asset,
// which is the point: a reversal of the same deposit would serialize on
// deposit_intents, but a reversal of a DIFFERENT one shares only the two
// projection rows.
//
// Closed entirely by ledger.Post's own internal L3 pre-lock - §4.5 needed
// no locking change in this package at all - and shown failing on HEAD
// with SQLSTATE 40P01 before that pre-lock existed.
func TestLockOrder_ConcurrentDepositAndDepositReversal_NoDeadlock(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider})

	// Deposit #1: already confirmed, and the one that will be reversed.
	const reversedAmount = int64(4_000)
	reversedRef := loConfirmedDeposit(t, pool, f, orch, provider, reversedAmount)

	// Deposit #2: initiated, confirmation delivered during the race.
	const newAmount = int64(2_500)
	newDepositPayload := loPendingDeposit(t, pool, f, orch, provider, newAmount)

	reversalPayload := provider.CallbackPayload(CallbackEventDepositReversal,
		"lockorder-reversal-ref", reversedRef, OutcomeSucceeded, reversedAmount, "EUR", "", false)

	cashID := loPaymentsAccount(t, pool, f, &f.walletID, ledger.AccountPlayerCash)
	clearingID := loPaymentsAccount(t, pool, f, nil, ledger.AccountPSPClearing)

	blockerCash := loHoldProjectionRow(t, pool, f.tenantID, cashID, "player_cash")
	blockerClearing := loHoldProjectionRow(t, pool, f.tenantID, clearingID, "psp_clearing")

	startDeposit := func() *loRacer {
		return loStartRacer(t, pool, f.tenantID, "postDepositSuccess(clearing,cash)", func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-psp", newDepositPayload)
			return err
		})
	}
	startReversal := func() *loRacer {
		return loStartRacer(t, pool, f.tenantID, "depositReversal(cash,clearing)", func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-psp", reversalPayload)
			return err
		})
	}

	racerDep, racerRev, errDep, errRev := loRunABBA(t, pool, startDeposit, startReversal,
		[]*loBlocker{blockerCash, blockerClearing})
	loAssertNoDeadlock(t, "LOCK-1b (deposit vs. reversal of a different deposit)",
		map[string]error{racerDep.name: errDep, racerRev.name: errRev})
	if errDep != nil {
		t.Fatalf("the second deposit must post: %v", errDep)
	}
	if errRev != nil {
		t.Fatalf("the reversal must post: %v", errRev)
	}

	// 4000 in, 2500 in, 4000 back out.
	if want, got := reversedAmount+newAmount-reversedAmount, cashBalance(t, pool, f); got != want {
		t.Fatalf("expected player_cash %d, got %d", want, got)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}
