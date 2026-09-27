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

	// The zero-config policy fallback fails closed (threshold 0, always
	// requiring 2 approvals - internal/withdrawal/policy.go) until a
	// tenant configures a real threshold; these tests are about
	// resolution, not approval-count policy, so configure one high
	// enough for a single finance approval to suffice.
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, effective_from)
			 VALUES ($1, 'EUR', 1000000, 2, now() - interval '1 hour')`,
			tenant.ID,
		)
		return err
	}); err != nil {
		t.Fatalf("configure withdrawal policy: %v", err)
	}

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

// TestWithdrawalResolve_UnlinkedStaffAccountRejected kills SM7
// (rv-prh-i1-payout-security.md/registry PAY-SEC-TESTS-1): submit already
// has TestWithdrawalSubmit_UnlinkedStaffAccountRejected pinning the same
// approverEligibilityCheck call, but /resolve had no equivalent test - the
// mutation "remove the linked-staff check from /resolve" passed the whole
// httpserver suite. A staff account with no confirmed Person linkage must
// get 403 on /resolve, exactly like submit, and must cause no state
// change or effect.
func TestWithdrawalResolve_UnlinkedStaffAccountRejected(t *testing.T) {
	f := setupStage3CResolutionFixture(t, 10000)
	submitted := mustSubmitWithdrawal(t, f.srv, f.financeToken.AccessToken, f.withdrawalID)
	if submitted.State != "submitted" {
		t.Fatalf("expected state submitted after submit, got %q", submitted.State)
	}

	unlinked := mustCreateUnlinkedStaff(t, f.pool, f.tenant.ID, identity.StaffRoleFinance, "a-decent-password-3")
	unlinkedToken := mustLoginStaff(t, f.srv, f.tenant.Slug, unlinked.Email, "a-decent-password-3")

	resp := postJSON(t, f.srv, "/v1/admin/withdrawals/"+f.withdrawalID+"/resolve", unlinkedToken.AccessToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 403 for an unlinked staff account resolving, got %d: %+v", resp.StatusCode, apiErr)
	}

	// No effect: the withdrawal must still be exactly `submitted`, resolvable
	// by a genuinely eligible actor afterward (proving the refused attempt
	// left no partial state behind).
	f.mock.Resolve(submitted.ProviderReference, payments.OutcomeSucceeded, "", false)
	resolved := mustResolve(t, f)
	if resolved.State != "completed" {
		t.Fatalf("expected the withdrawal to still resolve normally afterward (no residual effect from the refused attempt), got %q", resolved.State)
	}
}

// TestWithdrawalResolve_SuspendedStaffAccountRejected is SM7's other half:
// a linked, otherwise-eligible finance staff account whose status is
// changed to suspended AFTER its token was issued must still be refused on
// /resolve, exactly like TestWithdrawalApprove_InactiveStaffAccountRejected
// proves for approve.
func TestWithdrawalResolve_SuspendedStaffAccountRejected(t *testing.T) {
	f := setupStage3CResolutionFixture(t, 10000)
	submitted := mustSubmitWithdrawal(t, f.srv, f.financeToken.AccessToken, f.withdrawalID)
	if submitted.State != "submitted" {
		t.Fatalf("expected state submitted after submit, got %q", submitted.State)
	}

	resolver := mustCreateStaff(t, f.pool, f.tenant.ID, identity.StaffRoleFinance, "a-decent-password-4")
	resolverToken := mustLoginStaff(t, f.srv, f.tenant.Slug, resolver.Email, "a-decent-password-4")
	if err := f.pool.WithTenant(context.Background(), f.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, resolver.ID)
		return err
	}); err != nil {
		t.Fatalf("suspend staff account: %v", err)
	}

	resp := postJSON(t, f.srv, "/v1/admin/withdrawals/"+f.withdrawalID+"/resolve", resolverToken.AccessToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 403 for a suspended staff account resolving with an already-issued token, got %d: %+v", resp.StatusCode, apiErr)
	}
}

// --- Permanent cross-tenant tests for submit and /resolve (PAY-SEC-TESTS-1,
// probe HP1) -----------------------------------------------------------------

// TestWithdrawalSubmit_CrossTenantDenied is HP1's submit half, made
// permanent: tenant B's approved withdrawal must be completely invisible
// to tenant A's finance staff - 404, zero effect, never a leaked 403/409
// that would confirm the id exists in another tenant.
func TestWithdrawalSubmit_CrossTenantDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	brandB := mustCreateBrand(t, pool, tenantB)
	mustRegisterCapability(t, pool, tenantB.ID, mock)

	financeA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleFinance, "finance-a-submit-pw-1")
	financeATokens := mustLoginStaff(t, srv, tenantA.Slug, financeA.Email, "finance-a-submit-pw-1")
	financeB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleFinance, "finance-b-submit-pw-1")
	financeBTokens := mustLoginStaff(t, srv, tenantB.Slug, financeB.Email, "finance-b-submit-pw-1")

	var playerAccountB uuid.UUID
	if err := pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := identity.RegisterPlayer(ctx, tx, brandB, "cross-tenant-submit-wd@example.com", "hash")
		playerAccountB = account.ID
		return err
	}); err != nil {
		t.Fatalf("seed player B: %v", err)
	}
	walletB := fundWallet(t, pool, tenantB.ID, brandB.ID, playerAccountB, "EUR", 100000)
	wrB := mustCreateWithdrawalRequest(t, pool, tenantB.ID, brandB.ID, playerAccountB, walletB.ID, "EUR", 5000)

	if err := pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, effective_from)
			 VALUES ($1, 'EUR', 1000000, 1, now() - interval '1 hour')`,
			tenantB.ID,
		)
		return err
	}); err != nil {
		t.Fatalf("configure withdrawal policy (tenant B): %v", err)
	}

	mustOpenReviewQueue(t, srv, financeBTokens.AccessToken)
	resp := postJSON(t, srv, "/v1/admin/withdrawals/"+wrB.ID.String()+"/approve", financeBTokens.AccessToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve withdrawal (tenant B): status %d", resp.StatusCode)
	}

	auditBefore := countRows(t, pool, tenantB.ID, `SELECT count(*) FROM audit_log WHERE action LIKE 'withdrawal.submit%' AND target_id = $1`, wrB.ID.String())

	resp = postJSON(t, srv, "/v1/admin/withdrawals/"+wrB.ID.String()+"/submit", financeATokens.AccessToken, map[string]string{"payment_method": "card"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 404 for tenant A's finance staff submitting tenant B's withdrawal, got %d: %+v", resp.StatusCode, apiErr)
	}

	auditAfter := countRows(t, pool, tenantB.ID, `SELECT count(*) FROM audit_log WHERE action LIKE 'withdrawal.submit%' AND target_id = $1`, wrB.ID.String())
	if auditAfter != auditBefore {
		t.Fatalf("expected no new submit audit row from the cross-tenant attempt, before=%d after=%d", auditBefore, auditAfter)
	}
	if got := countRows(t, pool, tenantB.ID, `SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id = $1`, wrB.ID); got != 0 {
		t.Fatalf("expected zero payment attempts after a cross-tenant submit attempt, got %d", got)
	}

	// The withdrawal must still submit normally afterward for the RIGHT
	// tenant's own staff - proving the refused cross-tenant attempt left
	// no state change or lock behind.
	resp = postJSON(t, srv, "/v1/admin/withdrawals/"+wrB.ID.String()+"/submit", financeBTokens.AccessToken, map[string]string{"payment_method": "card"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected tenant B's own finance staff to still submit successfully afterward, got %d: %+v", resp.StatusCode, apiErr)
	}
}

// TestWithdrawalResolve_CrossTenantDenied is HP1's /resolve half, made
// permanent: tenant B's submitted withdrawal must be completely invisible
// to tenant A's finance staff on /resolve too - 404, zero audit rows, no
// state change.
func TestWithdrawalResolve_CrossTenantDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	brandB := mustCreateBrand(t, pool, tenantB)
	mustRegisterCapability(t, pool, tenantB.ID, mock)

	financeA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleFinance, "finance-a-resolve-pw-1")
	financeATokens := mustLoginStaff(t, srv, tenantA.Slug, financeA.Email, "finance-a-resolve-pw-1")
	financeB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleFinance, "finance-b-resolve-pw-1")
	financeBTokens := mustLoginStaff(t, srv, tenantB.Slug, financeB.Email, "finance-b-resolve-pw-1")

	var playerAccountB uuid.UUID
	if err := pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := identity.RegisterPlayer(ctx, tx, brandB, "cross-tenant-resolve-wd@example.com", "hash")
		playerAccountB = account.ID
		return err
	}); err != nil {
		t.Fatalf("seed player B: %v", err)
	}
	walletB := fundWallet(t, pool, tenantB.ID, brandB.ID, playerAccountB, "EUR", 100000)
	wrB := mustCreateWithdrawalRequest(t, pool, tenantB.ID, brandB.ID, playerAccountB, walletB.ID, "EUR", 5000)

	if err := pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, effective_from)
			 VALUES ($1, 'EUR', 1000000, 1, now() - interval '1 hour')`,
			tenantB.ID,
		)
		return err
	}); err != nil {
		t.Fatalf("configure withdrawal policy (tenant B): %v", err)
	}

	mustOpenReviewQueue(t, srv, financeBTokens.AccessToken)
	resp := postJSON(t, srv, "/v1/admin/withdrawals/"+wrB.ID.String()+"/approve", financeBTokens.AccessToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve withdrawal (tenant B): status %d", resp.StatusCode)
	}
	submitted := mustSubmitWithdrawal(t, srv, financeBTokens.AccessToken, wrB.ID.String())
	if submitted.State != "submitted" {
		t.Fatalf("expected state submitted after submit, got %q", submitted.State)
	}

	auditBefore := countRows(t, pool, tenantB.ID, `SELECT count(*) FROM audit_log WHERE action = 'withdrawal.resolve_attempted.http' AND target_id = $1`, wrB.ID.String())

	resp = postJSON(t, srv, "/v1/admin/withdrawals/"+wrB.ID.String()+"/resolve", financeATokens.AccessToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 404 for tenant A's finance staff resolving tenant B's withdrawal, got %d: %+v", resp.StatusCode, apiErr)
	}

	auditAfter := countRows(t, pool, tenantB.ID, `SELECT count(*) FROM audit_log WHERE action = 'withdrawal.resolve_attempted.http' AND target_id = $1`, wrB.ID.String())
	if auditAfter != auditBefore {
		t.Fatalf("expected no new resolve audit row from the cross-tenant attempt, before=%d after=%d", auditBefore, auditAfter)
	}

	// The withdrawal must still resolve normally afterward for the RIGHT
	// tenant's own staff.
	mock.Resolve(submitted.ProviderReference, payments.OutcomeSucceeded, "", false)
	resp = postJSON(t, srv, "/v1/admin/withdrawals/"+wrB.ID.String()+"/resolve", financeBTokens.AccessToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected tenant B's own finance staff to still resolve successfully afterward, got %d: %+v", resp.StatusCode, apiErr)
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

// TestWithdrawalResolve_ProviderAmountMismatchRejected is a Stage 3C
// specialist review fix (payments/ledger-finance P1): completing a
// withdrawal on a succeeded provider outcome must cross-check the
// provider's own confirmed amount/asset against what was originally
// requested, never trust the reference match alone. A provider that
// reports a different confirmed amount for the same reference (partial
// settlement, a fee-adjusted figure, a provider-side data error) must
// be refused - never silently completed with the ORIGINALLY REQUESTED
// amount released against a payout whose provider-confirmed facts don't
// match it.
func TestWithdrawalResolve_ProviderAmountMismatchRejected(t *testing.T) {
	f := setupStage3CResolutionFixture(t, 10000)
	submitted := mustSubmitWithdrawal(t, f.srv, f.financeToken.AccessToken, f.withdrawalID)

	f.mock.Resolve(submitted.ProviderReference, payments.OutcomeSucceeded, "", false)
	f.mock.SetConfirmedAmount(submitted.ProviderReference, submitted.Amount+1, submitted.AssetCode)

	resp := postJSON(t, f.srv, "/v1/admin/withdrawals/"+f.withdrawalID+"/resolve", f.financeToken.AccessToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 409 for a provider-confirmed amount mismatch, got %d: %+v", resp.StatusCode, apiErr)
	}

	// The request must remain `submitted` - never completed on
	// mismatched data, and no release ledger transaction posted.
	var state string
	err := f.pool.WithTenant(context.Background(), f.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM withdrawal_requests WHERE id = $1`, f.withdrawalID).Scan(&state)
	})
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state != "submitted" {
		t.Fatalf("expected state to remain 'submitted' after a rejected amount mismatch, got %q", state)
	}
	if got := releaseLedgerTransactionCount(t, f.pool, f.tenant.ID, f.withdrawalID); got != 0 {
		t.Fatalf("expected zero release ledger transactions after a rejected amount mismatch, got %d", got)
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
