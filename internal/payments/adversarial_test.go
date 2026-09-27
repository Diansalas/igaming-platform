//go:build integration

package payments

// This file closes out the payments/provider-domain subset of the
// Stage 3B "26 mandatory adversarial financial tests" directive. Each
// test below is annotated with which numbered item (per the governing
// prompt's own payments-domain numbering) it verifies. It reuses the
// existing integration conventions from orchestrator_integration_test.go
// and capability_integration_test.go (testPool, seedOrchFixture,
// registerCapability, cashBalance) rather than redefining them.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// spyProvider wraps a *MockProvider so a test can count calls and,
// optionally, force a transport-level error - test-only instrumentation,
// never a second adapter implementation (the mock remains Stage 3B's only
// adapter; this wrapper only observes what it delegates to).
type spyProvider struct {
	*MockProvider

	mu               sync.Mutex
	depositCalls     int
	queryStatusCalls int
	// depositErr, when set, makes Deposit return this error instead of
	// delegating to the wrapped mock - simulating a provider call that
	// times out or errors transport-side (item 15) rather than returning
	// any DepositResult at all.
	depositErr error
}

func newSpyProvider(inner *MockProvider) *spyProvider {
	return &spyProvider{MockProvider: inner}
}

func (s *spyProvider) Deposit(ctx context.Context, req DepositRequest) (DepositResult, error) {
	s.mu.Lock()
	s.depositCalls++
	err := s.depositErr
	s.mu.Unlock()
	if err != nil {
		return DepositResult{}, err
	}
	return s.MockProvider.Deposit(ctx, req)
}

func (s *spyProvider) QueryStatus(ctx context.Context, providerReference string) (StatusResult, error) {
	s.mu.Lock()
	s.queryStatusCalls++
	s.mu.Unlock()
	return s.MockProvider.QueryStatus(ctx, providerReference)
}

func (s *spyProvider) DepositCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.depositCalls
}

func (s *spyProvider) QueryStatusCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queryStatusCalls
}

// --- Item 3: concurrent duplicate idempotency -----------------------------

// TestInitiateDeposit_ConcurrentSameIdempotencyKey_OnlyOneProviderCall fires
// N concurrent InitiateDeposit calls carrying the identical
// (tenant, player, idempotency_key) tuple and proves the database's own
// UNIQUE(tenant_id, player_account_id, idempotency_key) constraint (via
// db.IdempotentInsert), not application-level locking, is what makes only
// one of them ever reach the provider - every goroutine ends up with the
// SAME deposit intent, and the provider's Deposit method is called exactly
// once despite N concurrent attempts.
func TestInitiateDeposit_ConcurrentSameIdempotencyKey_OnlyOneProviderCall(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	spy := newSpyProvider(NewMockProvider("mock-psp", "EUR"))
	registerCapability(t, pool, f, spy, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": spy}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(spy.MockProvider)})

	params := InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 4000, PaymentMethod: "card", IdempotencyKey: "dep-concurrent-same-key",
	}

	const n = 8
	var wg sync.WaitGroup
	intents := make([]DepositIntent, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				intents[i], err = orch.InitiateDeposit(ctx, tx, params)
				return err
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, err)
		}
		if intents[i].ID != intents[0].ID {
			t.Fatalf("goroutine %d got a different intent id: %s vs %s", i, intents[i].ID, intents[0].ID)
		}
	}
	if got := spy.DepositCallCount(); got != 1 {
		t.Fatalf("expected exactly 1 provider Deposit call across %d concurrent InitiateDeposit calls sharing one idempotency key, got %d", n, got)
	}
}

// TestReceiveCallback_ConcurrentDuplicateCallbacksOnlyOnePosts fires N
// concurrent redeliveries of the identical success callback for one
// pending deposit intent and proves only one of them actually posts to the
// ledger - protected by ledger.Post's own (tenant_id, idempotency_key)
// uniqueness (ADR 0020), the same guarantee internal/ledger's own
// TestPost_ConcurrentDuplicatesOnlyOneWins exercises directly, exercised
// here through the payments orchestrator's ReceiveCallback path instead.
func TestReceiveCallback_ConcurrentDuplicateCallbacksOnlyOnePosts(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = initiateDepositWithAttempt(ctx, tx, orch, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 9000, PaymentMethod: "card", IdempotencyKey: "dep-concurrent-callback",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}

	payload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, *intent.ProviderReference, "", OutcomeSucceeded, 9000, "EUR", "", false)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", payload)
				return err
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, err)
		}
	}
	if balance := cashBalance(t, pool, f); balance != 9000 {
		t.Fatalf("expected exactly one deposit's worth (9000) after %d concurrent duplicate callbacks, got %d", n, balance)
	}
}

// TestReceiveCallback_ConcurrentDuplicateCallbacks_SecondBlocksOnReceiptKey
// strengthens the N=8 uncontrolled race above (A7-TESTS-1 R0 hardening):
// this is a deterministic, two-party version that proves WHERE the second
// identical delivery actually blocks - R0's own (tenant_id, provider_id,
// event_fingerprint) unique index on payment_provider_events (ADR 0082 A7
// §(7); the receipt insert is ApplyReceiptEvidence's first write, before
// the parent/attempt lock). The first delivery is held open (uncommitted)
// via loHoldWith so its receipt row is a real, in-flight, not-yet-visible
// unique-index entry; the second, identical delivery must queue on that
// row (the standard "wait to see if the conflicting inserter commits"
// behavior for ON CONFLICT), not race ahead and post a second time.
func TestReceiveCallback_ConcurrentDuplicateCallbacks_SecondBlocksOnReceiptKey(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp-r0", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-r0": provider}, MultiWebhookCredentialResolver{"mock-psp-r0": NewMockWebhookCredentials(provider)})

	var intent DepositIntent
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = initiateDepositWithAttempt(ctx, tx, orch, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 9000, PaymentMethod: "card", IdempotencyKey: "dep-r0-block",
		})
		return err
	}); err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	payload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, *intent.ProviderReference, "", OutcomeSucceeded, 9000, "EUR", "", false)

	// Blocker: the FIRST delivery, held open (uncommitted) right after its
	// R0 receipt insert (and everything after it, in the same tx) so a
	// second, identical delivery has a real, in-flight conflicting row to
	// queue on.
	blocker := loHoldWith(t, pool, f.tenantID, "first-delivery", func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp-r0", payload)
		return err
	})

	// Racer: the SECOND, identical delivery.
	racer := loStartRacer(t, pool, f.tenantID, "second-delivery", func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp-r0", payload)
		return err
	})
	blockingPIDs, ok := loWaitBlocked(t, pool, racer.pid, racer.done)
	if !ok {
		blocker.release()
		<-racer.done
		t.Fatalf("the second, identical delivery never blocked - R0's unique index is not serializing concurrent identical deliveries")
	}
	if !loContains(blockingPIDs, blocker.pid) {
		blocker.release()
		<-racer.done
		t.Fatalf("the second delivery is blocked by someone other than the first delivery (pid %d), blocked by %v", blocker.pid, blockingPIDs)
	}
	// A7-C1 (FH-6): pg_locks/pg_class cannot distinguish this wait from any
	// OTHER row-lock wait the same blocker might also hold (a `FOR UPDATE`
	// row wait is locktype='transactionid' with relation IS NULL - see
	// a7_lockorder_integration_test.go's own doc comment on this exact
	// Postgres behavior) - the blocker here holds BOTH the R0 receipt row
	// AND, later in the same held transaction, the parent lock, so a bare
	// "blocked by blocker.pid" check does not by itself prove WHICH of the
	// two the racer queued on. The query text does: the racer's own
	// backend, while blocked, must still be executing R0's own
	// payment_provider_events INSERT specifically, not e.g. a later
	// deposit_intents FOR UPDATE this same delivery would only reach AFTER
	// that insert returns.
	if q := loBackendQuery(t, pool, racer.pid); !strings.Contains(q, "INSERT INTO payment_provider_events") {
		blocker.release()
		<-racer.done
		t.Fatalf("the second delivery is blocked on something other than the R0 receipt insert: %q", q)
	}

	blocker.release()
	<-racer.done
	if racer.err != nil {
		t.Fatalf("the second, identical delivery must not error once unblocked: %v", racer.err)
	}

	if balance := cashBalance(t, pool, f); balance != 9000 {
		t.Fatalf("expected exactly one deposit's worth (9000) after the second delivery unblocked, got %d", balance)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- Item 4: same key, different payload -----------------------------------

// TestInitiateDeposit_RetryWithDifferentAmountRejected proves a retried
// InitiateDeposit call reusing the same idempotency key but a different
// amount is rejected with ErrIdempotencyKeyReused, never silently accepted
// with the new payload and never silently returned as the stale original
// without any signal to the caller that something is wrong. This exercises
// a real gap fixed in orchestrator.go as part of this task: InitiateDeposit
// previously returned the original intent unconditionally on any
// idempotency-key conflict, without checking whether the retried
// parameters actually matched - inconsistent with the identical, already-
// tested pattern in internal/ledger.Post and
// internal/withdrawal.RequestWithdrawal.
func TestInitiateDeposit_RetryWithDifferentAmountRejected(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	original := InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 3000, PaymentMethod: "card", IdempotencyKey: "dep-same-key-different-payload",
	}
	var first DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		first, err = orch.InitiateDeposit(ctx, tx, original)
		return err
	})
	if err != nil {
		t.Fatalf("first InitiateDeposit: %v", err)
	}

	retried := original
	retried.Amount = 999999 // same idempotency key, different amount

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.InitiateDeposit(ctx, tx, retried)
		return err
	})
	if !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("expected ErrIdempotencyKeyReused for a retried call with a different amount, got %v", err)
	}

	// Also confirm neither field-level narrowing on payment_method survives
	// the same check.
	retriedMethod := original
	retriedMethod.PaymentMethod = "bank_transfer"
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.InitiateDeposit(ctx, tx, retriedMethod)
		return err
	})
	if !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("expected ErrIdempotencyKeyReused for a retried call with a different payment method, got %v", err)
	}

	// The original, unmodified retry must still succeed as before.
	var second DepositIntent
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		second, err = orch.InitiateDeposit(ctx, tx, original)
		return err
	})
	if err != nil {
		t.Fatalf("unmodified retry: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected an unmodified retry to still return the original intent, got %s vs %s", second.ID, first.ID)
	}
}

// --- Item 15 & 17: provider timeout, and client retry after it -------------

// TestInitiateDeposit_ProviderTransportError_AmbiguousNotCascadedThenRetryNoSecondCall
// covers two related invariants in one scenario:
//
//   - Item 15: a provider call that errors/times out synchronously (never
//     returning a DepositResult at all) is modeled as OutcomeAmbiguous, not
//     a decline and not a success, and does NOT auto-cascade to the next
//     candidate provider.
//   - Item 17: a client-side retry with the same idempotency key after that
//     timeout returns the existing (still-ambiguous) intent and never calls
//     any provider a second time.
func TestInitiateDeposit_ProviderTransportError_AmbiguousNotCascadedThenRetryNoSecondCall(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	failing := newSpyProvider(NewMockProvider("mock-a", "EUR"))
	failing.depositErr = errors.New("simulated transport timeout")
	accepting := newSpyProvider(NewMockProvider("mock-b", "EUR"))
	registerCapability(t, pool, f, failing, 10)
	registerCapability(t, pool, f, accepting, 20)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-a": failing, "mock-b": accepting}, MultiWebhookCredentialResolver{"mock-a": NewMockWebhookCredentials(failing.MockProvider), "mock-b": NewMockWebhookCredentials(accepting.MockProvider)})

	params := InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 4500, PaymentMethod: "card", IdempotencyKey: "dep-timeout",
	}

	var first DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		first, err = orch.InitiateDeposit(ctx, tx, params)
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	if first.Status != DepositIntentAmbiguous {
		t.Fatalf("expected a provider transport error to finalize as ambiguous (never a silent decline or success), got %v", first.Status)
	}
	if first.ProviderID == nil || *first.ProviderID != "mock-a" {
		t.Fatalf("expected the ambiguous attempt to stay attributed to mock-a, got %v", first.ProviderID)
	}
	if got := failing.DepositCallCount(); got != 1 {
		t.Fatalf("expected exactly 1 Deposit call on the failing provider, got %d", got)
	}
	if got := accepting.DepositCallCount(); got != 0 {
		t.Fatalf("a transport-error/ambiguous outcome must never cascade to another provider, but mock-b's Deposit was called %d times", got)
	}

	var second DepositIntent
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		second, err = orch.InitiateDeposit(ctx, tx, params)
		return err
	})
	if err != nil {
		t.Fatalf("retried InitiateDeposit: %v", err)
	}
	if second.ID != first.ID || second.Status != DepositIntentAmbiguous {
		t.Fatalf("expected the retry to return the SAME still-ambiguous intent, got %+v", second)
	}
	if got := failing.DepositCallCount(); got != 1 {
		t.Fatalf("a client-side retry after a provider timeout must never call the provider a second time, got %d Deposit calls", got)
	}
	if got := accepting.DepositCallCount(); got != 0 {
		t.Fatalf("a client-side retry must never cascade to another provider either, but mock-b's Deposit was called %d times", got)
	}
}

// --- Item 16: ambiguous outcome must call QueryStatus before any decision --

// TestInitiateDeposit_AmbiguousOutcomeCallsQueryStatusBeforeNotCascading
// strengthens TestInitiateDeposit_AmbiguousOutcomeIsNotCascaded (which
// already proves the resulting intent status) with a direct call-count
// assertion: QueryStatus is actually invoked exactly once on the
// synchronously-ambiguous provider, and the second candidate's Deposit is
// never called - the ambiguity is resolved (or left open) via QueryStatus,
// not skipped.
func TestInitiateDeposit_AmbiguousOutcomeCallsQueryStatusBeforeNotCascading(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	a := newSpyProvider(NewMockProvider("mock-a", "EUR"))
	b := newSpyProvider(NewMockProvider("mock-b", "EUR"))
	b.AcceptAllAmounts = true
	registerCapability(t, pool, f, a, 10)
	registerCapability(t, pool, f, b, 20)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-a": a, "mock-b": b}, MultiWebhookCredentialResolver{"mock-a": NewMockWebhookCredentials(a.MockProvider), "mock-b": NewMockWebhookCredentials(b.MockProvider)})

	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = orch.InitiateDeposit(ctx, tx, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: MockAmountAmbiguous, PaymentMethod: "card", IdempotencyKey: "dep-ambiguous-querystatus",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	if intent.Status != DepositIntentAmbiguous {
		t.Fatalf("expected ambiguous, got %v", intent.Status)
	}
	if got := a.QueryStatusCallCount(); got != 1 {
		t.Fatalf("expected exactly 1 QueryStatus call to attempt resolving the ambiguous outcome before any cascade decision, got %d", got)
	}
	if got := b.DepositCallCount(); got != 0 {
		t.Fatalf("an ambiguous outcome must never auto-cascade without first calling QueryStatus, but mock-b's Deposit was called %d times", got)
	}
}

// TestReceiveCallback_AmbiguousCallbackResolvedViaQueryStatus_NotCascaded
// covers the same invariant on the OTHER code path that can observe an
// ambiguous outcome: a callback (not a synchronous Deposit return) that
// itself carries Outcome=ambiguous. Confirms ReceiveCallback also calls
// QueryStatus rather than treating the callback's ambiguity as a final
// answer, never silently posts a ledger entry, and never cascades.
// TestReceiveCallback_AmbiguousCallbackResolvedViaQueryStatus_NotCascaded
// name kept for history; behaviour adapted for the
// PRH-payments-callback-cutover (ADR 0095 §6.5, receipt.go's own package
// doc comment: "what a callback may never do - no cascade I/O, no
// QueryStatus"). Old->new: the pre-cutover callback path called
// provider.QueryStatus SYNCHRONOUSLY, inline, to try to resolve an
// ambiguous callback before returning - itself an instance of the
// provider-I/O-inside-a-transaction anti-pattern ADR 0095 eliminates.
// The receipt path never makes a provider call at all: an ambiguous
// callback moves the attempt to `ambiguous` with a scheduled
// next_action_at for the SWEEPER (asynchronous, outside this
// transaction) to resolve later - QueryStatus is therefore correctly
// called ZERO times here now. This test's original intent (an ambiguous
// callback must be actively tracked for resolution, never silently
// dropped, never treated as success/failure, never cascaded) is
// expressed by the equally strong replacement checks below: the attempt
// is durably `ambiguous` with a next_action_at scheduled (proving it is
// NOT a dead end), QueryStatus was NOT called synchronously (proving the
// I/O-in-transaction defect is gone), no cascade, no posting.
func TestReceiveCallback_AmbiguousCallbackResolvedViaQueryStatus_NotCascaded(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	a := newSpyProvider(NewMockProvider("mock-a", "EUR"))
	b := newSpyProvider(NewMockProvider("mock-b", "EUR"))
	b.AcceptAllAmounts = true
	registerCapability(t, pool, f, a, 10)
	registerCapability(t, pool, f, b, 20)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-a": a, "mock-b": b}, MultiWebhookCredentialResolver{"mock-a": NewMockWebhookCredentials(a.MockProvider), "mock-b": NewMockWebhookCredentials(b.MockProvider)})

	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = initiateDepositWithAttempt(ctx, tx, orch, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 4700, PaymentMethod: "card", IdempotencyKey: "dep-cb-ambiguous",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	if intent.Status != DepositIntentPending {
		t.Fatalf("expected a normal pending deposit awaiting callback, got %v", intent.Status)
	}

	ambiguousPayload := a.CallbackPayload(f.tenantID, CallbackEventDeposit, *intent.ProviderReference, "", OutcomeAmbiguous, 4700, "EUR", "", false)
	var result ReceiveCallbackResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-a", ambiguousPayload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}
	if result.Status != DepositIntentAmbiguous {
		t.Fatalf("an ambiguous callback must never be silently treated as success or failure, got %v", result.Status)
	}
	if got := a.QueryStatusCallCount(); got != 0 {
		t.Fatalf("ADR 0095 §6.5: a callback must never make a provider call (no cascade I/O, no QueryStatus) - expected 0 synchronous QueryStatus calls, got %d", got)
	}
	if got := b.DepositCallCount(); got != 0 {
		t.Fatalf("an ambiguous callback must never auto-cascade to another provider, but mock-b's Deposit was called %d times", got)
	}
	if balance := cashBalance(t, pool, f); balance != 0 {
		t.Fatalf("an unresolved ambiguous callback must never post a ledger entry, got balance %d", balance)
	}

	// The attempt is durably tracked for LATER (asynchronous, sweeper-
	// driven) resolution, not a silent dead end: state 'ambiguous' with a
	// next_action_at scheduled.
	var attemptState string
	var nextActionAt *time.Time
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT state, next_action_at FROM payment_attempts WHERE provider_id = 'mock-a' AND provider_reference = $1`,
			*intent.ProviderReference,
		).Scan(&attemptState, &nextActionAt)
	}); err != nil {
		t.Fatalf("query attempt state: %v", err)
	}
	if attemptState != "ambiguous" {
		t.Fatalf("expected the attempt itself to be 'ambiguous', got %q", attemptState)
	}
	if nextActionAt == nil {
		t.Fatal("expected next_action_at to be scheduled so the sweeper resolves this ambiguity later")
	}
}

// --- Item 18: provider swap requires no wallet/ledger code change ---------

// TestInitiateDeposit_ProviderSwapDoesNotRequireWalletOrLedgerCodeChange
// runs the identical InitiateDeposit -> ReceiveCallback call sequence
// against two differently-configured mock provider instances standing in
// for "the tenant swapped its active PSP" - mock-a is disabled and mock-b
// becomes the tenant's routable provider between the two deposits, using
// only a ProviderCapability configuration change (WriteCapability /
// registerCapability). No orchestrator, wallet, or ledger code differs
// between the two calls - the same generic InitiateDeposit/ReceiveCallback
// functions and the same wallet.GetSummary-backed balance check are used
// for both, exactly as docs/decisions/0022's provider-agnosticism claim
// requires.
func TestInitiateDeposit_ProviderSwapDoesNotRequireWalletOrLedgerCodeChange(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	providerA := NewMockProvider("mock-a", "EUR")
	registerCapability(t, pool, f, providerA, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-a": providerA}, MultiWebhookCredentialResolver{"mock-a": NewMockWebhookCredentials(providerA)})

	var intentA DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intentA, err = initiateDepositWithAttempt(ctx, tx, orch, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 3300, PaymentMethod: "card", IdempotencyKey: "dep-swap-a",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit against mock-a: %v", err)
	}
	payloadA := providerA.CallbackPayload(f.tenantID, CallbackEventDeposit, *intentA.ProviderReference, "", OutcomeSucceeded, 3300, "EUR", "", false)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-a", payloadA)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback against mock-a: %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 3300 {
		t.Fatalf("expected 3300 after mock-a's deposit, got %d", balance)
	}

	// Swap: disable mock-a's capability row and register mock-b as the
	// tenant's active provider instead. Purely configuration - no code path
	// below this line differs from the code path above it.
	declaredA := providerA.Capabilities()
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := WriteCapability(ctx, tx, providerA, f.tenantID, nil, CapabilityConfig{
			SupportedFiatCurrencies: declaredA.SupportedFiatCurrencies,
			SupportedPaymentMethods: declaredA.SupportedPaymentMethods,
			SupportsDeposit:         true, SupportsWithdrawal: true,
			AmountLimits: declaredA.AmountLimits,
			Status:       CapabilityDisabled,
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable mock-a: %v", err)
	}
	providerB := NewMockProvider("mock-b", "EUR")
	registerCapability(t, pool, f, providerB, 100)
	orch = NewOrchestrator(map[string]PaymentProvider{"mock-a": providerA, "mock-b": providerB}, MultiWebhookCredentialResolver{"mock-a": NewMockWebhookCredentials(providerA), "mock-b": NewMockWebhookCredentials(providerB)})

	var intentB DepositIntent
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intentB, err = initiateDepositWithAttempt(ctx, tx, orch, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 2200, PaymentMethod: "card", IdempotencyKey: "dep-swap-b",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit against mock-b: %v", err)
	}
	if intentB.ProviderID == nil || *intentB.ProviderID != "mock-b" {
		t.Fatalf("expected the swapped-in provider mock-b to be selected once mock-a is disabled, got %v", intentB.ProviderID)
	}
	payloadB := providerB.CallbackPayload(f.tenantID, CallbackEventDeposit, *intentB.ProviderReference, "", OutcomeSucceeded, 2200, "EUR", "", false)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-b", payloadB)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback against mock-b: %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 3300+2200 {
		t.Fatalf("expected both deposits (3300 via mock-a + 2200 via mock-b) posted through the identical InitiateDeposit/ReceiveCallback call paths, got %d", balance)
	}
}

// --- Item 22: provider capability mismatch --------------------------------

// TestRouteProvider_RefusesUnsupportedPaymentMethod proves RouteProvider
// refuses to select a provider whose capability row does not declare the
// requested payment method, rather than attempting the provider anyway.
func TestRouteProvider_RefusesUnsupportedPaymentMethod(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR") // declares only "card", "bank_transfer"
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orch.RouteProvider(ctx, tx, RoutingRequest{
			TenantID: f.tenantID, BrandID: f.brandID, AssetCode: "EUR", PaymentMethod: "crypto_rail", Amount: 1000, Operation: OperationDeposit,
		})
		return err
	})
	if !errors.Is(err, ErrNoRoutableProvider) {
		t.Fatalf("expected ErrNoRoutableProvider for a payment method the capability does not declare, got %v", err)
	}
}

// TestRouteProvider_RefusesAmountOutsideCapabilityLimits proves RouteProvider
// refuses to select a provider for an amount outside its declared
// per-asset amount_limits, rather than attempting the provider anyway.
func TestRouteProvider_RefusesAmountOutsideCapabilityLimits(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR") // declares 100 - 100,000,000 for EUR
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orch.RouteProvider(ctx, tx, RoutingRequest{
			TenantID: f.tenantID, BrandID: f.brandID, AssetCode: "EUR", PaymentMethod: "card", Amount: 1, Operation: OperationDeposit,
		})
		return err
	})
	if !errors.Is(err, ErrNoRoutableProvider) {
		t.Fatalf("expected ErrNoRoutableProvider for an amount below the capability's declared minimum, got %v", err)
	}
}

// TestInitiateDeposit_CapabilityMethodMismatch_NeverCallsProvider proves
// the capability-mismatch refusal holds at the full InitiateDeposit level
// too, not merely at RouteProvider's own return value: the intent is
// finalized declined ("no_routable_provider") and the provider's Deposit
// method is never invoked at all.
func TestInitiateDeposit_CapabilityMethodMismatch_NeverCallsProvider(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := newSpyProvider(NewMockProvider("mock-psp", "EUR"))
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider.MockProvider)})

	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = orch.InitiateDeposit(ctx, tx, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 5000, PaymentMethod: "crypto_rail", IdempotencyKey: "dep-method-mismatch",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	if intent.Status != DepositIntentDeclined {
		t.Fatalf("expected a capability mismatch to decline rather than attempt routing, got %v", intent.Status)
	}
	if got := provider.DepositCallCount(); got != 0 {
		t.Fatalf("a capability mismatch must refuse to route rather than attempting the provider anyway, but Deposit was called %d times", got)
	}
}

// --- Item 23: custody-shaped capability rows never enter routing ----------

// TestRouteProvider_CapabilityWithoutDepositSupportNeverRoutesForDeposit
// proves a capability row lacking supports_deposit is never selected as a
// deposit routing candidate, even when every other dimension (asset,
// method, amount, health) matches - and that the SAME row IS a valid
// withdrawal candidate, isolating the exclusion to the specific missing
// flag rather than a broader misconfiguration. This is the concrete
// runtime proof behind docs/decisions/0022 §2's rule that a capability row
// lacking deposit/withdrawal support (the shape a Crypto Custodian, which
// has no PaymentProvider capability declaration at all, would present if
// it ever wrongly acquired one) can never become routable.
func TestRouteProvider_CapabilityWithoutDepositSupportNeverRoutesForDeposit(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := WriteCapability(ctx, tx, provider, f.tenantID, nil, CapabilityConfig{
			SupportedFiatCurrencies: []string{"EUR"}, SupportedPaymentMethods: []string{"card"},
			SupportsDeposit: false, SupportsWithdrawal: true, // withdrawal-only row
			AmountLimits: []AmountLimit{{AssetCode: "EUR", MinAmount: 100, MaxAmount: 100000}},
			Status:       CapabilityActive,
		})
		return err
	})
	if err != nil {
		t.Fatalf("WriteCapability: %v", err)
	}
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orch.RouteProvider(ctx, tx, RoutingRequest{
			TenantID: f.tenantID, BrandID: f.brandID, AssetCode: "EUR", PaymentMethod: "card", Amount: 1000, Operation: OperationDeposit,
		})
		return err
	})
	if !errors.Is(err, ErrNoRoutableProvider) {
		t.Fatalf("a capability row with supports_deposit=false must never be a deposit routing candidate, got %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, capability, err := orch.RouteProvider(ctx, tx, RoutingRequest{
			TenantID: f.tenantID, BrandID: f.brandID, AssetCode: "EUR", PaymentMethod: "card", Amount: 1000, Operation: OperationWithdrawal,
		})
		if err != nil {
			return err
		}
		if capability.ProviderID != "mock-psp" {
			t.Fatalf("expected mock-psp to remain a valid withdrawal candidate, got %s", capability.ProviderID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RouteProvider (withdrawal): %v", err)
	}
}

// TestProviderCapabilities_ProviderKindRejectsNonPaymentValueAtDatabaseLevel
// is a defense-in-depth check independent of WriteCapability's own
// validateNarrowing logic: it bypasses the Go API entirely and attempts a
// direct SQL insert of a provider_capabilities row with a non-payment
// provider_kind (the shape a Crypto Custodian would carry if one were ever
// mistakenly represented in this table). Migration 0024's own
// CHECK (provider_kind IN ('fiat', 'crypto_payment')) constraint is what
// makes this structurally impossible even if application code had a bug -
// "a Crypto Custodian is deliberately not representable here" is enforced
// by the schema, not merely by application-code discipline.
func TestProviderCapabilities_ProviderKindRejectsNonPaymentValueAtDatabaseLevel(t *testing.T) {
	pool := testPool(t)
	f := seedCapFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO provider_capabilities
				(id, tenant_id, brand_id, provider_id, provider_kind,
				 supported_fiat_currencies, supported_crypto_assets, supported_payment_methods, supported_countries,
				 supports_deposit, supports_withdrawal, supports_refund_reversal,
				 settlement_behavior, callback_capabilities, priority, status)
			 VALUES ($1, $2, NULL, 'rogue-custodian', 'crypto_custody', '{}', '{}', '{}', '{}', true, true, false, 'instant', 'webhook', 100, 'active')`,
			uuid.New(), f.tenantID,
		)
		return err
	})
	if err == nil {
		t.Fatal("expected the database's own provider_kind CHECK constraint to reject a non-payment kind, but the insert succeeded")
	}
}

// --- Item 24: key material boundary at another injection point -----------

// TestMockProvider_HandleCallback_RejectsKeyMaterialInReversalEvent extends
// the existing key-material rejection coverage (which only exercises
// event_type="deposit" payloads) to a genuinely different wire shape - a
// deposit_reversal callback - proving the generic pre-parse scan
// (containsKeyMaterialField) runs before ANY typed field, including
// event_type, is even inspected, so it cannot be bypassed by choosing a
// different callback event shape.
//
// Other candidate injection points were checked and found structurally
// closed, not merely untested: DepositRequest, WithdrawRequest,
// StatusResult, CallbackEvent, AdapterCapability, and CapabilityConfig
// (types.go, capability.go) have no free-form/passthrough field at all -
// every field crossing the PaymentProvider interface or the capability
// write path is explicitly enumerated and typed (docs/decisions/0022
// §4.1 point 3), so there is no field literal to even attempt an injection
// through for a "capability write attempt" the way there is for a raw
// webhook payload. WriteCapability's CapabilityConfig deliberately has no
// SettlementBehavior/ProviderKind/credential field at all (capability.go's
// own doc comment) - those are always taken from the adapter's own
// Capabilities() call, never from caller input, so a malicious cfg value
// has nowhere to write to even in principle.
func TestMockProvider_HandleCallback_RejectsKeyMaterialInReversalEvent(t *testing.T) {
	provider := NewMockProvider("mock-psp")
	ctx := context.Background()
	tenantID := uuid.New()

	poisoned := []byte(`{
		"event_type": "deposit_reversal",
		"provider_reference": "reversal-ref-1",
		"original_provider_reference": "dep-ref-1",
		"outcome": "declined",
		"amount": 1000,
		"asset_code": "EUR",
		"decline_reason": "chargeback",
		"gateway_metadata": {"signing_key": "zpriv1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"}
	}`)
	// Stage 10.1 security review P2-1/code review F1/architect PW-1:
	// HandleCallback verifies the signature BEFORE any body parsing, so
	// the key-material scan is only reached for a genuinely, correctly
	// signed body - SignRawBody signs these exact bytes for tenantID.
	cred := mockCredentialFor(t, provider, tenantID)
	inbound := provider.SignRawBody(tenantID, poisoned)
	_, err := provider.HandleCallback(ctx, inbound, cred)
	if !errors.Is(err, ErrInboundKeyMaterial) {
		t.Fatalf("expected ErrInboundKeyMaterial for key material nested in a deposit_reversal callback, got %v", err)
	}
}
