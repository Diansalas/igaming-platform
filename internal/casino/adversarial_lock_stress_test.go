//go:build integration

// QA adversarial review (Stage 4G-FINAL): the shipped regression test
// (TestConcurrent_DuplicateBetDeliveryDuringSelfExclusion) only exercises
// TWO concurrent deliveries of the same bet callback. This file pushes
// the same scenario to a much wider fan-out to check for (a) divergent
// outcomes across N>2 truly-concurrent deliveries, and (b) starvation -
// pg_advisory_xact_lock serializes correctly, but if delivery N has to
// wait behind N-1 full transactions it could look like a hang/timeout to
// a real provider integration with a tight callback SLA.
package casino

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/rg"
)

// TestConcurrentStress_ManyDuplicateBetDeliveries fires N identical bet
// callbacks at once (well beyond the shipped test's N=2) and asserts:
//  1. Exactly one ledger_transactions row ever exists for the
//     provider_tx_id (ledger-level idempotency holds under wider fan-out).
//  2. Every one of the N deliveries reports the SAME outcome and, when
//     Succeeded, the SAME ledger transaction id (no divergent results).
//  3. No single delivery is starved beyond a generous bound relative to
//     N - i.e. the lock serializes without pathological queueing that
//     would look like a hung provider callback.
func TestConcurrentStress_ManyDuplicateBetDeliveries(t *testing.T) {
	pool := testPool(t)
	const n = 10

	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	providerTxID := uuid.New().String()
	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, providerTxID, "", uuid.New().String(), "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)

	results := make([]ReceiveCallbackResult, n)
	errs := make([]error, n)
	durations := make([]time.Duration, n)

	var wg sync.WaitGroup
	wg.Add(n)
	overallStart := time.Now()
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			start := time.Now()
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				results[i], err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
				return err
			})
			durations[i] = time.Since(start)
		}()
	}
	wg.Wait()
	overallElapsed := time.Since(overallStart)

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("delivery %d: %v", i, errs[i])
		}
	}

	balance := cashBalance(t, pool, f)
	if balance != 5000 && balance != 4000 {
		t.Fatalf("balance %d is neither 5000 (all declined) nor 4000 (posted exactly once)", balance)
	}

	// All N results must agree with each other.
	first := results[0]
	for i := 1; i < n; i++ {
		if results[i].Outcome != first.Outcome {
			t.Fatalf("delivery %d outcome %v diverges from delivery 0 outcome %v: full results=%+v", i, results[i].Outcome, first.Outcome, results)
		}
	}
	if first.Outcome == OutcomeSucceeded {
		txID := results[0].LedgerTransactionID
		if txID == nil {
			t.Fatalf("outcome Succeeded but LedgerTransactionID is nil: %+v", first)
		}
		for i := 1; i < n; i++ {
			if results[i].LedgerTransactionID == nil || *results[i].LedgerTransactionID != *txID {
				t.Fatalf("delivery %d reports a different (or nil) ledger transaction id than delivery 0: %+v vs %+v", i, results[i], first)
			}
		}
	}

	var betCount int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = 'mock-casino' AND provider_tx_id = $2`,
			f.tenantID, providerTxID,
		).Scan(&betCount)
	})
	if err != nil {
		t.Fatalf("count ledger transactions: %v", err)
	}
	if betCount > 1 {
		t.Fatalf("expected at most one ledger_transactions row for provider_tx_id=%s under %d-way concurrency, got %d", providerTxID, n, betCount)
	}

	// Starvation check: no single delivery should take an outlandish
	// multiple of the overall wall-clock time to complete - a healthy
	// serialized queue of N short transactions should have its slowest
	// member land close to the overall elapsed time, not blow far past
	// it (which would indicate lock convoy / unbounded queueing rather
	// than orderly serialization).
	maxAllowed := overallElapsed + 2*time.Second
	for i, d := range durations {
		if d > maxAllowed {
			t.Fatalf("delivery %d took %v, more than overall elapsed %v + 2s slack - possible starvation under the advisory lock", i, d, overallElapsed)
		}
	}
	t.Logf("n=%d overallElapsed=%v durations=%v", n, overallElapsed, durations)
}

// TestConcurrentStress_ManyDuplicateBetDeliveriesDuringSelfExclusion widens
// the shipped flake-fix regression (2 deliveries + 1 concurrent
// self-exclusion) to N deliveries racing a single self-exclusion, run
// across several iterations to surface any residual non-determinism the
// 2-way case might not expose.
func TestConcurrentStress_ManyDuplicateBetDeliveriesDuringSelfExclusion(t *testing.T) {
	pool := testPool(t)
	const n = 8
	const iterations = 5

	for iter := 0; iter < iterations; iter++ {
		f := seedCasinoFixture(t, pool)
		fundWallet(t, pool, f, 5000)
		provider := NewMockCasinoProvider("mock-casino", "EUR")
		registerCasinoCapability(t, pool, f, provider, 100)
		sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
		orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

		providerTxID := uuid.New().String()
		payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, providerTxID, "", uuid.New().String(), "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)

		results := make([]ReceiveCallbackResult, n)
		errs := make([]error, n)
		var exclusionErr error

		var wg sync.WaitGroup
		wg.Add(n + 1)
		for i := 0; i < n; i++ {
			i := i
			go func() {
				defer wg.Done()
				errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					var err error
					results[i], err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
					return err
				})
			}()
		}
		go func() {
			defer wg.Done()
			exclusionErr = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := rg.CreateSelfExclusion(ctx, tx, rg.CreateSelfExclusionParams{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID})
				return err
			})
		}()
		wg.Wait()

		for i := 0; i < n; i++ {
			if errs[i] != nil {
				t.Fatalf("iteration %d delivery %d: %v", iter, i, errs[i])
			}
		}
		if exclusionErr != nil {
			t.Fatalf("iteration %d: create self-exclusion: %v", iter, exclusionErr)
		}

		balance := cashBalance(t, pool, f)
		if balance != 5000 && balance != 4000 {
			t.Fatalf("iteration %d: balance %d is neither 5000 nor 4000", iter, balance)
		}

		first := results[0]
		for i := 1; i < n; i++ {
			if results[i].Outcome != first.Outcome {
				t.Fatalf("iteration %d: delivery %d outcome %v diverges from delivery 0 outcome %v: full=%+v", iter, i, results[i].Outcome, first.Outcome, results)
			}
		}
		if balance == 4000 {
			if first.Outcome != OutcomeSucceeded {
				t.Fatalf("iteration %d: balance posted but reported outcome is %v", iter, first.Outcome)
			}
			txID := first.LedgerTransactionID
			for i := 1; i < n; i++ {
				if results[i].LedgerTransactionID == nil || txID == nil || *results[i].LedgerTransactionID != *txID {
					t.Fatalf("iteration %d: delivery %d ledger tx id mismatch: %+v vs %+v", iter, i, results[i], first)
				}
			}
		} else if first.Outcome != OutcomeDeclined {
			t.Fatalf("iteration %d: balance shows nothing posted, but reported outcome is %v", iter, first.Outcome)
		}

		var betCount int
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = 'mock-casino' AND provider_tx_id = $2`,
				f.tenantID, providerTxID,
			).Scan(&betCount)
		})
		if err != nil {
			t.Fatalf("iteration %d: count ledger transactions: %v", iter, err)
		}
		if betCount > 1 {
			t.Fatalf("iteration %d: expected at most one ledger_transactions row, got %d", iter, betCount)
		}
	}
}
