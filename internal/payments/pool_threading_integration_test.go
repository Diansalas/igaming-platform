//go:build integration

// RV-PRH-I1 kill-switch phase 2 code review C2: the commit message claims
// "pool is threaded through every call site", but every existing test used
// MockCredentialResolver (which ignores its pool argument) or a fake that
// also ignored it, so nothing actually noticed a nil pool - mutants M5
// (callProvider passes nil to Resolve) and M6 (DispatchWithdraw passes nil
// to callProvider) both survived. This file adds a recording resolver that
// asserts it received the caller's own, non-nil pool, driven through each
// named call site: InitiateDepositAttempt, driveCreatedAttempt (cascade),
// DispatchWithdraw, PollPayoutStatus and Sweeper.processViaQueryStatus.
package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providercred"
)

// poolRecordingResolver wraps MockCredentialResolver's own credential
// synthesis but additionally records every pool argument it was called
// with, so a test can assert it was never nil and was always the SAME
// pool the caller itself was given - the exact property C2 requires.
type poolRecordingResolver struct {
	calls int
	pools []providercred.TenantTxRunner
}

func (r *poolRecordingResolver) Resolve(ctx context.Context, pool providercred.TenantTxRunner, tenantID uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	r.calls++
	r.pools = append(r.pools, pool)
	return MockCredentialResolver{}.Resolve(ctx, pool, tenantID, providerID)
}

// assertAllPoolsAre fails the test unless every recorded pool call
// received exactly want (never nil, never a different pool).
func (r *poolRecordingResolver) assertAllPoolsAre(t *testing.T, want *db.Pool) {
	t.Helper()
	if r.calls == 0 {
		t.Fatal("expected at least one resolver call, got none")
	}
	for i, p := range r.pools {
		if p == nil {
			t.Fatalf("call %d: resolver received a nil pool", i)
		}
		got, ok := p.(*db.Pool)
		if !ok {
			t.Fatalf("call %d: resolver received a pool of unexpected type %T", i, p)
		}
		if got != want {
			t.Fatalf("call %d: resolver received a DIFFERENT pool than the caller's own", i)
		}
	}
}

// TestInitiateDepositAttempt_PoolThreadedToResolver kills M5 for the
// deposit T1+T2/phase-B call site.
func TestInitiateDepositAttempt_PoolThreadedToResolver(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp-pool-a", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-pool-a": provider}, MultiWebhookCredentialResolver{"mock-psp-pool-a": NewMockWebhookCredentials(provider)})

	resolver := &poolRecordingResolver{}
	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, resolver, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "pool-thread-deposit",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if !res.AttemptCreated {
		t.Fatal("expected an attempt to be created")
	}
	resolver.assertAllPoolsAre(t, pool)
}

// TestDriveCreatedAttemptCascade_PoolThreadedToResolver kills M5 for the
// cascade driver (drive.go's driveCreatedAttempt, invoked from
// InitiateDepositAttempt's own synchronous cascade loop).
func TestDriveCreatedAttemptCascade_PoolThreadedToResolver(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	declining := NewMockProvider("mock-psp-pool-b1", "EUR")
	accepting := NewMockProvider("mock-psp-pool-b2", "EUR")
	accepting.AcceptAllAmounts = true
	registerCapability(t, pool, f, declining, 10)
	registerCapability(t, pool, f, accepting, 20)
	orch := NewOrchestrator(
		map[string]PaymentProvider{"mock-psp-pool-b1": declining, "mock-psp-pool-b2": accepting},
		MultiWebhookCredentialResolver{"mock-psp-pool-b1": NewMockWebhookCredentials(declining), "mock-psp-pool-b2": NewMockWebhookCredentials(accepting)},
	)

	resolver := &poolRecordingResolver{}
	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, resolver, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: MockAmountProviderDeclineCascade, PaymentMethod: "card", IdempotencyKey: "pool-thread-cascade",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Intent.Status != DepositIntentPending || res.Intent.ProviderID == nil || *res.Intent.ProviderID != "mock-psp-pool-b2" {
		t.Fatalf("setup: expected the cascade to land pending on mock-psp-pool-b2, got %+v", res.Intent)
	}
	if resolver.calls < 2 {
		t.Fatalf("expected at least 2 resolver calls (T1+T2 then the cascade child), got %d", resolver.calls)
	}
	resolver.assertAllPoolsAre(t, pool)
}

// TestDispatchWithdraw_PoolThreadedToResolver kills M6 directly:
// DispatchWithdraw must forward its OWN pool argument to the gate/resolver,
// not a nil placeholder. No withdrawal_request/payment_attempts row needs
// to exist in the database for this - DispatchWithdraw itself makes no
// database read; it only forwards pool to the resolver.
func TestDispatchWithdraw_PoolThreadedToResolver(t *testing.T) {
	pool := depositV2ScratchPool(t)
	provider := NewMockProvider("mock-psp-pool-c", "EUR")
	claimToken := uuid.New()
	providerID := "mock-psp-pool-c"
	attempt := PaymentAttempt{
		ID: uuid.New(), TenantID: uuid.New(), Operation: AttemptOperationPayout,
		ProviderID: &providerID, ClaimToken: &claimToken, State: AttemptSubmitting,
		PaymentMethod: "bank_transfer", AssetCode: "EUR", Amount: 500,
	}
	resolver := &poolRecordingResolver{}
	gr := DispatchWithdraw(context.Background(), pool, resolver, provider, attempt)
	if gr.Class == ErrorClassNotSent && !gr.Attempted {
		// A pre-flight gate refusal (e.g. a binding mismatch) would mean
		// the resolver was never reached the way this test intends -
		// fail loudly rather than silently pass on 0 calls.
		if resolver.calls == 0 {
			t.Fatalf("DispatchWithdraw never reached the resolver: %v", gr.Err)
		}
	}
	resolver.assertAllPoolsAre(t, pool)
}

// TestPollPayoutStatus_PoolThreadedToResolver kills M5 for the payout
// QueryStatus poll path (PollPayoutStatus's own callProvider call, reached
// only when the attempt is ambiguous/pending/submitting WITH a persisted
// provider reference - not the "no reference yet, reschedule" branch,
// which never reaches the gate at all).
func TestPollPayoutStatus_PoolThreadedToResolver(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-psp-pool-d", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-pool-d": provider}, MultiWebhookCredentialResolver{"mock-psp-pool-d": NewMockWebhookCredentials(provider)})

	wr := approvedWithdrawal(t, pool, f, MockAmountAmbiguous, "pool-thread-payout-poll")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("ClaimForDispatch: %v", err)
	}

	dispatchResolver := &poolRecordingResolver{}
	gr := DispatchWithdraw(context.Background(), pool, dispatchResolver, provider, claim.Attempt)
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}
	dispatchResolver.assertAllPoolsAre(t, pool)

	var attempt PaymentAttempt
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = GetAttemptByID(ctx, tx, claim.Attempt.ID)
		return err
	}); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if attempt.State != AttemptAmbiguous || attempt.ProviderReference == nil {
		t.Fatalf("setup: expected an ambiguous attempt with a persisted provider reference, got state=%s ref=%v", attempt.State, attempt.ProviderReference)
	}

	pollResolver := &poolRecordingResolver{}
	if err := PollPayoutStatus(context.Background(), pool, orch, pollResolver, f.tenantID, attempt, time.Now().Add(30*time.Second), nil); err != nil {
		t.Fatalf("PollPayoutStatus: %v", err)
	}
	pollResolver.assertAllPoolsAre(t, pool)
}

// TestSweeperProcessViaQueryStatus_PoolThreadedToResolver kills M5 for the
// deposit sweeper's own QueryStatus poll path
// (Sweeper.processViaQueryStatus, invoked via RunOnce -> processAttempt for
// a 'pending' deposit attempt).
func TestSweeperProcessViaQueryStatus_PoolThreadedToResolver(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp-pool-e", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-pool-e": provider}, MultiWebhookCredentialResolver{"mock-psp-pool-e": NewMockWebhookCredentials(provider)})

	resolver := &poolRecordingResolver{}
	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, resolver, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "pool-thread-sweep",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Attempt.State != AttemptPending {
		t.Fatalf("setup: expected pending, got %s", res.Attempt.State)
	}
	ref := *res.Attempt.ProviderReference
	setNextActionNow(t, pool, f.tenantID, res.Attempt.ID)
	provider.Resolve(ref, OutcomeSucceeded, "", false)

	sweeper := NewSweeper(pool, orch, AllowAllDepositKYCGate{}, resolver)
	stats := sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID})
	if len(stats.Errors) != 0 {
		t.Fatalf("sweeper errors: %v", stats.Errors)
	}
	if stats.Processed != 1 {
		t.Fatalf("expected 1 processed, got %d", stats.Processed)
	}
	if resolver.calls < 2 {
		t.Fatalf("expected at least 2 resolver calls (the initial deposit, then the sweeper's own QueryStatus poll), got %d", resolver.calls)
	}
	resolver.assertAllPoolsAre(t, pool)
}
