//go:build integration

// Stage 5 (Operator Back Office MVP) integration tests for the new bonus
// admin READ surface (bonus_admin_read_handlers.go): campaign list, the
// change-request approval queue, and grant list (tenant-wide or
// per-player). Mirrors bonus_governance_flow_integration_test.go's own
// fixture conventions (seedActiveBonusPlayer/seedBonusCampaignForHTTP/
// seedBonusOfferForHTTP, defined there, same package).
package httpserver

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// seedBonusGrantForHTTP inserts a bonus_grants row directly (bypassing
// the full issue/activate T.1 gate pipeline, mirroring this file's own
// sibling fixtures' "fixtures via the domain package" convention) under
// an existing offer/campaign, in the given status. triggerRef must be
// unique per (campaign, offer version, player) to satisfy the DB's own
// idempotency uniqueness constraint (grant.go's own doc comment).
func seedBonusGrantForHTTP(t *testing.T, pool *db.Pool, tenantID, brandID, playerID, walletID, campaignID, campaignVersionID, offerID, offerVersionID, staffID uuid.UUID, triggerRef string, status bonus.GrantStatus) bonus.Grant {
	t.Helper()
	var grant bonus.Grant
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g, err := bonus.CreateGrant(ctx, tx, bonus.Grant{
			TenantID: tenantID, BrandID: brandID, PlayerAccountID: playerID, WalletID: walletID,
			CampaignID: campaignID, CampaignVersionID: campaignVersionID, OfferID: offerID, OfferVersionID: offerVersionID,
			AssetCode: "USD", DecimalExponent: 2,
			FundingSource: "operator", FulfillmentDestination: bonus.FulfillmentIntoPlatformWallet, FulfillmentOwner: "internal",
			TriggerReference: triggerRef, CreatedByActorType: bonus.ActorStaff, CreatedByActorID: staffID,
		})
		if err != nil {
			return err
		}
		if status != bonus.GrantIssued {
			g, err = bonus.UpdateGrantStatus(ctx, tx, tenantID, g.ID, bonus.GrantIssued, status, time.Now().UTC())
			if err != nil {
				return err
			}
		}
		grant = g
		return nil
	})
	if err != nil {
		t.Fatalf("seed bonus grant: %v", err)
	}
	return grant
}

// --- GET /v1/admin/bonus/campaigns ---

func TestListCampaigns_HTTP(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	promo := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-read-1")
	token := mustLoginStaff(t, srv, tenant.Slug, promo.Email, "promo-read-1")

	draftID := seedBonusCampaignForHTTP(t, pool, tenant.ID, brand.ID, promo.ID)
	var activeID uuid.UUID
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := bonus.CreateCampaign(ctx, tx, bonus.Campaign{TenantID: tenant.ID, BrandID: &brand.ID, Status: bonus.CampaignActive, CreatedByActorType: bonus.ActorStaff, CreatedByActorID: promo.ID})
		activeID = c.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed active campaign: %v", err)
	}

	// (a) authorized request succeeds with correct shape/pagination.
	resp := getJSON(t, srv, "/v1/admin/bonus/campaigns", token.AccessToken)
	var page pagedResponse[campaignResponse]
	decodeBody(t, resp, &page)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if page.Total < 2 || len(page.Items) < 2 {
		t.Fatalf("expected at least 2 campaigns, got total=%d items=%d", page.Total, len(page.Items))
	}
	if page.Limit != defaultPageLimit || page.Offset != 0 {
		t.Fatalf("expected default pagination limit=%d offset=0, got limit=%d offset=%d", defaultPageLimit, page.Limit, page.Offset)
	}
	var sawDraft, sawActive bool
	for _, c := range page.Items {
		if c.ID == draftID.String() {
			sawDraft = true
		}
		if c.ID == activeID.String() {
			sawActive = true
			if c.Status != string(bonus.CampaignActive) {
				t.Fatalf("expected active campaign status 'active', got %q", c.Status)
			}
		}
	}
	if !sawDraft || !sawActive {
		t.Fatalf("expected to see both seeded campaigns, sawDraft=%v sawActive=%v", sawDraft, sawActive)
	}

	// ?status= filters to the real closed-set value.
	filteredResp := getJSON(t, srv, "/v1/admin/bonus/campaigns?status=active", token.AccessToken)
	var filtered pagedResponse[campaignResponse]
	decodeBody(t, filteredResp, &filtered)
	for _, c := range filtered.Items {
		if c.Status != string(bonus.CampaignActive) {
			t.Fatalf("expected only active campaigns with ?status=active, got %q", c.Status)
		}
	}
	var sawDraftFiltered bool
	for _, c := range filtered.Items {
		if c.ID == draftID.String() {
			sawDraftFiltered = true
		}
	}
	if sawDraftFiltered {
		t.Fatal("did not expect the draft campaign in a ?status=active filtered list")
	}

	// An invalid status value is a 400, not silently ignored.
	invalidResp := getJSON(t, srv, "/v1/admin/bonus/campaigns?status=not_a_real_status", token.AccessToken)
	invalidResp.Body.Close()
	if invalidResp.StatusCode != 400 {
		t.Fatalf("expected 400 for an invalid status filter, got %d", invalidResp.StatusCode)
	}

	// No bearer token at all.
	noAuthResp := getJSON(t, srv, "/v1/admin/bonus/campaigns", "")
	noAuthResp.Body.Close()
	if noAuthResp.StatusCode != 401 {
		t.Fatalf("expected 401 with no bearer token, got %d", noAuthResp.StatusCode)
	}
}

// (b) a role that does not hold bonus:read is refused.
func TestListCampaigns_WrongPermissionDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "finance-read-1")
	token := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "finance-read-1")

	resp := getJSON(t, srv, "/v1/admin/bonus/campaigns", token.AccessToken)
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("expected 403 for a role lacking bonus:read, got %d", resp.StatusCode)
	}
}

// (c) tenant A's staff cannot see tenant B's campaigns.
func TestListCampaigns_CrossTenantIsolation(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	brandB := mustCreateBrand(t, pool, tenantB)
	promoA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRolePromotionsManager, "promo-read-2a")
	promoB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRolePromotionsManager, "promo-read-2b")
	tokenA := mustLoginStaff(t, srv, tenantA.Slug, promoA.Email, "promo-read-2a")

	campaignBID := seedBonusCampaignForHTTP(t, pool, tenantB.ID, brandB.ID, promoB.ID)

	resp := getJSON(t, srv, "/v1/admin/bonus/campaigns", tokenA.AccessToken)
	var page pagedResponse[campaignResponse]
	decodeBody(t, resp, &page)
	for _, c := range page.Items {
		if c.ID == campaignBID.String() {
			t.Fatal("expected tenant A's campaign list to never contain tenant B's campaign")
		}
	}
}

// --- GET /v1/admin/bonus/change-requests ---

func TestListChangeRequests_HTTP(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	promo1 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-read-3a")
	promo2 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-read-3b")
	promo3 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-read-3c")
	token1 := mustLoginStaff(t, srv, tenant.Slug, promo1.Email, "promo-read-3a")
	token2 := mustLoginStaff(t, srv, tenant.Slug, promo2.Email, "promo-read-3b")
	token3 := mustLoginStaff(t, srv, tenant.Slug, promo3.Email, "promo-read-3c")

	campaignID := seedBonusCampaignForHTTP(t, pool, tenant.ID, brand.ID, promo1.ID)

	fileResp := postJSON(t, srv, "/v1/admin/bonus/change-requests", token1.AccessToken, map[string]any{
		"operation": "campaign_activate", "target_type": "bonus_campaigns", "target_id": campaignID.String(),
		"payload": map[string]string{"action": "activate"}, "reason_code": "launch",
	})
	var filed changeRequestResponse
	decodeBody(t, fileResp, &filed)

	// (a) the default (no ?status=) view is the pending queue, and
	// contains the just-filed request with a populated projection.
	defaultResp := getJSON(t, srv, "/v1/admin/bonus/change-requests", token1.AccessToken)
	var defaultPage pagedResponse[changeRequestResponse]
	decodeBody(t, defaultResp, &defaultPage)
	if defaultResp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", defaultResp.StatusCode)
	}
	var found *changeRequestResponse
	for i := range defaultPage.Items {
		if defaultPage.Items[i].ID == filed.ID {
			found = &defaultPage.Items[i]
		}
	}
	if found == nil {
		t.Fatalf("expected the filed change request to appear in the default (pending) queue, got %+v", defaultPage)
	}
	if found.State != string(bonus.ChangeRequestPending) || found.Operation != "campaign_activate" || found.ReasonCode != "launch" {
		t.Fatalf("unexpected projection for filed change request: %+v", found)
	}

	// ?status=applied is empty before any approval/consumption.
	appliedBeforeResp := getJSON(t, srv, "/v1/admin/bonus/change-requests?status=applied", token1.AccessToken)
	var appliedBefore pagedResponse[changeRequestResponse]
	decodeBody(t, appliedBeforeResp, &appliedBefore)
	for _, cr := range appliedBefore.Items {
		if cr.ID == filed.ID {
			t.Fatal("did not expect the still-pending request under ?status=applied")
		}
	}

	// Two approvals plus the consuming action (activate) moves it to
	// 'applied' - never simulated here, driven through the SAME existing
	// endpoints bonus_governance_flow_integration_test.go already proves.
	for _, tok := range []string{token2.AccessToken, token3.AccessToken} {
		decideResp := postJSON(t, srv, "/v1/admin/bonus/change-requests/"+filed.ID+"/decide", tok, map[string]any{"decision": "approve"})
		if decideResp.StatusCode != 200 {
			t.Fatalf("expected 200 approving, got %d", decideResp.StatusCode)
		}
		decideResp.Body.Close()
	}
	activateResp := postJSON(t, srv, "/v1/admin/bonus/campaigns/"+campaignID.String()+"/activate", token1.AccessToken, map[string]any{})
	if activateResp.StatusCode != 200 {
		t.Fatalf("expected 200 activating after two approvals, got %d", activateResp.StatusCode)
	}
	activateResp.Body.Close()

	appliedAfterResp := getJSON(t, srv, "/v1/admin/bonus/change-requests?status=applied", token1.AccessToken)
	var appliedAfter pagedResponse[changeRequestResponse]
	decodeBody(t, appliedAfterResp, &appliedAfter)
	var sawApplied bool
	for _, cr := range appliedAfter.Items {
		if cr.ID == filed.ID {
			sawApplied = true
			if cr.State != string(bonus.ChangeRequestApplied) || cr.AppliedByPrincipalID == "" || cr.AppliedAt == "" {
				t.Fatalf("expected a fully-populated applied projection, got %+v", cr)
			}
		}
	}
	if !sawApplied {
		t.Fatal("expected the consumed request to appear under ?status=applied")
	}

	defaultAfterResp := getJSON(t, srv, "/v1/admin/bonus/change-requests", token1.AccessToken)
	var defaultAfter pagedResponse[changeRequestResponse]
	decodeBody(t, defaultAfterResp, &defaultAfter)
	for _, cr := range defaultAfter.Items {
		if cr.ID == filed.ID {
			t.Fatal("did not expect the now-applied request in the default (pending) queue")
		}
	}
}

// (b) a role that does not hold bonus:read is refused.
func TestListChangeRequests_WrongPermissionDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "finance-read-2")
	token := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "finance-read-2")

	resp := getJSON(t, srv, "/v1/admin/bonus/change-requests", token.AccessToken)
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("expected 403 for a role lacking bonus:read, got %d", resp.StatusCode)
	}
}

// (c) tenant A's staff cannot see tenant B's change requests.
func TestListChangeRequests_CrossTenantIsolation(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	brandB := mustCreateBrand(t, pool, tenantB)
	promoA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRolePromotionsManager, "promo-read-4a")
	promoB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRolePromotionsManager, "promo-read-4b")
	tokenA := mustLoginStaff(t, srv, tenantA.Slug, promoA.Email, "promo-read-4a")
	tokenB := mustLoginStaff(t, srv, tenantB.Slug, promoB.Email, "promo-read-4b")

	campaignBID := seedBonusCampaignForHTTP(t, pool, tenantB.ID, brandB.ID, promoB.ID)
	fileResp := postJSON(t, srv, "/v1/admin/bonus/change-requests", tokenB.AccessToken, map[string]any{
		"operation": "campaign_activate", "target_type": "bonus_campaigns", "target_id": campaignBID.String(),
		"payload": map[string]string{"action": "activate"}, "reason_code": "launch",
	})
	var filedB changeRequestResponse
	decodeBody(t, fileResp, &filedB)

	resp := getJSON(t, srv, "/v1/admin/bonus/change-requests", tokenA.AccessToken)
	var page pagedResponse[changeRequestResponse]
	decodeBody(t, resp, &page)
	for _, cr := range page.Items {
		if cr.ID == filedB.ID {
			t.Fatal("expected tenant A's change-request queue to never contain tenant B's request")
		}
	}
}

// --- GET /v1/admin/bonus/grants ---

func TestListGrants_HTTP(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	promo := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-read-5")
	token := mustLoginStaff(t, srv, tenant.Slug, promo.Email, "promo-read-5")

	playerID, walletID := seedActiveBonusPlayer(t, pool, tenant.ID, brand.ID, "grant-list-http")
	offerID, offerVersionID := seedBonusOfferForHTTP(t, pool, tenant.ID, brand.ID, promo.ID)
	var campaignID, campaignVersionID uuid.UUID
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		o, err := bonus.GetOfferByID(ctx, tx, offerID)
		campaignID = o.CampaignID
		campaignVersionID = o.CampaignVersionID
		return err
	})
	if err != nil {
		t.Fatalf("resolve campaign for offer: %v", err)
	}

	issuedGrant := seedBonusGrantForHTTP(t, pool, tenant.ID, brand.ID, playerID, walletID, campaignID, campaignVersionID, offerID, offerVersionID, promo.ID, "grant-list-1", bonus.GrantIssued)
	completedGrant := seedBonusGrantForHTTP(t, pool, tenant.ID, brand.ID, playerID, walletID, campaignID, campaignVersionID, offerID, offerVersionID, promo.ID, "grant-list-2", bonus.GrantCompleted)

	// (a) per-player view: both grants for this player, correct shape.
	perPlayerResp := getJSON(t, srv, "/v1/admin/bonus/grants?player_account_id="+playerID.String(), token.AccessToken)
	var perPlayer pagedResponse[grantResponse]
	decodeBody(t, perPlayerResp, &perPlayer)
	if perPlayerResp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", perPlayerResp.StatusCode)
	}
	if perPlayer.Total != 2 || len(perPlayer.Items) != 2 {
		t.Fatalf("expected exactly 2 grants for this player, got total=%d items=%d", perPlayer.Total, len(perPlayer.Items))
	}
	seen := map[string]string{}
	for _, g := range perPlayer.Items {
		seen[g.ID] = g.Status
	}
	if seen[issuedGrant.ID.String()] != string(bonus.GrantIssued) {
		t.Fatalf("expected issued grant status, got %+v", seen)
	}
	if seen[completedGrant.ID.String()] != string(bonus.GrantCompleted) {
		t.Fatalf("expected completed grant status, got %+v", seen)
	}

	// tenant-wide list, filtered by status, finds only the completed one.
	statusFilteredResp := getJSON(t, srv, "/v1/admin/bonus/grants?status=completed", token.AccessToken)
	var statusFiltered pagedResponse[grantResponse]
	decodeBody(t, statusFilteredResp, &statusFiltered)
	var sawIssuedInFiltered, sawCompletedInFiltered bool
	for _, g := range statusFiltered.Items {
		if g.ID == issuedGrant.ID.String() {
			sawIssuedInFiltered = true
		}
		if g.ID == completedGrant.ID.String() {
			sawCompletedInFiltered = true
		}
	}
	if sawIssuedInFiltered || !sawCompletedInFiltered {
		t.Fatalf("expected only the completed grant under ?status=completed, sawIssued=%v sawCompleted=%v", sawIssuedInFiltered, sawCompletedInFiltered)
	}

	// tenant-wide list, no filters, still includes both.
	allResp := getJSON(t, srv, "/v1/admin/bonus/grants", token.AccessToken)
	var all pagedResponse[grantResponse]
	decodeBody(t, allResp, &all)
	var sawIssuedAll, sawCompletedAll bool
	for _, g := range all.Items {
		if g.ID == issuedGrant.ID.String() {
			sawIssuedAll = true
		}
		if g.ID == completedGrant.ID.String() {
			sawCompletedAll = true
		}
	}
	if !sawIssuedAll || !sawCompletedAll {
		t.Fatalf("expected the tenant-wide list to include both grants, sawIssued=%v sawCompleted=%v", sawIssuedAll, sawCompletedAll)
	}

	// invalid player_account_id is a 400.
	invalidResp := getJSON(t, srv, "/v1/admin/bonus/grants?player_account_id=not-a-uuid", token.AccessToken)
	invalidResp.Body.Close()
	if invalidResp.StatusCode != 400 {
		t.Fatalf("expected 400 for an invalid player_account_id, got %d", invalidResp.StatusCode)
	}

	// no bearer token at all.
	noAuthResp := getJSON(t, srv, "/v1/admin/bonus/grants", "")
	noAuthResp.Body.Close()
	if noAuthResp.StatusCode != 401 {
		t.Fatalf("expected 401 with no bearer token, got %d", noAuthResp.StatusCode)
	}
}

// (b) a role that does not hold bonus:read is refused.
func TestListGrants_WrongPermissionDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "finance-read-3")
	token := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "finance-read-3")

	resp := getJSON(t, srv, "/v1/admin/bonus/grants", token.AccessToken)
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("expected 403 for a role lacking bonus:read, got %d", resp.StatusCode)
	}
}

// (c) tenant A's staff cannot see tenant B's grants, including by
// directly requesting tenant B's player_account_id.
func TestListGrants_CrossTenantIsolation(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	brandB := mustCreateBrand(t, pool, tenantB)
	promoA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRolePromotionsManager, "promo-read-6a")
	promoB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRolePromotionsManager, "promo-read-6b")
	tokenA := mustLoginStaff(t, srv, tenantA.Slug, promoA.Email, "promo-read-6a")

	playerBID, walletBID := seedActiveBonusPlayer(t, pool, tenantB.ID, brandB.ID, "grant-xtenant")
	offerBID, offerVersionBID := seedBonusOfferForHTTP(t, pool, tenantB.ID, brandB.ID, promoB.ID)
	var campaignBID, campaignVersionBID uuid.UUID
	err := pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		o, err := bonus.GetOfferByID(ctx, tx, offerBID)
		campaignBID = o.CampaignID
		campaignVersionBID = o.CampaignVersionID
		return err
	})
	if err != nil {
		t.Fatalf("resolve tenant B campaign: %v", err)
	}
	grantB := seedBonusGrantForHTTP(t, pool, tenantB.ID, brandB.ID, playerBID, walletBID, campaignBID, campaignVersionBID, offerBID, offerVersionBID, promoB.ID, "grant-xtenant-1", bonus.GrantIssued)
	_ = brandA

	// Tenant-wide list from tenant A never contains tenant B's grant.
	allResp := getJSON(t, srv, "/v1/admin/bonus/grants", tokenA.AccessToken)
	var all pagedResponse[grantResponse]
	decodeBody(t, allResp, &all)
	for _, g := range all.Items {
		if g.ID == grantB.ID.String() {
			t.Fatal("expected tenant A's grant list to never contain tenant B's grant")
		}
	}

	// Even naming tenant B's player_account_id directly from tenant A's
	// own scope returns nothing - RLS/tenant_id isolation, not merely a
	// permission check.
	perPlayerResp := getJSON(t, srv, "/v1/admin/bonus/grants?player_account_id="+playerBID.String(), tokenA.AccessToken)
	var perPlayer pagedResponse[grantResponse]
	decodeBody(t, perPlayerResp, &perPlayer)
	if perPlayer.Total != 0 || len(perPlayer.Items) != 0 {
		t.Fatalf("expected zero results querying tenant B's player_account_id from tenant A's scope, got total=%d items=%d", perPlayer.Total, len(perPlayer.Items))
	}
}
