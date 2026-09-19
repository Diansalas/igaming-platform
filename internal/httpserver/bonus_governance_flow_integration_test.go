//go:build integration

// Stage 4H-B1 Wave 3 Phase 3 (item F) HTTP-level test suite for the new
// four-eyes/EOI-mint/per-domain-activation admin surface
// (bonus_governance_handlers.go, bonus_domain_ops_handlers.go). Per item
// G's own instruction, this suite doubles as the adversarial re-test for
// these NEW HTTP surfaces: authn (401), authz/RBAC (403, including the
// dynamic per-operation permission check), input validation (400),
// tenant isolation, and actor/subject-substitution-class refusals
// (self-approval, wrong-permission approval, no-approval activation).
package httpserver

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// seedActiveBonusPlayer inserts a player_account directly in 'active'
// status (identity.RegisterPlayer's own default, 'pending_verification',
// would otherwise deny every T.1 RG check this suite's Grant-activation
// endpoints run) plus its own USD wallet - mirrors internal/casino's own
// seedCasinoFixture convention exactly.
func seedActiveBonusPlayer(t *testing.T, pool *db.Pool, tenantID, brandID uuid.UUID, emailPrefix string) (playerID, walletID uuid.UUID) {
	t.Helper()
	playerID = uuid.New()
	walletID = uuid.New()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		personID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			playerID, tenantID, brandID, personID, emailPrefix+"-"+playerID.String()+"@example.com"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'USD')`,
			walletID, tenantID, brandID, playerID)
		return err
	})
	if err != nil {
		t.Fatalf("seed active bonus player: %v", err)
	}
	return playerID, walletID
}

// seedBonusCampaignForHTTP creates a Draft Campaign directly (bypassing
// HTTP, mirroring identity_flow_integration_test.go's own "fixtures via
// the domain package" convention) - tests exercise ACTIVATION over HTTP,
// not creation, which the existing /v1/admin/bonus/campaigns endpoint
// already covers.
func seedBonusCampaignForHTTP(t *testing.T, pool *db.Pool, tenantID, brandID, staffID uuid.UUID) uuid.UUID {
	t.Helper()
	var campaignID uuid.UUID
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := bonus.CreateCampaign(ctx, tx, bonus.Campaign{TenantID: tenantID, BrandID: &brandID, CreatedByActorType: bonus.ActorStaff, CreatedByActorID: staffID})
		campaignID = c.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed bonus campaign: %v", err)
	}
	return campaignID
}

// seedBonusOfferForHTTP creates a Draft Offer/OfferVersion under an
// Active Campaign - offer_publish's own target.
func seedBonusOfferForHTTP(t *testing.T, pool *db.Pool, tenantID, brandID, staffID uuid.UUID) (offerID, offerVersionID uuid.UUID) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := bonus.CreateCampaign(ctx, tx, bonus.Campaign{TenantID: tenantID, BrandID: &brandID, Status: bonus.CampaignActive, CreatedByActorType: bonus.ActorStaff, CreatedByActorID: staffID})
		if err != nil {
			return err
		}
		v, err := bonus.CreateCampaignVersion(ctx, tx, bonus.CampaignVersion{TenantID: tenantID, CampaignID: c.ID, VersionNumber: 1, Name: "V1", CreatedByActorType: bonus.ActorStaff, CreatedByActorID: staffID})
		if err != nil {
			return err
		}
		if _, err := bonus.SetCampaignCurrentVersion(ctx, tx, tenantID, c.ID, v.ID); err != nil {
			return err
		}
		o, err := bonus.CreateOffer(ctx, tx, bonus.Offer{TenantID: tenantID, BrandID: &brandID, CampaignID: c.ID, CampaignVersionID: v.ID, GrantPolicy: bonus.GrantPolicyAutoIssue, CreatedByActorType: bonus.ActorStaff, CreatedByActorID: staffID})
		if err != nil {
			return err
		}
		offerID = o.ID
		ov, err := bonus.CreateOfferVersion(ctx, tx, bonus.OfferVersion{
			TenantID: tenantID, OfferID: o.ID, VersionNumber: 1, RewardKind: bonus.RewardFixedValue, RewardAssetCode: "USD",
			FulfillmentDestination: bonus.FulfillmentIntoPlatformWallet, FundingSource: "operator", CreatedByActorType: bonus.ActorStaff, CreatedByActorID: staffID,
		})
		offerVersionID = ov.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed bonus offer: %v", err)
	}
	return offerID, offerVersionID
}

// --- file / decide: permission gating and self-approval refusal ---

func TestFileChangeRequest_PermissionGatedByOperation(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	promo := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-pw-1")
	bonusOps := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleBonusOperations, "ops-pw-1")
	promoToken := mustLoginStaff(t, srv, tenant.Slug, promo.Email, "promo-pw-1")
	opsToken := mustLoginStaff(t, srv, tenant.Slug, bonusOps.Email, "ops-pw-1")

	campaignID := seedBonusCampaignForHTTP(t, pool, tenant.ID, brand.ID, uuid.MustParse(promo.ID.String()))

	// promotions_manager holds bonus_campaign:activate - allowed to file.
	resp := postJSON(t, srv, "/v1/admin/bonus/change-requests", promoToken.AccessToken, map[string]any{
		"operation": "campaign_activate", "target_type": "bonus_campaigns", "target_id": campaignID.String(),
		"payload": map[string]string{"action": "activate"}, "reason_code": "launch",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 201 for promotions_manager filing campaign_activate, got %d: %+v", resp.StatusCode, apiErr)
	}

	// bonus_operations does NOT hold bonus_campaign:activate - refused.
	resp2 := postJSON(t, srv, "/v1/admin/bonus/change-requests", opsToken.AccessToken, map[string]any{
		"operation": "campaign_activate", "target_type": "bonus_campaigns", "target_id": campaignID.String(),
		"payload": map[string]string{"action": "activate"}, "reason_code": "launch",
	})
	defer resp2.Body.Close()
	if resp2.StatusCode != 403 {
		t.Fatalf("expected 403 for bonus_operations filing campaign_activate (lacks the permission), got %d", resp2.StatusCode)
	}
}

func TestFileChangeRequest_ValidationErrors(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	promo := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-pw-2")
	token := mustLoginStaff(t, srv, tenant.Slug, promo.Email, "promo-pw-2")

	cases := []map[string]any{
		{"operation": "", "target_type": "bonus_campaigns", "target_id": uuid.NewString(), "reason_code": "x"},
		{"operation": "campaign_activate", "target_type": "", "target_id": uuid.NewString(), "reason_code": "x"},
		{"operation": "campaign_activate", "target_type": "bonus_campaigns", "target_id": "not-a-uuid", "reason_code": "x"},
		{"operation": "campaign_activate", "target_type": "bonus_campaigns", "target_id": uuid.NewString(), "reason_code": ""},
		{"operation": "not_a_real_operation", "target_type": "bonus_campaigns", "target_id": uuid.NewString(), "reason_code": "x"},
	}
	for i, body := range cases {
		resp := postJSON(t, srv, "/v1/admin/bonus/change-requests", token.AccessToken, body)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("case %d: expected 400, got %d for body %+v", i, resp.StatusCode, body)
		}
	}
}

func TestFileChangeRequest_RequiresAuthentication(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)

	resp := postJSON(t, srv, "/v1/admin/bonus/change-requests", "", map[string]any{
		"operation": "campaign_activate", "target_type": "bonus_campaigns", "target_id": uuid.NewString(), "reason_code": "x",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 with no bearer token, got %d", resp.StatusCode)
	}
}

func TestDecideChangeRequest_SelfApprovalRefused(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	promo := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-pw-3")
	token := mustLoginStaff(t, srv, tenant.Slug, promo.Email, "promo-pw-3")

	campaignID := seedBonusCampaignForHTTP(t, pool, tenant.ID, brand.ID, promo.ID)

	fileResp := postJSON(t, srv, "/v1/admin/bonus/change-requests", token.AccessToken, map[string]any{
		"operation": "campaign_activate", "target_type": "bonus_campaigns", "target_id": campaignID.String(),
		"payload": map[string]string{"action": "activate"}, "reason_code": "launch",
	})
	var filed changeRequestResponse
	decodeBody(t, fileResp, &filed)

	// The SAME staff member attempts to approve their own request.
	decideResp := postJSON(t, srv, "/v1/admin/bonus/change-requests/"+filed.ID+"/decide", token.AccessToken, map[string]any{
		"decision": "approve",
	})
	defer decideResp.Body.Close()
	if decideResp.StatusCode != 403 {
		var apiErr apierror.Error
		decodeBody(t, decideResp, &apiErr)
		t.Fatalf("expected 403 for self-approval, got %d: %+v", decideResp.StatusCode, apiErr)
	}
}

func TestDecideChangeRequest_RequiresMatchingPermission(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	promo := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-pw-4")
	promoToken := mustLoginStaff(t, srv, tenant.Slug, promo.Email, "promo-pw-4")
	bonusOps := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleBonusOperations, "ops-pw-4")
	opsToken := mustLoginStaff(t, srv, tenant.Slug, bonusOps.Email, "ops-pw-4")

	campaignID := seedBonusCampaignForHTTP(t, pool, tenant.ID, brand.ID, promo.ID)

	fileResp := postJSON(t, srv, "/v1/admin/bonus/change-requests", promoToken.AccessToken, map[string]any{
		"operation": "campaign_activate", "target_type": "bonus_campaigns", "target_id": campaignID.String(),
		"payload": map[string]string{"action": "activate"}, "reason_code": "launch",
	})
	var filed changeRequestResponse
	decodeBody(t, fileResp, &filed)

	// bonus_operations does not hold bonus_campaign:activate - refused
	// even though it IS a distinct person from the filer.
	decideResp := postJSON(t, srv, "/v1/admin/bonus/change-requests/"+filed.ID+"/decide", opsToken.AccessToken, map[string]any{
		"decision": "approve",
	})
	defer decideResp.Body.Close()
	if decideResp.StatusCode != 403 {
		t.Fatalf("expected 403 for an approver lacking the operation's own permission, got %d", decideResp.StatusCode)
	}
}

// --- campaign_activate: end-to-end ---

func TestActivateCampaign_HTTP_RefusedThenSucceeds(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	promo1 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-pw-5a")
	promo2 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-pw-5b")
	promo3 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-pw-5c")
	token1 := mustLoginStaff(t, srv, tenant.Slug, promo1.Email, "promo-pw-5a")
	token2 := mustLoginStaff(t, srv, tenant.Slug, promo2.Email, "promo-pw-5b")
	token3 := mustLoginStaff(t, srv, tenant.Slug, promo3.Email, "promo-pw-5c")

	campaignID := seedBonusCampaignForHTTP(t, pool, tenant.ID, brand.ID, promo1.ID)

	// Refused before any approval.
	refusedResp := postJSON(t, srv, "/v1/admin/bonus/campaigns/"+campaignID.String()+"/activate", token1.AccessToken, map[string]any{})
	defer refusedResp.Body.Close()
	if refusedResp.StatusCode != 403 {
		t.Fatalf("expected 403 activating with no approved request, got %d", refusedResp.StatusCode)
	}

	fileResp := postJSON(t, srv, "/v1/admin/bonus/change-requests", token1.AccessToken, map[string]any{
		"operation": "campaign_activate", "target_type": "bonus_campaigns", "target_id": campaignID.String(),
		"payload": map[string]string{"action": "activate"}, "reason_code": "launch",
	})
	var filed changeRequestResponse
	decodeBody(t, fileResp, &filed)

	for _, tok := range []string{token2.AccessToken, token3.AccessToken} {
		decideResp := postJSON(t, srv, "/v1/admin/bonus/change-requests/"+filed.ID+"/decide", tok, map[string]any{"decision": "approve"})
		if decideResp.StatusCode != 200 {
			var apiErr apierror.Error
			decodeBody(t, decideResp, &apiErr)
			t.Fatalf("expected 200 approving, got %d: %+v", decideResp.StatusCode, apiErr)
		}
		decodeBody(t, decideResp, &changeRequestResponse{})
	}

	activateResp := postJSON(t, srv, "/v1/admin/bonus/campaigns/"+campaignID.String()+"/activate", token1.AccessToken, map[string]any{})
	defer activateResp.Body.Close()
	if activateResp.StatusCode != 200 {
		var apiErr apierror.Error
		decodeBody(t, activateResp, &apiErr)
		t.Fatalf("expected 200 activating after two approvals, got %d: %+v", activateResp.StatusCode, apiErr)
	}
	var activated campaignResponse
	decodeBody(t, activateResp, &activated)
	if activated.Status != "active" {
		t.Fatalf("expected active, got %s", activated.Status)
	}

	// A second activation attempt against the SAME already-consumed
	// approval is refused.
	secondResp := postJSON(t, srv, "/v1/admin/bonus/campaigns/"+campaignID.String()+"/activate", token1.AccessToken, map[string]any{})
	defer secondResp.Body.Close()
	if secondResp.StatusCode != 403 {
		t.Fatalf("expected 403 on a second activation with the approval already consumed, got %d", secondResp.StatusCode)
	}
}

func TestActivateCampaign_RequiresAuthentication(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	resp := postJSON(t, srv, "/v1/admin/bonus/campaigns/"+uuid.NewString()+"/activate", "", map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

// TestActivateCampaign_CrossTenantIsolation proves a staff member from
// tenant A cannot activate (or even discover) tenant B's Campaign - RLS
// scoping via WithTenant, not merely a permission check.
func TestActivateCampaign_CrossTenantIsolation(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	brandB := mustCreateBrand(t, pool, tenantB)
	promoA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRolePromotionsManager, "promo-pw-6")
	promoBOwner := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRolePromotionsManager, "promo-pw-6b")
	tokenA := mustLoginStaff(t, srv, tenantA.Slug, promoA.Email, "promo-pw-6")

	campaignBID := seedBonusCampaignForHTTP(t, pool, tenantB.ID, brandB.ID, promoBOwner.ID)

	resp := postJSON(t, srv, "/v1/admin/bonus/campaigns/"+campaignBID.String()+"/activate", tokenA.AccessToken, map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("expected tenant A's staff to be unable to activate tenant B's campaign")
	}
}

// --- offer_publish: end-to-end ---

func TestPublishOffer_HTTP_RefusedThenSucceeds(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	promo1 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-pw-7a")
	promo2 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-pw-7b")
	promo3 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-pw-7c")
	token1 := mustLoginStaff(t, srv, tenant.Slug, promo1.Email, "promo-pw-7a")
	token2 := mustLoginStaff(t, srv, tenant.Slug, promo2.Email, "promo-pw-7b")
	token3 := mustLoginStaff(t, srv, tenant.Slug, promo3.Email, "promo-pw-7c")

	offerID, offerVersionID := seedBonusOfferForHTTP(t, pool, tenant.ID, brand.ID, promo1.ID)

	refusedResp := postJSON(t, srv, "/v1/admin/bonus/offers/"+offerID.String()+"/publish", token1.AccessToken, map[string]any{
		"offer_version_id": offerVersionID.String(),
	})
	defer refusedResp.Body.Close()
	if refusedResp.StatusCode != 403 {
		t.Fatalf("expected 403 publishing with no approved request, got %d", refusedResp.StatusCode)
	}

	fileResp := postJSON(t, srv, "/v1/admin/bonus/change-requests", token1.AccessToken, map[string]any{
		"operation": "offer_publish", "target_type": "bonus_offers", "target_id": offerID.String(),
		"payload": map[string]string{"offer_version_id": offerVersionID.String()}, "reason_code": "launch",
	})
	var filed changeRequestResponse
	decodeBody(t, fileResp, &filed)

	for _, tok := range []string{token2.AccessToken, token3.AccessToken} {
		decideResp := postJSON(t, srv, "/v1/admin/bonus/change-requests/"+filed.ID+"/decide", tok, map[string]any{"decision": "approve"})
		if decideResp.StatusCode != 200 {
			var apiErr apierror.Error
			decodeBody(t, decideResp, &apiErr)
			t.Fatalf("expected 200 approving, got %d: %+v", decideResp.StatusCode, apiErr)
		}
	}

	publishResp := postJSON(t, srv, "/v1/admin/bonus/offers/"+offerID.String()+"/publish", token1.AccessToken, map[string]any{
		"offer_version_id": offerVersionID.String(),
	})
	defer publishResp.Body.Close()
	if publishResp.StatusCode != 200 {
		var apiErr apierror.Error
		decodeBody(t, publishResp, &apiErr)
		t.Fatalf("expected 200 publishing after two approvals, got %d: %+v", publishResp.StatusCode, apiErr)
	}
	var published offerResponse
	decodeBody(t, publishResp, &published)
	if published.Status != "active" || published.CurrentVersionID != offerVersionID.String() {
		t.Fatalf("expected active with current_version_id=%s, got %+v", offerVersionID, published)
	}
}

// --- EOI minting ---

func TestMintEconomicOperation_PermissionAndIdempotency(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	bonusOps := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleBonusOperations, "ops-pw-8")
	promo := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-pw-8")
	opsToken := mustLoginStaff(t, srv, tenant.Slug, bonusOps.Email, "ops-pw-8")
	promoToken := mustLoginStaff(t, srv, tenant.Slug, promo.Email, "promo-pw-8")

	idemKey := "eoi-" + uuid.NewString()
	body := map[string]any{
		"operation_type": "bonus_manual_grant", "subject_scope": "single_subject",
		"subject_ref": uuid.NewString(), "asset_code": "USD", "intended_aggregate_value": "100000",
		"idempotency_key": idemKey,
	}

	// promotions_manager does not hold bonus_grant:issue - refused.
	refused := postJSON(t, srv, "/v1/admin/bonus/economic-operations", promoToken.AccessToken, body)
	defer refused.Body.Close()
	if refused.StatusCode != 403 {
		t.Fatalf("expected 403 for promotions_manager minting a manual-grant EOI, got %d", refused.StatusCode)
	}

	first := postJSON(t, srv, "/v1/admin/bonus/economic-operations", opsToken.AccessToken, body)
	defer first.Body.Close()
	if first.StatusCode != 201 {
		var apiErr apierror.Error
		decodeBody(t, first, &apiErr)
		t.Fatalf("expected 201 for bonus_operations minting a manual-grant EOI, got %d: %+v", first.StatusCode, apiErr)
	}
	var firstOp economicOperationResponse
	decodeBody(t, first, &firstOp)

	second := postJSON(t, srv, "/v1/admin/bonus/economic-operations", opsToken.AccessToken, body)
	defer second.Body.Close()
	if second.StatusCode != 201 {
		t.Fatalf("expected 201 on idempotent re-mint, got %d", second.StatusCode)
	}
	var secondOp economicOperationResponse
	decodeBody(t, second, &secondOp)
	if secondOp.OperationID != firstOp.OperationID {
		t.Fatalf("expected the SAME operation id on an idempotent re-mint, got %s vs %s", secondOp.OperationID, firstOp.OperationID)
	}
}

// --- manual grant issue/activate: refusal path (success path is proven
// at the package level, four_eyes_ops_integration_test.go, which also
// exercises the full T.1 asset-authorization gate chain a second,
// duplicate httpserver-level fixture is not needed to re-prove) ---

func TestManualGrantIssueAndActivate_HTTP_RefusedWithoutApproval(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	bonusOps := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleBonusOperations, "ops-pw-9")
	token := mustLoginStaff(t, srv, tenant.Slug, bonusOps.Email, "ops-pw-9")

	playerID, walletID := seedActiveBonusPlayer(t, pool, tenant.ID, brand.ID, "manual-grant-http")
	offerID, offerVersionID := seedBonusOfferForHTTP(t, pool, tenant.ID, brand.ID, bonusOps.ID)
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

	eoiResp := postJSON(t, srv, "/v1/admin/bonus/economic-operations", token.AccessToken, map[string]any{
		"operation_type": "bonus_manual_grant", "subject_scope": "single_subject", "subject_ref": playerID.String(),
		"asset_code": "USD", "intended_aggregate_value": "100000", "idempotency_key": "eoi-manual-" + uuid.NewString(),
	})
	var eoi economicOperationResponse
	decodeBody(t, eoiResp, &eoi)

	issueResp := postJSON(t, srv, "/v1/admin/bonus/manual-grants", token.AccessToken, map[string]any{
		"player_account_id": playerID.String(), "campaign_id": campaignID.String(), "campaign_version_id": campaignVersionID.String(),
		"offer_id": offerID.String(), "offer_version_id": offerVersionID.String(), "asset_code": "USD",
		"funding_source": "operator", "parent_operation_id": eoi.OperationID,
	})
	defer issueResp.Body.Close()
	if issueResp.StatusCode != 201 {
		var apiErr apierror.Error
		decodeBody(t, issueResp, &apiErr)
		t.Fatalf("expected 201 issuing the manual grant request, got %d: %+v", issueResp.StatusCode, apiErr)
	}
	var issued grantResponse
	decodeBody(t, issueResp, &issued)
	if issued.Status != "issued" {
		t.Fatalf("expected issued, got %s", issued.Status)
	}
	_ = walletID

	activateResp := postJSON(t, srv, "/v1/admin/bonus/manual-grants/"+issued.ID+"/activate", token.AccessToken, map[string]any{
		"parent_operation_id": eoi.OperationID, "amount": "1000",
	})
	defer activateResp.Body.Close()
	if activateResp.StatusCode != 403 {
		var apiErr apierror.Error
		decodeBody(t, activateResp, &apiErr)
		t.Fatalf("expected 403 activating with no approved change request, got %d: %+v", activateResp.StatusCode, apiErr)
	}
}

// --- bulk grant job: refusal path (success path proven at the package
// level, four_eyes_ops_integration_test.go) ---

func TestBulkGrantJobExecute_HTTP_RefusedWithoutApproval(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	bonusOps := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleBonusOperations, "ops-pw-10")
	token := mustLoginStaff(t, srv, tenant.Slug, bonusOps.Email, "ops-pw-10")

	playerID, _ := seedActiveBonusPlayer(t, pool, tenant.ID, brand.ID, "bulk-grant-http")
	offerID, offerVersionID := seedBonusOfferForHTTP(t, pool, tenant.ID, brand.ID, bonusOps.ID)
	var campaignID uuid.UUID
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		o, err := bonus.GetOfferByID(ctx, tx, offerID)
		campaignID = o.CampaignID
		return err
	})
	if err != nil {
		t.Fatalf("resolve campaign: %v", err)
	}

	createResp := postJSON(t, srv, "/v1/admin/bonus/bulk-jobs", token.AccessToken, map[string]any{
		"campaign_id": campaignID.String(), "offer_version_id": offerVersionID.String(),
		"target_kind": "single_player", "target_player_account_ids": []string{playerID.String()},
		"idempotency_key": "bulk-http-" + uuid.NewString(),
	})
	defer createResp.Body.Close()
	if createResp.StatusCode != 201 {
		var apiErr apierror.Error
		decodeBody(t, createResp, &apiErr)
		t.Fatalf("expected 201 creating the bulk job, got %d: %+v", createResp.StatusCode, apiErr)
	}
	var job bulkGrantJobResponse
	decodeBody(t, createResp, &job)

	execResp := postJSON(t, srv, "/v1/admin/bonus/bulk-jobs/"+job.ID+"/execute", token.AccessToken, map[string]any{
		"amount": "1000",
	})
	defer execResp.Body.Close()
	if execResp.StatusCode != 403 {
		var apiErr apierror.Error
		decodeBody(t, execResp, &apiErr)
		t.Fatalf("expected 403 executing with no approved change request, got %d: %+v", execResp.StatusCode, apiErr)
	}
}
