//go:build integration

// Stage 4D-RG HTTP-layer tests: player self-service self-exclusion/status,
// and staff/admin restriction create/read authorization. Follows
// casino_flow_integration_test.go's own conventions exactly (fixtures via
// internal packages, bearer tokens minted through the real HTTP
// register/login flow).
package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

// --- 9. Stage 5 Back Office: GET /v1/admin/rg/restrictions with NO
// player_account_id lists every restriction in the tenant, paginated ---

func TestRGAdminRestriction_TenantWideListAuthorizedAndPaginated(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	targetA := mustRegisterPlayer(t, srv, brand.Slug)
	targetB := mustRegisterPlayer(t, srv, brand.Slug)

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-rg-queue-pw-1")
	complianceTokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-rg-queue-pw-1")

	for _, targetID := range []uuid.UUID{targetA.ID, targetB.ID} {
		resp := postJSON(t, srv, "/v1/admin/rg/restrictions", complianceTokens.AccessToken, map[string]any{
			"player_account_id": targetID.String(), "scope": "tenant", "reason_code": "test",
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("failed to seed restriction for %s: %d", targetID, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Unfiltered tenant-wide list: both restrictions visible, paginated
	// envelope shape (never the bare-array shape the player_account_id-
	// scoped path returns).
	resp := getJSON(t, srv, "/v1/admin/rg/restrictions", complianceTokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for compliance listing the tenant-wide RG queue, got %d", resp.StatusCode)
	}
	var page struct {
		Items  []map[string]any `json:"items"`
		Limit  int              `json:"limit"`
		Offset int              `json:"offset"`
		Total  int              `json:"total"`
	}
	decodeBody(t, resp, &page)
	if page.Total != 2 || len(page.Items) != 2 || page.Limit != 50 || page.Offset != 0 {
		t.Fatalf("expected a paginated envelope with 2 total/items, got %+v", page)
	}
	for _, item := range page.Items {
		if item["restriction_type"] != "self_exclusion" || item["scope"] != "tenant" {
			t.Fatalf("unexpected restriction in tenant-wide queue: %+v", item)
		}
	}

	// ?restriction_type=self_exclusion is accepted (the only value the
	// domain currently admits) and still returns both.
	filteredResp := getJSON(t, srv, "/v1/admin/rg/restrictions?restriction_type=self_exclusion", complianceTokens.AccessToken)
	defer filteredResp.Body.Close()
	var filteredPage struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	decodeBody(t, filteredResp, &filteredPage)
	if filteredPage.Total != 2 {
		t.Fatalf("expected restriction_type=self_exclusion to still return 2, got %+v", filteredPage)
	}

	// An unknown restriction_type value is a 400, never invented/ignored.
	badTypeResp := getJSON(t, srv, "/v1/admin/rg/restrictions?restriction_type=not_a_real_type", complianceTokens.AccessToken)
	defer badTypeResp.Body.Close()
	if badTypeResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid restriction_type filter, got %d", badTypeResp.StatusCode)
	}

	// Pagination bounds: limit/offset are honored.
	limitedResp := getJSON(t, srv, "/v1/admin/rg/restrictions?limit=1&offset=1", complianceTokens.AccessToken)
	defer limitedResp.Body.Close()
	var limitedPage struct {
		Items  []map[string]any `json:"items"`
		Limit  int              `json:"limit"`
		Offset int              `json:"offset"`
		Total  int              `json:"total"`
	}
	decodeBody(t, limitedResp, &limitedPage)
	if limitedPage.Limit != 1 || limitedPage.Offset != 1 || limitedPage.Total != 2 || len(limitedPage.Items) != 1 {
		t.Fatalf("expected limit=1/offset=1/total=2/one item, got %+v", limitedPage)
	}
}

// --- 10. The tenant-wide RG list still requires PermRGRestrictionRead -
// a player/finance token is denied exactly like the player_account_id-
// scoped path already is (test 5 above) ---

func TestRGAdminRestriction_TenantWideList_UnauthorizedDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	target := mustRegisterPlayer(t, srv, brand.Slug)

	resp := getJSON(t, srv, "/v1/admin/rg/restrictions", target.Tokens.AccessToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a player token listing the tenant-wide RG queue, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "finance-rg-queue-pw-1")
	financeTokens := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "finance-rg-queue-pw-1")
	resp = getJSON(t, srv, "/v1/admin/rg/restrictions", financeTokens.AccessToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for a finance token listing the tenant-wide RG queue, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- 11. Cross-tenant: tenant B's compliance staff cannot see tenant A's
// restrictions in the tenant-wide queue ---

// TestRGAdminRestriction_TenantWideList_CrossTenantDenied is the
// regression guard for the cross-tenant PII leak internal/rg.go's
// ListRestrictionsForTenant doc comment describes: a naive
// `SELECT * FROM player_restrictions` under WithTenant relies solely on
// staff_and_system_read's RLS policy, whose `tenant_id IS NULL OR
// tenant_id = app.tenant_id` clause is DELIBERATELY broad (a tenant must
// be able to enforce another tenant's player's platform-wide
// self-exclusion) - so a bare list surfaces every PLATFORM-WIDE row on
// the entire platform to any tenant's compliance staff. The join to
// player_accounts closes this. This test must seed a genuinely
// platform-wide (tenant_id IS NULL) restriction via the player's own
// self-exclusion endpoint - a staff-created "scope": "tenant" restriction
// is already filtered by the RLS policy on its own and would let this
// test pass even with the join removed, proving nothing. It must also
// give tenant B its own real restriction, so the isolation assertion is
// "tenant B sees its own 1 row and none of tenant A's", not a vacuous
// "tenant B's empty queue stayed empty" that would pass unconditionally
// against an empty tenant.
func TestRGAdminRestriction_TenantWideList_CrossTenantDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	targetA := mustRegisterPlayer(t, srv, brandA.Slug)

	// Genuinely platform-wide (tenant_id IS NULL): the player's OWN
	// self-exclusion, not a staff-created "scope":"tenant" restriction.
	selfExclResp := postJSON(t, srv, "/v1/me/rg/self-exclusion", targetA.Tokens.AccessToken, map[string]any{})
	if selfExclResp.StatusCode != http.StatusCreated {
		t.Fatalf("failed to seed tenant A player's platform-wide self-exclusion: %d", selfExclResp.StatusCode)
	}
	selfExclResp.Body.Close()

	// Confirm the fixture really produced a NULL-tenant row before
	// asserting anything about isolation - otherwise a broken fixture
	// could silently make this test vacuous again.
	var seededTenantID *uuid.UUID
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT tenant_id FROM player_restrictions WHERE player_account_id = $1`, targetA.ID,
		).Scan(&seededTenantID)
	}); err != nil {
		t.Fatalf("failed to verify seeded restriction's tenant scope: %v", err)
	}
	if seededTenantID != nil {
		t.Fatalf("test fixture bug: expected a platform-wide (tenant_id IS NULL) restriction, got tenant_id=%v", *seededTenantID)
	}

	tenantB := mustCreateTenant(t, pool)
	brandB := mustCreateBrand(t, pool, tenantB)
	targetB := mustRegisterPlayer(t, srv, brandB.Slug)
	selfExclRespB := postJSON(t, srv, "/v1/me/rg/self-exclusion", targetB.Tokens.AccessToken, map[string]any{})
	if selfExclRespB.StatusCode != http.StatusCreated {
		t.Fatalf("failed to seed tenant B player's own self-exclusion: %d", selfExclRespB.StatusCode)
	}
	selfExclRespB.Body.Close()

	complianceB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleCompliance, "compliance-rg-queue-crosstenant-b-pw-1")
	complianceBTokens := mustLoginStaff(t, srv, tenantB.Slug, complianceB.Email, "compliance-rg-queue-crosstenant-b-pw-1")

	resp := getJSON(t, srv, "/v1/admin/rg/restrictions", complianceBTokens.AccessToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for tenant B listing its own RG queue, got %d", resp.StatusCode)
	}
	var page struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	decodeBody(t, resp, &page)
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("expected tenant B to see exactly its own 1 restriction (non-vacuous check), got %+v", page)
	}
	if got := page.Items[0]["id"]; got == nil {
		t.Fatalf("expected the returned restriction to have an id, got %+v", page.Items[0])
	}
}

// --- 12. Regression: supplying ?player_account_id= is COMPLETELY
// unchanged by the Stage 5 tenant-wide addition - still the original bare
// JSON array (never the {"items":...} paginated envelope), still 404 for
// a nonexistent/cross-tenant account, and query params meaningless to
// that path (restriction_type/limit/offset) have no effect on it. ---

func TestRGAdminRestriction_PlayerAccountIDSuppliedBehaviorUnchanged(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	target := mustRegisterPlayer(t, srv, brand.Slug)
	other := mustRegisterPlayer(t, srv, brand.Slug)

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-rg-regression-pw-1")
	complianceTokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-rg-regression-pw-1")

	// Seed a restriction for BOTH players - a bare "?player_account_id=X"
	// list must return exactly one row (X's), never every tenant row,
	// proving the old single-account scoping still applies verbatim.
	for _, targetID := range []uuid.UUID{target.ID, other.ID} {
		seedResp := postJSON(t, srv, "/v1/admin/rg/restrictions", complianceTokens.AccessToken, map[string]any{
			"player_account_id": targetID.String(), "scope": "tenant", "reason_code": "test",
		})
		if seedResp.StatusCode != http.StatusCreated {
			t.Fatalf("failed to seed restriction: %d", seedResp.StatusCode)
		}
		seedResp.Body.Close()
	}

	// Bare array shape, exactly one row, even though limit/offset/
	// restriction_type are also present in the query string (this path
	// never consulted them before Stage 5 and must not start now).
	resp := getJSON(t, srv, "/v1/admin/rg/restrictions?player_account_id="+target.ID.String()+"&limit=1&offset=0&restriction_type=self_exclusion", complianceTokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 reading target's own restrictions, got %d", resp.StatusCode)
	}
	// Decoding directly into a bare []map[string]any (not a struct with an
	// "items" field) is itself part of the proof: the paginated envelope
	// ({"items":...,"limit":...}) would fail to decode into this shape.
	var restrictions []map[string]any
	decodeBody(t, resp, &restrictions)
	if len(restrictions) != 1 {
		t.Fatalf("expected exactly ONE restriction (target's own, not every tenant row), got %+v", restrictions)
	}

	// A nonexistent player_account_id under this tenant still 404s.
	notFoundResp := getJSON(t, srv, "/v1/admin/rg/restrictions?player_account_id="+uuid.NewString(), complianceTokens.AccessToken)
	defer notFoundResp.Body.Close()
	if notFoundResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for a nonexistent player_account_id, got %d", notFoundResp.StatusCode)
	}
}
