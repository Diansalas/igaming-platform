//go:build integration

// A7-TESTS-1 item #5c (rv-a7-tests.md, 1944de0). Ledger-finance's own
// runtime method (rv-fh3-ledger.md, 076e42e, condition C8) supersedes the
// earlier "blocked-on-design" static-check conclusion this file's
// predecessor investigation reached: rather than needing a SECOND,
// artificial call site to manufacture a genuine ABBA deadlock, the
// ordering itself is directly observable via WHERE the second of two
// identical deliveries blocks - exactly the technique FH-6's own A7-C1
// tests use for the main receipt path (asserting loBackendQuery contains
// "INSERT INTO payment_provider_events").
//
// Kept in its own file, separate from a7_1a_integration_test.go and the
// payout agent's own a7_lockorder_integration_test.go (FH-6, unmerged
// branch), to avoid a merge conflict.
package payments

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// a7c8WaitAnyQueryContains polls until ANY of pids currently shows a query
// containing needle AND is actually blocked waiting on a lock
// (pg_stat_activity.wait_event_type = 'Lock'), returning that pid.
// Deliberately checks BOTH pids on every poll: given two IDENTICAL,
// concurrently-started deliveries, this test cannot know in advance which
// one loses the race for the shared resource (R0's own unique-index
// entry) - by symmetry, either could be first, and the OTHER one may
// independently end up blocked on something else entirely (this test's
// own external L1 blocker) at the same time, which is not the signal
// being looked for here.
//
// Code review L2 (rv-fh3-code-review.md, FH3-FOLLOWUP-1): the query TEXT
// alone is not sufficient - a backend can show "INSERT INTO
// payment_provider_events" in pg_stat_activity.query while merely
// EXECUTING that statement (not yet blocked on anything), or while
// blocked on a WHOLLY DIFFERENT wait type (e.g. I/O). Requiring
// wait_event_type = 'Lock' on the SAME row confirms the backend is
// actually contending for the lock this test's own doc comment claims it
// is, not just that its last-reported statement text happens to match.
func a7c8WaitAnyQueryContains(t *testing.T, pool *db.Pool, pids []int, needle string, timeout time.Duration) (int, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, pid := range pids {
			query, waitEventType := loBackendQueryAndWaitEventType(t, pool, pid)
			if strings.Contains(query, needle) && waitEventType == "Lock" {
				return pid, true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0, false
}

// loBackendQueryAndWaitEventType reads both pg_stat_activity.query and
// pg_stat_activity.wait_event_type for pid in ONE row read, so the two
// values are never read from two different, potentially-inconsistent
// polling moments (loBackendQuery only ever returns the query text).
func loBackendQueryAndWaitEventType(t *testing.T, pool *db.Pool, pid int) (query, waitEventType string) {
	t.Helper()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(query, ''), COALESCE(wait_event_type, '') FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&query, &waitEventType)
	})
	if err != nil {
		t.Fatalf("read pg_stat_activity.query/wait_event_type for pid %d: %v", pid, err)
	}
	return query, waitEventType
}

// TestA7_5c_TombstoneBranch_SecondIdenticalReversalWaitsOnReceiptInsert
// closes A7 #5c / ledger-finance C8: two IDENTICAL deliveries of a
// reversal naming a RESOLVED-BUT-NEVER-POSTED original (the tombstone
// branch's own precondition - a real provider_reference, no
// ledger_transaction_id, exactly A7-TOMB-1's fixture) share the SAME
// event fingerprint, so whichever one's R0 INSERT runs second is refused
// entry by the OTHER's still-uncommitted (tenant_id, provider_id,
// event_fingerprint) unique-index entry and BLOCKS on that insert -
// observable directly via pg_stat_activity.query, exactly as FH-6's own
// A7-C1 tests assert for the main receipt path.
//
// An EXTERNAL blocker holds the intent's own L1 row (deposit_intents)
// throughout, exactly like a7_1a_integration_test.go's own a7HoldRow use -
// not to test THAT lock itself, but to force reliable overlap between the
// two racers regardless of goroutine scheduling: with the correct order
// (R0 before L1), whichever racer loses the R0 race blocks on the R0
// INSERT immediately, before ever reaching the (separately blocked) L1
// row at all - so this test's own polling, run BEFORE the blocker is ever
// released, reliably observes "payment_provider_events" in one of the two
// racers' own query text. Under the mutant (L1 moved before R0), BOTH
// racers reach L1 FIRST and simply queue behind the external blocker -
// NEITHER ever shows payment_provider_events in its query while the
// blocker is held, so this test's own timeout fires instead, catching the
// exact ordering violation A7-TOMB-1's doc comment describes.
func TestA7_5c_TombstoneBranch_SecondIdenticalReversalWaitsOnReceiptInsert(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("a7-5c", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"a7-5c": p}, MultiWebhookCredentialResolver{"a7-5c": NewMockWebhookCredentials(p)})

	// A never-posted original: rvInit puts the attempt in 'pending' with a
	// real provider_reference, but it has no ledger_transaction_id yet -
	// exactly the tombstone branch's own precondition (A7-TOMB-1's own
	// fixture, reused here).
	res := rvInit(t, pool, orch, f, 5000, "a7-5c")
	originalRef := *res.Attempt.ProviderReference
	payload := p.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "a7-5c-reversal-ref", originalRef, OutcomeSucceeded, 5000, "EUR", "", false)

	blocker := a7HoldRow(t, pool, f.tenantID, "deposit_intents", *res.Attempt.DepositIntentID, "a7-5c-blocker")

	type result struct {
		disp ReceiveCallbackResult
		err  error
	}
	results := make([]result, 2)
	pids := make([]int, 2)
	pidCh := make([]chan int, 2)
	done := make([]chan struct{}, 2)
	for i := range done {
		done[i] = make(chan struct{})
		pidCh[i] = make(chan int, 1)
	}
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer close(done[i])
			_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var pid int
				if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
					pidCh[i] <- 0
					return err
				}
				pidCh[i] <- pid
				r, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "a7-5c", payload)
				results[i] = result{disp: r, err: err}
				return err
			})
		}(i)
	}
	pids[0], pids[1] = <-pidCh[0], <-pidCh[1]
	if pids[0] == 0 || pids[1] == 0 {
		blocker.release()
		<-done[0]
		<-done[1]
		t.Fatalf("setup: a racer never reported a backend pid")
	}

	blockedPid, ok := a7c8WaitAnyQueryContains(t, pool, pids, "INSERT INTO payment_provider_events", loLockWaitTimeout)
	if !ok {
		blocker.release()
		<-done[0]
		<-done[1]
		t.Fatalf("A7 #5c: neither identical delivery's own query ever showed the R0 receipt insert while the external L1 blocker was held - expected one to block on R0 (the correct order); this is the ordering violation the test exists to catch (or the mutant is in place)")
	}
	// The OTHER racer, if not the one caught above, must never itself be
	// stuck on the SAME R0 insert too (that would mean neither reached L1
	// at all yet, inconclusive) - it is expected to either still be
	// running or already queued on the externally-held deposit_intents
	// row, never relevant to this assertion either way.
	_ = blockedPid

	blocker.release()
	<-done[0]
	<-done[1]

	for i, r := range results {
		if r.err != nil {
			if strings.Contains(r.err.Error(), "40P01") || strings.Contains(r.err.Error(), "deadlock detected") {
				t.Fatalf("A7 #5c: delivery %d deadlocked: %v", i, r.err)
			}
			t.Fatalf("A7 #5c: delivery %d unexpected error: %v", i, r.err)
		}
	}
	dispositions := map[ReceiptDisposition]int{}
	for _, r := range results {
		dispositions[r.disp.Disposition]++
	}
	if dispositions[DispositionApplied] != 1 || dispositions[DispositionDuplicateEffect] != 1 {
		t.Errorf("A7 #5c: expected exactly one applied + one duplicate_effect across the two identical deliveries, got %v", dispositions)
	}

	var tombCount int64
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE transaction_type='tombstone' AND provider_tx_id=$1`, originalRef).Scan(&tombCount)
	}); err != nil {
		t.Fatal(err)
	}
	if tombCount != 1 {
		t.Errorf("A7 #5c: expected exactly one tombstone, got %d", tombCount)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}
