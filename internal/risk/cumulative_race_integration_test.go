//go:build integration

// Stage 4H-B0-R6 Workstream D, review follow-up: the TOCTOU race test the
// cumulative-limit advisory lock actually exists to prevent.
//
// The gap `qa` found: TestEvaluate_ConcurrentEvaluationsAreConsistent
// proves only that N concurrent evaluations of a PER-TRANSACTION hard cap
// (one request's own amount against a fixed max) all deny deterministically
// - a property that needs no serialization at all, because no two requests
// share any state. The one property a limits engine genuinely needs
// pg_advisory_xact_lock for is untested by it: N requests each INDIVIDUALLY
// within a SHARED cumulative cap, which TOGETHER breach it.
//
// These tests close that gap against real PostgreSQL 16, posting real
// casino_bet ledger transactions (the single wired operation in
// operationCumulativeSpecs - deliberately not widened), because the race
// being tested is precisely the gap between the cumulative-usage READ and
// the eventual ledger WRITE:
//
//  1. TestEvaluate_ConcurrentCumulativeLimitNeverOvershoots is the real
//     property: Evaluate + ledger.Post in ONE transaction per worker, N
//     workers at once, and the committed total must never exceed the cap.
//  2. TestCumulativeUsage_WithoutTheAdvisoryLockTheRaceIsReal is the
//     control, and the reason (1) is not vacuous: the SAME workers, the
//     SAME arithmetic, with the one pg_advisory_xact_lock statement from
//     Rule.breach() omitted, deterministically overshoot the cap. Without
//     it, a passing (1) could just mean the goroutines never interleaved.
//
// Both were mutation-checked when written: replacing Rule.breach()'s
// pg_advisory_xact_lock statement with a no-op makes (1) fail 10 runs out
// of 10 (all 8 stakes commit, usage 2400 against a cap of 1000), so (1) is
// a genuine guard on the lock and not on scheduling luck.
package risk

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// racePool is testPool with a connection budget sized for the concurrent
// workers below. testPool's 5 connections are enough for a lock-serialized
// test (a worker holding the lock always already holds a connection, so it
// can always make progress), but NOT for the barrier-synchronized control
// test, where every worker must hold an open transaction simultaneously -
// with fewer connections than workers that would starve at pool.Acquire
// and hang at the barrier rather than proving anything.
func racePool(t *testing.T, workers int32) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, workers+2, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// betAccounts pre-creates both legs of casino's own bet posting shape
// (Dr player_cash / Cr house_gaming, financial-transaction-flows.md Flow
// 4) BEFORE the concurrent phase, so the workers race on the limit check
// alone and not on ledger_accounts' own first-use upsert.
func betAccounts(t *testing.T, pool *db.Pool, f fixture, walletID uuid.UUID, assetCode string) (cash, house uuid.UUID) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		cash, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &walletID, ledger.AccountPlayerCash, assetCode)
		if err != nil {
			return err
		}
		house, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountHouseGaming, assetCode)
		return err
	})
	if err != nil {
		t.Fatalf("prepare bet ledger accounts: %v", err)
	}
	return cash, house
}

// postBetLeg posts the casino_bet transaction a worker's ALLOW decision
// authorizes, inside the SAME transaction that produced the decision -
// which is the whole point: the advisory lock Rule.breach() took is held
// until this transaction commits, so no other worker can read a
// cumulative total that excludes this bet and then also post.
func postBetLeg(ctx context.Context, tx pgx.Tx, f fixture, cash, house uuid.UUID, idempotencyKey string, amount int64) error {
	_, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID:        f.tenantID,
		TransactionType: ledger.TxCasinoBet,
		IdempotencyKey:  idempotencyKey,
		CorrelationID:   uuid.New(),
		Entries: []ledger.EntryInput{
			{LedgerAccountID: cash, Direction: ledger.Debit, Amount: amount},
			{LedgerAccountID: house, Direction: ledger.Credit, Amount: amount},
		},
	})
	return err
}

// committedCasinoBetUsage reads the authoritative, committed cumulative
// usage the same way Evaluate does - through cumulativeUsage with the
// PRODUCTION casino_bet spec, so the assertion is made against the exact
// measurement the limit is enforced with, not a parallel hand-rolled sum.
func committedCasinoBetUsage(t *testing.T, pool *db.Pool, f fixture, assetCode string, windowStart time.Time) int64 {
	t.Helper()
	var total *big.Int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		total, err = cumulativeUsage(ctx, tx, operationCumulativeSpecs[OperationCasinoBet], RiskRequest{
			TenantID: f.tenantID, PlayerAccountID: f.playerID, AssetCode: assetCode, Operation: OperationCasinoBet,
		}, windowStart)
		return err
	})
	if err != nil {
		t.Fatalf("read committed cumulative usage: %v", err)
	}
	return total.Int64()
}

// TestEvaluate_ConcurrentCumulativeLimitNeverOvershoots is the real
// check-and-post race.
//
// Each of `workers` concurrent bet-placement sequences requests a stake
// that is individually WELL under the shared rolling-hour cumulative cap
// (300 against 1000), but workers*stake (2400) is more than twice it. Every
// worker runs Evaluate and, only on ALLOW, posts its casino_bet in the same
// transaction - exactly internal/casino's postBet shape.
//
// The cap therefore admits exactly floor(1000/300) = 3 bets: the fourth
// would make 1200 > 1000 and must DENY, whichever worker happens to be
// fourth. This is deterministic in COUNT (all stakes are equal) while the
// winners are not, which is precisely what a correct serialization should
// produce - so the assertion is exact, not a tolerance.
func TestEvaluate_ConcurrentCumulativeLimitNeverOvershoots(t *testing.T) {
	const (
		workers        = 8
		stake    int64 = 300
		capLimit int64 = 1000
	)
	pool := racePool(t, workers)
	f := seedFixture(t, pool)
	walletID := seedWalletFor(t, pool, f, "EUR", 10_000_000)
	cash, house := betAccounts(t, pool, f, walletID, "EUR")

	// One asset-scoped HARD cumulative cap. Asset-scoped so the rule's own
	// scope is its denomination (ADR 0031 §35) - no threshold exponent to
	// declare, and the comparison is in EUR minor units on both sides.
	createTestRule(t, pool, &f.tenantID, CreateRuleParams{
		Operation: OperationCasinoBet, LimitKind: LimitCumulativeAmount, TimeWindow: WindowRollingHour,
		AssetCode: "EUR", Threshold: capLimit, RuleKind: RuleHardLimit,
	})

	windowStart := time.Now().UTC().Add(-time.Hour)
	runID := uuid.New().String()

	outcomes := make([]Outcome, workers)
	errs := make([]error, workers)
	// start gates every worker on the same channel close, so the
	// evaluations genuinely overlap instead of trickling in sequentially as
	// goroutines are scheduled.
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			req := baseRequest(f)
			req.Amount = stake
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				decision, err := Evaluate(ctx, tx, req)
				if err != nil {
					return err
				}
				outcomes[i] = decision.Outcome
				if decision.Outcome != OutcomeAllow {
					// A denial posts nothing; the transaction still commits
					// (as a no-op) and releases the advisory lock, exactly
					// as a real enforcement point's declined bet does.
					return nil
				}
				return postBetLeg(ctx, tx, f, cash, house, fmt.Sprintf("race-bet-%s-%d", runID, i), stake)
			})
		}(i)
	}
	close(start)
	wg.Wait()

	allowed, denied := 0, 0
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("worker %d: unexpected error (a cumulative evaluation must not fail): %v", i, errs[i])
		}
		switch outcomes[i] {
		case OutcomeAllow:
			allowed++
		case OutcomeDeny:
			denied++
		default:
			t.Fatalf("worker %d: unexpected outcome %q", i, outcomes[i])
		}
	}

	total := committedCasinoBetUsage(t, pool, f, "EUR", windowStart)

	// The property that matters: the committed tally never exceeds the cap.
	if total > capLimit {
		t.Fatalf("cumulative cap OVERSHOT: committed usage %d exceeds threshold %d (%d of %d concurrent stakes of %d were allowed) - the advisory lock did not serialize check-and-post",
			total, capLimit, allowed, workers, stake)
	}
	// And it is not under-shot either: the cap must admit everything that
	// genuinely fits, or "never overshoots" would be trivially satisfiable
	// by denying everything.
	if want := int64(allowed) * stake; total != want {
		t.Fatalf("committed usage %d does not match the %d ALLOW decisions (expected %d) - a decision and its ledger effect diverged", total, allowed, want)
	}
	if allowed != int(capLimit/stake) {
		t.Fatalf("expected exactly %d of %d equal stakes of %d to fit under cap %d, got %d allowed / %d denied (usage %d)",
			capLimit/stake, workers, stake, capLimit, allowed, denied, total)
	}
	if denied != workers-allowed {
		t.Fatalf("expected the remaining %d workers to DENY, got %d", workers-allowed, denied)
	}
}

// TestCumulativeUsage_WithoutTheAdvisoryLockTheRaceIsReal is the control
// that makes the test above meaningful: it reproduces the same workers and
// the same threshold arithmetic with ONE line removed - the
// pg_advisory_xact_lock statement in Rule.breach()'s LimitCumulativeAmount
// branch - and shows the cap is breached.
//
// A barrier holds every worker between its cumulative READ and its ledger
// WRITE until all of them have read, which is the check-then-insert
// interleaving the lock exists to make impossible. Deterministic by
// construction, so this test asserts a concrete overshoot rather than
// hoping the scheduler produces one.
//
// It deliberately does NOT call Evaluate (which correctly takes the lock);
// it calls the same cumulativeUsage primitive Evaluate uses, so the ONLY
// difference between this test and the one above is the lock itself.
func TestCumulativeUsage_WithoutTheAdvisoryLockTheRaceIsReal(t *testing.T) {
	const (
		workers        = 4
		stake    int64 = 300
		capLimit int64 = 1000
	)
	pool := racePool(t, workers)
	f := seedFixture(t, pool)
	walletID := seedWalletFor(t, pool, f, "EUR", 10_000_000)
	cash, house := betAccounts(t, pool, f, walletID, "EUR")

	windowStart := time.Now().UTC().Add(-time.Hour)
	runID := uuid.New().String()

	spec := operationCumulativeSpecs[OperationCasinoBet]
	errs := make([]error, workers)
	wouldAllow := make([]bool, workers)

	// read counts workers that have completed their unserialized check; the
	// barrier releases only once every one of them has.
	var read sync.WaitGroup
	read.Add(workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				// The check, with NO pg_advisory_xact_lock - the exact
				// check-then-insert shape the lock prevents.
				existing, err := cumulativeUsage(ctx, tx, spec, RiskRequest{
					TenantID: f.tenantID, PlayerAccountID: f.playerID, AssetCode: "EUR", Operation: OperationCasinoBet,
				}, windowStart)
				if err != nil {
					read.Done()
					return err
				}
				breached := new(big.Int).Add(existing, big.NewInt(stake)).Cmp(big.NewInt(capLimit)) > 0
				wouldAllow[i] = !breached

				// Every worker has now read; none has written.
				read.Done()
				read.Wait()

				if breached {
					return nil
				}
				return postBetLeg(ctx, tx, f, cash, house, fmt.Sprintf("unlocked-bet-%s-%d", runID, i), stake)
			})
		}(i)
	}
	wg.Wait()

	allowed := 0
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("worker %d: unexpected error: %v", i, errs[i])
		}
		if wouldAllow[i] {
			allowed++
		}
	}

	total := committedCasinoBetUsage(t, pool, f, "EUR", windowStart)
	if total <= capLimit {
		t.Fatalf("the unserialized check-then-insert sequence did NOT overshoot cap %d (committed %d, %d of %d allowed) - this control test no longer demonstrates the race the advisory lock prevents, so the test above proves nothing; investigate before trusting either",
			capLimit, total, allowed, workers)
	}
	if want := int64(workers) * stake; total != want {
		t.Fatalf("expected all %d unserialized stakes of %d to commit (%d), got %d", workers, stake, want, total)
	}
}
