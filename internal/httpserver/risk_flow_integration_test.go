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
		"operation": "casino_bet", "limit_kind": "max_amount", "time_window": "transaction", "threshold": 500, "threshold_exponent": 2,
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
		"operation": "casino_bet", "limit_kind": "max_amount", "time_window": "transaction", "threshold": 500, "threshold_exponent": 2,
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
		"operation": "casino_bet", "limit_kind": "max_amount", "time_window": "transaction", "threshold": 500, "threshold_exponent": 2,
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
		"operation": "casino_bet", "limit_kind": "max_amount", "time_window": "transaction", "threshold": 500, "threshold_exponent": 2,
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

// TestRiskRules_LicensingModeRoundTrip proves Stage 4G-FINAL's new
// licensing_mode field survives create -> create-response -> list
// end to end over HTTP, and that it is genuinely optional (omitted
// entirely on a second rule, expected to come back empty).
func TestRiskRules_LicensingModeRoundTrip(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	tenant := mustCreateTenant(t, pool)
	riskManager := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleRiskManager, "rm-lm-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, riskManager.Email, "rm-lm-pw-1")

	scopedResp := postJSON(t, srv, "/v1/admin/risk/rules", token.AccessToken, map[string]any{
		"operation": "casino_bet", "limit_kind": "max_amount", "time_window": "transaction", "threshold": 500, "threshold_exponent": 2,
		"licensing_mode": "own_licence",
	})
	defer scopedResp.Body.Close()
	if scopedResp.StatusCode != http.StatusCreated {
		var apiErr apierror.Error
		decodeBody(t, scopedResp, &apiErr)
		t.Fatalf("expected 201 creating a licensing_mode-scoped risk rule, got %d: %+v", scopedResp.StatusCode, apiErr)
	}
	var scoped riskRuleResponse
	decodeBody(t, scopedResp, &scoped)
	if scoped.LicensingMode != "own_licence" {
		t.Fatalf("expected create response to echo licensing_mode=own_licence, got %+v", scoped)
	}

	unscopedResp := postJSON(t, srv, "/v1/admin/risk/rules", token.AccessToken, map[string]any{
		"operation": "casino_bet", "limit_kind": "max_amount", "time_window": "transaction", "threshold": 500, "threshold_exponent": 2,
	})
	defer unscopedResp.Body.Close()
	if unscopedResp.StatusCode != http.StatusCreated {
		var apiErr apierror.Error
		decodeBody(t, unscopedResp, &apiErr)
		t.Fatalf("expected 201 creating a rule with licensing_mode omitted, got %d: %+v", unscopedResp.StatusCode, apiErr)
	}
	var unscoped riskRuleResponse
	decodeBody(t, unscopedResp, &unscoped)
	if unscoped.LicensingMode != "" {
		t.Fatalf("expected an omitted licensing_mode to round-trip empty (unscoped), got %+v", unscoped)
	}

	listResp := getJSON(t, srv, "/v1/admin/risk/rules?operation=casino_bet", token.AccessToken)
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing risk rules, got %d", listResp.StatusCode)
	}
	var rules []riskRuleResponse
	decodeBody(t, listResp, &rules)
	var foundScoped, foundUnscoped bool
	for _, r := range rules {
		if r.ID == scoped.ID {
			foundScoped = true
			if r.LicensingMode != "own_licence" {
				t.Fatalf("expected the listed rule to retain licensing_mode=own_licence, got %+v", r)
			}
		}
		if r.ID == unscoped.ID {
			foundUnscoped = true
			if r.LicensingMode != "" {
				t.Fatalf("expected the listed unscoped rule's licensing_mode to remain empty, got %+v", r)
			}
		}
	}
	if !foundScoped || !foundUnscoped {
		t.Fatalf("expected both rules to appear in the list, foundScoped=%v foundUnscoped=%v", foundScoped, foundUnscoped)
	}
}

// TestRiskRules_LicensingModeInvalidValueRejected proves an invalid
// licensing_mode is rejected with a clean 400 validation error and never
// reaches the database (where risk_rules' CHECK constraint would
// otherwise surface a raw Postgres error instead).
func TestRiskRules_LicensingModeInvalidValueRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	tenant := mustCreateTenant(t, pool)
	riskManager := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleRiskManager, "rm-lm-pw-2")
	token := mustLoginStaff(t, srv, tenant.Slug, riskManager.Email, "rm-lm-pw-2")

	resp := postJSON(t, srv, "/v1/admin/risk/rules", token.AccessToken, map[string]any{
		"operation": "casino_bet", "limit_kind": "max_amount", "time_window": "transaction", "threshold": 500, "threshold_exponent": 2,
		"licensing_mode": "nonsense",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 400 for an invalid licensing_mode, got %d: %+v", resp.StatusCode, apiErr)
	}
	var apiErr apierror.Error
	decodeBody(t, resp, &apiErr)
	if apiErr.Code != apierror.CodeValidation {
		t.Fatalf("expected a validation error code, got %+v", apiErr)
	}
}

// TestCreateRiskRule_ThresholdDenominationIsRequiredExactlyOnce is the
// HTTP-layer half of ADR 0031 §35: an amount rule whose threshold has no
// declared denomination (or two conflicting ones) is a 400, not a stored
// rule whose meaning silently changes with the asset it is read against.
func TestCreateRiskRule_ThresholdDenominationIsRequiredExactlyOnce(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	tenant := mustCreateTenant(t, pool)
	riskManager := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleRiskManager, "rm-denom-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, riskManager.Email, "rm-denom-pw-1")

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"neither asset_code nor threshold_exponent", map[string]any{
			"operation": "casino_bet", "limit_kind": "max_amount", "time_window": "transaction", "threshold": 500,
		}, http.StatusBadRequest},
		{"both asset_code and threshold_exponent", map[string]any{
			"operation": "casino_bet", "limit_kind": "max_amount", "time_window": "transaction", "threshold": 500,
			"asset_code": "EUR", "threshold_exponent": 2,
		}, http.StatusBadRequest},
		{"exponent out of range", map[string]any{
			"operation": "casino_bet", "limit_kind": "max_amount", "time_window": "transaction", "threshold": 500,
			"threshold_exponent": 19,
		}, http.StatusBadRequest},
		{"asset-scoped rule needs no exponent", map[string]any{
			"operation": "casino_bet", "limit_kind": "max_amount", "time_window": "transaction", "threshold": 500,
			"asset_code": "EUR",
		}, http.StatusCreated},
		{"asset-agnostic rule with a declared exponent", map[string]any{
			"operation": "casino_bet", "limit_kind": "max_amount", "time_window": "transaction", "threshold": 500,
			"threshold_exponent": 8,
		}, http.StatusCreated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := postJSON(t, srv, "/v1/admin/risk/rules", token.AccessToken, c.body)
			defer resp.Body.Close()
			if resp.StatusCode != c.want {
				var apiErr apierror.Error
				decodeBody(t, resp, &apiErr)
				t.Fatalf("expected %d, got %d: %+v", c.want, resp.StatusCode, apiErr)
			}
			if c.want != http.StatusCreated {
				return
			}
			var created riskRuleResponse
			decodeBody(t, resp, &created)
			// The response must echo the denomination back, so an
			// operator can see what the rule actually means.
			if created.AssetCode == "" && (created.ThresholdExponent == nil) {
				t.Fatalf("expected the created rule to carry a denomination, got %+v", created)
			}
			if created.AssetCode != "" && created.ThresholdExponent != nil {
				t.Fatalf("an asset-scoped rule must not also carry a threshold_exponent, got %+v", created)
			}
		})
	}
}
