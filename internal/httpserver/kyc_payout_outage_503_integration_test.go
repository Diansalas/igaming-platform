//go:build integration

// PRH-2 F-pay, PAY-KYC-UNAVAIL-1 (security C-F1) end to end: a genuine KYC
// store outage during the staff "submit payout" call returns the retryable
// 503 (not the non-retryable 500 it returned before), the withdrawal stays
// `approved`, exactly one `unavailable` decision row and one denied
// `withdrawal.submit.http` audit row commit, and there is no attempt row and
// no ledger posting. Fault injection: LOCK TABLE + a lock_timeout session
// default on the HANDLER's pool only (see kyc_outage_503_integration_test.go).
package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/identity"
)

func TestSubmitWithdrawalHandler_KYCStoreOutageReturns503(t *testing.T) {
	pool, issuer := testEnv(t)
	lockTimeoutPool := testEnvWithLockTimeout(t, "300ms")
	orchestrator, mock := newMockOrchestrator()
	// Setup server on the ordinary pool; the lock_timeout pool serves only
	// the submit call under test.
	setupSrv := newFinancialTestServer(t, pool, issuer, orchestrator)
	outageSrv := newFinancialTestServer(t, lockTimeoutPool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)
	player := mustRegisterPlayer(t, setupSrv, brand.Slug)
	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-1")
	financeToken := mustLoginStaff(t, setupSrv, tenant.Slug, finance.Email, "a-decent-password-1")
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 1_000_000)
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletIDFor(t, pool, tenant.ID, player.ID, "EUR"), "EUR", 5000)
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, effective_from)
			 VALUES ($1, 'EUR', 1000000, 2, now() - interval '1 hour')`, tenant.ID)
		return err
	}); err != nil {
		t.Fatalf("configure withdrawal policy: %v", err)
	}
	mustOpenReviewQueue(t, setupSrv, financeToken.AccessToken)
	resp := postJSON(t, setupSrv, "/v1/admin/withdrawals/"+wr.ID.String()+"/approve", financeToken.AccessToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve withdrawal: status %d", resp.StatusCode)
	}

	ledgerBefore := countRows(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID)
	decisionsBefore := countRows(t, pool, tenant.ID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND operation = 'withdrawal_payout'`, tenant.ID)

	release := lockKYCVerificationsTable(t, pool)
	defer release()
	resp = postJSON(t, outageSrv, "/v1/admin/withdrawals/"+wr.ID.String()+"/submit", financeToken.AccessToken, map[string]string{"payment_method": "bank_transfer"})
	resp.Body.Close()
	release()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for a KYC-store outage during payout submit, got %d", resp.StatusCode)
	}
	if st := countRows(t, pool, tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE id = $1 AND state = 'approved'`, wr.ID); st != 1 {
		t.Fatalf("the request must stay approved")
	}
	if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND operation = 'withdrawal_payout' AND outcome = 'unavailable'`, tenant.ID); n != 1 {
		t.Fatalf("expected exactly 1 unavailable payout decision row, got %d", n)
	}
	if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND operation = 'withdrawal_payout'`, tenant.ID); n != decisionsBefore+1 {
		t.Fatalf("expected exactly one new payout decision row, before=%d after=%d", decisionsBefore, n)
	}
	if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'withdrawal.submit.http' AND target_id = $2 AND outcome = 'denied'`, tenant.ID, wr.ID.String()); n != 1 {
		t.Fatalf("expected exactly 1 denied submit audit row, got %d", n)
	}
	if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id = $1`, wr.ID); n != 0 {
		t.Fatalf("expected no attempt row, got %d", n)
	}
	if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID); n != ledgerBefore {
		t.Fatalf("expected no ledger posting, %d -> %d", ledgerBefore, n)
	}

	// Retry after the outage on the healthy server succeeds.
	mustSubmitWithdrawal(t, setupSrv, financeToken.AccessToken, wr.ID.String())
}
