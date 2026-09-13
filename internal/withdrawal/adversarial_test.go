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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_requests
				(id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key)
			 VALUES ($1, $2, $3, $4, $5, 'EUR', 100, 'forged-cross-tenant-key')`,
			uuid.New(), fB.tenantID, fB.brandID, fB.playerAccountID, fB.walletID,
		)
		return err
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

	approver := uuid.New()
	const threshold = int64(50_000) // amount (100,000) >= threshold: two humans required, so one approval leaves the request open to a second decision attempt.
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		approved, err := Approve(ctx, tx, wr.ID, approver, threshold, false, nil)
		if err != nil {
			return err
		}
		if approved {
			t.Fatal("expected a single above-threshold approval to be insufficient")
		}
		return nil
	})

	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Reject(ctx, tx, wr.ID, approver, threshold, "changed_my_mind")
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

	approver := uuid.New()
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		approved, err := Approve(ctx, tx, wr.ID, approver, 1_000_000, false, nil)
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

	// Decision 1: threshold below the request amount -> requiresTwoHumans
	// is true, and a single human approval must not be enough.
	approver1 := uuid.New()
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		approved, err := Approve(ctx, tx, wr.ID, approver1, 50_000, false, nil)
		if err != nil {
			return err
		}
		if approved {
			t.Fatal("expected the first above-threshold approval to be insufficient on its own")
		}
		return nil
	})

	// Decision 2: a DIFFERENT threshold, now above the request amount -
	// requiresTwoHumans is false for THIS call, so this second approval
	// alone satisfies readiness (the mechanism reads its own call's
	// threshold input, never a value memoized from decision 1).
	approver2 := uuid.New()
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		approved, err := Approve(ctx, tx, wr.ID, approver2, 200_000, false, nil)
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
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		approved, err := Approve(ctx, tx, wr.ID, uuid.New(), 1_000_000, false, nil)
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
