//go:build integration

// Stage 5 (Operator Back Office MVP): the staff-scoped, read-only
// withdrawal history/detail views this task adds
// (newListAdminWithdrawalsHandler / newGetAdminWithdrawalHandler,
// withdrawal_handlers.go). These are purely additive on top of the
// FROZEN withdrawal state machine/four-eyes controls exercised elsewhere
// in this package (stage3c_*/stage3d_*/financial_flow_integration_test.go) -
// this file never calls approve/reject/submit for any reason other than
// to produce a fixture in a specific state to list/read back, and never
// touches the pending-review queue's own promotion side effect beyond
// what's needed to get a fixture to `pending_review`.
package httpserver

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// configureLenientWithdrawalPolicy sets a per-tenant/asset policy whose
// threshold sits comfortably above every amount this file's tests use, so
// a single finance approval is enough to reach `approved` - the same
// pattern financial_flow_integration_test.go's own happy-path test and
// stage3d_withdrawal_governance_test.go's submit test use to escape the
// zero-config fail-closed default (always 2 approvals - policy.go).
func configureLenientWithdrawalPolicy(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, effective_from)
			 VALUES ($1, 'EUR', 1000000, 2, now() - interval '1 hour')`,
			tenantID,
		)
		return err
	}); err != nil {
		t.Fatalf("configure withdrawal policy: %v", err)
	}
}

// --- (a) Authorized staff request: correct shape/pagination/filtering ---

func TestAdminListWithdrawals_FullHistoryPaginatedAndFiltered(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	configureLenientWithdrawalPolicy(t, pool, tenant.ID)

	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "finance-hist-pw-1")
	financeToken := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "finance-hist-pw-1")

	player := mustRegisterPlayer(t, srv, brand.Slug)
	walletID := fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100000).ID

	wr1 := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletID, "EUR", 1000)
	wr2 := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletID, "EUR", 2000)
	wr3 := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletID, "EUR", 3000)

	// Before any staff action: all three are `requested` and visible in the
	// full history view (unlike the pending-only queue, which would show
	// none of them yet).
	resp := getJSON(t, srv, "/v1/admin/withdrawals/history", financeToken.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing full withdrawal history, got %d", resp.StatusCode)
	}
	var page pagedResponse[staffWithdrawalResponse]
	decodeBody(t, resp, &page)
	if page.Total != 3 {
		t.Fatalf("expected total=3 before any staff action, got %d (items=%d)", page.Total, len(page.Items))
	}
	if page.Limit != defaultPageLimit || page.Offset != 0 {
		t.Errorf("expected default limit=%d offset=0, got limit=%d offset=%d", defaultPageLimit, page.Limit, page.Offset)
	}
	for _, item := range page.Items {
		if item.State != string(withdrawal.StateRequested) {
			t.Errorf("expected every fresh request to be 'requested', got %q for %s", item.State, item.ID)
		}
		if item.PlayerAccountID != player.ID.String() {
			t.Errorf("expected player_account_id=%s, got %q", player.ID, item.PlayerAccountID)
		}
	}

	// Open the review queue (promotes all three to pending_review), then
	// drive wr1 to `approved` and wr2 to `rejected` via the existing,
	// frozen endpoints - wr3 is left at `pending_review`.
	mustOpenReviewQueue(t, srv, financeToken.AccessToken)

	approveResp := postJSON(t, srv, "/v1/admin/withdrawals/"+wr1.ID.String()+"/approve", financeToken.AccessToken, nil)
	if approveResp.StatusCode != http.StatusOK {
		t.Fatalf("approve wr1: status %d", approveResp.StatusCode)
	}
	approveResp.Body.Close()

	rejectResp := postJSON(t, srv, "/v1/admin/withdrawals/"+wr2.ID.String()+"/reject", financeToken.AccessToken, map[string]string{"reason_code": "test_reject"})
	if rejectResp.StatusCode != http.StatusNoContent {
		t.Fatalf("reject wr2: status %d", rejectResp.StatusCode)
	}
	rejectResp.Body.Close()

	// Full, unfiltered history now shows all three states.
	resp = getJSON(t, srv, "/v1/admin/withdrawals/history", financeToken.AccessToken)
	decodeBody(t, resp, &page)
	if page.Total != 3 {
		t.Fatalf("expected total=3 after staff actions, got %d", page.Total)
	}
	states := map[uuid.UUID]string{}
	for _, item := range page.Items {
		id, err := uuid.Parse(item.ID)
		if err != nil {
			t.Fatalf("parse item id: %v", err)
		}
		states[id] = item.State
	}
	if states[wr1.ID] != string(withdrawal.StateApproved) {
		t.Errorf("expected wr1 state 'approved', got %q", states[wr1.ID])
	}
	if states[wr2.ID] != string(withdrawal.StateRejected) {
		t.Errorf("expected wr2 state 'rejected', got %q", states[wr2.ID])
	}
	if states[wr3.ID] != string(withdrawal.StatePendingReview) {
		t.Errorf("expected wr3 state 'pending_review', got %q", states[wr3.ID])
	}

	// ?status= filtering across the full range - not just pending_review.
	for _, tc := range []struct {
		status string
		wantID uuid.UUID
	}{
		{"approved", wr1.ID},
		{"rejected", wr2.ID},
		{"pending_review", wr3.ID},
	} {
		resp := getJSON(t, srv, "/v1/admin/withdrawals/history?status="+tc.status, financeToken.AccessToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%s: expected 200, got %d", tc.status, resp.StatusCode)
		}
		var filtered pagedResponse[staffWithdrawalResponse]
		decodeBody(t, resp, &filtered)
		if filtered.Total != 1 || len(filtered.Items) != 1 {
			t.Fatalf("status=%s: expected exactly 1 matching row, got total=%d items=%d", tc.status, filtered.Total, len(filtered.Items))
		}
		if filtered.Items[0].ID != tc.wantID.String() {
			t.Errorf("status=%s: expected id=%s, got %s", tc.status, tc.wantID, filtered.Items[0].ID)
		}
	}

	// A status with no matching rows in this tenant returns an empty,
	// non-null items array with total=0 - never a 404/error.
	resp = getJSON(t, srv, "/v1/admin/withdrawals/history?status=completed", financeToken.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=completed: expected 200, got %d", resp.StatusCode)
	}
	var empty pagedResponse[staffWithdrawalResponse]
	decodeBody(t, resp, &empty)
	if empty.Total != 0 || len(empty.Items) != 0 {
		t.Fatalf("status=completed: expected total=0 and zero items, got total=%d items=%d", empty.Total, len(empty.Items))
	}

	// An invalid status value is a validation error, never silently
	// ignored or passed through to the query.
	resp = getJSON(t, srv, "/v1/admin/withdrawals/history?status=not_a_real_state", financeToken.AccessToken)
	if resp.StatusCode != http.StatusBadRequest {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 400 for an invalid status filter, got %d: %+v", resp.StatusCode, apiErr)
	}
	resp.Body.Close()

	// Pagination: limit=1 pages through all three rows (most-recently-
	// requested first - wr3, wr2, wr1) without ever repeating or skipping
	// one, and `total` always reflects the FULL unfiltered count of 3,
	// not the page size.
	seen := map[string]bool{}
	for offset := 0; offset < 3; offset++ {
		resp := getJSON(t, srv, "/v1/admin/withdrawals/history?limit=1&offset="+strconv.Itoa(offset), financeToken.AccessToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("offset=%d: expected 200, got %d", offset, resp.StatusCode)
		}
		var pg pagedResponse[staffWithdrawalResponse]
		decodeBody(t, resp, &pg)
		if pg.Limit != 1 || pg.Offset != offset || pg.Total != 3 {
			t.Fatalf("offset=%d: expected limit=1 offset=%d total=3, got limit=%d offset=%d total=%d", offset, offset, pg.Limit, pg.Offset, pg.Total)
		}
		if len(pg.Items) != 1 {
			t.Fatalf("offset=%d: expected exactly 1 item, got %d", offset, len(pg.Items))
		}
		seen[pg.Items[0].ID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("expected paging with limit=1 to visit all 3 distinct withdrawals, saw %d distinct ids", len(seen))
	}
}

// --- (a, continued) Detail view for a single withdrawal ---

func TestAdminGetWithdrawal_ReturnsDetailForAnyPlayerInTenant(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "finance-detail-pw-1")
	financeToken := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "finance-detail-pw-1")

	player := mustRegisterPlayer(t, srv, brand.Slug)
	walletID := fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 50000).ID
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletID, "EUR", 4500)

	resp := getJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String(), financeToken.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 reading withdrawal detail as staff (not the withdrawing player), got %d", resp.StatusCode)
	}
	var detail staffWithdrawalResponse
	decodeBody(t, resp, &detail)
	if detail.ID != wr.ID.String() {
		t.Errorf("expected id=%s, got %s", wr.ID, detail.ID)
	}
	if detail.PlayerAccountID != player.ID.String() {
		t.Errorf("expected player_account_id=%s, got %s", player.ID, detail.PlayerAccountID)
	}
	if detail.Amount != 4500 || detail.AssetCode != "EUR" {
		t.Errorf("expected amount=4500 asset_code=EUR, got amount=%d asset_code=%s", detail.Amount, detail.AssetCode)
	}
	if detail.State != string(withdrawal.StateRequested) {
		t.Errorf("expected state 'requested', got %q", detail.State)
	}

	// A random, non-existent id is a clean 404, not a 500.
	resp = getJSON(t, srv, "/v1/admin/withdrawals/"+uuid.NewString(), financeToken.AccessToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for a non-existent withdrawal id, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- (b) Unauthorized/wrong-permission denied ---

func TestAdminWithdrawalViews_WrongPermissionDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	walletID := fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 20000).ID
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletID, "EUR", 1500)

	randomID := uuid.NewString()

	// Neither support nor compliance nor tenant_admin holds
	// PermWithdrawalReview (internal/auth/permission.go's rolePermissions -
	// only RoleFinance does, post-Stage-3D's permission split).
	for _, role := range []identity.StaffRole{identity.StaffRoleSupport, identity.StaffRoleCompliance, identity.StaffRoleTenantAdmin} {
		staff := mustCreateStaff(t, pool, tenant.ID, role, "wrong-perm-pw-1")
		token := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "wrong-perm-pw-1")

		resp := getJSON(t, srv, "/v1/admin/withdrawals/history", token.AccessToken)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("role=%s: expected 403 listing withdrawal history, got %d", role, resp.StatusCode)
		}
		resp.Body.Close()

		resp = getJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String(), token.AccessToken)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("role=%s: expected 403 reading withdrawal detail, got %d", role, resp.StatusCode)
		}
		resp.Body.Close()

		resp = getJSON(t, srv, "/v1/admin/withdrawals/"+randomID, token.AccessToken)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("role=%s: expected 403 (permission denial, not a 404 leak) reading an arbitrary id, got %d", role, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// A player's own bearer token is denied on both admin routes too.
	resp := getJSON(t, srv, "/v1/admin/withdrawals/history", player.Tokens.AccessToken)
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("player token: expected 403/401 listing withdrawal history, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = getJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String(), player.Tokens.AccessToken)
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("player token: expected 403/401 reading withdrawal detail, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// No token at all is unauthorized.
	resp = getJSON(t, srv, "/v1/admin/withdrawals/history", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: expected 401 listing withdrawal history, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// platform_admin's nil-tenant token is denied by RequireTenantScope,
	// exactly like every other tenant-scoped admin route (mirrors
	// TestPlatformAdmin_AdminFinancialRoutesDenied for the existing routes).
	platformAdmin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-view-pw-1")
	platformToken := mustLoginStaff(t, srv, "", platformAdmin.Email, "pa-view-pw-1")
	resp = getJSON(t, srv, "/v1/admin/withdrawals/history", platformToken.AccessToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("platform_admin nil-tenant token: expected 403 listing withdrawal history, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- (c) Cross-tenant denied: list never returns tenant B's rows, and
// direct ID access from tenant A gets a 404, not a leak ---

func TestAdminWithdrawalViews_CrossTenantDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	brandB := mustCreateBrand(t, pool, tenantB)

	financeA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleFinance, "finance-a-view-pw-1")
	financeATokens := mustLoginStaff(t, srv, tenantA.Slug, financeA.Email, "finance-a-view-pw-1")

	// A real withdrawal belonging entirely to tenant B.
	var playerAccountB uuid.UUID
	err := pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := identity.RegisterPlayer(ctx, tx, brandB, "cross-tenant-view@example.com", "hash")
		playerAccountB = account.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed player B: %v", err)
	}
	walletB := fundWallet(t, pool, tenantB.ID, brandB.ID, playerAccountB, "EUR", 10000)
	wrB := mustCreateWithdrawalRequest(t, pool, tenantB.ID, brandB.ID, playerAccountB, walletB.ID, "EUR", 6000)

	// Tenant A's own history is empty - tenant B's request must never
	// appear, even unfiltered.
	resp := getJSON(t, srv, "/v1/admin/withdrawals/history", financeATokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing tenant A's (empty) history, got %d", resp.StatusCode)
	}
	var page pagedResponse[staffWithdrawalResponse]
	decodeBody(t, resp, &page)
	if page.Total != 0 {
		t.Fatalf("expected total=0 for tenant A's history (tenant B has the only withdrawal), got %d", page.Total)
	}
	for _, item := range page.Items {
		if item.ID == wrB.ID.String() {
			t.Fatalf("tenant A's history leaked tenant B's withdrawal %s", wrB.ID)
		}
	}

	// Direct ID access to tenant B's withdrawal from tenant A's finance
	// staff is a 404, not a data leak (and not a 403, which would confirm
	// the row's existence to an unauthorized tenant).
	resp = getJSON(t, srv, "/v1/admin/withdrawals/"+wrB.ID.String(), financeATokens.AccessToken)
	if resp.StatusCode != http.StatusNotFound {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 404 for tenant A's finance staff reading tenant B's withdrawal by id, got %d: %+v", resp.StatusCode, apiErr)
	}
	apiErr := decodeAPIError(t, resp)
	if apiErr.Message == "" {
		t.Fatal("expected a non-empty 404 error message")
	}

	// Tenant B's own finance staff CAN see it - proves the above is a
	// genuine tenant boundary, not a broken route.
	financeB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleFinance, "finance-b-view-pw-1")
	financeBTokens := mustLoginStaff(t, srv, tenantB.Slug, financeB.Email, "finance-b-view-pw-1")
	resp = getJSON(t, srv, "/v1/admin/withdrawals/"+wrB.ID.String(), financeBTokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for tenant B's own finance staff reading tenant B's withdrawal, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = getJSON(t, srv, "/v1/admin/withdrawals/history", financeBTokens.AccessToken)
	decodeBody(t, resp, &page)
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != wrB.ID.String() {
		t.Fatalf("expected tenant B's own history to show exactly its own withdrawal %s, got total=%d items=%v", wrB.ID, page.Total, page.Items)
	}
}
