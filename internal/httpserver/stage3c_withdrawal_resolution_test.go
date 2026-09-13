//go:build integration

// Stage 3C hardening: the withdrawal stranded-hold-recovery adversarial
// tests (directive item 3 and 8.G-8.I). Closes the gap Stage 3B left
// documented: a `submitted` withdrawal had no automated path forward,
// since the mock adapter's default behavior for a non-magic amount is
// OutcomePending (an entirely ordinary async-settlement simulation, not
// a mock deficiency - see MockProvider's own doc comment), and Stage 3B
// implemented no withdrawal callback path. newResolveWithdrawalHandler
// (withdrawal_handlers.go) and MockProvider.Resolve (already built in
// Stage 3B as a test-only knob for simulating delayed provider-side
// resolution) are what these tests exercise together.
package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// mustSubmitWithdrawal drives a withdrawal from `approved` to `submitted`
// via the real HTTP submit endpoint (not a direct package call), so
// these tests exercise the exact provider_id/provider_reference the
// running system would actually record.
func mustSubmitWithdrawal(t *testing.T, srv *httptest.Server, financeToken, requestID string) submitWithdrawalResponse {
	t.Helper()
	resp := postJSON(t, srv, "/v1/admin/withdrawals/"+requestID+"/submit", financeToken, map[string]string{"payment_method": "card"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("submit withdrawal: status %d: %+v", resp.StatusCode, apiErr)
	}
	var out submitWithdrawalResponse
	decodeBody(t, resp, &out)
	return out
}

// stage3CResolutionFixture bundles everything every test in this file
// needs: a funded player with an approved (submittable) withdrawal, and
// the mock adapter instance so the test can simulate the provider's own
// eventual outcome via Resolve.
type stage3CResolutionFixture struct {
	srv          *httptest.Server
	pool         *db.Pool
	mock         *payments.MockProvider
	tenant       identity.Tenant
	financeToken tokenPairResponse
	withdrawalID string
}

func setupStage3CResolutionFixture(t *testing.T, amount int64) stage3CResolutionFixture {
	t.Helper()
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	financeStaff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-1")
	financeToken := mustLoginStaff(t, srv, tenant.Slug, financeStaff.Email, "a-decent-password-1")

	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 1_000_000)
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletIDFor(t, pool, tenant.ID, player.ID, "EUR"), "EUR", amount)

	mustOpenReviewQueue(t, srv, financeToken.AccessToken)
	resp := postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/approve", financeToken.AccessToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve withdrawal: status %d", resp.StatusCode)
	}

	return stage3CResolutionFixture{srv: srv, pool: pool, mock: mock, tenant: tenant, financeToken: financeToken, withdrawalID: wr.ID.String()}
}

func mustResolve(t *testing.T, f stage3CResolutionFixture) submitWithdrawalResponse {
	t.Helper()
	resp := postJSON(t, f.srv, "/v1/admin/withdrawals/"+f.withdrawalID+"/resolve", f.financeToken.AccessToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("resolve withdrawal: status %d: %+v", resp.StatusCode, apiErr)
	}
	var out submitWithdrawalResponse
	decodeBody(t, resp, &out)
	return out
}

// TestWithdrawalResolve_DelayedSuccessAfterTimeout is adversarial test
// 8.H: the mock returns OutcomePending on submission (an ordinary async-
// settlement simulation), the provider later confirms success (simulated
// via MockProvider.Resolve, standing in for "the provider's backend
// eventually finds out what really happened" - see its own doc comment),
// and a status-query resolution completes the withdrawal - releasing the
// hold - without ever calling Withdraw a second time.
func TestWithdrawalResolve_DelayedSuccessAfterTimeout(t *testing.T) {
	f := setupStage3CResolutionFixture(t, 10000)
	submitted := mustSubmitWithdrawal(t, f.srv, f.financeToken.AccessToken, f.withdrawalID)
	if submitted.State != "submitted" {
		t.Fatalf("expected state submitted after submit, got %q", submitted.State)
	}

	// The provider's backend later confirms success - simulated exactly
	// as internal/payments' own tests simulate delayed resolution.
	f.mock.Resolve(submitted.ProviderReference, payments.OutcomeSucceeded, "", false)

	resolved := mustResolve(t, f)
	if resolved.State != "completed" {
		t.Fatalf("expected state completed after resolving a delayed success, got %q", resolved.State)
	}
}

// TestWithdrawalResolve_DelayedFailureAfterTimeout is adversarial test
// 8.I: the mirror image of 8.H - a delayed DECLINE must fail the
// withdrawal (releasing the hold back to the player), never leave it
// silently stuck or wrongly complete it.
func TestWithdrawalResolve_DelayedFailureAfterTimeout(t *testing.T) {
	f := setupStage3CResolutionFixture(t, 10000)
	submitted := mustSubmitWithdrawal(t, f.srv, f.financeToken.AccessToken, f.withdrawalID)

	f.mock.Resolve(submitted.ProviderReference, payments.OutcomeDeclined, "bank_account_closed", false)

	resolved := mustResolve(t, f)
	if resolved.State != "failed" {
		t.Fatalf("expected state failed after resolving a delayed decline, got %q", resolved.State)
	}
}

// TestWithdrawalResolve_DuplicateStatusResponsesIdempotent is adversarial
// test 8.G: the provider is queried twice for the same still-pending (and
// then resolved) withdrawal - as a real reconciliation sweep calling this
// endpoint on a schedule would naturally do - and the SECOND resolve call
// after the first already completed the request must be a safe no-op
// (Complete's own state check reports ErrStateConflict, mapped to 409),
// never a duplicate ledger posting.
func TestWithdrawalResolve_DuplicateStatusResponsesIdempotent(t *testing.T) {
	f := setupStage3CResolutionFixture(t, 10000)
	submitted := mustSubmitWithdrawal(t, f.srv, f.financeToken.AccessToken, f.withdrawalID)

	// First resolve attempt: still pending at the provider - must be a
	// safe no-op, request stays `submitted`.
	stillPending := mustResolve(t, f)
	if stillPending.State != "submitted" {
		t.Fatalf("expected state to remain submitted while the provider is still pending, got %q", stillPending.State)
	}

	f.mock.Resolve(submitted.ProviderReference, payments.OutcomeSucceeded, "", false)
	completed := mustResolve(t, f)
	if completed.State != "completed" {
		t.Fatalf("expected state completed, got %q", completed.State)
	}

	// A THIRD resolve call, after completion - the duplicate/late status
	// response this test is actually about - must not error the caller
	// into thinking something went wrong, and must not touch the ledger
	// again. The handler maps the resulting ErrStateConflict to 409.
	resp := postJSON(t, f.srv, "/v1/admin/withdrawals/"+f.withdrawalID+"/resolve", f.financeToken.AccessToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for resolving an already-completed withdrawal, got %d", resp.StatusCode)
	}

	if got := releaseLedgerTransactionCount(t, f.pool, f.tenant.ID, f.withdrawalID); got != 1 {
		t.Fatalf("expected exactly 1 release ledger transaction after two completion-adjacent resolve attempts, got %d", got)
	}
}

// releaseLedgerTransactionCount counts DISTINCT release_ledger_transaction_id
// values ever recorded for requestID - proving a duplicate resolve
// attempt never produced a second ledger posting, not merely that the
// request's CURRENT column value looks right (which a bug that posted
// twice but only kept the last id would still pass).
func releaseLedgerTransactionCount(t *testing.T, pool *db.Pool, tenantID uuid.UUID, requestID string) int {
	t.Helper()
	id, err := uuid.Parse(requestID)
	if err != nil {
		t.Fatalf("parse request id: %v", err)
	}
	var count int
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(DISTINCT id) FROM ledger_transactions WHERE correlation_id = $1 AND transaction_type IN ('withdrawal_completed', 'withdrawal_failed')`,
			id,
		).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count release ledger transactions: %v", err)
	}
	return count
}
