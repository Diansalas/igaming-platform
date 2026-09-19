//go:build integration

// Stage 4H-B1 Wave 3 Phase 9 (`qa`) - closes a genuine coverage gap this
// review found: every mutating handler bonus_governance_handlers.go/
// bonus_domain_ops_handlers.go adds this Wave calls audit.Record (its own
// top-of-file doc comment says so explicitly), but NOTHING in
// bonus_governance_flow_integration_test.go or any sibling test file in
// this Wave's diff ever reads audit_log back and asserts a row actually
// landed - every existing test asserts on the HTTP response/domain-object
// state only. Per the qa dispatch's own item 5 ("verify every endpoint
// has... an audit-record-written test") and CLAUDE.md's "every mutating
// administrative/financial action writes an audit record" rule, an
// audit.Record call site with no test proving it actually executes (as
// opposed to being dead code, or erroring silently in a code path that
// happens not to be exercised) is not verified to satisfy that rule.
// Mirrors the established pattern for this exact class of assertion,
// internal/httpserver/identity_flow_integration_test.go's own
// requireEntryFor helper, applied here by reading audit_log directly
// (mirroring internal/bonus/deposit_sweep_integration_test.go's own
// "SELECT count(*) FROM audit_log WHERE ... action = ..." style) rather
// than via the /v1/admin/audit-log endpoint, since bonus_operations/
// promotions_manager do not hold the audit-log-read permission and
// provisioning a third staff role's worth of fixture for this file alone
// would not be proportionate.
//
// bulk_grant_job.executed is DELIBERATELY NOT tested via HTTP here:
// bonus_bulk_job_execute_failclosed_integration_test.go's own F3 finding
// (Stage 4H-B1 Wave 3 Phase 6, `security`) already proves
// POST /v1/admin/bonus/bulk-jobs/{jobID}/execute can never reach that
// audit.Record call today (newCreateBulkGrantJobHandler never populates
// parent_operation_id, so RunStaticBulkGrantJob always refuses first, and
// the whole transaction - including any audit row - rolls back). Asserting
// that audit row exists via HTTP here would be asserting something
// structurally impossible today, exactly the "fake completion" CLAUDE.md
// forbids. It IS exercised at the package level, where the fixture can
// legitimately pre-populate parent_operation_id -
// TestExecuteBulkGrantJobWithApproval_WritesAuditRecord in
// internal/bonus/four_eyes_ops_audit_integration_test.go.
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

// countAuditEntries reads audit_log directly (RLS-scoped via
// pool.WithTenant, exactly like every production write in this Wave) -
// action AND target_id are both checked so a coincidental action-name
// match against an unrelated target cannot produce a false pass.
func countAuditEntries(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action, targetID string) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
			tenantID, action, targetID,
		).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count audit_log entries for action=%q target_id=%q: %v", action, targetID, err)
	}
	return count
}

func TestFileAndDecideChangeRequest_HTTP_WriteAuditRecords(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	promo1 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "audit-promo-1")
	promo2 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "audit-promo-2")
	token1 := mustLoginStaff(t, srv, tenant.Slug, promo1.Email, "audit-promo-1")
	token2 := mustLoginStaff(t, srv, tenant.Slug, promo2.Email, "audit-promo-2")

	campaignID := seedBonusCampaignForHTTP(t, pool, tenant.ID, brand.ID, promo1.ID)

	fileResp := postJSON(t, srv, "/v1/admin/bonus/change-requests", token1.AccessToken, map[string]any{
		"operation": "campaign_activate", "target_type": "bonus_campaigns", "target_id": campaignID.String(),
		"payload": map[string]string{"action": "activate"}, "reason_code": "audit-test",
	})
	defer fileResp.Body.Close()
	if fileResp.StatusCode != 201 {
		var apiErr apierror.Error
		decodeBody(t, fileResp, &apiErr)
		t.Fatalf("expected 201 filing, got %d: %+v", fileResp.StatusCode, apiErr)
	}
	var filed changeRequestResponse
	decodeBody(t, fileResp, &filed)

	if got := countAuditEntries(t, pool, tenant.ID, "bonus_change_request.filed", filed.ID); got != 1 {
		t.Fatalf("expected exactly 1 audit_log row for bonus_change_request.filed targeting %s, got %d", filed.ID, got)
	}

	decideResp := postJSON(t, srv, "/v1/admin/bonus/change-requests/"+filed.ID+"/decide", token2.AccessToken, map[string]any{"decision": "approve"})
	defer decideResp.Body.Close()
	if decideResp.StatusCode != 200 {
		var apiErr apierror.Error
		decodeBody(t, decideResp, &apiErr)
		t.Fatalf("expected 200 approving, got %d: %+v", decideResp.StatusCode, apiErr)
	}

	if got := countAuditEntries(t, pool, tenant.ID, "bonus_change_request.approved", filed.ID); got != 1 {
		t.Fatalf("expected exactly 1 audit_log row for bonus_change_request.approved targeting %s, got %d", filed.ID, got)
	}
}

func TestActivateCampaign_HTTP_WritesAuditRecord(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	promo1 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "audit-campaign-1")
	promo2 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "audit-campaign-2")
	promo3 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "audit-campaign-3")
	token1 := mustLoginStaff(t, srv, tenant.Slug, promo1.Email, "audit-campaign-1")
	token2 := mustLoginStaff(t, srv, tenant.Slug, promo2.Email, "audit-campaign-2")
	token3 := mustLoginStaff(t, srv, tenant.Slug, promo3.Email, "audit-campaign-3")

	campaignID := seedBonusCampaignForHTTP(t, pool, tenant.ID, brand.ID, promo1.ID)

	fileResp := postJSON(t, srv, "/v1/admin/bonus/change-requests", token1.AccessToken, map[string]any{
		"operation": "campaign_activate", "target_type": "bonus_campaigns", "target_id": campaignID.String(),
		"payload": map[string]string{"action": "activate"}, "reason_code": "audit-test",
	})
	var filed changeRequestResponse
	decodeBody(t, fileResp, &filed)
	for _, tok := range []string{token2.AccessToken, token3.AccessToken} {
		resp := postJSON(t, srv, "/v1/admin/bonus/change-requests/"+filed.ID+"/decide", tok, map[string]any{"decision": "approve"})
		resp.Body.Close()
	}

	activateResp := postJSON(t, srv, "/v1/admin/bonus/campaigns/"+campaignID.String()+"/activate", token1.AccessToken, map[string]any{})
	defer activateResp.Body.Close()
	if activateResp.StatusCode != 200 {
		var apiErr apierror.Error
		decodeBody(t, activateResp, &apiErr)
		t.Fatalf("expected 200 activating, got %d: %+v", activateResp.StatusCode, apiErr)
	}

	if got := countAuditEntries(t, pool, tenant.ID, "bonus_campaign.activated", campaignID.String()); got != 1 {
		t.Fatalf("expected exactly 1 audit_log row for bonus_campaign.activated targeting %s, got %d", campaignID, got)
	}
}

func TestCreateOfferAndVersion_HTTP_WriteAuditRecords(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	promo := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "audit-offer-1")
	token := mustLoginStaff(t, srv, tenant.Slug, promo.Email, "audit-offer-1")

	campaignID := seedBonusCampaignForHTTP(t, pool, tenant.ID, brand.ID, promo.ID)
	// campaign_activate first, not needed for offer creation itself
	// (Draft Offer creation does not require an Active Campaign), so skip
	// straight to resolving campaignVersionID directly.
	var campaignVersionID uuid.UUID
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := bonus.GetCampaignByID(ctx, tx, campaignID)
		if c.CurrentVersionID != nil {
			campaignVersionID = *c.CurrentVersionID
		}
		return err
	})
	if err != nil || campaignVersionID == uuid.Nil {
		v, verr := seedCampaignVersionForHTTP(t, pool, tenant.ID, campaignID, promo.ID)
		if verr != nil {
			t.Fatalf("resolve/seed campaign version: %v / %v", err, verr)
		}
		campaignVersionID = v
	}

	createOfferResp := postJSON(t, srv, "/v1/admin/bonus/offers", token.AccessToken, map[string]any{
		"brand_id": brand.ID.String(), "campaign_id": campaignID.String(), "campaign_version_id": campaignVersionID.String(),
		"grant_policy": "auto_issue",
	})
	defer createOfferResp.Body.Close()
	if createOfferResp.StatusCode != 201 {
		var apiErr apierror.Error
		decodeBody(t, createOfferResp, &apiErr)
		t.Fatalf("expected 201 creating offer, got %d: %+v", createOfferResp.StatusCode, apiErr)
	}
	var offer offerResponse
	decodeBody(t, createOfferResp, &offer)

	if got := countAuditEntries(t, pool, tenant.ID, "bonus_offer.created", offer.ID); got != 1 {
		t.Fatalf("expected exactly 1 audit_log row for bonus_offer.created targeting %s, got %d", offer.ID, got)
	}

	createVersionResp := postJSON(t, srv, "/v1/admin/bonus/offers/"+offer.ID+"/versions", token.AccessToken, map[string]any{
		"reward_kind": "R2", "reward_asset_code": "USD", "reward_calculation": `{"amount":"1000"}`,
		"fulfillment_destination": "into_platform_wallet", "funding_source": "operator",
	})
	defer createVersionResp.Body.Close()
	if createVersionResp.StatusCode != 201 {
		var apiErr apierror.Error
		decodeBody(t, createVersionResp, &apiErr)
		t.Fatalf("expected 201 creating offer version, got %d: %+v", createVersionResp.StatusCode, apiErr)
	}
	var version offerVersionResponse
	decodeBody(t, createVersionResp, &version)

	if got := countAuditEntries(t, pool, tenant.ID, "bonus_offer_version.created", version.ID); got != 1 {
		t.Fatalf("expected exactly 1 audit_log row for bonus_offer_version.created targeting %s, got %d", version.ID, got)
	}
}

// seedCampaignVersionForHTTP is a fallback for
// TestCreateOfferAndVersion_HTTP_WriteAuditRecords in case
// seedBonusCampaignForHTTP's own campaign has no current version pinned
// (defensive - seedBonusCampaignForHTTP does not itself create a
// CampaignVersion, only seedBonusOfferForHTTP does).
func seedCampaignVersionForHTTP(t *testing.T, pool *db.Pool, tenantID, campaignID, staffID uuid.UUID) (uuid.UUID, error) {
	t.Helper()
	var versionID uuid.UUID
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		v, err := bonus.CreateCampaignVersion(ctx, tx, bonus.CampaignVersion{
			TenantID: tenantID, CampaignID: campaignID, VersionNumber: 1, Name: "V1",
			CreatedByActorType: bonus.ActorStaff, CreatedByActorID: staffID,
		})
		if err != nil {
			return err
		}
		versionID = v.ID
		_, err = bonus.SetCampaignCurrentVersion(ctx, tx, tenantID, campaignID, v.ID)
		return err
	})
	return versionID, err
}

func TestPublishOffer_HTTP_WritesAuditRecord(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	promo1 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "audit-publish-1")
	promo2 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "audit-publish-2")
	promo3 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "audit-publish-3")
	token1 := mustLoginStaff(t, srv, tenant.Slug, promo1.Email, "audit-publish-1")
	token2 := mustLoginStaff(t, srv, tenant.Slug, promo2.Email, "audit-publish-2")
	token3 := mustLoginStaff(t, srv, tenant.Slug, promo3.Email, "audit-publish-3")

	offerID, offerVersionID := seedBonusOfferForHTTP(t, pool, tenant.ID, brand.ID, promo1.ID)

	fileResp := postJSON(t, srv, "/v1/admin/bonus/change-requests", token1.AccessToken, map[string]any{
		"operation": "offer_publish", "target_type": "bonus_offers", "target_id": offerID.String(),
		"payload": map[string]string{"offer_version_id": offerVersionID.String()}, "reason_code": "audit-test",
	})
	var filed changeRequestResponse
	decodeBody(t, fileResp, &filed)
	for _, tok := range []string{token2.AccessToken, token3.AccessToken} {
		resp := postJSON(t, srv, "/v1/admin/bonus/change-requests/"+filed.ID+"/decide", tok, map[string]any{"decision": "approve"})
		resp.Body.Close()
	}

	publishResp := postJSON(t, srv, "/v1/admin/bonus/offers/"+offerID.String()+"/publish", token1.AccessToken, map[string]any{
		"offer_version_id": offerVersionID.String(),
	})
	defer publishResp.Body.Close()
	if publishResp.StatusCode != 200 {
		var apiErr apierror.Error
		decodeBody(t, publishResp, &apiErr)
		t.Fatalf("expected 200 publishing, got %d: %+v", publishResp.StatusCode, apiErr)
	}

	if got := countAuditEntries(t, pool, tenant.ID, "bonus_offer.published", offerID.String()); got != 1 {
		t.Fatalf("expected exactly 1 audit_log row for bonus_offer.published targeting %s, got %d", offerID, got)
	}
}

func TestMintEconomicOperation_HTTP_WritesAuditRecord(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	bonusOps := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleBonusOperations, "audit-eoi-1")
	token := mustLoginStaff(t, srv, tenant.Slug, bonusOps.Email, "audit-eoi-1")

	mintResp := postJSON(t, srv, "/v1/admin/bonus/economic-operations", token.AccessToken, map[string]any{
		"operation_type": "bonus_manual_grant", "subject_scope": "single_subject",
		"subject_ref": uuid.NewString(), "asset_code": "USD", "intended_aggregate_value": "100000",
		"recipient_ceiling": 1, "idempotency_key": "audit-eoi-" + uuid.NewString(),
	})
	defer mintResp.Body.Close()
	if mintResp.StatusCode != 201 {
		var apiErr apierror.Error
		decodeBody(t, mintResp, &apiErr)
		t.Fatalf("expected 201 minting, got %d: %+v", mintResp.StatusCode, apiErr)
	}
	var op economicOperationResponse
	decodeBody(t, mintResp, &op)

	if got := countAuditEntries(t, pool, tenant.ID, "economic_operation.minted", op.OperationID); got != 1 {
		t.Fatalf("expected exactly 1 audit_log row for economic_operation.minted targeting %s, got %d", op.OperationID, got)
	}
}

// TestManualGrantIssueAndActivate_HTTP_WritesAuditRecords proves the
// FIRST of the two HTTP-handler-owned audit.Record calls on this surface
// (bonus_grant.manual_issue_requested) actually fires when reached
// through the real HTTP surface - issuance (T.2's "issued is a decision,
// not a movement") skips the AssetAuthorization leg of the T.1 gate
// entirely (RG/Risk only), so it is reachable with this file's own
// lightweight fixtures.
//
// NAMED, DISCLOSED GAP (not silently narrowed): the SECOND call on this
// surface, bonus_grant.manual_issue_activated, fires only after
// ActivateGrant's FULL T.1 gate (AssetAuthorization -> RG -> Risk)
// allows - and AssetAuthorization fails closed on absent jurisdiction/
// tenant-jurisdiction-config data (ADR 0037 §C.1), which no HTTP-level
// test ANYWHERE in this codebase (not merely this Wave) currently
// constructs; every existing successful-activation test for this exact
// T.1 gate chain (internal/bonus's own lifecycleFixture) sets it up via
// several platform-admin-approved assetregistry.FileChangeRequest/
// DecideChangeRequest round-trips plus a dedicated test-only asset and
// jurisdiction, entirely at the package level. Building that full harness
// a second time, at the HTTP layer, purely to observe one audit.Record
// call whose call site is otherwise identical in shape to
// bonus_campaign.activated/bonus_offer.published (both already proven to
// fire via HTTP in this same file) would be materially disproportionate
// scope for this one assertion (CLAUDE.md's "no uncontrolled scope
// expansion") and risks mutating shared platform/asset-authorization
// state other concurrently-run tests in this shared dev database depend
// on. Verified instead by code inspection: newActivateManualGrantHandler
// calls audit.Record unconditionally immediately after
// ActivateManualGrantWithApproval returns outcome.Allowed=true, in the
// SAME transaction, before any response is written - structurally
// identical to the two call sites already proven live. Flagged here as a
// real, bounded pre-existing testability gap in the HTTP test harness
// (a jurisdiction/asset-authorization HTTP fixture builder does not exist
// for ANY domain yet), not something this dispatch silently worked around.
func TestManualGrantIssueAndActivate_HTTP_WritesAuditRecords(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	bonusOps := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleBonusOperations, "audit-mg-1")
	token := mustLoginStaff(t, srv, tenant.Slug, bonusOps.Email, "audit-mg-1")

	playerID, _ := seedActiveBonusPlayer(t, pool, tenant.ID, brand.ID, "audit-manual-grant")
	offerID, offerVersionID := seedBonusOfferForHTTP(t, pool, tenant.ID, brand.ID, bonusOps.ID)
	var campaignID, campaignVersionID uuid.UUID
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		o, err := bonus.GetOfferByID(ctx, tx, offerID)
		campaignID = o.CampaignID
		campaignVersionID = o.CampaignVersionID
		return err
	}); err != nil {
		t.Fatalf("resolve campaign for offer: %v", err)
	}

	eoiResp := postJSON(t, srv, "/v1/admin/bonus/economic-operations", token.AccessToken, map[string]any{
		"operation_type": "bonus_manual_grant", "subject_scope": "single_subject", "subject_ref": playerID.String(),
		"asset_code": "USD", "intended_aggregate_value": "100000", "recipient_ceiling": 1,
		"idempotency_key": "audit-mg-eoi-" + uuid.NewString(),
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
		t.Fatalf("expected 201 issuing, got %d: %+v", issueResp.StatusCode, apiErr)
	}
	var issued grantResponse
	decodeBody(t, issueResp, &issued)

	if got := countAuditEntries(t, pool, tenant.ID, "bonus_grant.manual_issue_requested", issued.ID); got != 1 {
		t.Fatalf("expected exactly 1 audit_log row for bonus_grant.manual_issue_requested targeting %s, got %d", issued.ID, got)
	}
}

func TestCreateBulkGrantJob_HTTP_WritesAuditRecord(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	bonusOps := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleBonusOperations, "audit-bulk-1")
	token := mustLoginStaff(t, srv, tenant.Slug, bonusOps.Email, "audit-bulk-1")

	playerID, _ := seedActiveBonusPlayer(t, pool, tenant.ID, brand.ID, "audit-bulk-job")
	offerID, offerVersionID := seedBonusOfferForHTTP(t, pool, tenant.ID, brand.ID, bonusOps.ID)
	var campaignID uuid.UUID
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		o, err := bonus.GetOfferByID(ctx, tx, offerID)
		campaignID = o.CampaignID
		return err
	}); err != nil {
		t.Fatalf("resolve campaign: %v", err)
	}

	createResp := postJSON(t, srv, "/v1/admin/bonus/bulk-jobs", token.AccessToken, map[string]any{
		"campaign_id": campaignID.String(), "offer_version_id": offerVersionID.String(),
		"target_kind": "single_player", "target_player_account_ids": []string{playerID.String()},
		"idempotency_key": "audit-bulk-" + uuid.NewString(),
	})
	defer createResp.Body.Close()
	if createResp.StatusCode != 201 {
		var apiErr apierror.Error
		decodeBody(t, createResp, &apiErr)
		t.Fatalf("expected 201 creating bulk job, got %d: %+v", createResp.StatusCode, apiErr)
	}
	var job bulkGrantJobResponse
	decodeBody(t, createResp, &job)

	if got := countAuditEntries(t, pool, tenant.ID, "bulk_grant_job.created", job.ID); got != 1 {
		t.Fatalf("expected exactly 1 audit_log row for bulk_grant_job.created targeting %s, got %d", job.ID, got)
	}
}
