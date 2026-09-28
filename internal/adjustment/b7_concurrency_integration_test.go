//go:build integration

package adjustment

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/capability"
)

// waitForLockWaiter polls pg_stat_activity until a backend whose query
// matches pattern is waiting on a heavyweight lock (wait_event_type =
// 'Lock'), proving real, forced contention rather than lucky timing.
func (w *world) waitForLockWaiter(t *testing.T, pattern string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := w.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock' AND state = 'active' AND query LIKE $1`, pattern).Scan(&n)
		}); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no backend ever waited on a lock for %q (no real contention)", pattern)
}

// B-7 (CON) part 1, LF test 3 / C-K1-3(b): the executor's FOR SHARE locks
// on staff and in-force grant rows are real - a concurrent revoke (and a
// concurrent suspend) of the final approver WAITS for the execution to
// commit, from a tenant session AND from an acting session, inside the
// real executor.
func TestB7_ForShareBlocksConcurrentRevokeAndSuspend(t *testing.T) {
	for _, family := range []string{"tenant", "acting"} {
		t.Run(family, func(t *testing.T) {
			w := newWorld(t, worldOpts{base: 1})
			approver := w.F2
			if family == "acting" {
				approver = w.Acting
			}
			r, err := w.submit(w.F1, w.credit(250, ReasonOperationalErrorCorrection))
			if err != nil {
				t.Fatal(err)
			}

			locked := make(chan struct{})
			proceed := make(chan struct{})
			testHookAfterShareLocks = func(ctx context.Context, id uuid.UUID) {
				if id == r.ID {
					close(locked)
					<-proceed
				}
			}
			defer func() { testHookAfterShareLocks = nil }()

			var execOut Outcome
			var execErr error
			var execDone time.Time
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				execOut, execErr = w.decide(approver, r, DecisionApprove)
				execDone = time.Now()
			}()
			<-locked

			// Concurrent revoke of the approver's grant: must wait.
			var revokeDone time.Time
			var revokeErr error
			wg.Add(1)
			go func() {
				defer wg.Done()
				gid := w.grantIDs[grantKey(approver.ID, capability.CapabilityLedgerAdjustmentApprove)]
				revokeErr = w.pool.WithPlatformAdmin(context.Background(), w.GrantApprover.ID, func(ctx context.Context, tx pgx.Tx) error {
					return capability.RevokeGrant(ctx, tx, w.Tenant, gid, "k2-b7-race")
				})
				revokeDone = time.Now()
			}()
			w.waitForLockWaiter(t, "%staff_capability_grants%")

			// Concurrent suspend of the INITIATOR's staff row: must wait.
			var suspendDone time.Time
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
					_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, w.F1.ID)
					return err
				})
				suspendDone = time.Now()
			}()
			w.waitForLockWaiter(t, "%UPDATE staff_users SET status%")

			releasedAt := time.Now()
			close(proceed)
			wg.Wait()
			if execErr != nil || !execOut.Executed {
				t.Fatalf("execution: %v %+v", execErr, execOut)
			}
			if revokeErr != nil {
				t.Fatalf("revoke: %v", revokeErr)
			}
			if revokeDone.Before(releasedAt) || suspendDone.Before(releasedAt) {
				t.Fatalf("FOR SHARE did not hold: revoke/suspend finished before the executor was released")
			}
			if revokeDone.Before(execDone.Add(-time.Second)) {
				t.Fatalf("revoke completed well before the execution committed")
			}
			w.assertInvariants()
		})
	}
}

// B-7 part 1b, C-K1-3(b) "a revoke committed first is re-read as revoked":
// the first approver's grant is revoked and committed just before the
// final approval takes its FOR SHARE locks; the locked re-read sees the
// revoke, so that approval does not count and nothing executes.
func TestB7_RevokeCommittedFirstIsReReadAsRevoked(t *testing.T) {
	w := newWorld(t, worldOpts{base: 2})
	r, err := w.submit(w.F1, w.credit(250, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := w.decide(w.F2, r, DecisionApprove); err != nil || out.Counted != 1 {
		t.Fatalf("first approval: %v %+v", err, out)
	}
	w.revokeGrant(w.F2.ID, capability.CapabilityLedgerAdjustmentApprove)
	out, err := w.decide(w.Acting, r, DecisionApprove)
	if err != nil {
		t.Fatal(err)
	}
	requireNotCounted(t, w, out, "revoked-first approver (acting executor)")
}

// B-7 part 2: two concurrent final approvals -> exactly one ledger
// transaction. The first executor is held after its locks until the second
// is observed waiting on the L1 request row, then released.
func TestB7_ConcurrentFinalApprovalsPostOnce(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	r, err := w.submit(w.F1, w.credit(400, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	locked := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	testHookAfterShareLocks = func(ctx context.Context, id uuid.UUID) {
		once.Do(func() { close(locked); <-proceed })
	}
	defer func() { testHookAfterShareLocks = nil }()

	type res struct {
		out Outcome
		err error
	}
	results := make(chan res, 2)
	go func() { o, e := w.decide(w.F2, r, DecisionApprove); results <- res{o, e} }()
	<-locked
	go func() { o, e := w.decide(w.F3, r, DecisionApprove); results <- res{o, e} }()
	w.waitForLockWaiter(t, "%FROM ledger_adjustment_requests WHERE id = $1 AND tenant_id = $2 FOR UPDATE%")
	close(proceed)
	a, b := <-results, <-results
	executed, notPending := 0, 0
	for _, x := range []res{a, b} {
		switch {
		case x.err == nil && x.out.Executed:
			executed++
		case errors.Is(x.err, ErrNotPending):
			notPending++
		default:
			t.Fatalf("unexpected result: %v %+v", x.err, x.out)
		}
	}
	if executed != 1 || notPending != 1 {
		t.Fatalf("expected exactly one execution, got executed=%d notPending=%d", executed, notPending)
	}
	var n int
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE correlation_id = $1 AND transaction_type = 'manual_adjustment'`, r.ID).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected exactly one manual_adjustment transaction, got %d", n)
	}
	w.assertInvariants()
}

// B-7 part 3: two concurrent compensating_entry requests on ONE causation
// -> the §5.4 cumulative cap holds (INV-ADJ-6) because the L2 causation
// lock serializes them. Both executors are driven to the hook together
// (a 2-party barrier) and then released at once, so they genuinely race
// for the causation row.
func TestB7_ConcurrentCompensationsRespectCap(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	w.fund(5_000)
	bet := w.postCasino("casino_bet", 1_000)
	in := w.credit(700, ReasonCompensatingEntry)
	in.CausationTransactionID = &bet
	ra, err := w.submit(w.F1, in)
	if err != nil {
		t.Fatalf("submit a: %v", err)
	}
	rb, err := w.submit(w.F1, in)
	if err != nil {
		t.Fatalf("submit b: %v", err)
	}
	before := w.playerCash()

	var mu sync.Mutex
	arrived := 0
	allIn := make(chan struct{})
	testHookAfterShareLocks = func(ctx context.Context, id uuid.UUID) {
		mu.Lock()
		arrived++
		if arrived == 2 {
			close(allIn)
		}
		mu.Unlock()
		<-allIn
	}
	defer func() { testHookAfterShareLocks = nil }()

	type res struct {
		out Outcome
		err error
	}
	results := make(chan res, 2)
	go func() { o, e := w.decide(w.F2, ra, DecisionApprove); results <- res{o, e} }()
	go func() { o, e := w.decide(w.F3, rb, DecisionApprove); results <- res{o, e} }()
	a, b := <-results, <-results
	executed, capped := 0, 0
	for _, x := range []res{a, b} {
		if x.err != nil {
			t.Fatalf("decide: %v", x.err)
		}
		switch {
		case x.out.Executed:
			executed++
		case x.out.Request.State == StateRefusedAtExecution && x.out.Request.RefusalCode != nil && *x.out.Request.RefusalCode == "compensation_cap_exceeded":
			capped++
		default:
			t.Fatalf("unexpected outcome: %+v", x.out)
		}
	}
	if executed != 1 || capped != 1 {
		t.Fatalf("cap breached under concurrency: executed=%d capped=%d", executed, capped)
	}
	if got := w.playerCash(); got != before+700 {
		t.Fatalf("balance: want %d got %d", before+700, got)
	}
	w.assertInvariants()
}
