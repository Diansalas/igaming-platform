//go:build integration

// Stage 9 §11 - withdrawal / four-eyes RE-AUDIT (ledger-finance). This is
// a re-test, not a re-read: the existing Stage 3B/3C/3D suites already
// cover cross-tenant RLS (TestCrossTenant_WithdrawalRequestsRLSBlocksRead-
// AndWrite), request idempotency (TestRequestWithdrawal_IsIdempotentOn-
// Retry), concurrent requests racing one balance (TestRequestWithdrawal_
// ConcurrentRequestsOnlyOneSucceeds), concurrent DISTINCT approvers
// (TestApprove_ConcurrentDistinctApproversOnlyRequiredCountSatisfies-
// Readiness), concurrent submission (TestLockApprovedForSubmission_
// ConcurrentSubmitsOnlyOneReachesProvider), beneficiary self-approval
// (TestApprove_BeneficiaryCannotApproveOwnWithdrawal), the governance
// trigger's direct-SQL bypass attempts, and every hold/resolution flow
// (Reject/Fail/Cancel/Complete restore or consume the hold).
//
// Three things that suite did NOT test are covered here, all of them
// four-eyes bypass vectors rather than accounting bugs:
//
//  1. Duplicate APPROVE by one approver. Only the REJECT side of the
//     unique (withdrawal_request_id, approver_principal_id) constraint
//     was tested (TestReject_DuplicateDecisionBySameApproverRejected).
//  2. The same approver double-submitting CONCURRENTLY - the realistic
//     shape of the attack (a double-clicked button, a retried request),
//     where a "check then insert" would let both sides observe "no prior
//     decision" and one human alone satisfy a 2-approval threshold.
//  3. ONE human holding TWO staff logins linked to the SAME person,
//     approving with both. Approve's COUNT(DISTINCT COALESCE(
//     su.person_id, wa.approver_principal_id)) exists specifically to
//     defeat this (its doc comment records it as a Stage 3C code-review
//     P1), and no test in this package exercised it - sequentially or
//     concurrently. It is the only one of the three the unique
//     constraint does NOT catch, because the two approvals are genuinely
//     distinct rows by distinct principals.
package withdrawal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// s9RequestPendingReview creates a withdrawal for amount and moves it to
// pending_review, the only state Approve accepts.
func s9RequestPendingReview(t *testing.T, pool *db.Pool, f fixture, amount int64, idemKey string) WithdrawalRequest {
	t.Helper()
	wr := mustRequestWithdrawal(t, pool, f, amount, idemKey)
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MoveToPendingReview(ctx, tx, wr.ID)
	})
	return wr
}

// s9CreateApproversSharingOnePerson inserts n distinct, active, linked
// staff_users rows that all resolve to the SAME persons row - one human
// with n logins. Each id individually satisfies migration 0034's
// governance trigger (real staff row, non-NULL person_id, status
// 'active'), so nothing below the Go layer will refuse either decision;
// only Approve's person-level COUNT(DISTINCT ...) can tell they are the
// same human.
func s9CreateApproversSharingOnePerson(t *testing.T, pool *db.Pool, tenantID uuid.UUID, n int) []uuid.UUID {
	t.Helper()
	personID := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed shared person: %v", err)
	}
	ids := make([]uuid.UUID, n)
	for i := range ids {
		staffID := uuid.New()
		err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
				 VALUES ($1, $2, $3, 'x', 'finance', 'active', $4)`,
				staffID, tenantID, staffID.String()+"@example.com", personID)
			return err
		})
		if err != nil {
			t.Fatalf("seed staff login %d for shared person: %v", i, err)
		}
		ids[i] = staffID
	}
	return ids
}

// s9State reads a request's current state straight from the row.
func s9State(t *testing.T, pool *db.Pool, f fixture, requestID uuid.UUID) State {
	t.Helper()
	var wr WithdrawalRequest
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wr, err = GetByID(ctx, tx, requestID)
		return err
	})
	return wr.State
}

// s9ApprovalRowCount counts recorded approve decisions for a request.
func s9ApprovalRowCount(t *testing.T, pool *db.Pool, f fixture, requestID uuid.UUID) int {
	t.Helper()
	var count int
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM withdrawal_approvals WHERE withdrawal_request_id = $1 AND decision = 'approve'`,
			requestID).Scan(&count)
	})
	return count
}

// s9AssertLedgerBalanced re-asserts invariant #1 over this tenant's own
// entries. Every test here also asserts it, because an approval path that
// mistakenly posted or reposted a hold would show up here and nowhere
// else in the four-eyes assertions.
func s9AssertLedgerBalanced(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	var debits, credits int64
	mustRunTx(t, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT
				COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0),
				COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)
			 FROM ledger_entries WHERE tenant_id = $1`, tenantID).Scan(&debits, &credits)
	})
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}

// --- 1. Duplicate approval by one approver (sequential) -------------------

// TestStage9_DuplicateApproveBySameApprover_RejectedAndNeverCounted is the
// approve-side counterpart of the already-covered
// TestReject_DuplicateDecisionBySameApproverRejected. A retried/replayed
// approval action is NOT silently idempotent here and must not be: it is
// refused with ErrDuplicateApproval so the caller learns its second
// decision did not count, and critically the refusal must leave the
// request still short of its threshold rather than approved.
func TestStage9_DuplicateApproveBySameApprover_RejectedAndNeverCounted(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1_000_000)
	// Threshold below the amount => 2 distinct human approvals required.
	mustSetWithdrawalPolicy(t, pool, f.tenantID, "EUR", 50_000, 2, time.Now().Add(-time.Hour))
	wr := s9RequestPendingReview(t, pool, f, 100_000, "s9-dup-approve")
	approver := mustCreateApprover(t, pool, f.tenantID)

	var firstApproved bool
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		firstApproved, err = Approve(ctx, tx, wr.ID, approver, false, nil, alwaysEligible)
		return err
	})
	if firstApproved {
		t.Fatal("one approval must not satisfy a 2-approval threshold")
	}

	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Approve(ctx, tx, wr.ID, approver, false, nil, alwaysEligible)
		return err
	})
	if !errors.Is(err, ErrDuplicateApproval) {
		t.Fatalf("expected ErrDuplicateApproval on a replayed approval, got %v", err)
	}

	if got := s9ApprovalRowCount(t, pool, f, wr.ID); got != 1 {
		t.Fatalf("expected exactly 1 recorded approval after a duplicate attempt, got %d", got)
	}
	if got := s9State(t, pool, f, wr.ID); got != StatePendingReview {
		t.Fatalf("one approver approving twice must never approve the request; state is %q", got)
	}
	s9AssertLedgerBalanced(t, pool, f.tenantID)
}

// --- 2. Duplicate approval by one approver (CONCURRENT) -------------------

// TestStage9_ConcurrentSameApproverDoubleSubmit_RecordsExactlyOneApproval
// is the realistic shape of case 1: one staff member's client submits the
// same approval twice at the same instant (double-click, retry after a
// timeout). Both goroutines enter Approve before either has committed, so
// both read the same "zero approvals so far" state - only the database's
// UNIQUE (withdrawal_request_id, approver_principal_id) constraint, not
// any read this code performs, can stop the second from being recorded.
//
// Both goroutines also contend on lockRequestForUpdate's row lock, so one
// may instead observe the other's committed effect; either serialization
// is legal. What is never legal is TWO recorded approvals, or the request
// reaching `approved` on one human's say-so.
func TestStage9_ConcurrentSameApproverDoubleSubmit_RecordsExactlyOneApproval(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1_000_000)
	mustSetWithdrawalPolicy(t, pool, f.tenantID, "EUR", 50_000, 2, time.Now().Add(-time.Hour))
	wr := s9RequestPendingReview(t, pool, f, 100_000, "s9-concurrent-dup-approve")
	approver := mustCreateApprover(t, pool, f.tenantID)

	const n = 4
	results := make([]bool, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				approved, err := Approve(ctx, tx, wr.ID, approver, false, nil, alwaysEligible)
				results[i] = approved
				return err
			})
		}(i)
	}
	close(start)
	wg.Wait()

	var recorded, duplicates int
	for i, err := range errs {
		switch {
		case err == nil:
			recorded++
			if results[i] {
				t.Fatalf("goroutine %d: a single human's approval satisfied a 2-approval threshold", i)
			}
		case errors.Is(err, ErrDuplicateApproval):
			duplicates++
		default:
			t.Fatalf("goroutine %d: expected nil or ErrDuplicateApproval, got %v", i, err)
		}
	}
	if recorded != 1 || duplicates != n-1 {
		t.Fatalf("expected exactly 1 of %d simultaneous identical approvals to be recorded "+
			"(got recorded=%d duplicates=%d)", n, recorded, duplicates)
	}
	if got := s9ApprovalRowCount(t, pool, f, wr.ID); got != 1 {
		t.Fatalf("expected exactly 1 withdrawal_approvals row, got %d", got)
	}
	if got := s9State(t, pool, f, wr.ID); got != StatePendingReview {
		t.Fatalf("expected the request to remain %q after one human's concurrent double-submit, got %q", StatePendingReview, got)
	}
	s9AssertLedgerBalanced(t, pool, f.tenantID)
}

// --- 3. One human, two staff logins -> four-eyes must still not be met ---

// TestStage9_TwoStaffLoginsOnePerson_CannotSatisfyFourEyesAlone is the
// bypass the unique constraint cannot catch: two DISTINCT
// approver_principal_ids, two legitimately-inserted rows, one human. Only
// Approve's person-level COUNT(DISTINCT COALESCE(su.person_id,
// wa.approver_principal_id)) collapses them, and until now nothing tested
// it. Sequential form first, so a failure is unambiguous.
func TestStage9_TwoStaffLoginsOnePerson_CannotSatisfyFourEyesAlone(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1_000_000)
	mustSetWithdrawalPolicy(t, pool, f.tenantID, "EUR", 50_000, 2, time.Now().Add(-time.Hour))
	wr := s9RequestPendingReview(t, pool, f, 100_000, "s9-two-logins-one-person")
	logins := s9CreateApproversSharingOnePerson(t, pool, f.tenantID, 2)

	for i, login := range logins {
		var approved bool
		mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			approved, err = Approve(ctx, tx, wr.ID, login, false, nil, alwaysEligible)
			return err
		})
		if approved {
			t.Fatalf("login %d: one human using two staff logins satisfied a 2-APPROVER threshold - four-eyes bypassed", i)
		}
	}

	if got := s9ApprovalRowCount(t, pool, f, wr.ID); got != 2 {
		t.Fatalf("expected both decisions to be RECORDED (they are legitimate rows; only the COUNT must collapse them), got %d", got)
	}
	if got := s9State(t, pool, f, wr.ID); got != StatePendingReview {
		t.Fatalf("expected the request to remain %q, got %q", StatePendingReview, got)
	}

	// A genuinely different human then tips it over - proving the refusal
	// above was person-collapsing and not some unrelated blanket failure.
	third := mustCreateApprover(t, pool, f.tenantID)
	var approved bool
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		approved, err = Approve(ctx, tx, wr.ID, third, false, nil, alwaysEligible)
		return err
	})
	if !approved {
		t.Fatal("a second genuinely distinct person's approval must satisfy the 2-approver threshold")
	}
	if got := s9State(t, pool, f, wr.ID); got != StateApproved {
		t.Fatalf("expected %q, got %q", StateApproved, got)
	}
	s9AssertLedgerBalanced(t, pool, f.tenantID)
}

// TestStage9_ConcurrentTwoStaffLoginsOnePerson_CannotSatisfyFourEyesAlone
// races the same bypass. The person-level count is a read inside each
// transaction, so if the two decisions committed without ever serializing
// against each other, each could count only its own row, see 1 < 2, and
// both return false - safe - OR, worse, each could see 2 rows without
// collapsing them. lockRequestForUpdate is what forces them into a serial
// order; this pins that down rather than assuming it.
func TestStage9_ConcurrentTwoStaffLoginsOnePerson_CannotSatisfyFourEyesAlone(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1_000_000)
	mustSetWithdrawalPolicy(t, pool, f.tenantID, "EUR", 50_000, 2, time.Now().Add(-time.Hour))
	wr := s9RequestPendingReview(t, pool, f, 100_000, "s9-concurrent-two-logins")
	logins := s9CreateApproversSharingOnePerson(t, pool, f.tenantID, 2)

	results := make([]bool, len(logins))
	errs := make([]error, len(logins))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range logins {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				approved, err := Approve(ctx, tx, wr.ID, logins[i], false, nil, alwaysEligible)
				results[i] = approved
				return err
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("login %d: both decisions are individually legitimate and must record cleanly, got %v", i, err)
		}
		if results[i] {
			t.Fatalf("login %d: one human using two staff logins approved a withdrawal under a 2-APPROVER threshold "+
				"- four-eyes bypassed under concurrency", i)
		}
	}
	if got := s9State(t, pool, f, wr.ID); got != StatePendingReview {
		t.Fatalf("expected the request to remain %q, got %q", StatePendingReview, got)
	}
	if got := s9ApprovalRowCount(t, pool, f, wr.ID); got != 2 {
		t.Fatalf("expected 2 recorded (but person-collapsed) approvals, got %d", got)
	}
	s9AssertLedgerBalanced(t, pool, f.tenantID)
}
