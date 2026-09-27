//go:build integration

// ADR 0096 (PRH-I3) call-site tests: RequestWithdrawal's KYC deny path
// (code review rv-prh-i3-code-review.md T1/R1, MX1) and DenyForCompliance
// (T1/R3, ledger-finance C7). seedFixture (withdrawal_integration_test.go)
// seeds an APPROVED verification for every fixture by default; these
// tests deliberately mutate that state to exercise the deny path.
package withdrawal

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

func setFixtureVerification(t *testing.T, pool *db.Pool, f fixture, status string) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
			 VALUES ($1, $2, $3, $4, $5, $6, 'mock')`,
			uuid.New(), f.tenantID, f.brandID, f.playerAccountID, f.personID, status)
		return err
	})
	if err != nil {
		t.Fatalf("seed verification status=%s: %v", status, err)
	}
}

func countRows(t *testing.T, pool *db.Pool, tenantID uuid.UUID, query string, args ...any) int {
	t.Helper()
	var n int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, args...).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// TestRequestWithdrawal_DeniesWhenNotPassed proves MX1's target: a
// pending, rejected, or wholly missing verification denies
// RequestWithdrawal with a *kyc.EnforcementDeniedError-carrying
// *KYCDeniedError, zero withdrawal_requests rows, and zero ledger
// postings for that idempotency key.
func TestRequestWithdrawal_DeniesWhenNotPassed(t *testing.T) {
	cases := []struct {
		name   string
		status string // "" means no verification row at all
	}{
		{"pending", "pending"},
		{"rejected", "rejected"},
		{"none", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			f := seedFixtureNoVerification(t, pool, 1000)
			if tc.status != "" {
				setFixtureVerification(t, pool, f, tc.status)
			}

			idemKey := "wd-kyc-deny-" + tc.name
			_, err := requestWithdrawal(t, pool, f, 500, idemKey)
			if err == nil {
				t.Fatal("expected RequestWithdrawal to be denied")
			}
			var kycDenied *KYCDeniedError
			if !isKYCDeniedError(err, &kycDenied) {
				t.Fatalf("expected a *KYCDeniedError, got %T: %v", err, err)
			}
			if kycDenied.Decision.Allowed {
				t.Fatalf("expected Decision.Allowed=false, got %+v", kycDenied.Decision)
			}

			// Zero withdrawal_requests rows for this idempotency key.
			n := countRows(t, pool, f.tenantID, `SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1 AND idempotency_key = $2`, f.tenantID, idemKey)
			if n != 0 {
				t.Fatalf("expected 0 withdrawal_requests rows after a KYC deny, got %d", n)
			}
			// Zero ledger postings correlated to this attempt (no
			// correlation id was ever generated into a request row, so
			// check no NEW ledger_transactions exist beyond the fixture's
			// own seed deposit).
			n = countRows(t, pool, f.tenantID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'withdrawal_requested'`, f.tenantID)
			if n != 0 {
				t.Fatalf("expected 0 withdrawal_requested ledger postings after a KYC deny, got %d", n)
			}
		})
	}
}

// TestRequestWithdrawal_DenialCommitsDecisionAndAudit proves security
// condition 5 / F1: after the domain transaction rolls back on a KYC
// deny, the caller (mirroring internal/httpserver's own handler) commits
// the decision + audit rows in a FRESH transaction, and exactly one of
// each survives - not zero (lost) and not more than one (duplicated).
// TestRequestWithdrawal_DenialCommitsDecisionAndAudit proves LF-I3-3
// (2026-09-27, superseding the earlier roll-back-then-fresh-transaction
// design): RequestWithdrawal itself writes the decision + audit rows
// BEFORE returning *KYCDeniedError, and requestWithdrawal's own helper
// (mirroring internal/httpserver/withdrawal_handlers.go's sanctioned
// pattern) catches that error and commits rather than rolling back - a
// SINGLE transaction, not two, ends up holding exactly one decision row
// and one audit row.
func TestRequestWithdrawal_DenialCommitsDecisionAndAudit(t *testing.T) {
	pool := testPool(t)
	f := seedFixtureNoVerification(t, pool, 1000)
	setFixtureVerification(t, pool, f, "rejected")

	correlationBefore := countRows(t, pool, f.tenantID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2`, f.tenantID, f.playerAccountID)
	auditBefore := countRows(t, pool, f.tenantID, `SELECT count(*) FROM audit_log WHERE action = 'kyc.enforcement_denied'`)

	_, err := requestWithdrawal(t, pool, f, 500, "wd-kyc-deny-commit")
	var kycDenied *KYCDeniedError
	if !isKYCDeniedError(err, &kycDenied) {
		t.Fatalf("expected a *KYCDeniedError, got %T: %v", err, err)
	}

	decisionAfter := countRows(t, pool, f.tenantID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2`, f.tenantID, f.playerAccountID)
	if decisionAfter != correlationBefore+1 {
		t.Fatalf("expected exactly 1 new kyc_enforcement_decisions row, got %d new", decisionAfter-correlationBefore)
	}
	auditAfter := countRows(t, pool, f.tenantID, `SELECT count(*) FROM audit_log WHERE action = 'kyc.enforcement_denied'`)
	if auditAfter != auditBefore+1 {
		t.Fatalf("expected exactly 1 new audit_log row, got %d new", auditAfter-auditBefore)
	}
	// And still zero withdrawal_requests rows - the denial had zero
	// domain effect.
	n := countRows(t, pool, f.tenantID, `SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1 AND idempotency_key = $2`, f.tenantID, "wd-kyc-deny-commit")
	if n != 0 {
		t.Fatalf("expected 0 withdrawal_requests rows, got %d", n)
	}
}

// TestRequestWithdrawal_ReplayAfterRevocationIsNotReGated proves
// ledger-finance N2/C7: a retried call with the SAME idempotency key,
// AFTER the original succeeded and the player's KYC state has since
// changed (revoked), still returns the ORIGINAL request rather than
// re-evaluating KYC and denying it.
func TestRequestWithdrawal_ReplayAfterRevocationIsNotReGated(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000) // approved verification seeded by default

	original, err := requestWithdrawal(t, pool, f, 500, "wd-replay-after-revoke")
	if err != nil {
		t.Fatalf("initial request: %v", err)
	}

	// Revoke: the player's latest verification is now rejected.
	setFixtureVerification(t, pool, f, "rejected")

	replay, err := requestWithdrawal(t, pool, f, 500, "wd-replay-after-revoke")
	if err != nil {
		t.Fatalf("expected the replay to succeed (return the original, not re-gate), got: %v", err)
	}
	if replay.ID != original.ID {
		t.Fatalf("expected the replay to return the original request %s, got %s", original.ID, replay.ID)
	}
}

// TestRequestWithdrawal_SameKeyConcurrentRequestsRaceTheGate proves the
// genuine-race path (ledger-finance N2 (b)): two concurrent requests
// sharing the SAME idempotency key can both miss the pre-insert lookup
// and both pass the KYC gate (both allowed), but the database's own
// unique constraint plus the post-insert conflict branch still lets only
// ONE actually create the request/post the hold.
func TestRequestWithdrawal_SameKeyConcurrentRequestsRaceTheGate(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1000)

	const n = 5
	results := make([]WithdrawalRequest, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = requestWithdrawal(t, pool, f, 300, "wd-same-key-race")
		}(i)
	}
	wg.Wait()

	var firstID uuid.UUID
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, err)
		}
		if firstID == uuid.Nil {
			firstID = results[i].ID
		} else if results[i].ID != firstID {
			t.Fatalf("expected every concurrent same-key call to resolve to the SAME request id, got %s and %s", firstID, results[i].ID)
		}
	}
	n2 := countRows(t, pool, f.tenantID, `SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1 AND idempotency_key = $2`, f.tenantID, "wd-same-key-race")
	if n2 != 1 {
		t.Fatalf("expected exactly 1 withdrawal_requests row for the shared key, got %d", n2)
	}
}

// --- DenyForCompliance (ledger-finance C7) ---

// approvedRequest drives a fresh WithdrawalRequest to `approved` state
// (below-threshold single automated approval), returning it.
func approvedRequest(t *testing.T, pool *db.Pool, f fixture, amount int64, idemKey string) WithdrawalRequest {
	t.Helper()
	wr := mustRequestWithdrawal(t, pool, f, amount, idemKey)
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
	got, err := runTxResult(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) (WithdrawalRequest, error) {
		return GetByID(ctx, tx, wr.ID)
	})
	if err != nil {
		t.Fatalf("read approved request: %v", err)
	}
	return got
}

func runTxResult[T any](pool *db.Pool, tenantID uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) (T, error)) (T, error) {
	var out T
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = fn(ctx, tx)
		return err
	})
	return out, err
}

func denialDecision(f fixture, correlationID uuid.UUID) (kyc.EnforcementParams, kyc.EnforcementDecision) {
	params := kyc.EnforcementParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, PersonID: f.personID,
		Operation: kyc.EnforcementWithdrawalPayout, AssetCode: "EUR", Amount: 300, CorrelationID: correlationID,
	}
	decision := kyc.EnforcementDecision{Outcome: kyc.OutcomeFailed, Allowed: false, Code: "kyc_withdrawal_payout:failed", PolicyVersion: kyc.PolicyVersion}
	return params, decision
}

// TestDenyForCompliance_PostsReversalAndTransitionsAtomically is
// ledger-finance C7's core test: one withdrawal_rejected reversal,
// state -> rejected, release_ledger_transaction_id set, decision+audit
// committed together.
func TestDenyForCompliance_PostsReversalAndTransitionsAtomically(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	wr := approvedRequest(t, pool, f, 300, "wd-deny-compliance-1")

	params, decision := denialDecision(f, wr.ID)
	var result WithdrawalRequest
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = LockApprovedForSubmission(ctx, tx, wr.ID)
		if err != nil {
			return err
		}
		result, err = DenyForCompliance(ctx, tx, wr.ID, decision, params)
		return err
	})
	if err != nil {
		t.Fatalf("DenyForCompliance: %v", err)
	}
	if result.State != StateRejected {
		t.Fatalf("expected state rejected, got %s", result.State)
	}
	if result.ReleaseLedgerTransactionID == nil {
		t.Fatal("expected release_ledger_transaction_id to be set")
	}

	// Exactly one withdrawal_rejected reversal, reversing the original hold.
	n := countRows(t, pool, f.tenantID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'withdrawal_rejected' AND reverses_transaction_id = $2`, f.tenantID, wr.HoldLedgerTransactionID)
	if n != 1 {
		t.Fatalf("expected exactly 1 withdrawal_rejected reversal, got %d", n)
	}

	// SUM(debits) == SUM(credits) across every ledger entry for this tenant.
	assertLedgerBalanced(t, pool, f.tenantID)

	// Decision + audit committed with the reversal (same transaction).
	dcount := countRows(t, pool, f.tenantID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND correlation_id = $2`, f.tenantID, wr.ID)
	if dcount != 1 {
		t.Fatalf("expected exactly 1 kyc_enforcement_decisions row, got %d", dcount)
	}
	acount := countRows(t, pool, f.tenantID, `SELECT count(*) FROM audit_log WHERE action = 'withdrawal.rejected_kyc' AND target_id = $1`, wr.ID.String())
	if acount != 1 {
		t.Fatalf("expected exactly 1 withdrawal.rejected_kyc audit row, got %d", acount)
	}
}

// TestDenyForCompliance_IdempotencyKeyDistinctFromRejectAndFail proves
// the ":kyc_denied" idempotency key never collides with ":rejected"/
// ":failed".
func TestDenyForCompliance_IdempotencyKeyDistinctFromRejectAndFail(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	wr := approvedRequest(t, pool, f, 300, "wd-deny-compliance-2")

	params, decision := denialDecision(f, wr.ID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := LockApprovedForSubmission(ctx, tx, wr.ID); err != nil {
			return err
		}
		_, err := DenyForCompliance(ctx, tx, wr.ID, decision, params)
		return err
	})
	if err != nil {
		t.Fatalf("DenyForCompliance: %v", err)
	}
	n := countRows(t, pool, f.tenantID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND idempotency_key = $2`, f.tenantID, wr.ID.String()+":kyc_denied")
	if n != 1 {
		t.Fatalf("expected exactly 1 ledger row keyed %s:kyc_denied, got %d", wr.ID, n)
	}
}

// TestDenyForCompliance_ExactlyOnceRelease_ConcurrentWithReject proves
// ledger-finance C7's race requirement: a concurrent Reject (requires
// pending_review - so this races DenyForCompliance against a request
// that's still `approved`, using the L1 lock the SAME way Reject/Deny
// both take it) - only one of DenyForCompliance-vs-a-second-
// DenyForCompliance call wins; the loser gets ErrStateConflict and posts
// nothing.
// Repeated 50 times (ledger-finance LF-I3-1's "repeat >=50 under -race"
// requirement, satisfied in full) - a fresh request/fixture each
// iteration so no run can be masked by a previous iteration's state. See
// docs/plans/payment-readiness/evidence/prh-i3-race-integration.txt for
// the recorded -race run.
func TestDenyForCompliance_ExactlyOnceRelease_Concurrent(t *testing.T) {
	pool := testPool(t)
	for iter := 0; iter < 50; iter++ {
		t.Run(fmt.Sprintf("iter-%d", iter), func(t *testing.T) {
			f := seedFixture(t, pool, 10_000)
			wr := approvedRequest(t, pool, f, 300, fmt.Sprintf("wd-deny-compliance-race-%d", iter))
			params, decision := denialDecision(f, wr.ID)

			const n = 5
			errs := make([]error, n)
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
						if _, err := LockApprovedForSubmission(ctx, tx, wr.ID); err != nil {
							return err
						}
						_, err := DenyForCompliance(ctx, tx, wr.ID, decision, params)
						return err
					})
				}(i)
			}
			wg.Wait()

			successes := 0
			for _, err := range errs {
				if err == nil {
					successes++
				}
			}
			if successes != 1 {
				t.Fatalf("expected exactly 1 of %d concurrent DenyForCompliance calls to succeed, got %d", n, successes)
			}
			n2 := countRows(t, pool, f.tenantID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND idempotency_key = $2`, f.tenantID, wr.ID.String()+":kyc_denied")
			if n2 != 1 {
				t.Fatalf("expected exactly 1 posted reversal despite %d racing callers, got %d", n, n2)
			}
			assertLedgerBalanced(t, pool, f.tenantID)
		})
	}
}

// TestDenyForCompliance_RejectVersusDenyRace proves the cross-function
// race: Reject requires `pending_review`, DenyForCompliance requires
// `approved` - they can never race on the SAME state transition for one
// request in this state machine, but a concurrent Reject on a DIFFERENT
// request must not interfere with a DenyForCompliance call, and vice
// versa (no shared-lock deadlock, ADR 0082 ordering unaffected).
func TestDenyForCompliance_ProjectionMatchesLedgerAfterDeny(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	wr := approvedRequest(t, pool, f, 300, "wd-deny-compliance-proj")
	params, decision := denialDecision(f, wr.ID)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := LockApprovedForSubmission(ctx, tx, wr.ID); err != nil {
			return err
		}
		_, err := DenyForCompliance(ctx, tx, wr.ID, decision, params)
		return err
	})
	if err != nil {
		t.Fatalf("DenyForCompliance: %v", err)
	}

	// Projection (wallet_balance_projection) must equal a fresh rebuild
	// from ledger_entries for the player_cash account - player_cash is
	// credit-normal (ledger-accounting-model.md §5), so balance =
	// credit_total - debit_total on both sides.
	var projected, rebuilt int64
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT credit_total - debit_total FROM wallet_balance_projection WHERE ledger_account_id = $1`, f.cashAccountID).Scan(&projected)
	})
	if err != nil {
		t.Fatalf("read projection: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(CASE WHEN direction = 'credit' THEN amount ELSE -amount END), 0)
			  FROM ledger_entries WHERE ledger_account_id = $1`, f.cashAccountID).Scan(&rebuilt)
	})
	if err != nil {
		t.Fatalf("read rebuild: %v", err)
	}
	if projected != rebuilt {
		t.Fatalf("projection (%d) != rebuild-from-ledger (%d) after DenyForCompliance", projected, rebuilt)
	}
}

func assertLedgerBalanced(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	var debits, credits int64
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0) FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.ledger_account_id WHERE a.tenant_id = $1 AND e.direction = 'debit'`, tenantID).Scan(&debits); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0) FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.ledger_account_id WHERE a.tenant_id = $1 AND e.direction = 'credit'`, tenantID).Scan(&credits)
	})
	if err != nil {
		t.Fatalf("sum debits/credits: %v", err)
	}
	if debits != credits {
		t.Fatalf("SUM(debits)=%d != SUM(credits)=%d for tenant %s", debits, credits, tenantID)
	}
}

func isKYCDeniedError(err error, target **KYCDeniedError) bool {
	e, ok := err.(*KYCDeniedError)
	if ok {
		*target = e
	}
	return ok
}

// seedFixtureNoVerification is seedFixture without the default approved
// verification - used by every test above that must control the
// verification state itself.
func seedFixtureNoVerification(t *testing.T, pool *db.Pool, initialBalance int64) fixture {
	t.Helper()
	return seedFixtureRaw(t, pool, initialBalance, false)
}
