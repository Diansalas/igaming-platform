//go:build integration

// Stage 5 (Operator Back Office MVP): HTTP-level tests for the new
// read/query API surface plus the one new mutation (reinstate) this stage
// adds - tenant list/detail, brand list/detail, the improved paginated
// player list, player reinstate, the platform-scoped audit log, and
// pagination on the pre-existing tenant-scoped audit log. Mirrors this
// package's existing conventions exactly (real Postgres, real tokens, the
// actual route table - see tenant_licence_admin_integration_test.go and
// asset_registry_admin_test.go for the precedents this file follows).
package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// mustGetLatestTenantAuditMetadata reads the most recent tenant-scoped
// audit_log row for (tenantID, action, targetID) and decodes its metadata
// JSONB - the tenant-scoped counterpart to casino_flow_integration_test.go's
// mustGetLatestAuditMetadata (which is platform-scoped only).
func mustGetLatestTenantAuditMetadata(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action, targetID string) map[string]any {
	t.Helper()
	var raw []byte
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3
			 ORDER BY created_at DESC LIMIT 1`,
			tenantID, action, targetID,
		).Scan(&raw)
	})
	if err != nil {
		t.Fatalf("query tenant audit_log metadata for %s/%s: %v", action, targetID, err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("unmarshal audit metadata: %v", err)
	}
	return metadata
}

// --- Tenant list/detail ---

func TestListTenantsAPI_PlatformAdminSearchAndPagination(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-list-tenants-1")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-list-tenants-1").AccessToken

	suffix := uuid.NewString()[:8]
	create := func(name, slug string) tenantResponse {
		resp := sendAssetJSON(t, srv.URL+"/v1/admin/tenants", http.MethodPost, token, map[string]any{
			"name": name, "slug": slug, "licensing_model": "under_platform_licence", "reason_code": "test-setup",
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 creating tenant fixture %q, got %d", name, resp.StatusCode)
		}
		var tr tenantResponse
		decodeBody(t, resp, &tr)
		return tr
	}

	target := create("Backoffice Search Target "+suffix, "bo-target-"+suffix)
	create("Backoffice Unrelated "+suffix, "bo-other-"+suffix)

	resp := getJSON(t, srv, "/v1/admin/tenants?q="+url.QueryEscape("Search Target "+suffix), token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var page pagedResponse[tenantResponse]
	decodeBody(t, resp, &page)
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != target.ID {
		t.Fatalf("expected exactly the matching tenant, got %+v", page)
	}
	if page.Limit != defaultPageLimit || page.Offset != 0 {
		t.Errorf("expected default limit/offset in the envelope, got limit=%d offset=%d", page.Limit, page.Offset)
	}

	// status filter: both fixtures are 'active' by default - filtering by a
	// status neither has must return zero, proving the filter is applied
	// (not silently ignored).
	resp = getJSON(t, srv, "/v1/admin/tenants?status=suspended&q="+url.QueryEscape(suffix), token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var suspendedPage pagedResponse[tenantResponse]
	decodeBody(t, resp, &suspendedPage)
	if suspendedPage.Total != 0 {
		t.Errorf("expected zero 'suspended' tenants among this test's active fixtures, got %d", suspendedPage.Total)
	}
}

func TestListTenantsAPI_TenantAdminDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	ta := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-list-tenants-1")
	token := mustLoginStaff(t, srv, tenant.Slug, ta.Email, "ta-list-tenants-1").AccessToken

	resp := getJSON(t, srv, "/v1/admin/tenants", token)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a tenant-scoped caller listing tenants, got %d", resp.StatusCode)
	}
}

func TestGetTenantAPI_AuthorizationScoping(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)

	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-get-tenant-1")
	adminToken := mustLoginStaff(t, srv, "", admin.Email, "pa-get-tenant-1").AccessToken

	taA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleTenantAdmin, "ta-get-tenant-1")
	tokenA := mustLoginStaff(t, srv, tenantA.Slug, taA.Email, "ta-get-tenant-1").AccessToken

	// platform_admin can read any tenant.
	for _, tgt := range []identity.Tenant{tenantA, tenantB} {
		resp := getJSON(t, srv, "/v1/admin/tenants/"+tgt.ID.String(), adminToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 for platform_admin reading tenant %s, got %d", tgt.ID, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// tenant_admin can read its own.
	resp := getJSON(t, srv, "/v1/admin/tenants/"+tenantA.ID.String(), tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for tenant_admin reading its own tenant, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// tenant_admin cannot read a different tenant.
	resp = getJSON(t, srv, "/v1/admin/tenants/"+tenantB.ID.String(), tokenA)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for tenant_admin reading a different tenant, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown tenant id is a controlled 404.
	resp = getJSON(t, srv, "/v1/admin/tenants/"+uuid.NewString(), adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown tenant id, got %d", resp.StatusCode)
	}
}

// --- Brand list/detail ---

func TestListBrandsAPI_AuthorizationAndPagination(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	brandA1 := mustCreateBrand(t, pool, tenantA)
	brandA2 := mustCreateBrand(t, pool, tenantA)
	mustCreateBrand(t, pool, tenantB)

	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-list-brands-1")
	adminToken := mustLoginStaff(t, srv, "", admin.Email, "pa-list-brands-1").AccessToken

	taA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleTenantAdmin, "ta-list-brands-1")
	tokenA := mustLoginStaff(t, srv, tenantA.Slug, taA.Email, "ta-list-brands-1").AccessToken

	taB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleTenantAdmin, "ta-list-brands-2")
	tokenB := mustLoginStaff(t, srv, tenantB.Slug, taB.Email, "ta-list-brands-2").AccessToken

	// platform_admin can list tenant A's brands, and only tenant A's.
	resp := getJSON(t, srv, "/v1/admin/tenants/"+tenantA.ID.String()+"/brands", adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for platform_admin listing tenant A's brands, got %d", resp.StatusCode)
	}
	var page pagedResponse[brandResponse]
	decodeBody(t, resp, &page)
	if page.Total != 2 {
		t.Fatalf("expected exactly 2 brands for tenant A, got total=%d items=%+v", page.Total, page.Items)
	}
	ids := map[string]bool{}
	for _, b := range page.Items {
		ids[b.ID] = true
		if b.TenantID != tenantA.ID.String() {
			t.Errorf("expected every returned brand to belong to tenant A, got tenant_id=%s", b.TenantID)
		}
	}
	if !ids[brandA1.ID.String()] || !ids[brandA2.ID.String()] {
		t.Errorf("expected both of tenant A's brands in the list, got %+v", page.Items)
	}

	// tenant A's own admin can list its own.
	resp = getJSON(t, srv, "/v1/admin/tenants/"+tenantA.ID.String()+"/brands", tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for tenant A's own admin, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// tenant B's admin cannot list tenant A's brands.
	resp = getJSON(t, srv, "/v1/admin/tenants/"+tenantA.ID.String()+"/brands", tokenB)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for tenant B's admin listing tenant A's brands, got %d", resp.StatusCode)
	}
}

func TestGetBrandAPI_CrossTenantBrandIDNotFound(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)

	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-get-brand-1")
	adminToken := mustLoginStaff(t, srv, "", admin.Email, "pa-get-brand-1").AccessToken

	// brandA under its correct tenant's path: 200.
	resp := getJSON(t, srv, "/v1/admin/tenants/"+tenantA.ID.String()+"/brands/"+brandA.ID.String(), adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The SAME brand id under tenant B's path must be 404: brand_public_read
	// is USING (true) (migration 0008), so the row is otherwise readable -
	// the handler's own tenant-match check is what must catch this, never
	// RLS alone.
	resp = getJSON(t, srv, "/v1/admin/tenants/"+tenantB.ID.String()+"/brands/"+brandA.ID.String(), adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for a brand id under the wrong tenant's path, got %d", resp.StatusCode)
	}

	// Tenant B's own admin, using its own (correctly-authorized) tenant
	// scope, still cannot reach tenant A's brand via a mismatched pairing.
	taB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleTenantAdmin, "ta-get-brand-1")
	tokenB := mustLoginStaff(t, srv, tenantB.Slug, taB.Email, "ta-get-brand-1").AccessToken
	resp = getJSON(t, srv, "/v1/admin/tenants/"+tenantB.ID.String()+"/brands/"+brandA.ID.String(), tokenB)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// --- Player list: pagination, search, status filter, tenant isolation ---

func TestListPlayersAPI_SearchStatusFilterAndPagination(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	suffix := uuid.NewString()[:8]
	targetEmail := "search-target-" + suffix + "@example.com"
	var targetID uuid.UUID
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := identity.RegisterPlayer(ctx, tx, brand, targetEmail, "hash")
		targetID = account.ID
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed target player: %v", err)
	}
	for i := 0; i < 2; i++ {
		email := "other-" + uuid.NewString() + "@example.com"
		err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := identity.RegisterPlayer(ctx, tx, brand, email, "hash")
			return err
		})
		if err != nil {
			t.Fatalf("failed to seed unrelated player: %v", err)
		}
	}

	admin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-list-players-1")
	token := mustLoginStaff(t, srv, tenant.Slug, admin.Email, "ta-list-players-1").AccessToken

	// q filter: only the target email matches.
	resp := getJSON(t, srv, "/v1/admin/players?q="+url.QueryEscape("search-target-"+suffix), token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var page pagedResponse[playerAccountResponse]
	decodeBody(t, resp, &page)
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != targetID.String() {
		t.Fatalf("expected exactly the matching player, got %+v", page)
	}

	// status filter: suspend the target, then filter by status=suspended.
	resp = postJSON(t, srv, "/v1/admin/players/"+targetID.String()+"/suspend", token, map[string]string{"reason": "test"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 suspending the target, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = getJSON(t, srv, "/v1/admin/players?status=suspended", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var suspendedPage pagedResponse[playerAccountResponse]
	decodeBody(t, resp, &suspendedPage)
	found := false
	for _, p := range suspendedPage.Items {
		if p.Status != "suspended" {
			t.Errorf("expected only suspended players in this filtered result, got status=%q", p.Status)
		}
		if p.ID == targetID.String() {
			found = true
		}
	}
	if !found {
		t.Error("expected the suspended target player in the status=suspended filter result")
	}

	// pagination: limit=1 returns exactly 1 item; total still reflects all
	// 3 seeded players.
	resp = getJSON(t, srv, "/v1/admin/players?limit=1&offset=0", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var pagedOne pagedResponse[playerAccountResponse]
	decodeBody(t, resp, &pagedOne)
	if len(pagedOne.Items) != 1 {
		t.Fatalf("expected exactly 1 item with limit=1, got %d", len(pagedOne.Items))
	}
	if pagedOne.Limit != 1 || pagedOne.Offset != 0 {
		t.Errorf("expected envelope limit=1 offset=0, got limit=%d offset=%d", pagedOne.Limit, pagedOne.Offset)
	}
	if pagedOne.Total < 3 {
		t.Errorf("expected total >= 3 seeded players, got %d", pagedOne.Total)
	}
}

// TestListPlayersAPI_TenantIsolation proves a tenant B staff token cannot
// see tenant A's players through the list endpoint, even with a matching
// ?q= - the request must be correctly scoped, not merely denied outright
// (unlike the single-player-read/suspend cross-tenant cases, which return
// 404; a LIST endpoint's correct cross-tenant behavior is an EMPTY page,
// per this stage's directive).
func TestListPlayersAPI_TenantIsolation(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	tenantB := mustCreateTenant(t, pool)

	suffix := uuid.NewString()[:8]
	email := "isolation-" + suffix + "@example.com"
	err := pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := identity.RegisterPlayer(ctx, tx, brandA, email, "hash")
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed tenant A's player: %v", err)
	}

	staffB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleTenantAdmin, "ta-isolation-1")
	tokenB := mustLoginStaff(t, srv, tenantB.Slug, staffB.Email, "ta-isolation-1").AccessToken

	resp := getJSON(t, srv, "/v1/admin/players?q="+url.QueryEscape("isolation-"+suffix), tokenB)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var page pagedResponse[playerAccountResponse]
	decodeBody(t, resp, &page)
	if page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("expected tenant B to see zero of tenant A's players even with a matching q, got %+v", page)
	}
}

// --- Reinstate ---

func TestReinstatePlayerAPI_HappyPathAndAuditRow(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	var playerID uuid.UUID
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := identity.RegisterPlayer(ctx, tx, brand, "reinstate-"+uuid.NewString()+"@example.com", "hash")
		playerID = account.ID
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed player: %v", err)
	}

	admin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-reinstate-1")
	token := mustLoginStaff(t, srv, tenant.Slug, admin.Email, "ta-reinstate-1").AccessToken

	resp := postJSON(t, srv, "/v1/admin/players/"+playerID.String()+"/suspend", token, map[string]string{"reason": "setup"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 suspending, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = postJSON(t, srv, "/v1/admin/players/"+playerID.String()+"/reinstate", token, map[string]string{"reason_code": "appeal-approved"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 reinstating a suspended player, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = getJSON(t, srv, "/v1/admin/players/"+playerID.String(), token)
	var playerResp playerAccountResponse
	decodeBody(t, resp, &playerResp)
	if playerResp.Status != string(identity.PlayerStatusActive) {
		t.Errorf("expected status 'active' after reinstate, got %q", playerResp.Status)
	}

	metadata := mustGetLatestTenantAuditMetadata(t, pool, tenant.ID, "player.reinstated", playerID.String())
	if metadata["reason_code"] != "appeal-approved" {
		t.Errorf("expected audit metadata reason_code 'appeal-approved', got %v", metadata["reason_code"])
	}
	if metadata["before_status"] != "suspended" {
		t.Errorf("expected audit metadata before_status 'suspended', got %v", metadata["before_status"])
	}
	if metadata["after_status"] != "active" {
		t.Errorf("expected audit metadata after_status 'active', got %v", metadata["after_status"])
	}

	// Confirm the actor recorded on the row is the acting tenant_admin.
	respAudit := getJSON(t, srv, "/v1/admin/audit-log?action=player.reinstated", token)
	var page pagedResponse[auditEntryResponse]
	decodeBody(t, respAudit, &page)
	var entry auditEntryResponse
	found := false
	for _, e := range page.Items {
		if e.TargetID == playerID.String() {
			entry = e
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a player.reinstated audit_log entry for %s", playerID)
	}
	if entry.ActorID != admin.ID.String() {
		t.Errorf("expected actor %s, got %s", admin.ID, entry.ActorID)
	}
	if entry.Outcome != "success" {
		t.Errorf("expected outcome success, got %q", entry.Outcome)
	}
}

func TestReinstatePlayerAPI_NotSuspendedConflict(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	var playerID uuid.UUID
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := identity.RegisterPlayer(ctx, tx, brand, "reinstate-conflict-"+uuid.NewString()+"@example.com", "hash")
		playerID = account.ID
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed player: %v", err)
	}

	admin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-reinstate-conflict-1")
	token := mustLoginStaff(t, srv, tenant.Slug, admin.Email, "ta-reinstate-conflict-1").AccessToken

	// player is 'pending_verification', not 'suspended'.
	resp := postJSON(t, srv, "/v1/admin/players/"+playerID.String()+"/reinstate", token, map[string]string{"reason_code": "attempt"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 reinstating a non-suspended player, got %d", resp.StatusCode)
	}
}

func TestReinstatePlayerAPI_MissingReasonCodeRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	var playerID uuid.UUID
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := identity.RegisterPlayer(ctx, tx, brand, "reinstate-noreason-"+uuid.NewString()+"@example.com", "hash")
		playerID = account.ID
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed player: %v", err)
	}

	admin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-reinstate-noreason-1")
	token := mustLoginStaff(t, srv, tenant.Slug, admin.Email, "ta-reinstate-noreason-1").AccessToken

	resp := postJSON(t, srv, "/v1/admin/players/"+playerID.String()+"/reinstate", token, map[string]string{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing reason_code, got %d", resp.StatusCode)
	}
}

// TestReinstatePlayerAPI_SupportDenied proves reinstate is gated by the
// SAME permission (PermPlayerSuspend) as suspend, not the broader
// PermPlayerRead every staff role holds - support holds PermPlayerRead but
// not PermPlayerSuspend.
func TestReinstatePlayerAPI_SupportDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	var playerID uuid.UUID
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := identity.RegisterPlayer(ctx, tx, brand, "reinstate-support-"+uuid.NewString()+"@example.com", "hash")
		playerID = account.ID
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed player: %v", err)
	}

	support := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleSupport, "support-reinstate-1")
	token := mustLoginStaff(t, srv, tenant.Slug, support.Email, "support-reinstate-1").AccessToken

	resp := postJSON(t, srv, "/v1/admin/players/"+playerID.String()+"/reinstate", token, map[string]string{"reason_code": "attempt"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for support attempting reinstate, got %d", resp.StatusCode)
	}
}

// TestReinstatePlayerAPI_CrossTenantNotFound proves a tenant B admin cannot
// reinstate tenant A's player, even with the exact id - mirrors
// TestCrossTenantPlayerAccess_Denied's own suspend case exactly (RLS-
// filtered 404, not a data leak).
func TestReinstatePlayerAPI_CrossTenantNotFound(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	tenantB := mustCreateTenant(t, pool)

	var playerAID uuid.UUID
	err := pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := identity.RegisterPlayer(ctx, tx, brandA, "reinstate-cross-"+uuid.NewString()+"@example.com", "hash")
		if err != nil {
			return err
		}
		playerAID = account.ID
		return identity.SetPlayerAccountStatus(ctx, tx, account.ID, identity.PlayerStatusSuspended)
	})
	if err != nil {
		t.Fatalf("failed to seed suspended player: %v", err)
	}

	staffB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleTenantAdmin, "staffb-reinstate-1")
	tokenB := mustLoginStaff(t, srv, tenantB.Slug, staffB.Email, "staffb-reinstate-1").AccessToken

	resp := postJSON(t, srv, "/v1/admin/players/"+playerAID.String()+"/reinstate", tokenB, map[string]string{"reason_code": "attempt"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-tenant reinstate attempt, got %d", resp.StatusCode)
	}
}

// --- Platform-scoped audit log, and tenant-scoped audit log pagination ---

func TestPlatformAuditLogAPI_PlatformAdminSuccess(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-platform-audit-1")
	token := mustLoginStaff(t, srv, "", admin.Email, "pa-platform-audit-1").AccessToken

	// tenant.created is a platform-scoped (tenant_id IS NULL) audit action.
	resp := sendAssetJSON(t, srv.URL+"/v1/admin/tenants", http.MethodPost, token, map[string]any{
		"name": "Platform Audit Target", "slug": "pa-audit-target-" + uuid.NewString()[:8],
		"licensing_model": "under_platform_licence", "reason_code": "test-setup",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating tenant fixture, got %d", resp.StatusCode)
	}
	var tr tenantResponse
	decodeBody(t, resp, &tr)

	resp = getJSON(t, srv, "/v1/admin/platform/audit-log?action=tenant.created", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var page pagedResponse[auditEntryResponse]
	decodeBody(t, resp, &page)
	found := false
	for _, e := range page.Items {
		if e.Action != "tenant.created" {
			t.Errorf("expected only action=tenant.created rows in the filtered result, got %q", e.Action)
		}
		if e.TargetID == tr.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the platform audit log to contain the tenant.created row for %s", tr.ID)
	}
}

func TestPlatformAuditLogAPI_TenantAdminDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	ta := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-platform-audit-1")
	token := mustLoginStaff(t, srv, tenant.Slug, ta.Email, "ta-platform-audit-1").AccessToken

	resp := getJSON(t, srv, "/v1/admin/platform/audit-log", token)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a tenant-scoped caller reading the platform audit log, got %d", resp.StatusCode)
	}
}

func TestPlatformAuditLogAPI_PlayerDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	email := "player-platform-audit-" + uuid.NewString() + "@example.com"
	resp := postJSON(t, srv, "/v1/auth/register", "", map[string]string{
		"brand_slug": brand.Slug, "email": email, "password": "a-decent-password-1",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 registering player fixture, got %d", resp.StatusCode)
	}
	var tokens tokenPairResponse
	decodeBody(t, resp, &tokens)

	resp = getJSON(t, srv, "/v1/admin/platform/audit-log", tokens.AccessToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a player token, got %d", resp.StatusCode)
	}
}

// TestAuditLogAPIs_ScopeIsolation proves each audit endpoint reads ONLY its
// own scope: the tenant-scoped route never returns platform-level
// (tenant_id IS NULL) rows, and the platform-scoped route never returns a
// tenant-scoped row.
func TestAuditLogAPIs_ScopeIsolation(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	ta := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-scope-iso-1")
	tenantToken := mustLoginStaff(t, srv, tenant.Slug, ta.Email, "ta-scope-iso-1").AccessToken

	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-scope-iso-1")
	platformToken := mustLoginStaff(t, srv, "", admin.Email, "pa-scope-iso-1").AccessToken

	// tenant.created is ALWAYS platform-scoped - the tenant's own audit-log
	// route must never surface it, even unfiltered.
	resp := getJSON(t, srv, "/v1/admin/audit-log?action=tenant.created", tenantToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var tenantPage pagedResponse[auditEntryResponse]
	decodeBody(t, resp, &tenantPage)
	if tenantPage.Total != 0 {
		t.Errorf("expected zero tenant.created rows via the tenant-scoped audit log, got %d", tenantPage.Total)
	}

	// brand.created is tenant-scoped - create one via the audited HTTP
	// endpoint, then prove the PLATFORM audit log never surfaces it.
	resp = sendAssetJSON(t, srv.URL+"/v1/admin/tenants/"+tenant.ID.String()+"/brands", http.MethodPost, tenantToken, map[string]any{
		"name": "Scope Isolation Brand", "slug": "scope-iso-brand-" + uuid.NewString()[:8],
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating brand fixture, got %d", resp.StatusCode)
	}
	var br brandResponse
	decodeBody(t, resp, &br)

	// Sanity: the tenant's OWN audit log DOES contain it (proves the
	// filter/fixture setup is not simply vacuous).
	resp = getJSON(t, srv, "/v1/admin/audit-log?action=brand.created", tenantToken)
	var ownPage pagedResponse[auditEntryResponse]
	decodeBody(t, resp, &ownPage)
	ownFound := false
	for _, e := range ownPage.Items {
		if e.TargetID == br.ID {
			ownFound = true
		}
	}
	if !ownFound {
		t.Fatalf("expected the tenant's own audit log to contain its brand.created row for %s", br.ID)
	}

	resp = getJSON(t, srv, "/v1/admin/platform/audit-log?action=brand.created", platformToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var platformPage pagedResponse[auditEntryResponse]
	decodeBody(t, resp, &platformPage)
	for _, e := range platformPage.Items {
		if e.TargetID == br.ID {
			t.Errorf("expected the platform audit log to never contain a tenant-scoped brand.created row, found one for %s", br.ID)
		}
	}
}

func TestTenantAuditLogAPI_Pagination(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	ta := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-audit-page-1")
	token := mustLoginStaff(t, srv, tenant.Slug, ta.Email, "ta-audit-page-1").AccessToken

	var playerID uuid.UUID
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := identity.RegisterPlayer(ctx, tx, brand, "audit-page-"+uuid.NewString()+"@example.com", "hash")
		playerID = account.ID
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed player: %v", err)
	}

	for i := 0; i < 3; i++ {
		resp := postJSON(t, srv, "/v1/admin/players/"+playerID.String()+"/suspend", token, map[string]string{"reason": "cycle"})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("expected 204 suspending (cycle %d), got %d", i, resp.StatusCode)
		}
		resp.Body.Close()
		resp = postJSON(t, srv, "/v1/admin/players/"+playerID.String()+"/reinstate", token, map[string]string{"reason_code": "cycle"})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("expected 204 reinstating (cycle %d), got %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}

	resp := getJSON(t, srv, "/v1/admin/audit-log?limit=2&offset=0", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var page pagedResponse[auditEntryResponse]
	decodeBody(t, resp, &page)
	if len(page.Items) != 2 {
		t.Fatalf("expected exactly 2 items with limit=2, got %d", len(page.Items))
	}
	if page.Limit != 2 || page.Offset != 0 {
		t.Errorf("expected envelope limit=2 offset=0, got limit=%d offset=%d", page.Limit, page.Offset)
	}
	// 3 suspends + 3 reinstates + at least 1 player.registered.
	if page.Total < 7 {
		t.Errorf("expected total >= 7, got %d", page.Total)
	}

	// actor_type/outcome filters, exact match.
	resp = getJSON(t, srv, "/v1/admin/audit-log?actor_type=staff&outcome=success&action=player.suspended", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var filtered pagedResponse[auditEntryResponse]
	decodeBody(t, resp, &filtered)
	if filtered.Total != 3 {
		t.Fatalf("expected exactly 3 player.suspended/staff/success rows, got %d", filtered.Total)
	}
	for _, e := range filtered.Items {
		if e.ActorType != "staff" || e.Outcome != "success" || e.Action != "player.suspended" {
			t.Errorf("expected every filtered row to match all three filters exactly, got %+v", e)
		}
	}
}
