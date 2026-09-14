//go:build integration

// Stage 4E HTTP-layer tests for the admin "clear identity review" endpoint
// (ADR 0027 §6/§8). Follows rg_flow_integration_test.go's own conventions
// exactly (fixtures via internal packages, bearer tokens minted through
// the real HTTP register/login flow). No HTTP-reachable path exists today
// to land a registration in identity_review_required (the mock resolver
// always resolves NoMatch for the empty verified attributes a bare HTTP
// registration carries - see internal/identityresolution's own doc
// comment), so these tests force the status directly at the database
// layer, exactly like internal/rg's own integration tests seed
// cross-brand fixtures directly.
package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/identity"
)

// --- 1. Compliance can clear a player_account out of identity_review_required ---

func TestClearIdentityReview_ComplianceCanClear(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	target := mustRegisterPlayer(t, srv, brand.Slug)

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return identity.SetPlayerAccountStatus(ctx, tx, target.ID, identity.PlayerStatusIdentityReviewRequired)
	})
	if err != nil {
		t.Fatalf("failed to force identity_review_required: %v", err)
	}

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-idr-pw-1")
	complianceTokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-idr-pw-1")

	resp := postJSON(t, srv, "/v1/admin/players/"+target.ID.String()+"/identity-review/clear", complianceTokens.AccessToken, map[string]any{
		"reason_code": "manual_review_passed",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 clearing identity review, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = getJSON(t, srv, "/v1/admin/players/"+target.ID.String(), complianceTokens.AccessToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 reading player after clearing, got %d", resp.StatusCode)
	}
	var account map[string]any
	decodeBody(t, resp, &account)
	if account["status"] != "active" {
		t.Errorf("expected status active after clearing identity review, got %+v", account["status"])
	}
}

// --- 2. Only compliance may clear identity review; tenant_admin (who
// holds PermPlayerSuspend but NOT PermIdentityReviewManage) is denied,
// mirroring RG restriction-write's identical separation-of-duties test ---

func TestClearIdentityReview_OnlyComplianceCanWrite(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	target := mustRegisterPlayer(t, srv, brand.Slug)

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return identity.SetPlayerAccountStatus(ctx, tx, target.ID, identity.PlayerStatusIdentityReviewRequired)
	})
	if err != nil {
		t.Fatalf("failed to force identity_review_required: %v", err)
	}

	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-idr-pw-1")
	tenantAdminTokens := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-idr-pw-1")

	resp := postJSON(t, srv, "/v1/admin/players/"+target.ID.String()+"/identity-review/clear", tenantAdminTokens.AccessToken, map[string]any{
		"reason_code": "manual_review_passed",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for tenant_admin clearing identity review, got %d", resp.StatusCode)
	}
}

// --- 3. Adversarial safety: the endpoint must refuse to "clear" an
// account that is NOT currently identity_review_required (e.g. a
// suspended or self-excluded account) - it must never become a generic
// "set player active" button ---

func TestClearIdentityReview_RefusesWhenAccountNotInReview(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	target := mustRegisterPlayer(t, srv, brand.Slug)

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-idr-pw-2")
	complianceTokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-idr-pw-2")

	// Suspend the account via the existing, unrelated suspend endpoint.
	resp := postJSON(t, srv, "/v1/admin/players/"+target.ID.String()+"/suspend", complianceTokens.AccessToken, map[string]any{
		"reason": "unrelated suspension",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("failed to suspend target: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Attempting to "clear identity review" on a SUSPENDED (not
	// review-required) account must be refused, never silently reactivate it.
	resp = postJSON(t, srv, "/v1/admin/players/"+target.ID.String()+"/identity-review/clear", complianceTokens.AccessToken, map[string]any{
		"reason_code": "attempted_bypass",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 clearing identity review on a non-review account, got %d", resp.StatusCode)
	}

	// Confirm the account is STILL suspended, not active.
	resp2 := getJSON(t, srv, "/v1/admin/players/"+target.ID.String(), complianceTokens.AccessToken)
	defer resp2.Body.Close()
	var account map[string]any
	decodeBody(t, resp2, &account)
	if account["status"] != "suspended" {
		t.Fatalf("expected the account to remain suspended, got %+v", account["status"])
	}
}

// --- 4. reason_code is mandatory (CLAUDE.md's administrative-action
// reason-code requirement) ---

func TestClearIdentityReview_RequiresReasonCode(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	target := mustRegisterPlayer(t, srv, brand.Slug)

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return identity.SetPlayerAccountStatus(ctx, tx, target.ID, identity.PlayerStatusIdentityReviewRequired)
	})
	if err != nil {
		t.Fatalf("failed to force identity_review_required: %v", err)
	}

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-idr-pw-3")
	complianceTokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-idr-pw-3")

	resp := postJSON(t, srv, "/v1/admin/players/"+target.ID.String()+"/identity-review/clear", complianceTokens.AccessToken, map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 with no reason_code, got %d", resp.StatusCode)
	}
}

// --- 6. A tenant's compliance staff cannot clear a DIFFERENT tenant's
// player_account_id via this endpoint (adversarial testing / multi-
// tenancy specialist review finding, Stage 4E: this was the one admin
// mutation in this stage with no explicit cross-tenant test, unlike its
// sibling RG-restriction and player-suspend endpoints) ---

func TestClearIdentityReview_CrossTenantClearingRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	targetA := mustRegisterPlayer(t, srv, brandA.Slug)
	err := pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return identity.SetPlayerAccountStatus(ctx, tx, targetA.ID, identity.PlayerStatusIdentityReviewRequired)
	})
	if err != nil {
		t.Fatalf("failed to force identity_review_required: %v", err)
	}

	tenantB := mustCreateTenant(t, pool)
	complianceB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleCompliance, "compliance-idr-crosstenant-pw-1")
	complianceBTokens := mustLoginStaff(t, srv, tenantB.Slug, complianceB.Email, "compliance-idr-crosstenant-pw-1")

	resp := postJSON(t, srv, "/v1/admin/players/"+targetA.ID.String()+"/identity-review/clear", complianceBTokens.AccessToken, map[string]any{
		"reason_code": "cross_tenant_attempt",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for tenant B naming tenant A's own player_account_id, got %d", resp.StatusCode)
	}

	// Confirm account A is STILL identity_review_required, never touched.
	complianceA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleCompliance, "compliance-idr-crosstenant-pw-2")
	complianceATokens := mustLoginStaff(t, srv, tenantA.Slug, complianceA.Email, "compliance-idr-crosstenant-pw-2")
	resp2 := getJSON(t, srv, "/v1/admin/players/"+targetA.ID.String(), complianceATokens.AccessToken)
	defer resp2.Body.Close()
	var account map[string]any
	decodeBody(t, resp2, &account)
	if account["status"] != "identity_review_required" {
		t.Fatalf("expected account A to remain identity_review_required, got %+v", account["status"])
	}
}

// --- 7. Concurrent identity-review-clear and suspend on the SAME
// account: the loser must never win (security specialist review finding,
// Stage 4E - closes the TOCTOU a plain read-then-write would have) ---

func TestClearIdentityReview_ConcurrentWithSuspend_NeverReactivatesASuspendedAccount(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	target := mustRegisterPlayer(t, srv, brand.Slug)

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return identity.SetPlayerAccountStatus(ctx, tx, target.ID, identity.PlayerStatusIdentityReviewRequired)
	})
	if err != nil {
		t.Fatalf("failed to force identity_review_required: %v", err)
	}

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-idr-race-pw-1")
	complianceTokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-idr-race-pw-1")

	// Suspend the account FIRST (its status is no longer
	// identity_review_required by the time clear is attempted below), then
	// attempt to clear it - the atomic conditional update
	// (identity.SetPlayerAccountStatusIfCurrent) must refuse, never
	// silently reactivate a since-suspended account.
	resp := postJSON(t, srv, "/v1/admin/players/"+target.ID.String()+"/suspend", complianceTokens.AccessToken, map[string]any{
		"reason": "race setup",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("failed to suspend target: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp2 := postJSON(t, srv, "/v1/admin/players/"+target.ID.String()+"/identity-review/clear", complianceTokens.AccessToken, map[string]any{
		"reason_code": "attempted_clear_after_suspend",
	})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 clearing an account suspended out from under it, got %d", resp2.StatusCode)
	}

	resp3 := getJSON(t, srv, "/v1/admin/players/"+target.ID.String(), complianceTokens.AccessToken)
	defer resp3.Body.Close()
	var account map[string]any
	decodeBody(t, resp3, &account)
	if account["status"] != "suspended" {
		t.Fatalf("expected the account to remain suspended, got %+v", account["status"])
	}
}

// --- 5. A player self-excluded via one brand, resolved (Match) to the
// same person on a second registration and landing in
// identity_review_required is NOT what closes cross-brand self-exclusion
// evasion by itself - clearing the review still requires compliance
// action, and the resulting active account remains subject to the SAME
// person's platform-wide restriction once RG is evaluated. This is a
// smoke test that the two features (identity review clearing, RG
// enforcement) compose correctly rather than one silently undermining
// the other; the authoritative proof is
// internal/identityresolution's own TestCrossBrandSelfExclusion_
// ResolvedPersonCannotEvadeViaSecondBrand.
func TestClearIdentityReview_PlayerAndFinanceTokensDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	target := mustRegisterPlayer(t, srv, brand.Slug)

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return identity.SetPlayerAccountStatus(ctx, tx, target.ID, identity.PlayerStatusIdentityReviewRequired)
	})
	if err != nil {
		t.Fatalf("failed to force identity_review_required: %v", err)
	}

	resp := postJSON(t, srv, "/v1/admin/players/"+target.ID.String()+"/identity-review/clear", target.Tokens.AccessToken, map[string]any{
		"reason_code": "self_bypass_attempt",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a player token clearing their own identity review, got %d", resp.StatusCode)
	}

	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "finance-idr-pw-1")
	financeTokens := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "finance-idr-pw-1")
	resp2 := postJSON(t, srv, "/v1/admin/players/"+target.ID.String()+"/identity-review/clear", financeTokens.AccessToken, map[string]any{
		"reason_code": "finance_attempt",
	})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a finance token clearing identity review, got %d", resp2.StatusCode)
	}
}
