//go:build integration

// Stage 3D hardening: the withdrawal-policy admin API security tests
// (directive item 5) - unauthorized/cross-role access, invalid input,
// cross-tenant brand ownership, removal, and audit-trail coverage for
// the new minimal admin API boundary (withdrawal_policy_handlers.go).
package httpserver

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// hasAuditEntry reports whether an audit_log row exists for the given
// tenant/action/target - CLAUDE.md's "every mutating administrative/
// financial action writes an audit record" requirement, checked directly
// against the table rather than through the read-only audit-log HTTP
// endpoint (which is a separate, already-tested surface).
func hasAuditEntry(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action, targetID string) bool {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
			tenantID, action, targetID,
		).Scan(&count)
	})
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	return count > 0
}

// TestWithdrawalPolicyAdmin_TenantAdminCanCreateListDelete is the
// baseline positive path: RoleTenantAdmin (the sole grantee of
// PermWithdrawalPolicyWrite) can create, see, and remove a policy row for
// its own tenant, and every mutation is audited.
func TestWithdrawalPolicyAdmin_TenantAdminCanCreateListDelete(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-policy-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-policy-pw-1")

	resp := postJSON(t, srv, "/v1/admin/withdrawal-policies", token.AccessToken, map[string]any{
		"asset_code": "EUR", "approval_threshold_minor_units": 500000, "required_approvals": 2,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 201 creating a policy, got %d: %+v", resp.StatusCode, apiErr)
	}
	var created withdrawalPolicyResponse
	decodeBody(t, resp, &created)
	if created.AssetCode != "EUR" || created.ApprovalThresholdMinorUnits != 500000 || created.RequiredApprovals != 2 {
		t.Fatalf("unexpected created policy: %+v", created)
	}

	listResp := getJSON(t, srv, "/v1/admin/withdrawal-policies", token.AccessToken)
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing policies, got %d", listResp.StatusCode)
	}
	var list []withdrawalPolicyResponse
	decodeBody(t, listResp, &list)
	found := false
	for _, p := range list {
		if p.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the created policy to appear in the list")
	}

	if !hasAuditEntry(t, pool, tenant.ID, "withdrawal_policy.created", created.ID) {
		t.Error("expected an audit_log entry for withdrawal_policy.created")
	}

	delResp := deleteRequest(t, srv, "/v1/admin/withdrawal-policies/"+created.ID+"?reason_code=cleanup", token.AccessToken)
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 deleting the policy, got %d", delResp.StatusCode)
	}
	if !hasAuditEntry(t, pool, tenant.ID, "withdrawal_policy.deleted", created.ID) {
		t.Error("expected an audit_log entry for withdrawal_policy.deleted")
	}

	listResp2 := getJSON(t, srv, "/v1/admin/withdrawal-policies", token.AccessToken)
	defer listResp2.Body.Close()
	var list2 []withdrawalPolicyResponse
	decodeBody(t, listResp2, &list2)
	for _, p := range list2 {
		if p.ID == created.ID {
			t.Fatal("expected the deleted policy to no longer be listed")
		}
	}
}

// TestWithdrawalPolicyAdmin_FinanceRoleDenied proves business decision
// #4/#5's other half from the policy side: RoleFinance (the role that
// approves withdrawals) must never also be able to write the policy
// gating its own approvals.
func TestWithdrawalPolicyAdmin_FinanceRoleDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "finance-policy-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "finance-policy-pw-1")

	resp := postJSON(t, srv, "/v1/admin/withdrawal-policies", token.AccessToken, map[string]any{
		"asset_code": "EUR", "approval_threshold_minor_units": 500000, "required_approvals": 2,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for finance writing a withdrawal policy, got %d", resp.StatusCode)
	}

	listResp := getJSON(t, srv, "/v1/admin/withdrawal-policies", token.AccessToken)
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for finance listing withdrawal policies, got %d", listResp.StatusCode)
	}
}

// TestWithdrawalPolicyAdmin_CrossTenantInvisibleAndBrandOwnershipEnforced
// proves tenant isolation (RLS) on reads and the composite (brand_id,
// tenant_id) FK on writes: tenant A's admin never sees tenant B's
// policies, and cannot write a policy naming tenant B's brand_id.
func TestWithdrawalPolicyAdmin_CrossTenantInvisibleAndBrandOwnershipEnforced(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	brandB := mustCreateBrand(t, pool, tenantB)

	adminA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleTenantAdmin, "ta-a-pw-1")
	tokenA := mustLoginStaff(t, srv, tenantA.Slug, adminA.Email, "ta-a-pw-1")
	adminB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleTenantAdmin, "ta-b-pw-1")
	tokenB := mustLoginStaff(t, srv, tenantB.Slug, adminB.Email, "ta-b-pw-1")

	respB := postJSON(t, srv, "/v1/admin/withdrawal-policies", tokenB.AccessToken, map[string]any{
		"asset_code": "EUR", "approval_threshold_minor_units": 100, "required_approvals": 1,
	})
	defer respB.Body.Close()
	if respB.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating tenant B's policy, got %d", respB.StatusCode)
	}
	var policyB withdrawalPolicyResponse
	decodeBody(t, respB, &policyB)

	listA := getJSON(t, srv, "/v1/admin/withdrawal-policies", tokenA.AccessToken)
	defer listA.Body.Close()
	var listAResp []withdrawalPolicyResponse
	decodeBody(t, listA, &listAResp)
	for _, p := range listAResp {
		if p.ID == policyB.ID {
			t.Fatal("expected tenant A to never see tenant B's withdrawal policy")
		}
	}

	// tenant A's admin attempts to write a policy naming tenant B's brand -
	// the composite FK must refuse it as an invalid reference, not
	// silently create a dead row.
	respBadBrand := postJSON(t, srv, "/v1/admin/withdrawal-policies", tokenA.AccessToken, map[string]any{
		"asset_code": "EUR", "approval_threshold_minor_units": 100, "required_approvals": 1, "brand_id": brandB.ID.String(),
	})
	defer respBadBrand.Body.Close()
	if respBadBrand.StatusCode != http.StatusBadRequest {
		var apiErr apierror.Error
		decodeBody(t, respBadBrand, &apiErr)
		t.Fatalf("expected 400 writing a policy naming another tenant's brand, got %d: %+v", respBadBrand.StatusCode, apiErr)
	}

	// tenant A cannot delete tenant B's policy either (RLS scopes the
	// DELETE, so it simply matches zero rows).
	delResp := deleteRequest(t, srv, "/v1/admin/withdrawal-policies/"+policyB.ID+"?reason_code=cleanup", tokenA.AccessToken)
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for tenant A deleting tenant B's policy, got %d", delResp.StatusCode)
	}
}

// TestWithdrawalPolicyAdmin_InvalidInputRejected covers directive item
// 5's "invalid asset/threshold/approver-count" security tests - every
// bad input must fail closed with a 400, never a 500 or a silently
// accepted row.
func TestWithdrawalPolicyAdmin_InvalidInputRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-invalid-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-invalid-pw-1")

	cases := []struct {
		name string
		body map[string]any
	}{
		{"unknown asset", map[string]any{"asset_code": "NOTAREALASSET", "approval_threshold_minor_units": 100, "required_approvals": 1}},
		{"missing asset", map[string]any{"approval_threshold_minor_units": 100, "required_approvals": 1}},
		{"negative threshold", map[string]any{"asset_code": "EUR", "approval_threshold_minor_units": -1, "required_approvals": 1}},
		{"zero required_approvals", map[string]any{"asset_code": "EUR", "approval_threshold_minor_units": 100, "required_approvals": 0}},
		{"malformed brand_id", map[string]any{"asset_code": "EUR", "approval_threshold_minor_units": 100, "required_approvals": 1, "brand_id": "not-a-uuid"}},
	}
	for _, tc := range cases {
		resp := postJSON(t, srv, "/v1/admin/withdrawal-policies", token.AccessToken, tc.body)
		if resp.StatusCode != http.StatusBadRequest {
			var apiErr apierror.Error
			decodeBody(t, resp, &apiErr)
			t.Errorf("%s: expected 400, got %d: %+v", tc.name, resp.StatusCode, apiErr)
		}
		resp.Body.Close()
	}
}

// TestWithdrawalPolicyAdmin_DeletionDoesNotRetroactivelyAlterPastDecision
// closes a gap this stage's own qa specialist review flagged: the
// baseline create/list/delete test only exercises deleting an UNUSED
// policy row. This proves the actual financial-audit-trail claim this
// file's own doc comment makes - deleting a withdrawal_policies row
// after a real decision was recorded under it can never rewrite what
// that decision actually applied, because threshold_amount_at_decision/
// request_amount_at_decision are snapshotted onto the withdrawal_
// approvals row at decision time, not re-derived from withdrawal_
// policies afterward.
func TestWithdrawalPolicyAdmin_DeletionDoesNotRetroactivelyAlterPastDecision(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)
	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-audit-int-pw-1")
	adminToken := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-audit-int-pw-1")
	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "finance-audit-int-pw-1")
	financeToken := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "finance-audit-int-pw-1")

	policyResp := postJSON(t, srv, "/v1/admin/withdrawal-policies", adminToken.AccessToken, map[string]any{
		"asset_code": "EUR", "approval_threshold_minor_units": 1_000_000, "required_approvals": 2,
	})
	var policy withdrawalPolicyResponse
	decodeBody(t, policyResp, &policy)
	policyResp.Body.Close()

	player := mustRegisterPlayer(t, srv, brand.Slug)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100000)
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletIDFor(t, pool, tenant.ID, player.ID, "EUR"), "EUR", 5000)
	mustOpenReviewQueue(t, srv, financeToken.AccessToken)

	approveResp := postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/approve", financeToken.AccessToken, nil)
	approveResp.Body.Close()
	if approveResp.StatusCode != http.StatusOK {
		t.Fatalf("approve withdrawal: status %d", approveResp.StatusCode)
	}

	var thresholdBefore, amountBefore int64
	mustScanApprovalSnapshot := func() {
		t.Helper()
		if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT threshold_amount_at_decision, request_amount_at_decision FROM withdrawal_approvals WHERE withdrawal_request_id = $1`,
				wr.ID,
			).Scan(&thresholdBefore, &amountBefore)
		}); err != nil {
			t.Fatalf("read approval snapshot: %v", err)
		}
	}
	mustScanApprovalSnapshot()
	if thresholdBefore != 1_000_000 {
		t.Fatalf("expected snapshotted threshold 1000000 before policy deletion, got %d", thresholdBefore)
	}

	delResp := deleteRequest(t, srv, "/v1/admin/withdrawal-policies/"+policy.ID+"?reason_code=cleanup", adminToken.AccessToken)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 deleting the policy, got %d", delResp.StatusCode)
	}

	var thresholdAfter, amountAfter int64
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT threshold_amount_at_decision, request_amount_at_decision FROM withdrawal_approvals WHERE withdrawal_request_id = $1`,
			wr.ID,
		).Scan(&thresholdAfter, &amountAfter)
	}); err != nil {
		t.Fatalf("read approval snapshot after deletion: %v", err)
	}
	if thresholdAfter != thresholdBefore || amountAfter != amountBefore {
		t.Fatalf("expected the recorded decision's snapshot to survive policy deletion unchanged, got threshold=%d amount=%d (was %d/%d)",
			thresholdAfter, amountAfter, thresholdBefore, amountBefore)
	}
	if !hasAuditEntry(t, pool, tenant.ID, "withdrawal.approval_recorded", wr.ID.String()) {
		t.Error("expected the original withdrawal.approval_recorded audit entry to still exist after the policy was deleted")
	}
}

// TestWithdrawalPolicyAdmin_DeleteRequiresReasonCode proves the removal
// endpoint fails closed on the missing reason_code CLAUDE.md requires for
// every mutating administrative/financial action.
func TestWithdrawalPolicyAdmin_DeleteRequiresReasonCode(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-noreason-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-noreason-pw-1")

	createResp := postJSON(t, srv, "/v1/admin/withdrawal-policies", token.AccessToken, map[string]any{
		"asset_code": "EUR", "approval_threshold_minor_units": 100, "required_approvals": 1,
	})
	var created withdrawalPolicyResponse
	decodeBody(t, createResp, &created)
	createResp.Body.Close()

	delResp := deleteRequest(t, srv, "/v1/admin/withdrawal-policies/"+created.ID, token.AccessToken)
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 deleting a policy with no reason_code, got %d", delResp.StatusCode)
	}
}

// TestWithdrawalPolicyAdmin_BackdatedEffectiveFromRejected proves the
// admin API refuses a past effective_from - see newWriteWithdrawalPolicyHandler's
// own doc comment for why a backdated policy row would undermine the
// insert-only-history claim this table is documented to uphold.
func TestWithdrawalPolicyAdmin_BackdatedEffectiveFromRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-backdate-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-backdate-pw-1")

	resp := postJSON(t, srv, "/v1/admin/withdrawal-policies", token.AccessToken, map[string]any{
		"asset_code": "EUR", "approval_threshold_minor_units": 100, "required_approvals": 1,
		"effective_from": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 400 for a backdated effective_from, got %d: %+v", resp.StatusCode, apiErr)
	}
}

// TestWithdrawalPolicyAdmin_UnauthenticatedDenied proves the route
// requires a valid bearer token at all, like every other admin route.
func TestWithdrawalPolicyAdmin_UnauthenticatedDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	resp := postJSON(t, srv, "/v1/admin/withdrawal-policies", "", map[string]any{
		"asset_code": "EUR", "approval_threshold_minor_units": 100, "required_approvals": 1,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for an unauthenticated policy write, got %d", resp.StatusCode)
	}
}
