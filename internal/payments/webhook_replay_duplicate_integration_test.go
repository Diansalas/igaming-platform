//go:build integration

// PAY-WH-TENANT-1 (ADR 0090; docs/decisions/0022 §3 amendment 2026-09-26).
// QA binding test plan (docs/plans/stage-10.1-planning/
// 16-pay-wh-review-qa-test-plan.md): T8a/T8b/T8c at the Orchestrator level
// (package payments). Flagged by the implementer as covered only
// TRANSITIVELY (via older suites) before this file existed - these tests
// exercise ReceiveCallback directly, asserting the exact ledger-row-count
// invariant the plan requires, not merely "the HTTP call returned 200
// twice".
package payments

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// ledgerTransactionCount is a package-local count helper (mirrors
// internal/httpserver's ledgerTransactionCountForProviderRef) scoped by
// provider_tx_id, run under tenantID's own RLS scope.
func ledgerTransactionCount(t *testing.T, pool *db.Pool, tenantID uuid.UUID, providerTxID string) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_tx_id = $2`, tenantID, providerTxID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count ledger transactions for %q: %v", providerTxID, err)
	}
	return count
}

// TestWebhook_Replay_SameSuccess_NoSecondCredit is QA plan T8a: a
// same-tenant success callback delivered twice, SEQUENTIALLY, must post
// exactly one ledger credit total - the second delivery re-verifies (it is
// not short-circuited before authentication) but is absorbed by
// postDepositSuccess's own redelivered-success idempotency short-circuit.
func TestWebhook_Replay_SameSuccess_NoSecondCredit(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = orch.InitiateDeposit(ctx, tx, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "t8a-idem",
		})
		return err
	})
	if err != nil || intent.ProviderReference == nil {
		t.Fatalf("InitiateDeposit: intent=%+v err=%v", intent, err)
	}
	ref := *intent.ProviderReference

	payload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)

	for i := 0; i < 2; i++ {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-psp", payload)
			return err
		})
		if err != nil {
			t.Fatalf("delivery %d: expected success (idempotent replay must still return no error), got %v", i+1, err)
		}
	}

	if got := ledgerTransactionCount(t, pool, f.tenantID, ref); got != 1 {
		t.Fatalf("expected exactly 1 ledger transaction after 2 identical deliveries, got %d", got)
	}
}

// TestWebhook_Replay_ReversalReplayed_Conflict is QA plan T8b: a reversal
// callback delivered twice must post exactly one compensating entry - the
// second delivery is rejected (PAY-REV-1's ErrDepositAlreadyReversed), not
// silently absorbed, but must produce NO second reversal ledger row.
func TestWebhook_Replay_ReversalReplayed_Conflict(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = orch.InitiateDeposit(ctx, tx, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 6000, PaymentMethod: "card", IdempotencyKey: "t8b-idem",
		})
		return err
	})
	if err != nil || intent.ProviderReference == nil {
		t.Fatalf("InitiateDeposit: intent=%+v err=%v", intent, err)
	}
	originalRef := *intent.ProviderReference

	depositPayload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, originalRef, "", OutcomeSucceeded, 6000, "EUR", "", false)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-psp", depositPayload)
		return err
	})
	if err != nil {
		t.Fatalf("post the original deposit: %v", err)
	}

	const reversalRef = "t8b-reversal-ref"
	reversalPayload := provider.CallbackPayload(f.tenantID, CallbackEventDepositReversal, reversalRef, originalRef, OutcomeSucceeded, 6000, "EUR", "", false)

	// First reversal delivery: accepted.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-psp", reversalPayload)
		return err
	})
	if err != nil {
		t.Fatalf("first reversal delivery must succeed: %v", err)
	}
	if got := ledgerTransactionCount(t, pool, f.tenantID, reversalRef); got != 1 {
		t.Fatalf("expected exactly 1 reversal ledger transaction after the first delivery, got %d", got)
	}

	// Second, replayed delivery of the IDENTICAL reversal reference: the
	// per-tenant/provider ledger idempotency key already covers the exact
	// resend, so this must remain a no-op returning no error (not a second
	// posting) - distinct from a DIFFERENT reversal reference naming an
	// already-reversed original, which is ErrDepositAlreadyReversed.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-psp", reversalPayload)
		return err
	})
	if err != nil {
		t.Fatalf("replayed reversal (identical reference) must be an idempotent no-op, got error: %v", err)
	}
	if got := ledgerTransactionCount(t, pool, f.tenantID, reversalRef); got != 1 {
		t.Fatalf("expected STILL exactly 1 reversal ledger transaction after the replay, got %d", got)
	}

	// A DIFFERENT reversal reference naming the SAME already-reversed
	// original must be rejected outright (409-shaped ErrDepositAlreadyReversed),
	// with no third posting.
	const secondReversalRef = "t8b-second-reversal-ref"
	secondReversalPayload := provider.CallbackPayload(f.tenantID, CallbackEventDepositReversal, secondReversalRef, originalRef, OutcomeSucceeded, 6000, "EUR", "", false)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-psp", secondReversalPayload)
		return err
	})
	if !errors.Is(err, ErrDepositAlreadyReversed) {
		t.Fatalf("expected ErrDepositAlreadyReversed for a distinct-reference reversal of an already-reversed deposit, got %v", err)
	}
	if got := ledgerTransactionCount(t, pool, f.tenantID, secondReversalRef); got != 0 {
		t.Fatalf("expected NO ledger transaction for the rejected second-reference reversal, got %d", got)
	}
}

// TestWebhook_ConcurrentDuplicates_ExactlyOnePosting is QA plan T8c: N
// goroutines deliver the IDENTICAL, correctly-signed A->A success callback
// concurrently. Exactly one ledger posting must result, and no duplicate-
// key error may surface as anything other than a clean, absorbed no-op
// (never a panic, never a 500-shaped unexpected error). Run under
// `go test -race`.
func TestWebhook_ConcurrentDuplicates_ExactlyOnePosting(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = orch.InitiateDeposit(ctx, tx, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 7000, PaymentMethod: "card", IdempotencyKey: "t8c-idem",
		})
		return err
	})
	if err != nil || intent.ProviderReference == nil {
		t.Fatalf("InitiateDeposit: intent=%+v err=%v", intent, err)
	}
	ref := *intent.ProviderReference
	payload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 7000, "EUR", "", false)

	const n = 8
	// A start barrier (never a sleep) maximizes actual concurrent overlap:
	// every goroutine blocks on the same channel close rather than being
	// staggered by scheduling order.
	start := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-psp", payload)
				return err
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: expected every concurrent identical delivery to be absorbed as a clean no-op, got %v", i, err)
		}
	}

	if got := ledgerTransactionCount(t, pool, f.tenantID, ref); got != 1 {
		t.Fatalf("expected exactly 1 ledger transaction after %d concurrent identical deliveries, got %d", n, got)
	}
}
