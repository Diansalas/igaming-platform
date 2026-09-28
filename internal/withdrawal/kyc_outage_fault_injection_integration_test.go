//go:build integration

// KYC-ENF-OUTAGE-1 fault-injection test (code review
// rv-prh-i3-code-review.md N1 / "Re-review (FH-7)"'s own probe, ADR 0096
// §7.6). Reproduces a GENUINE Postgres error inside
// kyc.EvaluateEnforcement's read (not a seeded verification status) by
// having a second connection hold `kyc_verifications` under
// `LOCK TABLE ... IN ACCESS EXCLUSIVE MODE` while the request transaction
// runs with a short `lock_timeout` - deterministic (the blocker's LOCK
// TABLE statement only returns once the lock is actually held, and the
// racer is only started after that point) and never dependent on wall-
// clock timing beyond Postgres's own `lock_timeout` GUC, which is exactly
// what is under test.
//
// Before this fix (N1), the read's Postgres error aborted the CALLER's
// transaction, so RequestWithdrawal's own attempt to record the
// resulting "unavailable" decision in that same (now-aborted) transaction
// failed with SQLSTATE 25P02 - a raw error, no *KYCDeniedError, no
// committed decision or audit row, and (through the HTTP handler) a
// non-retryable 500 instead of the retryable 503 ADR 0096 intends for an
// outage. This test proves the fixed behavior directly against
// RequestWithdrawal.
package withdrawal

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

// lockKYCVerificationsTable starts a blocker transaction on a SEPARATE
// pool connection that takes `LOCK TABLE kyc_verifications IN ACCESS
// EXCLUSIVE MODE` and holds it until the returned release func is
// called. Returning from this function is only possible once the lock is
// genuinely held (LOCK TABLE blocks the acquiring statement itself until
// granted) - there is no contention here (nothing else holds the lock
// yet), so no polling is needed for the "is it held" question; the
// determinism this test needs is "the racer never starts before the lock
// is held", which this function's own synchronous handoff guarantees.
func lockKYCVerificationsTable(t *testing.T, pool *db.Pool) (release func()) {
	t.Helper()
	ready := make(chan error, 1)
	proceed := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `LOCK TABLE kyc_verifications IN ACCESS EXCLUSIVE MODE`); err != nil {
				ready <- err
				return err
			}
			ready <- nil
			<-proceed
			return nil // COMMIT - a lock-only transaction has nothing to undo.
		})
	}()

	if err := <-ready; err != nil {
		t.Fatalf("blocker failed to acquire ACCESS EXCLUSIVE lock on kyc_verifications: %v", err)
	}

	var released bool
	return func() {
		if released {
			return
		}
		released = true
		close(proceed)
		if err := <-done; err != nil {
			t.Fatalf("blocker transaction failed: %v", err)
		}
	}
}

// TestRequestWithdrawal_KYCStoreOutage_FailsClosedWithOneUnavailableDecision
// is KYC-ENF-OUTAGE-1's own required fault-injection test.
func TestRequestWithdrawal_KYCStoreOutage_FailsClosedWithOneUnavailableDecision(t *testing.T) {
	pool := testPool(t)
	f := seedFixtureNoVerification(t, pool, 1000)

	release := lockKYCVerificationsTable(t, pool)
	defer release() // safety net; the happy path releases explicitly below.

	decisionsBefore := countRows(t, pool, f.tenantID,
		`SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2`,
		f.tenantID, f.playerAccountID)
	auditBefore := countRows(t, pool, f.tenantID,
		`SELECT count(*) FROM audit_log WHERE action = 'kyc.enforcement_denied'`)

	idemKey := "wd-kyc-outage-" + uuid.NewString()
	var kycDenied *KYCDeniedError
	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// A short lock_timeout on the REQUEST transaction, not the
		// blocker: this is what turns "wait forever behind the ACCESS
		// EXCLUSIVE lock" into "a genuine, prompt Postgres error", the
		// exact condition N1 names (a real DB-read failure, not a
		// seeded verification status).
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
			return err
		}
		_, reqErr := RequestWithdrawal(ctx, tx, RequestParams{
			TenantID: f.tenantID, BrandID: f.brandID,
			PlayerAccountID: f.playerAccountID, PersonID: f.personID,
			WalletID: f.walletID, AssetCode: "EUR", Amount: 500,
			IdempotencyKey: idemKey,
		})
		if errors.As(reqErr, &kycDenied) {
			return nil
		}
		return reqErr
	})
	release()

	if err != nil {
		t.Fatalf("expected the outer transaction to commit cleanly (the read failure must be contained to a savepoint), got: %v", err)
	}
	if kycDenied == nil {
		t.Fatalf("expected *KYCDeniedError with Outcome=unavailable, got nil (RequestWithdrawal did not fail closed)")
	}
	if kycDenied.Decision.Outcome != kyc.OutcomeUnavailable {
		t.Fatalf("expected Decision.Outcome=%q, got %+v", kyc.OutcomeUnavailable, kycDenied.Decision)
	}
	if kycDenied.Decision.Allowed {
		t.Fatalf("expected Decision.Allowed=false for an unavailable outcome, got %+v", kycDenied.Decision)
	}

	// Exactly one decision row, with outcome='unavailable'.
	decisionsAfter := countRows(t, pool, f.tenantID,
		`SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2`,
		f.tenantID, f.playerAccountID)
	if decisionsAfter != decisionsBefore+1 {
		t.Fatalf("expected exactly 1 new kyc_enforcement_decisions row, got %d new", decisionsAfter-decisionsBefore)
	}
	unavailableCount := countRows(t, pool, f.tenantID,
		`SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2 AND outcome = 'unavailable'`,
		f.tenantID, f.playerAccountID)
	if unavailableCount != 1 {
		t.Fatalf("expected exactly 1 kyc_enforcement_decisions row with outcome='unavailable', got %d", unavailableCount)
	}

	// Exactly one audit row.
	auditAfter := countRows(t, pool, f.tenantID,
		`SELECT count(*) FROM audit_log WHERE action = 'kyc.enforcement_denied'`)
	if auditAfter != auditBefore+1 {
		t.Fatalf("expected exactly 1 new audit_log row, got %d new", auditAfter-auditBefore)
	}

	// LF-20: no withdrawal_requests row, no ledger posting.
	reqCount := countRows(t, pool, f.tenantID,
		`SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1 AND idempotency_key = $2`,
		f.tenantID, idemKey)
	if reqCount != 0 {
		t.Fatalf("expected 0 withdrawal_requests rows after a KYC-unavailable outcome, got %d", reqCount)
	}
	ledgerCount := countRows(t, pool, f.tenantID,
		`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND correlation_id = $2`,
		f.tenantID, kycDenied.Params.CorrelationID)
	if ledgerCount != 0 {
		t.Fatalf("expected 0 ledger postings correlated to this attempt, got %d", ledgerCount)
	}
}
