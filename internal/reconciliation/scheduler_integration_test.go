//go:build integration

// Stage 3C hardening (directive item 4 and adversarial tests 8.K-8.N):
// proves the reconciliation scheduler mechanism (scheduler.go) is
// concurrency-safe, failure-safe/retryable, and never silently mutates
// the ledger or the projection it is investigating.
package reconciliation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
)

// findOutcome returns the SweepOutcome for tenantID, failing the test if
// the sweep's result set does not include it. The tests sweep through
// RunSweepTenants scoped to their own tenant(s): RunSweep over the shared
// dev database would iterate every tenant every other integration test
// has ever left behind (thousands, each with four streams), making each
// call cost minutes (CAS-RECON-SCALE-1). RunSweep's only difference - the
// unrestricted tenant enumeration - is covered directly by
// TestSweepTenantSelection_ActiveOnlyScopedAndUnscoped.
func findOutcome(t *testing.T, outcomes []SweepOutcome, f fixture) SweepOutcome {
	t.Helper()
	for _, o := range outcomes {
		if o.TenantID == f.tenantID {
			return o
		}
	}
	t.Fatalf("RunSweep result did not include tenant %s", f.tenantID)
	return SweepOutcome{}
}

// TestTryRunLedgerVsProjectionForTenant_ConcurrentLockContention is
// adversarial test 8.K: two concurrent reconciliation attempts for the
// SAME tenant must not both proceed - the second must observe the lock
// held and skip, never race the first to insert a second Run for the
// same tick. Uses a real advisory lock on a real PostgreSQL 16
// connection (per directive item 8's own requirement), not a mock.
func TestTryRunLedgerVsProjectionForTenant_ConcurrentLockContention(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	holdRelease := make(chan struct{})
	firstAcquired := make(chan bool, 1)
	secondAcquired := make(chan bool, 1)
	secondErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, _, acquired, err := TryRunLedgerVsProjectionForTenant(ctx, tx, f.tenantID, time.Now().Add(-time.Hour), time.Now())
			firstAcquired <- acquired
			if err != nil {
				return err
			}
			// Hold the transaction (and therefore the transaction-scoped
			// advisory lock) open until the second attempt has had its
			// chance to observe contention.
			<-holdRelease
			return nil
		})
		if err != nil {
			t.Errorf("first sweep attempt: %v", err)
		}
	}()

	if acquired := <-firstAcquired; !acquired {
		close(holdRelease)
		wg.Wait()
		t.Fatalf("expected the first attempt to acquire the tenant's advisory lock")
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, _, acquired, err := TryRunLedgerVsProjectionForTenant(ctx, tx, f.tenantID, time.Now().Add(-time.Hour), time.Now())
			secondAcquired <- acquired
			return err
		})
		secondErr <- err
	}()

	acquired := <-secondAcquired
	err := <-secondErr
	close(holdRelease)
	wg.Wait()

	if acquired {
		t.Fatalf("expected the second concurrent attempt to find the lock held and skip, but it acquired it")
	}
	if err != nil {
		t.Fatalf("second attempt returned an unexpected error: %v", err)
	}

	// The skipped attempt must not have written a second Run row for
	// this tenant during the window the first held the lock.
	var runCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1`, f.tenantID).Scan(&runCount)
	})
	if err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runCount != 1 {
		t.Fatalf("expected exactly 1 reconciliation_runs row after lock contention, got %d", runCount)
	}
}

// TestRunSweep_FailedTenantRunDoesNotCorruptStateAndRetrySucceeds is
// adversarial test 8.L: one tenant's reconciliation attempt fails for a
// genuine data-level reason (here: an inverted period, which trips
// reconciliation_runs' own period_end >= period_start CHECK constraint on
// insert - a real Postgres-enforced failure, not a mock), and the
// failure must (a) leave no partial Run/Mismatch data behind, and (b) not
// prevent a subsequent, valid retry from succeeding.
func TestRunSweep_FailedTenantRunDoesNotCorruptStateAndRetrySucceeds(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	now := time.Now()
	invertedStart, invertedEnd := now, now.Add(-time.Hour) // end before start: violates the CHECK constraint

	outcomes, err := RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{f.tenantID}, invertedStart, invertedEnd, sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
	if err != nil {
		t.Fatalf("RunSweep itself must not fail even though one tenant's run failed: %v", err)
	}
	failed := findOutcome(t, outcomes, f)
	if failed.Err == nil {
		t.Fatalf("expected the inverted-period run to fail, got a nil error")
	}

	var runCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1`, f.tenantID).Scan(&runCount)
	})
	if err != nil {
		t.Fatalf("count runs after failure: %v", err)
	}
	if runCount != 0 {
		t.Fatalf("expected the failed attempt to leave zero reconciliation_runs rows, got %d", runCount)
	}

	// Retry with a valid period - must succeed cleanly, proving the
	// earlier failure left the tenant (and the advisory lock, which is
	// transaction-scoped and therefore released on the failed tx's
	// rollback) in a normal, reconcilable state.
	outcomes, err = RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{f.tenantID}, now.Add(-time.Hour), now, sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
	if err != nil {
		t.Fatalf("RunSweep retry: %v", err)
	}
	retried := findOutcome(t, outcomes, f)
	if retried.Err != nil {
		t.Fatalf("expected the retry to succeed, got: %v", retried.Err)
	}
	if retried.Run.Status != StatusClean {
		t.Fatalf("expected StatusClean on retry, got %s", retried.Run.Status)
	}
}

// TestRunSweep_DetectsMismatchThenSafeRebuildResolves is adversarial test
// 8.M (a corrupted projection is rebuilt) combined with 8.N (detection
// never itself mutates the ledger or the projection): RunSweep must
// report the corruption without touching wallet_balance_projection or
// ledger_entries, and only the separate, explicit
// ledger.RebuildProjectionRow call (the safe rebuild path directive item
// 4 requires exist) actually fixes it.
func TestRunSweep_DetectsMismatchThenSafeRebuildResolves(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var ledgerEntryCountBefore, ledgerEntrySumBefore int64
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE wallet_balance_projection SET credit_total = credit_total + 999 WHERE ledger_account_id = $1`,
			f.cashAccountID,
		); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT count(*), COALESCE(SUM(amount), 0) FROM ledger_entries WHERE ledger_account_id = $1`, f.cashAccountID,
		).Scan(&ledgerEntryCountBefore, &ledgerEntrySumBefore)
	})
	if err != nil {
		t.Fatalf("inject drift: %v", err)
	}

	outcomes, err := RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{f.tenantID}, time.Now().Add(-time.Hour), time.Now(), sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
	if err != nil {
		t.Fatalf("RunSweep: %v", err)
	}
	dirty := findOutcome(t, outcomes, f)
	if dirty.Err != nil {
		t.Fatalf("sweep run itself must succeed even when it finds a mismatch: %v", dirty.Err)
	}
	if dirty.Run.Status != StatusMismatchesFound {
		t.Fatalf("expected StatusMismatchesFound, got %s", dirty.Run.Status)
	}

	// 8.N: detection alone must not have touched ledger_entries, nor
	// silently fixed wallet_balance_projection.
	var ledgerEntryCountAfter, ledgerEntrySumAfter, projectedCredit int64
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT count(*), COALESCE(SUM(amount), 0) FROM ledger_entries WHERE ledger_account_id = $1`, f.cashAccountID,
		).Scan(&ledgerEntryCountAfter, &ledgerEntrySumAfter); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT credit_total FROM wallet_balance_projection WHERE ledger_account_id = $1`, f.cashAccountID,
		).Scan(&projectedCredit)
	})
	if err != nil {
		t.Fatalf("read post-sweep state: %v", err)
	}
	if ledgerEntryCountAfter != ledgerEntryCountBefore || ledgerEntrySumAfter != ledgerEntrySumBefore {
		t.Fatalf("reconciliation detection must never mutate ledger_entries: before=(%d,%d) after=(%d,%d)",
			ledgerEntryCountBefore, ledgerEntrySumBefore, ledgerEntryCountAfter, ledgerEntrySumAfter)
	}
	const seededCredit = 1000 // seedFixture's own deposit amount, see fixture()
	if projectedCredit != seededCredit+999 {
		t.Fatalf("reconciliation detection must never silently correct the projection either; expected the injected drift (%d) to still be present, got %d", seededCredit+999, projectedCredit)
	}

	// 8.M: the safe, explicit rebuild path actually fixes it.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ledger.RebuildProjectionRow(ctx, tx, f.cashAccountID)
		return err
	})
	if err != nil {
		t.Fatalf("rebuild projection row: %v", err)
	}

	outcomes, err = RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{f.tenantID}, time.Now().Add(-time.Hour), time.Now(), sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
	if err != nil {
		t.Fatalf("RunSweep after rebuild: %v", err)
	}
	clean := findOutcome(t, outcomes, f)
	if clean.Err != nil {
		t.Fatalf("sweep after rebuild: %v", clean.Err)
	}
	if clean.Run.Status != StatusClean {
		t.Fatalf("expected StatusClean after rebuild, got %s", clean.Run.Status)
	}
}

// TestSweepTenantSelection_ActiveOnlyScopedAndUnscoped pins the tenant
// enumeration RunSweep and RunSweepTenants share (activeTenantIDs): the
// unrestricted list RunSweep uses includes every active tenant and never a
// suspended/closed one, and a scoped sweep reconciles exactly the active
// tenants it names - a suspended, unknown or duplicated id is not swept
// (and writes nothing), and an empty list sweeps nothing rather than
// everything.
func TestSweepTenantSelection_ActiveOnlyScopedAndUnscoped(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	active := seedFixture(t, pool)
	suspended := seedFixture(t, pool)
	closed := seedFixture(t, pool)
	for id, status := range map[uuid.UUID]string{suspended.tenantID: "suspended", closed.tenantID: "closed"} {
		if err := pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
			return execOne(ctx, tx, `UPDATE tenants SET status = $2 WHERE id = $1`, id, status)
		}); err != nil {
			t.Fatalf("set tenant %s %s: %v", id, status, err)
		}
	}

	// RunSweep's unrestricted enumeration.
	all, err := allTenantIDs(ctx, pool)
	if err != nil {
		t.Fatalf("allTenantIDs: %v", err)
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range all {
		seen[id] = true
	}
	if !seen[active.tenantID] {
		t.Fatalf("the unrestricted sweep enumeration must include active tenant %s", active.tenantID)
	}
	if seen[suspended.tenantID] || seen[closed.tenantID] {
		t.Fatal("the unrestricted sweep enumeration must exclude suspended/closed tenants")
	}

	// Empty scope sweeps nothing (never "all").
	for _, ids := range [][]uuid.UUID{nil, {}} {
		outcomes, err := RunSweepTenants(ctx, pool, nil, ids, time.Now().Add(-time.Hour), time.Now(), sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
		if err != nil || len(outcomes) != 0 {
			t.Fatalf("empty scope %v: expected no outcomes, got %d (err %v)", ids, len(outcomes), err)
		}
	}

	// Scoped: exactly the active named tenant, once.
	outcomes, err := RunSweepTenants(ctx, pool, nil,
		[]uuid.UUID{suspended.tenantID, active.tenantID, uuid.New(), active.tenantID, closed.tenantID},
		time.Now().Add(-time.Hour), time.Now(), sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
	if err != nil {
		t.Fatalf("RunSweepTenants: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].TenantID != active.tenantID {
		t.Fatalf("expected exactly one outcome for the active tenant, got %+v", outcomes)
	}
	o := outcomes[0]
	if o.Err != nil || o.Run.Status != StatusClean || o.Sportsbook.Err != nil || o.Casino.Err != nil || o.CasinoStatement.Err != nil {
		t.Fatalf("unexpected outcome for the active tenant: %+v", o)
	}
	for _, f := range []fixture{suspended, closed} {
		var runs int
		if err := pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1`, f.tenantID).Scan(&runs)
		}); err != nil {
			t.Fatal(err)
		}
		if runs != 0 {
			t.Fatalf("a non-active tenant %s must not be swept, found %d runs", f.tenantID, runs)
		}
	}
}
