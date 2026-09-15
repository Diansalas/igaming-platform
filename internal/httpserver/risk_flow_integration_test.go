//go:build integration

// Stage 4G HTTP-layer tests: risk_manager staff-creation self-escalation
// guard (mirrors Stage 3D's identical finance-role regression test) and
// the minimum Risk & Limits admin API (create/list/disable), RBAC-gated.
package httpserver

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// TestCreateStaff_TenantAdminCannotCreateRiskManagerRole mirrors
// TestCreateStaff_TenantAdminCannotCreateFinanceRole exactly - the
// identical self-escalation path (a tenant_admin's own PermStaffManage
// minting a brand-new, self-controlled account holding a narrower,
// separately-authorized capability) applies equally to risk_manager.
func TestCreateStaff_TenantAdminCannotCreateRiskManagerRole(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	tenant := mustCreateTenant(t, pool)
	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-mint-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-mint-pw-1")

	resp := postJSON(t, srv, "/v1/admin/tenants/"+tenant.ID.String()+"/staff", token.AccessToken, map[string]string{
		"email": "shadow-risk@example.com", "password": "a-decent-password-1", "role": "risk_manager",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 403 for tenant_admin creating a risk_manager-role staff account, got %d: %+v", resp.StatusCode, apiErr)
	}
}

func TestCreateStaff_PlatformAdminCanCreateRiskManagerRole(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	tenant := mustCreateTenant(t, pool)
	platformAdmin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-mint-pw-1")
	platformToken := mustLoginStaff(t, srv, "", platformAdmin.Email, "pa-mint-pw-1")

	resp := postJSON(t, srv, "/v1/admin/tenants/"+tenant.ID.String()+"/staff", platformToken.AccessToken, map[string]string{
		"email": "real-risk-manager@example.com", "password": "a-decent-password-1", "role": "risk_manager",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 201 for platform_admin creating a risk_manager-role staff account, got %d: %+v", resp.StatusCode, apiErr)
	}
}

// --- Risk & Limits admin API: create, list, disable ---

func TestRiskRules_CreateListDisable(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	tenant := mustCreateTenant(t, pool)
	riskManager := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleRiskManager, "rm-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, riskManager.Email, "rm-pw-1")

	createResp := postJSON(t, srv, "/v1/admin/risk/rules", token.AccessToken, map[string]any{
		"operation": "casino_bet", "limit_kind": "max_amount", "time_window": "transaction", "threshold": 500,
	})
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		var apiErr apierror.Error
		decodeBody(t, createResp, &apiErr)
		t.Fatalf("expected 201 creating a risk rule, got %d: %+v", createResp.StatusCode, apiErr)
	}
	var created riskRuleResponse
	decodeBody(t, createResp, &created)
	if created.ID == "" || created.Status != "active" {
		t.Fatalf("expected an active created rule, got %+v", created)
	}

	listResp := getJSON(t, srv, "/v1/admin/risk/rules?operation=casino_bet", token.AccessToken)
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing risk rules, got %d", listResp.StatusCode)
	}
	var rules []riskRuleResponse
	decodeBody(t, listResp, &rules)
	found := false
	for _, r := range rules {
		if r.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the newly created rule to appear in the list")
	}

	disableResp := postJSON(t, srv, "/v1/admin/risk/rules/"+created.ID+"/disable", token.AccessToken, map[string]any{})
	defer disableResp.Body.Close()
	if disableResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 disabling the rule, got %d", disableResp.StatusCode)
	}
	var disabled riskRuleResponse
	decodeBody(t, disableResp, &disabled)
	if disabled.Status != "disabled" {
		t.Fatalf("expected the rule to be disabled, got %+v", disabled)
	}
}

// TestRiskRules_TenantAdminCannotManageOnlyRead proves the read/manage
// separation (directive §24): tenant_admin may list rules but never
// create or disable one.
func TestRiskRules_TenantAdminCannotManageOnlyRead(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	tenant := mustCreateTenant(t, pool)
	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-risk-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-risk-pw-1")

	listResp := getJSON(t, srv, "/v1/admin/risk/rules?operation=casino_bet", token.AccessToken)
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected tenant_admin to have read access (200), got %d", listResp.StatusCode)
	}

	createResp := postJSON(t, srv, "/v1/admin/risk/rules", token.AccessToken, map[string]any{
		"operation": "casino_bet", "limit_kind": "max_amount", "time_window": "transaction", "threshold": 500,
	})
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected tenant_admin to be forbidden from creating a risk rule (403), got %d", createResp.StatusCode)
	}
}

// TestRiskRules_CrossTenantDisableDenied proves tenant B's risk_manager
// cannot disable tenant A's risk rule over HTTP - adversarial specialist
// review finding: only the list/read cross-tenant path had a test;
// DisableRule's UPDATE relies entirely on RLS scoping with no explicit
// tenant check in Go code, so this mutating admin endpoint's isolation
// was previously unverified by any test.
func TestRiskRules_CrossTenantDisableDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	riskManagerA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleRiskManager, "rm-a-pw-2")
	riskManagerB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleRiskManager, "rm-b-pw-2")
	tokenA := mustLoginStaff(t, srv, tenantA.Slug, riskManagerA.Email, "rm-a-pw-2")
	tokenB := mustLoginStaff(t, srv, tenantB.Slug, riskManagerB.Email, "rm-b-pw-2")

	createResp := postJSON(t, srv, "/v1/admin/risk/rules", tokenA.AccessToken, map[string]any{
		"operation": "casino_bet", "limit_kind": "max_amount", "time_window": "transaction", "threshold": 500,
	})
	defer createResp.Body.Close()
	var created riskRuleResponse
	decodeBody(t, createResp, &created)

	disableResp := postJSON(t, srv, "/v1/admin/risk/rules/"+created.ID+"/disable", tokenB.AccessToken, map[string]any{})
	defer disableResp.Body.Close()
	if disableResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected tenant B disabling tenant A's rule to return 404 (not found, not forbidden - never confirming existence), got %d", disableResp.StatusCode)
	}

	// Confirm the rule is STILL active - the cross-tenant attempt must
	// have had zero effect, not merely returned an error after a partial
	// mutation.
	listResp := getJSON(t, srv, "/v1/admin/risk/rules?operation=casino_bet", tokenA.AccessToken)
	defer listResp.Body.Close()
	var rules []riskRuleResponse
	decodeBody(t, listResp, &rules)
	found := false
	for _, r := range rules {
		if r.ID == created.ID {
			found = true
			if r.Status != "active" {
				t.Fatalf("expected the rule to remain active after tenant B's denied disable attempt, got status %q", r.Status)
			}
		}
	}
	if !found {
		t.Fatal("expected tenant A's own rule to still be listed")
	}
}

// TestRiskRules_CrossTenantNotVisible proves a tenant-scoped rule never
// leaks to a different tenant's risk_manager.
func TestRiskRules_CrossTenantNotVisible(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	riskManagerA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleRiskManager, "rm-a-pw-1")
	riskManagerB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleRiskManager, "rm-b-pw-1")
	tokenA := mustLoginStaff(t, srv, tenantA.Slug, riskManagerA.Email, "rm-a-pw-1")
	tokenB := mustLoginStaff(t, srv, tenantB.Slug, riskManagerB.Email, "rm-b-pw-1")

	createResp := postJSON(t, srv, "/v1/admin/risk/rules", tokenA.AccessToken, map[string]any{
		"operation": "casino_bet", "limit_kind": "max_amount", "time_window": "transaction", "threshold": 500,
	})
	defer createResp.Body.Close()
	var created riskRuleResponse
	decodeBody(t, createResp, &created)

	listResp := getJSON(t, srv, "/v1/admin/risk/rules?operation=casino_bet", tokenB.AccessToken)
	defer listResp.Body.Close()
	var rules []riskRuleResponse
	decodeBody(t, listResp, &rules)
	for _, r := range rules {
		if r.ID == created.ID {
			t.Fatal("expected tenant B to never see tenant A's own tenant-scoped risk rule")
		}
	}
}
