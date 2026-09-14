//go:build integration

// Stage 4D-RG HTTP-layer tests: player self-service self-exclusion/status,
// and staff/admin restriction create/read authorization. Follows
// casino_flow_integration_test.go's own conventions exactly (fixtures via
// internal packages, bearer tokens minted through the real HTTP
// register/login flow).
package httpserver

import (
	"net/http"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/identity"
)

// --- 1. Player self-service: self-exclude, then see it in their own status ---

func TestRGSelfExclusion_PlayerCanSelfExcludeAndSeesOwnStatus(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	player := mustRegisterPlayer(t, srv, brand.Slug)

	resp := postJSON(t, srv, "/v1/me/rg/self-exclusion", player.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating a self-exclusion, got %d", resp.StatusCode)
	}
	var created map[string]any
	decodeBody(t, resp, &created)
	if created["restriction_type"] != "self_exclusion" || created["scope"] != "platform" || created["indefinite"] != true {
		t.Fatalf("unexpected self-exclusion response: %+v", created)
	}

	resp = getJSON(t, srv, "/v1/me/rg/status", player.Tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 reading own status, got %d", resp.StatusCode)
	}
	var restrictions []map[string]any
	decodeBody(t, resp, &restrictions)
	if len(restrictions) != 1 || restrictions[0]["active"] != true {
		t.Fatalf("expected exactly one active restriction in own status, got %+v", restrictions)
	}
}

// --- 2. A self-excluded player is denied a casino launch over HTTP ---

func TestRGSelfExclusion_BlocksCasinoLaunchOverHTTP(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-rg-launch-pw-1")
	tenantAdminTokens := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-rg-launch-pw-1")
	resp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tenantAdminTokens.AccessToken, validCasinoCapabilityBody())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("failed to register capability: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = postJSON(t, srv, "/v1/me/rg/self-exclusion", player.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 self-excluding, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = postJSON(t, srv, "/v1/me/casino/games/"+game.ID.String()+"/launch", player.Tokens.AccessToken, map[string]string{
		"asset_code": "EUR", "mode": "real",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 launching a game while self-excluded, got %d", resp.StatusCode)
	}
}

// --- 3. Only compliance may create a staff-initiated restriction ---

func TestRGAdminRestriction_OnlyComplianceCanWrite(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	target := mustRegisterPlayer(t, srv, brand.Slug)

	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-rg-pw-1")
	tenantAdminTokens := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-rg-pw-1")

	body := map[string]any{
		"player_account_id": target.ID.String(), "scope": "tenant", "reason_code": "test",
	}
	resp := postJSON(t, srv, "/v1/admin/rg/restrictions", tenantAdminTokens.AccessToken, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for tenant_admin creating an RG restriction, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-rg-pw-1")
	complianceTokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-rg-pw-1")
	resp = postJSON(t, srv, "/v1/admin/rg/restrictions", complianceTokens.AccessToken, body)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected 201 for compliance creating an RG restriction, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- 4. Read requires PermRGRestrictionRead; a support role is denied ---

func TestRGAdminRestriction_ReadRequiresPermission(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	target := mustRegisterPlayer(t, srv, brand.Slug)

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-rg-pw-2")
	complianceTokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-rg-pw-2")
	resp := postJSON(t, srv, "/v1/admin/rg/restrictions", complianceTokens.AccessToken, map[string]any{
		"player_account_id": target.ID.String(), "scope": "tenant", "reason_code": "test",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("failed to seed restriction: %d", resp.StatusCode)
	}
	resp.Body.Close()

	support := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleSupport, "support-rg-pw-1")
	supportTokens := mustLoginStaff(t, srv, tenant.Slug, support.Email, "support-rg-pw-1")
	resp = getJSON(t, srv, "/v1/admin/rg/restrictions?player_account_id="+target.ID.String(), supportTokens.AccessToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for support reading RG restrictions, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-rg-pw-2")
	tenantAdminTokens := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-rg-pw-2")
	resp = getJSON(t, srv, "/v1/admin/rg/restrictions?player_account_id="+target.ID.String(), tenantAdminTokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for tenant_admin reading RG restrictions, got %d", resp.StatusCode)
	}
	var restrictions []map[string]any
	decodeBody(t, resp, &restrictions)
	if len(restrictions) != 1 {
		t.Errorf("expected exactly one restriction, got %+v", restrictions)
	}
}

// --- 5. RolePlayer and RoleFinance tokens are denied on admin RG routes
// over HTTP, not merely at the RBAC-table level (adversarial testing
// specialist review finding: only the pure permission-table unit test
// existed before, never an actual HTTP call proving the route wiring
// itself denies them) ---

func TestRGAdminRestriction_PlayerAndFinanceTokensDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	target := mustRegisterPlayer(t, srv, brand.Slug)

	body := map[string]any{
		"player_account_id": target.ID.String(), "scope": "tenant", "reason_code": "test",
	}

	// A player token (RolePlayer) - denied on both routes.
	resp := postJSON(t, srv, "/v1/admin/rg/restrictions", target.Tokens.AccessToken, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a player token writing an RG restriction, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = getJSON(t, srv, "/v1/admin/rg/restrictions?player_account_id="+target.ID.String(), target.Tokens.AccessToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a player token reading RG restrictions, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A finance token (RoleFinance) - denied on both routes; finance holds
	// withdrawal-decision permissions only, never RG ones.
	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "finance-rg-pw-1")
	financeTokens := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "finance-rg-pw-1")
	resp = postJSON(t, srv, "/v1/admin/rg/restrictions", financeTokens.AccessToken, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a finance token writing an RG restriction, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = getJSON(t, srv, "/v1/admin/rg/restrictions?player_account_id="+target.ID.String(), financeTokens.AccessToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a finance token reading RG restrictions, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- 6. A tenant's compliance staff cannot read/target a DIFFERENT
// tenant's player_account_id via the admin read route ---

func TestRGAdminRestriction_CrossTenantReadNotFound(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	playerA := mustRegisterPlayer(t, srv, brandA.Slug)

	tenantB := mustCreateTenant(t, pool)
	tenantBAdmin := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleTenantAdmin, "tb-admin-rg-pw-1")
	tenantBAdminTokens := mustLoginStaff(t, srv, tenantB.Slug, tenantBAdmin.Email, "tb-admin-rg-pw-1")

	resp := getJSON(t, srv, "/v1/admin/rg/restrictions?player_account_id="+playerA.ID.String(), tenantBAdminTokens.AccessToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for tenant B naming tenant A's own player_account_id, got %d", resp.StatusCode)
	}
}

// --- 7. duration_days over HTTP: valid duration sets a time-bound
// restriction; invalid values (0, negative) are rejected with 400 ---

func TestRGSelfExclusion_DurationDaysOverHTTP(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	brand := mustCreateBrand(t, pool, mustCreateTenant(t, pool))
	player := mustRegisterPlayer(t, srv, brand.Slug)

	resp := postJSON(t, srv, "/v1/me/rg/self-exclusion", player.Tokens.AccessToken, map[string]any{"duration_days": 30})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for a valid duration_days, got %d", resp.StatusCode)
	}
	var created map[string]any
	decodeBody(t, resp, &created)
	if created["indefinite"] != false || created["ends_at"] == nil {
		t.Fatalf("expected a time-bound (non-indefinite) restriction with ends_at set, got %+v", created)
	}

	for _, days := range []int{0, -1} {
		second := mustRegisterPlayer(t, srv, brand.Slug)
		resp := postJSON(t, srv, "/v1/me/rg/self-exclusion", second.Tokens.AccessToken, map[string]any{"duration_days": days})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("duration_days=%d: expected 400, got %d", days, resp.StatusCode)
		}
	}
}

// --- 8. An invalid scope ("platform", or garbage) on the admin write
// route is rejected with 400, never silently coerced or 500'd ---

func TestRGAdminRestriction_InvalidScopeRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	target := mustRegisterPlayer(t, srv, brand.Slug)

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-rg-badscope-pw-1")
	complianceTokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-rg-badscope-pw-1")

	for _, scope := range []string{"platform", "garbage", ""} {
		resp := postJSON(t, srv, "/v1/admin/rg/restrictions", complianceTokens.AccessToken, map[string]any{
			"player_account_id": target.ID.String(), "scope": scope, "reason_code": "test",
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("scope=%q: expected 400, got %d", scope, resp.StatusCode)
		}
	}
}
