//go:build integration

// Stage 4H-B1 Wave 3 Phase 9 (`qa`) — genuine concurrency coverage for the
// three sweep/scheduler mechanisms this Wave adds (deposit_sweep.go,
// cashback_scheduler.go, schedulers.go's tryAdvisoryLockedTenantJob).
//
// Two gaps this file closes, both named in the qa dispatch:
//
//  1. deposit_sweep_integration_test.go's own
//     TestRunDepositSweepScheduler_TenantAdvisoryLockSerializesOverlappingTicks
//     runs its two lock attempts SEQUENTIALLY (one call returns before the
//     next begins) - it proves the SQL-level try-lock call itself works,
//     but it never actually has two ticks in flight AT THE SAME TIME, so it
//     cannot detect a bug where two truly concurrent transactions both
//     observe the lock as free (a classic TOCTOU the sequential version is
//     structurally unable to exercise). This file adds a REAL overlap, in
//     the same style as internal/reconciliation's own
//     TestTryRunLedgerVsProjectionForTenant_ConcurrentLockContention: one
//     goroutine acquires the lock and blocks (via a channel) inside its own
//     open transaction while a second goroutine's attempt races it for
//     real.
//
//  2. Phase 4's (risk) review flagged, as a named-but-unverified finding,
//     that the deposit sweep and the cashback scheduler use two DIFFERENT
//     advisory-lock namespaces ("bonus_deposit_sweep" vs
//     "bonus_cashback_scheduler") and asked whether two overlapping ticks
//     of the two DIFFERENT jobs for the SAME tenant could deadlock each
//     other under Postgres's own deadlock detector, rather than merely
//     serializing cleanly. This file answers that empirically rather than
//     by inspection alone: it runs both jobs for the SAME tenant, in real,
//     overlapping goroutines, repeated, and asserts (a) neither ever
//     surfaces a Postgres "deadlock detected" error (SQLSTATE 40P01 - the
//     specific, falsifiable claim, not just "no error"), and (b) both
//     complete with materially correct results despite the overlap.
//
//     Why this is safe by construction, stated here as the finding rather
//     than left implicit: tryAdvisoryLockedTenantJob (schedulers.go) uses
//     pg_try_advisory_xact_lock - the NON-BLOCKING try-lock family. A
//     session that calls the try-lock variant NEVER waits for another
//     session's lock; it returns immediately with acquired=false if the
//     lock is already held. Postgres's deadlock detector only ever needs
//     to intervene when two sessions are each BLOCKED waiting on a lock the
//     other holds (a genuine wait-for cycle) - a try-lock, by definition,
//     never enters a wait state, so it can never participate in such a
//     cycle. Two DIFFERENT lock namespaces (as here) additionally never
//     contend with each other at all, blocking or not. This test proves
//     that reasoning empirically against a real Postgres 16 instance rather
//     than leaving it as an unverified inference.
package bonus

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestDepositSweep_TrueConcurrentTicksSameTenant_SecondObservesLockHeldAndSkips
// is the genuine-overlap companion to
// TestRunDepositSweepScheduler_TenantAdvisoryLockSerializesOverlappingTicks:
// the first goroutine's tick genuinely blocks (holding its own transaction,
// and therefore the transaction-scoped advisory lock, open) until the
// second goroutine's attempt has had its own chance to run and observe
// contention - mirroring
// internal/reconciliation/scheduler_integration_test.go's own
// TestTryRunLedgerVsProjectionForTenant_ConcurrentLockContention pattern
// exactly, applied to the NEW bonus_deposit_sweep lock namespace.
func TestDepositSweep_TrueConcurrentTicksSameTenant_SecondObservesLockHeldAndSkips(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	seedDepositMatchableOffer(t, pool, f, false, nil)
	seedRawDeposit(t, pool, f, 10000, "card")

	holdRelease := make(chan struct{})
	firstAcquired := make(chan bool, 1)
	secondAcquired := make(chan bool, 1)
	secondErr := make(chan error, 1)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		acquired, err := tryAdvisoryLockedTenantJob(context.Background(), pool, f.tenantID, "bonus_deposit_sweep", func(ctx context.Context, tx pgx.Tx) error {
			firstAcquired <- true
			if _, runErr := RunDepositSweepForTenant(ctx, tx, f.tenantID, systemSchedulerActorID); runErr != nil {
				return runErr
			}
			// Hold the transaction (and therefore the tenant's own
			// transaction-scoped advisory lock) open until the second,
			// genuinely concurrent attempt has had its chance to observe
			// contention.
			<-holdRelease
			return nil
		})
		if err != nil {
			t.Errorf("first concurrent deposit sweep tick: %v", err)
		}
		if !acquired {
			t.Errorf("expected the first tick to acquire the lock")
		}
	}()

	<-firstAcquired

	wg.Add(1)
	go func() {
		defer wg.Done()
		acquired, err := tryAdvisoryLockedTenantJob(context.Background(), pool, f.tenantID, "bonus_deposit_sweep", func(ctx context.Context, tx pgx.Tx) error {
			_, runErr := RunDepositSweepForTenant(ctx, tx, f.tenantID, systemSchedulerActorID)
			return runErr
		})
		secondAcquired <- acquired
		secondErr <- err
	}()

	acquired := <-secondAcquired
	err := <-secondErr
	close(holdRelease)
	wg.Wait()

	if acquired {
		t.Fatal("expected the second, GENUINELY CONCURRENT deposit sweep tick to find the lock held and skip, but it acquired it")
	}
	if err != nil {
		t.Fatalf("second concurrent tick returned an unexpected error: %v", err)
	}

	// Exactly one grant ATTEMPT resulted (the first tick's own), never two
	// - the second tick's skip must be a true no-op, not a partial/racing
	// re-read of the same deposit.
	var grantCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM bonus_grants WHERE tenant_id = $1`, f.tenantID).Scan(&grantCount)
	}); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if grantCount != 1 {
		t.Fatalf("expected exactly 1 grant attempt after genuinely concurrent overlapping ticks, got %d", grantCount)
	}
}

// isPostgresDeadlock reports whether err is a real Postgres "deadlock
// detected" error (SQLSTATE 40P01) - the specific, falsifiable condition
// this file's F1 investigation checks for, never inferred from a generic
// non-nil error.
func isPostgresDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// TestDepositAndCashbackSchedulers_ConcurrentDifferentNamespaces_NoDeadlock
// is Stage 4H-B1 Wave 3 Phase 9's own answer to Phase 4's (risk) named-but-
// unverified F1 concern: running the deposit sweep and the cashback
// scheduler CONCURRENTLY, for the SAME tenant, under their own two
// DIFFERENT advisory-lock namespaces, must never deadlock (SQLSTATE
// 40P01) and must leave both jobs' own results correct despite the real
// overlap. Repeated across several tenants/iterations (a single pair of
// goroutines racing once is not, on its own, strong evidence against a
// timing-dependent condition - CLAUDE.md's own "stress, not a two-actor
// happy path" standard, applied at the scheduler-namespace level rather
// than the row-lock level this platform's other concurrency stress tests
// already cover).
func TestDepositAndCashbackSchedulers_ConcurrentDifferentNamespaces_NoDeadlock(t *testing.T) {
	const iterations = 8
	for i := 0; i < iterations; i++ {
		pool := testPool(t)
		f := seedLifecycleFixture(t, pool)
		seedDepositMatchableOffer(t, pool, f, false, nil)
		seedRawDeposit(t, pool, f, 10000, "card")
		seedCashbackOffer(t, pool, f, 1000, 86400)
		seedCasinoBetForCashback(t, pool, f, 1000)
		asOf := time.Now().UTC().Add(48 * time.Hour)

		var wg sync.WaitGroup
		depositErrCh := make(chan error, 1)
		cashbackErrCh := make(chan error, 1)
		depositAcquiredCh := make(chan bool, 1)
		cashbackAcquiredCh := make(chan bool, 1)

		wg.Add(2)
		go func() {
			defer wg.Done()
			acquired, err := tryAdvisoryLockedTenantJob(context.Background(), pool, f.tenantID, "bonus_deposit_sweep", func(ctx context.Context, tx pgx.Tx) error {
				_, runErr := RunDepositSweepForTenant(ctx, tx, f.tenantID, systemSchedulerActorID)
				return runErr
			})
			depositAcquiredCh <- acquired
			depositErrCh <- err
		}()
		go func() {
			defer wg.Done()
			acquired, err := tryAdvisoryLockedTenantJob(context.Background(), pool, f.tenantID, "bonus_cashback_scheduler", func(ctx context.Context, tx pgx.Tx) error {
				_, runErr := RunCashbackSchedulerForTenant(ctx, tx, f.tenantID, systemSchedulerActorID, asOf)
				return runErr
			})
			cashbackAcquiredCh <- acquired
			cashbackErrCh <- err
		}()
		wg.Wait()

		depositAcquired, cashbackAcquired := <-depositAcquiredCh, <-cashbackAcquiredCh
		depositErr, cashbackErr := <-depositErrCh, <-cashbackErrCh

		if isPostgresDeadlock(depositErr) || isPostgresDeadlock(cashbackErr) {
			t.Fatalf("iteration %d: F1 CONFIRMED - Postgres deadlock (40P01) between the deposit sweep and cashback scheduler for the same tenant: deposit_err=%v cashback_err=%v", i, depositErr, cashbackErr)
		}
		if depositErr != nil {
			t.Fatalf("iteration %d: deposit sweep tick failed: %v", i, depositErr)
		}
		if cashbackErr != nil {
			t.Fatalf("iteration %d: cashback scheduler tick failed: %v", i, cashbackErr)
		}
		// Different lock namespaces never contend with each other -
		// both ticks must acquire their own lock even though they ran
		// concurrently for the same tenant.
		if !depositAcquired {
			t.Fatalf("iteration %d: expected the deposit sweep to acquire its own namespace's lock despite the concurrent cashback tick", i)
		}
		if !cashbackAcquired {
			t.Fatalf("iteration %d: expected the cashback scheduler to acquire its own namespace's lock despite the concurrent deposit tick", i)
		}

		// Both jobs' own results are still correct despite running
		// concurrently against the same tenant/database: one grant
		// ATTEMPT from each job (both denied at the pre-existing,
		// disclosed jurisdiction gap - see RunDepositSweepForTenant's own
		// doc comment - but the ATTEMPT, and therefore the absence of any
		// cross-job interference, is what this assertion proves).
		var grantCount int
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM bonus_grants WHERE tenant_id = $1`, f.tenantID).Scan(&grantCount)
		}); err != nil {
			t.Fatalf("iteration %d: count grants: %v", i, err)
		}
		if grantCount != 2 {
			t.Fatalf("iteration %d: expected exactly 2 grant attempts (1 deposit + 1 cashback) after a concurrent run with no cross-job interference, got %d", i, grantCount)
		}
	}
}
