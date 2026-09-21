//go:build integration

// Stage 9.2 fix round (qa + code-reviewer, real coverage gap): before this
// fix round, internal/httpserver/sportsbook_exposure_admin_handlers.go (266
// lines) had ZERO test coverage - no auth test, no cross-tenant test, no
// happy-path test - unlike its sibling
// sportsbook_jurisdiction_admin_flow_integration_test.go. This file closes
// that gap, mirroring that sibling's own structure/rationale AND
// risk_flow_integration_test.go's RoleRiskManager-gated tenant-scoped
// pattern exactly - sb_exposure_limits is tenant-owned commercial
// configuration (unlike sb_jurisdiction_restrictions' platform-admin-only
// shape), so PermSportsbookExposureLimitManage/Read are TENANT
// risk_manager-class permissions (internal/auth/permission.go's own doc
// comment), never platform-admin-only.
package httpserver

import (
	"net/http"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// TestSportsbookExposureLimitAdmin_RiskManagerFullLifecycle proves the
// create -> list -> disable round trip via HTTP, gated by
// PermSportsbookExposureLimitManage/Read (RoleRiskManager).
func TestSportsbookExposureLimitAdmin_RiskManagerFullLifecycle(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)

	tenant := mustCreateTenant(t, pool)
	riskManager := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleRiskManager, "rm-exp-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, riskManager.Email, "rm-exp-pw-1")

	createResp := postJSON(t, srv, "/v1/admin/sportsbook/exposure-limits", token.AccessToken, map[string]any{
		"scope_kind": "selection", "asset_code": "EUR", "max_open_potential_payout": 500_000,
		"authorization_reference": "http-exp-authz-ref", "reason_code": "http-exp-reason",
	})
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		var apiErr apierror.Error
		decodeBody(t, createResp, &apiErr)
		t.Fatalf("expected 201 creating an exposure limit, got %d: %+v", createResp.StatusCode, apiErr)
	}
	var created exposureLimitResponse
	decodeBody(t, createResp, &created)
	if created.ID == "" || created.Status != "active" {
		t.Fatalf("expected an active exposure limit with a real id, got %+v", created)
	}
	if created.MaxOpenPotentialPayout != 500_000 {
		t.Fatalf("expected the created limit to echo max_open_potential_payout=500000, got %+v", created)
	}
	if created.Measure != "gross_potential_payout" {
		t.Fatalf("expected the created limit to state its measure, got %+v", created)
	}

	listResp := getJSON(t, srv, "/v1/admin/sportsbook/exposure-limits", token.AccessToken)
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing exposure limits, got %d", listResp.StatusCode)
	}
	var listed []exposureLimitResponse
	decodeBody(t, listResp, &listed)
	var found bool
	for _, l := range listed {
		if l.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the created exposure limit to appear in the list")
	}

	disableResp := postJSON(t, srv, "/v1/admin/sportsbook/exposure-limits/"+created.ID+"/disable", token.AccessToken, map[string]any{
		"reason_code": "http-exp-disabled",
	})
	defer disableResp.Body.Close()
	if disableResp.StatusCode != http.StatusOK {
		var apiErr apierror.Error
		decodeBody(t, disableResp, &apiErr)
		t.Fatalf("expected 200 disabling the exposure limit, got %d: %+v", disableResp.StatusCode, apiErr)
	}
	var disabled exposureLimitResponse
	decodeBody(t, disableResp, &disabled)
	if disabled.Status != "disabled" {
		t.Fatalf("expected status 'disabled', got %q", disabled.Status)
	}

	// Disabling an already-disabled limit is refused with 404 (mirrors
	// DisableExposureLimit's own "no such active row" contract).
	secondDisable := postJSON(t, srv, "/v1/admin/sportsbook/exposure-limits/"+created.ID+"/disable", token.AccessToken, map[string]any{
		"reason_code": "http-exp-disabled-again",
	})
	defer secondDisable.Body.Close()
	if secondDisable.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 disabling an already-disabled exposure limit, got %d", secondDisable.StatusCode)
	}
}

// TestSportsbookExposureLimitAdmin_TenantAdminCannotManageOnlyRead proves
// the read/manage separation (mirrors TestRiskRules_TenantAdminCannotManageOnlyRead
// exactly): tenant_admin holds PermSportsbookExposureLimitRead but not
// PermSportsbookExposureLimitManage, so it may list but never create or
// disable.
func TestSportsbookExposureLimitAdmin_TenantAdminCannotManageOnlyRead(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)

	tenant := mustCreateTenant(t, pool)
	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-exp-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-exp-pw-1")

	listResp := getJSON(t, srv, "/v1/admin/sportsbook/exposure-limits", token.AccessToken)
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected tenant_admin to have read access (200), got %d", listResp.StatusCode)
	}

	createResp := postJSON(t, srv, "/v1/admin/sportsbook/exposure-limits", token.AccessToken, map[string]any{
		"scope_kind": "selection", "asset_code": "EUR", "max_open_potential_payout": 500_000,
		"authorization_reference": "should-not-be-created", "reason_code": "should-not-be-created",
	})
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusForbidden {
		var apiErr apierror.Error
		decodeBody(t, createResp, &apiErr)
		t.Fatalf("expected 403 for tenant_admin creating an exposure limit, got %d: %+v", createResp.StatusCode, apiErr)
	}
}

// TestSportsbookExposureLimitAdmin_UnauthenticatedForbidden proves the
// surface requires a bearer token at all.
func TestSportsbookExposureLimitAdmin_UnauthenticatedForbidden(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)

	resp := getJSON(t, srv, "/v1/admin/sportsbook/exposure-limits", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unauthenticated list request, got %d", resp.StatusCode)
	}

	createResp := postJSON(t, srv, "/v1/admin/sportsbook/exposure-limits", "", map[string]any{
		"scope_kind": "selection", "asset_code": "EUR", "max_open_potential_payout": 500_000,
		"authorization_reference": "should-not-be-created", "reason_code": "should-not-be-created",
	})
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unauthenticated create request, got %d", createResp.StatusCode)
	}
}

// TestSportsbookExposureLimitAdmin_CrossTenantDisableDenied proves tenant
// B's risk_manager cannot disable tenant A's exposure limit over HTTP -
// mirrors TestRiskRules_CrossTenantDisableDenied exactly: DisableExposureLimit's
// UPDATE relies entirely on RLS scoping with no explicit tenant check in Go
// code, so this mutating admin endpoint's isolation needs its own explicit
// HTTP-layer proof.
func TestSportsbookExposureLimitAdmin_CrossTenantDisableDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)

	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	riskManagerA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleRiskManager, "rm-exp-a-pw-1")
	riskManagerB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleRiskManager, "rm-exp-b-pw-1")
	tokenA := mustLoginStaff(t, srv, tenantA.Slug, riskManagerA.Email, "rm-exp-a-pw-1")
	tokenB := mustLoginStaff(t, srv, tenantB.Slug, riskManagerB.Email, "rm-exp-b-pw-1")

	createResp := postJSON(t, srv, "/v1/admin/sportsbook/exposure-limits", tokenA.AccessToken, map[string]any{
		"scope_kind": "selection", "asset_code": "EUR", "max_open_potential_payout": 500_000,
		"authorization_reference": "tenant-a-ref", "reason_code": "tenant-a-reason",
	})
	defer createResp.Body.Close()
	var created exposureLimitResponse
	decodeBody(t, createResp, &created)

	disableResp := postJSON(t, srv, "/v1/admin/sportsbook/exposure-limits/"+created.ID+"/disable", tokenB.AccessToken, map[string]any{
		"reason_code": "tenant-b-should-not-be-able-to-do-this",
	})
	defer disableResp.Body.Close()
	if disableResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected tenant B disabling tenant A's exposure limit to return 404 (not found, not forbidden - never confirming existence), got %d", disableResp.StatusCode)
	}

	// Confirm the limit is STILL active under tenant A's own token - the
	// cross-tenant attempt must have had zero effect.
	listResp := getJSON(t, srv, "/v1/admin/sportsbook/exposure-limits", tokenA.AccessToken)
	defer listResp.Body.Close()
	var listed []exposureLimitResponse
	decodeBody(t, listResp, &listed)
	var found bool
	for _, l := range listed {
		if l.ID == created.ID {
			found = true
			if l.Status != "active" {
				t.Fatalf("expected the limit to remain active after tenant B's denied disable attempt, got status %q", l.Status)
			}
		}
	}
	if !found {
		t.Fatal("expected tenant A's own limit to still be listed")
	}
}

// TestSportsbookExposureLimitAdmin_CrossTenantNotVisible proves a
// tenant-scoped exposure limit never leaks to a different tenant's
// risk_manager - mirrors TestRiskRules_CrossTenantNotVisible exactly.
func TestSportsbookExposureLimitAdmin_CrossTenantNotVisible(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newSportsbookTestServer(t, pool, issuer)

	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	riskManagerA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleRiskManager, "rm-exp-a-pw-2")
	riskManagerB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleRiskManager, "rm-exp-b-pw-2")
	tokenA := mustLoginStaff(t, srv, tenantA.Slug, riskManagerA.Email, "rm-exp-a-pw-2")
	tokenB := mustLoginStaff(t, srv, tenantB.Slug, riskManagerB.Email, "rm-exp-b-pw-2")

	createResp := postJSON(t, srv, "/v1/admin/sportsbook/exposure-limits", tokenA.AccessToken, map[string]any{
		"scope_kind": "selection", "asset_code": "EUR", "max_open_potential_payout": 500_000,
		"authorization_reference": "tenant-a-ref-2", "reason_code": "tenant-a-reason-2",
	})
	defer createResp.Body.Close()
	var created exposureLimitResponse
	decodeBody(t, createResp, &created)

	listResp := getJSON(t, srv, "/v1/admin/sportsbook/exposure-limits", tokenB.AccessToken)
	defer listResp.Body.Close()
	var listed []exposureLimitResponse
	decodeBody(t, listResp, &listed)
	for _, l := range listed {
		if l.ID == created.ID {
			t.Fatal("expected tenant B to never see tenant A's own tenant-scoped exposure limit")
		}
	}
}
