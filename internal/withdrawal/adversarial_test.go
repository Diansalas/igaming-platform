//go:build integration

// This file closes the withdrawal-four-eyes/cross-tenant subset of the
// Stage 3B mandatory adversarial financial test list (see the Stage 3B
// completion report for the full 26-item list). It follows the exact
// fixture/runTx/mustRunTx conventions established in
// withdrawal_integration_test.go - see that file for testPool/seedFixture.
package withdrawal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
)

// pgRLSViolationCode is the Postgres SQLSTATE for "new row violates
// row-level security policy" - duplicated from internal/db's own test
// helper rather than exported/shared, per this repo's per-package test
// helper convention (see withdrawal_integration_test.go's fixture doc
// comment).
const pgRLSViolationCode = "42501"

func assertRLSViolation(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a row-level-security violation, got nil")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != pgRLSViolationCode {
		t.Fatalf("expected SQLSTATE %s (row-level security violation), got %s: %v", pgRLSViolationCode, pgErr.Code, err)
	}
}

// TestCrossTenant_WithdrawalRequestsRLSBlocksReadAndWrite is the
// withdrawal_requests-specific real-Postgres RLS proof required by
// mandatory adversarial test #5. internal/ledger and internal/wallet's
// own integration tests already prove this pattern for
// ledger_accounts/wallets; withdrawal_requests had no equivalent test of
// its own before this file, despite carrying the exact same two-policy
// RLS shape (migration 0026).
func TestCrossTenant_WithdrawalRequestsRLSBlocksReadAndWrite(t *testing.T) {
	pool := testPool(t)
	fA := seedFixture(t, pool, 1000)
	fB := seedFixture(t, pool, 1000)

	wrB := mustRequestWithdrawal(t, pool, fB, 300, "wd-cross-tenant")

	// A GetByID lookup for tenant B's request, scoped to tenant A, must
	// behave exactly as if the row did not exist - not error out with
	// some other status, and never return tenant B's data.
	err := runTx(pool, fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := GetByID(ctx, tx, wrB.ID)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound reading tenant B's withdrawal request from tenant A's scope, got %v", err)
	}

	// An unfiltered COUNT scoped to tenant A must not see tenant B's row
	// even when explicitly queried by id - proving the isolation is RLS
	// itself, not an accidental WHERE clause somewhere in this package.
	mustRunTx(t, pool, fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_requests WHERE id = $1`, wrB.ID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("expected tenant A's scope to see zero rows for tenant B's withdrawal request id, got %d", count)
		}
		return nil
	})

	// A write attempt (Cancel, which locks-then-updates) against tenant
	// B's request from tenant A's scope must also see nothing to act on -
	// not silently cancel another tenant's withdrawal.
	err = runTx(pool, fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Cancel(ctx, tx, wrB.ID)
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound cancelling tenant B's withdrawal request from tenant A's scope, got %v", err)
	}

	// Tenant B's own row is unaffected and still readable/cancellable
	// from tenant B's own scope.
	mustRunTx(t, pool, fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		got, err := GetByID(ctx, tx, wrB.ID)
		if err != nil {
			return err
		}
		if got.State != StateRequested {
			t.Fatalf("expected tenant B's request to remain in state %q, got %q", StateRequested, got.State)
		}
		return nil
	})

	// The WITH CHECK half: tenant A's scope cannot forge an INSERT
	// claiming to belong to tenant B, even supplying tenant B's own real
	// brand/player/wallet ids.
	err = runTx(pool, fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// B13-B: this attacks RLS WITH CHECK; the BEFORE INSERT binding guard (migration 0126) would
		// pre-empt it, so it is switched off for this one statement (re-enabled in the same tx).
		return pitest.WithoutBindingGuard(ctx, tx, func() error {
			_, err := tx.Exec(ctx,
				`INSERT INTO withdrawal_requests
					(id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key)
				 VALUES ($1, $2, $3, $4, $5, 'EUR', 100, 'forged-cross-tenant-key')`,
				uuid.New(), fB.tenantID, fB.brandID, fB.playerAccountID, fB.walletID,
			)
			return err
		})
	})
	assertRLSViolation(t, err)
}

// TestWithdrawalRequests_AmountImmutableAfterInsert is mandatory
// adversarial test #10. withdrawal-state-machine.md §7 documents amount
// (among other identifying fields) as immutable after insert, enforced by
// the withdrawal_requests_immutable_fields trigger (migration 0026) -
// without it, the four-eyes threshold check is bypassable by requesting a
// below-threshold amount, collecting the single approval it needs, then
// raising the amount. This test attacks the database directly with a raw
// SQL UPDATE, not through any Go-level code path, since the invariant
// under test is that Postgres itself refuses the mutation regardless of
// what application code does or doesn't check.
func TestWithdrawalRequests_AmountImmutableAfterInsert(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)

	wr := mustRequestWithdrawal(t, pool, f, 300, "wd-immutable-amount")

	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET amount = amount + 1 WHERE id = $1`, wr.ID)
		return err
	})
	if err == nil {
		t.Fatal("expected the database to reject a direct UPDATE of withdrawal_requests.amount, got nil error")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError from the immutability trigger, got %T: %v", err, err)
	}
	if pgErr.Message == "" {
		t.Fatalf("expected a non-empty trigger error message, got: %+v", pgErr)
	}

	// The rejected UPDATE must not have partially applied - re-read in a
	// fresh transaction (the failed one above was rolled back) and
	// confirm amount is exactly what it was at insert.
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		got, err := GetByID(ctx, tx, wr.ID)
		if err != nil {
			return err
		}
		if got.Amount != 300 {
			t.Fatalf("expected amount to remain 300 after a rejected mutation attempt, got %d", got.Amount)
		}
		return nil
	})
}

// TestReject_DuplicateDecisionBySameApproverRejected closes the remaining
// gap in mandatory adversarial test #11 (duplicate approval/decision):
// TestApprove_AboveThresholdRequiresTwoDistinctHumanApprovers already
// proves a second Approve call by the same approver is rejected; this
// test proves the identical UNIQUE (withdrawal_request_id,
// approver_principal_id) constraint also rejects a same-approver Reject
// call following an earlier Approve - i.e. an approver cannot record a
// second, contradictory decision on the same request either way. This
// specifically exercises Reject's own db.IsUniqueViolation handling
// (withdrawal.go), which was not reached by any prior test.
func TestReject_DuplicateDecisionBySameApproverRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1_000_000)

	wr := mustRequestWithdrawal(t, pool, f, 100_000, "wd-dup-reject")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})

	approver := mustCreateApprover(t, pool, f.tenantID)
	// amount (100,000) >= the default policy threshold (EUR 1,000.00 =
	// 100,000 minor units, via ">="): two humans required, so one
	// approval leaves the request open to a second decision attempt.
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		approved, err := Approve(ctx, tx, wr.ID, approver, false, nil, alwaysEligible)
		if err != nil {
			return err
		}
		if approved {
			t.Fatal("expected a single above-threshold approval to be insufficient")
		}
		return nil
	})

	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Reject(ctx, tx, wr.ID, approver, "changed_my_mind", alwaysEligible)
	})
	if !errors.Is(err, ErrDuplicateApproval) {
		t.Fatalf("expected ErrDuplicateApproval when the same approver who approved then attempts to reject, got %v", err)
	}

	// The request must remain exactly where the (sole, valid) approval
	// left it - still pending_review, not rejected.
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		got, err := GetByID(ctx, tx, wr.ID)
		if err != nil {
			return err
		}
		if got.State != StatePendingReview {
			t.Fatalf("expected state to remain %q after a rejected duplicate decision, got %q", StatePendingReview, got.State)
		}
		return nil
	})
}

// TestWithdrawalApprovals_ApproverPrincipalIdImmutableAndAccurate is
// mandatory adversarial test #12 (approval substitution). It proves two
// things a same-request/HTTP-layer test cannot: (1) the row Approve
// writes records approver_principal_id exactly as given, and (2) once
// written, approver_principal_id can never be changed or removed by a
// direct SQL attack - not merely "the API has no endpoint for it" but
// "the database itself refuses" (the withdrawal_approvals_immutable
// trigger, migration 0026, denies ANY UPDATE/DELETE, not just on this
// column). This is what makes approval substitution/forging impossible
// even for a caller with raw SQL access to the tenant-scoped connection,
// on top of the API-layer fact (see withdrawal_handlers.go's
// newApproveWithdrawalHandler/newRejectWithdrawalHandler) that
// approverPrincipalID is always derived from the authenticated JWT
// subject and Approve/Reject's Go signatures give a request body no way
// to supply it.
func TestWithdrawalApprovals_ApproverPrincipalIdImmutableAndAccurate(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)

	wr := mustRequestWithdrawal(t, pool, f, 100, "wd-approver-integrity")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})
	mustSetWithdrawalPolicy(t, pool, f.tenantID, "EUR", 1_000_000, 2, time.Now().Add(-time.Hour))

	approver := mustCreateApprover(t, pool, f.tenantID)
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		approved, err := Approve(ctx, tx, wr.ID, approver, false, nil, alwaysEligible)
		if err != nil {
			return err
		}
		if !approved {
			t.Fatal("expected a single below-threshold approval to approve the request")
		}
		return nil
	})

	// (1) accuracy: the row on disk names exactly the approver that was
	// passed in - never some other, substitutable id.
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var recorded uuid.UUID
		if err := tx.QueryRow(ctx,
			`SELECT approver_principal_id FROM withdrawal_approvals WHERE withdrawal_request_id = $1`, wr.ID,
		).Scan(&recorded); err != nil {
			return err
		}
		if recorded != approver {
			t.Fatalf("expected recorded approver_principal_id %s, got %s", approver, recorded)
		}
		return nil
	})

	// (2) immutability: a direct UPDATE substituting a different approver
	// identity for the same decision is refused outright.
	forgedApprover := uuid.New()
	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE withdrawal_approvals SET approver_principal_id = $1 WHERE withdrawal_request_id = $2`,
			forgedApprover, wr.ID,
		)
		return err
	})
	if err == nil {
		t.Fatal("expected the database to reject a direct UPDATE of withdrawal_approvals.approver_principal_id, got nil error")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError from the append-only trigger, got %T: %v", err, err)
	}

	// Deleting the row (to reinsert a substitute decision under the same
	// approval slot) is refused identically - append-only means append-
	// only, not "immutable but deletable".
	err = runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM withdrawal_approvals WHERE withdrawal_request_id = $1`, wr.ID)
		return err
	})
	if err == nil {
		t.Fatal("expected the database to reject a direct DELETE of a withdrawal_approvals row, got nil error")
	}
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError from the append-only trigger, got %T: %v", err, err)
	}
}

// TestApprove_ThresholdAmountRecordedPerDecisionAndCurrentThresholdGovernsReadiness
// is mandatory adversarial test #13. It verifies the audit-trail half
// (threshold_amount_at_decision/request_amount_at_decision are recorded
// verbatim per decision and never retroactively altered by a later
// decision or a later threshold change) and the mechanism half
// (requiresTwoHumans is evaluated, at each call, against THAT call's own
// thresholdAmount input - not a value cached from an earlier decision on
// the same request). withdrawal-state-machine.md §5 bypass #3 records
// that whether config-edit and withdrawal-approval permissions must be
// disjoint is an `OPEN DECISION` - this test does not take a position on
// that policy question, it only confirms Approve behaves exactly as
// documented given whatever threshold its caller supplies each time.
func TestApprove_ThresholdAmountRecordedPerDecisionAndCurrentThresholdGovernsReadiness(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1_000_000)

	wr := mustRequestWithdrawal(t, pool, f, 100_000, "wd-threshold-audit-trail")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})

	// Decision 1: an explicit policy row, threshold below the request
	// amount -> requiresMultipleApprovals is true, and a single human
	// approval must not be enough.
	mustSetWithdrawalPolicy(t, pool, f.tenantID, "EUR", 50_000, 2, time.Now().Add(-time.Hour))
	approver1 := mustCreateApprover(t, pool, f.tenantID)
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		approved, err := Approve(ctx, tx, wr.ID, approver1, false, nil, alwaysEligible)
		if err != nil {
			return err
		}
		if approved {
			t.Fatal("expected the first above-threshold approval to be insufficient on its own")
		}
		return nil
	})

	// Decision 2: a second, later-effective policy row raises the
	// threshold above the request amount - requiresMultipleApprovals is
	// false for THIS call, so this second approval alone satisfies
	// readiness (the mechanism resolves its own call's policy fresh
	// every time, never a value memoized from decision 1).
	mustSetWithdrawalPolicy(t, pool, f.tenantID, "EUR", 200_000, 2, time.Now())
	approver2 := mustCreateApprover(t, pool, f.tenantID)
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		approved, err := Approve(ctx, tx, wr.ID, approver2, false, nil, alwaysEligible)
		if err != nil {
			return err
		}
		if !approved {
			t.Fatal("expected the second approval, evaluated against its own (raised) threshold input, to satisfy readiness immediately")
		}
		return nil
	})

	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		got, err := GetByID(ctx, tx, wr.ID)
		if err != nil {
			return err
		}
		if got.State != StateApproved {
			t.Fatalf("expected state %q, got %q", StateApproved, got.State)
		}
		return nil
	})

	// Audit trail: each decision's row still shows exactly the
	// threshold/amount snapshot it was decided under - decision 1's row
	// was never rewritten to reflect decision 2's threshold, and vice
	// versa.
	type recordedDecision struct {
		approver  uuid.UUID
		threshold int64
		amount    int64
	}
	var decisions []recordedDecision
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT approver_principal_id, threshold_amount_at_decision, request_amount_at_decision
			 FROM withdrawal_approvals WHERE withdrawal_request_id = $1 ORDER BY decided_at ASC`,
			wr.ID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d recordedDecision
			if err := rows.Scan(&d.approver, &d.threshold, &d.amount); err != nil {
				return err
			}
			decisions = append(decisions, d)
		}
		return rows.Err()
	})

	if len(decisions) != 2 {
		t.Fatalf("expected exactly 2 recorded decisions, got %d", len(decisions))
	}
	if decisions[0].approver != approver1 || decisions[0].threshold != 50_000 || decisions[0].amount != 100_000 {
		t.Fatalf("decision 1 audit trail altered: got approver=%s threshold=%d amount=%d", decisions[0].approver, decisions[0].threshold, decisions[0].amount)
	}
	if decisions[1].approver != approver2 || decisions[1].threshold != 200_000 || decisions[1].amount != 100_000 {
		t.Fatalf("decision 2 audit trail incorrect: got approver=%s threshold=%d amount=%d", decisions[1].approver, decisions[1].threshold, decisions[1].amount)
	}
}

// TestLockApprovedForSubmission_ConcurrentSubmitsOnlyOneReachesProvider
// closes a P1 finding from the Stage 3B architecture/code review: the
// original newSubmitWithdrawalHandler took no row lock before calling
// out to a PaymentProvider, so two concurrent submit attempts (a staff
// double-click, or a client retry racing the original request) could
// both observe `approved` via a plain read and both call the provider -
// a real double payout, with the loser's evidence of having called the
// provider lost to its own rolled-back transaction. This proves
// LockApprovedForSubmission itself serializes correctly: of N concurrent
// callers, exactly one observes `approved` and "calls the provider"
// (modeled here by incrementing a shared counter while holding the
// lock), and every other caller gets ErrStateConflict WITHOUT ever
// incrementing that counter - proving the lock, not luck, is what
// prevents the double call.
func TestLockApprovedForSubmission_ConcurrentSubmitsOnlyOneReachesProvider(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)

	wr := mustRequestWithdrawal(t, pool, f, 300, "wd-submit-race")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})
	mustSetWithdrawalPolicy(t, pool, f.tenantID, "EUR", 1_000_000, 2, time.Now().Add(-time.Hour))
	firstApprover := mustCreateApprover(t, pool, f.tenantID)
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		approved, err := Approve(ctx, tx, wr.ID, firstApprover, false, nil, alwaysEligible)
		if err != nil {
			return err
		}
		if !approved {
			t.Fatal("expected a single below-threshold approval to approve the request")
		}
		return nil
	})

	const n = 5
	var providerCalls int32
	var mu sync.Mutex // guards providerCalls
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := LockApprovedForSubmission(ctx, tx, wr.ID); err != nil {
					return err
				}
				// Simulate "call the provider" - the exact operation that
				// must never happen twice for one withdrawal request.
				mu.Lock()
				providerCalls++
				mu.Unlock()
				return MarkSubmitted(ctx, tx, wr.ID, "mockpsp", "payout-ref-race-"+uuid.New().String())
			})
		}(i)
	}
	wg.Wait()

	var succeeded, stateConflict int
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrStateConflict):
			stateConflict++
		default:
			t.Fatalf("goroutine %d: unexpected error: %v", i, err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected exactly 1 goroutine to successfully submit, got %d (stateConflict=%d)", succeeded, stateConflict)
	}
	if stateConflict != n-1 {
		t.Fatalf("expected %d goroutines to see ErrStateConflict, got %d", n-1, stateConflict)
	}
	if providerCalls != 1 {
		t.Fatalf("expected the simulated provider call to happen exactly once, got %d - the lock did not prevent a concurrent double call", providerCalls)
	}

	var final WithdrawalRequest
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		final, err = GetByID(ctx, tx, wr.ID)
		return err
	})
	if final.State != StateSubmitted {
		t.Fatalf("expected final state %q, got %q", StateSubmitted, final.State)
	}
}

// TestApprove_ConcurrentDistinctApproversOnlyRequiredCountSatisfiesReadiness
// is Stage 3C adversarial test 8.D: "two simultaneous approval requests -
// only valid required approvals count". Three DISTINCT human approvers
// call Approve for the SAME above-threshold (RequiredApprovals=2) request
// at once. lockRequestForUpdate (withdrawal.go) must serialize them: the
// request transitions to `approved` exactly once, driven by exactly the
// first two approvals to acquire the row lock, in whatever order the
// scheduler happens to run them - never zero, never both simultaneously
// reporting `approved`, and the third (superfluous) approver's decision
// must still be recorded (a valid, distinct human approval is never
// silently dropped) but must see ErrStateConflict, never a phantom
// second `approved` transition, once the request has already left
// pending_review.
func TestApprove_ConcurrentDistinctApproversOnlyRequiredCountSatisfiesReadiness(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1_000_000)

	wr := mustRequestWithdrawal(t, pool, f, 100_000, "wd-concurrent-approvals") // >= the default 100,000 threshold: 2 approvals required.
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})

	const n = 3
	approvers := make([]uuid.UUID, n)
	for i := range approvers {
		approvers[i] = mustCreateApprover(t, pool, f.tenantID)
	}

	results := make([]bool, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				approved, err := Approve(ctx, tx, wr.ID, approvers[i], false, nil, alwaysEligible)
				results[i] = approved
				return err
			})
		}(i)
	}
	wg.Wait()

	var succeededApproved, stateConflicts, otherErrors int
	for i, err := range errs {
		switch {
		case err == nil && results[i]:
			succeededApproved++
		case err == nil && !results[i]:
			// This approver's decision was recorded (no error) but
			// didn't itself trigger the transition - valid when it was
			// one of the first two to be recorded, whichever those turn
			// out to be under the race.
		case errors.Is(err, ErrStateConflict):
			stateConflicts++
		default:
			otherErrors++
			t.Errorf("goroutine %d: unexpected error: %v", i, err)
		}
	}
	if otherErrors != 0 {
		t.Fatalf("expected only nil or ErrStateConflict outcomes, got %d unexpected error(s)", otherErrors)
	}
	// Exactly one goroutine's call is the one that observes and causes
	// the pending_review -> approved transition (Approve returns
	// approved=true only on that call).
	if succeededApproved != 1 {
		t.Fatalf("expected exactly 1 goroutine to observe approved=true, got %d", succeededApproved)
	}
	// The remaining n-1 calls each either recorded a (superfluous but
	// valid) decision with approved=false, or - if it lost the row-lock
	// race until AFTER the transition already committed - got
	// ErrStateConflict. Both are acceptable serializations of the same
	// underlying race; what matters is neither corrupted state nor
	// silently vanished. The precise split is verified below via the
	// actual recorded-approvals count.

	// However the race resolved, the request must have ended up
	// deterministically approved - never stuck in pending_review, never
	// double-approved.
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		got, err := GetByID(ctx, tx, wr.ID)
		if err != nil {
			return err
		}
		if got.State != StateApproved {
			t.Fatalf("expected final state %q, got %q", StateApproved, got.State)
		}
		return nil
	})

	// Only genuinely distinct human approvals were ever counted -
	// exactly as many withdrawal_approvals rows exist as approvers that
	// got a nil error (a decision was actually recorded for them).
	var recordedCount int
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COUNT(DISTINCT approver_principal_id) FROM withdrawal_approvals WHERE withdrawal_request_id = $1 AND decision = 'approve'`,
			wr.ID,
		).Scan(&recordedCount)
	})
	wantRecorded := n - stateConflicts
	if recordedCount != wantRecorded {
		t.Fatalf("expected %d recorded distinct approvals (n=%d minus %d state conflicts), got %d", wantRecorded, n, stateConflicts, recordedCount)
	}
	if recordedCount < 2 {
		t.Fatalf("expected at least the required 2 distinct approvals to have been recorded, got %d", recordedCount)
	}
}

// --- Stage 3D governance trigger: direct-SQL bypass attempts ---
//
// These tests bypass internal/withdrawal.Approve/Reject entirely (a raw
// INSERT into withdrawal_approvals), proving migration 0034's
// withdrawal_approvals_enforce_governance trigger - not this package's
// own Go-level ApproverEligibility closure - is what actually makes
// Stage 3D's mandatory-Person-linkage/active-status policy authoritative,
// per directive item 9 ("the database must not permit a prohibited
// approval merely because app-layer authorization was bypassed").

// mustInsertBareApprovalAttempt is the shared raw-SQL INSERT these tests
// use - deliberately identical in shape to the legitimate INSERT
// Approve/Reject issue, so a rejection can only be attributed to the
// trigger's own linkage/status check, never to some other malformed
// statement.
func mustInsertBareApprovalAttempt(pool *db.Pool, tenantID, requestID, approverID uuid.UUID, decision string) error {
	return runTx(pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_approvals
				(id, tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision,
				 threshold_amount_at_decision, request_amount_at_decision)
			 VALUES ($1, $2, $3, $4, false, $5, 0, 0)`,
			uuid.New(), tenantID, requestID, approverID, decision,
		)
		return err
	})
}

// TestWithdrawalApprovalsGovernance_UnresolvableApproverDirectSQLRejected
// proves the trigger rejects a decision naming a principal id with NO
// backing staff_users row at all - not merely "no linkage", but
// "identity cannot be resolved" (directive item 1's own wording),
// entirely independent of internal/httpserver's or this package's own
// Go-level checks.
func TestWithdrawalApprovalsGovernance_UnresolvableApproverDirectSQLRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)
	wr := mustRequestWithdrawal(t, pool, f, 100, "wd-gov-unresolvable")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})

	err := mustInsertBareApprovalAttempt(pool, f.tenantID, wr.ID, uuid.New(), "approve")
	if err == nil {
		t.Fatal("expected the database to reject a decision from a principal id with no staff_users row, got nil error")
	}
}

// TestWithdrawalApprovalsGovernance_UnlinkedStaffDirectSQLRejected proves
// the trigger rejects a decision from a REAL staff_users row that simply
// has no person_id - the exact legacy/pre-remediation case Stage 3C left
// optional and Stage 3D now forbids at the database layer, regardless of
// what any application-layer check believes.
func TestWithdrawalApprovalsGovernance_UnlinkedStaffDirectSQLRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)
	wr := mustRequestWithdrawal(t, pool, f, 100, "wd-gov-unlinked")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})

	unlinkedStaffID := uuid.New()
	if err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			 VALUES ($1, $2, $3, 'x', 'finance', 'active', NULL)`,
			unlinkedStaffID, f.tenantID, unlinkedStaffID.String()+"@example.com",
		)
		return err
	}); err != nil {
		t.Fatalf("seed unlinked staff user: %v", err)
	}

	err := mustInsertBareApprovalAttempt(pool, f.tenantID, wr.ID, unlinkedStaffID, "approve")
	if err == nil {
		t.Fatal("expected the database to reject a decision from a real but unlinked (person_id NULL) staff account, got nil error")
	}
}

// TestWithdrawalApprovalsGovernance_InactiveStaffDirectSQLRejected proves
// the trigger rejects a decision from a linked staff account whose status
// is not 'active' - closing adversarial test item E at the database
// layer directly, independent of any HTTP-layer or Go-level check.
func TestWithdrawalApprovalsGovernance_InactiveStaffDirectSQLRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)
	wr := mustRequestWithdrawal(t, pool, f, 100, "wd-gov-inactive")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})

	inactiveStaffID := mustCreateApprover(t, pool, f.tenantID)
	if err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, inactiveStaffID)
		return err
	}); err != nil {
		t.Fatalf("suspend staff user: %v", err)
	}

	err := mustInsertBareApprovalAttempt(pool, f.tenantID, wr.ID, inactiveStaffID, "approve")
	if err == nil {
		t.Fatal("expected the database to reject a decision from a linked but inactive staff account, got nil error")
	}
}

// TestWithdrawalApprovalsGovernance_RejectDecisionAlsoEnforced proves the
// trigger's linkage/status check fires for decision = 'reject' too, not
// only 'approve' - business decision #1's "approve, reject, or submit"
// wording, at the one transition (reject) that DOES insert into
// withdrawal_approvals for the trigger to see.
func TestWithdrawalApprovalsGovernance_RejectDecisionAlsoEnforced(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)
	wr := mustRequestWithdrawal(t, pool, f, 100, "wd-gov-reject-unlinked")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})

	err := mustInsertBareApprovalAttempt(pool, f.tenantID, wr.ID, uuid.New(), "reject")
	if err == nil {
		t.Fatal("expected the database to reject a 'reject' decision from an unresolvable principal id, got nil error")
	}
}

// TestWithdrawalApprovalsGovernance_AutomatedApprovalExemptFromLinkageCheck
// proves the trigger does NOT require a staff_users/person_id linkage
// when is_automated_approval is true - ADR 0014's service-identity
// pattern is a fundamentally different kind of principal (never a
// Person), and this exemption is verified directly against the trigger
// itself, not merely inferred from internal/withdrawal.Approve's own
// isAutomated branch.
func TestWithdrawalApprovalsGovernance_AutomatedApprovalExemptFromLinkageCheck(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)
	wr := mustRequestWithdrawal(t, pool, f, 100, "wd-gov-automated-exempt")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})

	serviceIdentity := uuid.New() // no backing staff_users row at all - exactly a real service identity's shape.
	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_approvals
				(id, tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision,
				 threshold_amount_at_decision, request_amount_at_decision)
			 VALUES ($1, $2, $3, $4, true, 'approve', 0, 0)`,
			uuid.New(), f.tenantID, wr.ID, serviceIdentity,
		)
		return err
	})
	if err != nil {
		t.Fatalf("expected the database to accept an automated approval with no staff linkage, got error: %v", err)
	}
}

// TestWithdrawalApprovalsGovernance_AutomatedFlagCannotBypassSelfApproval
// is a regression test for a P0 finding this stage's own code-reviewer
// caught before commit: an earlier draft of migration 0034 exempted
// EVERY is_automated_approval = true row from every check, including the
// self-approval equality check - meaning a real staff member could
// self-approve their own withdrawal simply by setting that one
// client-supplied boolean, reopening the exact bypass migration 0033
// deliberately closed. The fix makes the exemption depend on whether a
// staff_users row actually resolves for the principal, not on the flag:
// a REAL staff row must satisfy every rule regardless of
// is_automated_approval. This proves it directly against the trigger,
// using the exact dual-role (staff-is-also-player) fixture the self-
// approval tests use, with is_automated_approval = true.
func TestWithdrawalApprovalsGovernance_AutomatedFlagCannotBypassSelfApproval(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)
	wr := mustRequestWithdrawal(t, pool, f, 100, "wd-gov-automated-self-approve")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})

	// A real, linked, active staff account whose person_id is the SAME
	// person as the withdrawing player - the dual-role fixture.
	dualRoleStaffID := uuid.New()
	if err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var beneficiaryPersonID uuid.UUID
		if err := tx.QueryRow(ctx,
			`SELECT pa.person_id FROM withdrawal_requests wr JOIN player_accounts pa ON pa.id = wr.player_account_id WHERE wr.id = $1`,
			wr.ID,
		).Scan(&beneficiaryPersonID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			 VALUES ($1, $2, $3, 'x', 'finance', 'active', $4)`,
			dualRoleStaffID, f.tenantID, dualRoleStaffID.String()+"@example.com", beneficiaryPersonID,
		)
		return err
	}); err != nil {
		t.Fatalf("seed dual-role staff user: %v", err)
	}

	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_approvals
				(id, tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision,
				 threshold_amount_at_decision, request_amount_at_decision)
			 VALUES ($1, $2, $3, $4, true, 'approve', 0, 0)`,
			uuid.New(), f.tenantID, wr.ID, dualRoleStaffID,
		)
		return err
	})
	if err == nil {
		t.Fatal("expected the database to reject a self-approval from a real staff row even when is_automated_approval = true, got nil error")
	}
	if count := countApprovals(t, pool, f.tenantID, wr.ID); count != 0 {
		t.Fatalf("expected zero recorded approvals after a rejected automated-flagged self-approval, got %d", count)
	}
}

// TestWithdrawalApprovalsGovernance_AutomatedFlagCannotBypassLinkageCheck
// is the linkage-side twin of the test above: a real but UNLINKED staff
// row with is_automated_approval = true must still be refused - the flag
// cannot substitute for a real service identity (no staff row at all).
func TestWithdrawalApprovalsGovernance_AutomatedFlagCannotBypassLinkageCheck(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)
	wr := mustRequestWithdrawal(t, pool, f, 100, "wd-gov-automated-unlinked")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})

	unlinkedStaffID := uuid.New()
	if err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			 VALUES ($1, $2, $3, 'x', 'finance', 'active', NULL)`,
			unlinkedStaffID, f.tenantID, unlinkedStaffID.String()+"@example.com",
		)
		return err
	}); err != nil {
		t.Fatalf("seed unlinked staff user: %v", err)
	}

	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_approvals
				(id, tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision,
				 threshold_amount_at_decision, request_amount_at_decision)
			 VALUES ($1, $2, $3, $4, true, 'approve', 0, 0)`,
			uuid.New(), f.tenantID, wr.ID, unlinkedStaffID,
		)
		return err
	})
	if err == nil {
		t.Fatal("expected the database to reject an unlinked real staff row's decision even when is_automated_approval = true, got nil error")
	}
}

// countApprovals is a small shared helper for the tests above.
func countApprovals(t *testing.T, pool *db.Pool, tenantID, requestID uuid.UUID) int {
	t.Helper()
	var count int
	mustRunTx(t, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_approvals WHERE withdrawal_request_id = $1`, requestID).Scan(&count)
	})
	return count
}

// TestWithdrawalPolicies_UpdateDeniedAtDatabaseLayer proves migration
// 0034's withdrawal_policies_deny_update trigger: no UPDATE is ever
// permitted, direct SQL included - the table really is insert-only, not
// merely insert-only "by convention" of the admin API's own handlers.
func TestWithdrawalPolicies_UpdateDeniedAtDatabaseLayer(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 0)
	mustSetWithdrawalPolicy(t, pool, f.tenantID, "EUR", 100_000, 2, time.Now())

	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE withdrawal_policies SET approval_threshold_minor_units = 0 WHERE tenant_id = $1`, f.tenantID)
		return err
	})
	if err == nil {
		t.Fatal("expected the database to reject a direct UPDATE of withdrawal_policies, got nil error")
	}
}
