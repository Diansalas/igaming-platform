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
