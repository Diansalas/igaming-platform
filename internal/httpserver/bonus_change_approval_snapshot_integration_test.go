//go:build integration

// Stage 4H-B1 Wave 3 Phase 6 (`security`): regression proof for the
// approval-snapshot fix in bonus_governance_handlers.go.
//
// FINDING this closes: POST /v1/admin/bonus/change-requests/{id}/decide
// accepted threshold_at_decision and amount_at_decision from the REQUEST
// BODY and wrote them verbatim into bonus_change_approvals - the
// append-only, deny-update/deny-delete-protected row migration 0063
// created for exactly one purpose ("what stops a later threshold change
// from retroactively making a past decision look compliant... when the
// record is read during a dispute"). An approving principal could
// therefore author the very record meant to hold them to account:
// approve a large, above-threshold operation while asserting
// threshold_at_decision=999999999 and amount_at_decision=1, so the
// permanent, un-editable audit trail reads as a routine below-threshold
// decision. Both fields were also optional, so the record could simply be
// left blank.
//
// internal/withdrawal.Approve - the precedent migration 0063 names as its
// own model - already resolves the policy internally and snapshots it
// server-side. This test proves the bonus surface now does the same.
//
// SECOND FINDING this test also closes, found by writing it: filing a
// change request WITH an amount_at_request at all failed outright with a
// 500 - bonus.scanChangeRequest scanned that NUMERIC(38,0) column
// straight into a **big.Int, which only ever works while the column is
// NULL. Every pre-existing test filed amount-less requests, so the
// threshold/forensic data path had never once been exercised with a real
// amount.
//
// Pre-fix this test fails three ways: the filing step 500s, the
// snapshot-authoring approval is ACCEPTED (200) and writes 999999999/1,
// and an honest approval leaves both snapshot columns NULL. Post-fix the
// filing succeeds, the authoring attempt is refused with a 400, and the
// honest approval snapshots the server-resolved policy threshold and the
// request's own amount_at_request.
package httpserver

import (
	"context"
	"math/big"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

func TestDecideChangeRequest_SnapshotFieldsAreServerResolved(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	filer := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-pw-snap-1")
	approver := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-pw-snap-2")
	filerToken := mustLoginStaff(t, srv, tenant.Slug, filer.Email, "promo-pw-snap-1")
	approverToken := mustLoginStaff(t, srv, tenant.Slug, approver.Email, "promo-pw-snap-2")

	campaignID := seedBonusCampaignForHTTP(t, pool, tenant.ID, brand.ID, filer.ID)

	// A genuinely large, genuinely above-threshold request.
	const requestedAmount = "500000"
	fileResp := postJSON(t, srv, "/v1/admin/bonus/change-requests", filerToken.AccessToken, map[string]any{
		"operation": "campaign_activate", "target_type": "bonus_campaigns", "target_id": campaignID.String(),
		"payload": map[string]string{"action": "activate"}, "reason_code": "launch",
		"amount_at_request": requestedAmount, "asset_code": "USD",
	})
	var filed changeRequestResponse
	decodeBody(t, fileResp, &filed)
	if filed.ID == "" {
		t.Fatalf("filing the change request did not return an id")
	}

	// (a) The approver attempts to author its own forensic record. The
	// fields no longer exist on the request shape at all, so decodeJSON's
	// DisallowUnknownFields refuses the whole body - 400, nothing
	// recorded. Pre-fix this returned 200 and wrote 999999999/1.
	authored := postJSON(t, srv, "/v1/admin/bonus/change-requests/"+filed.ID+"/decide", approverToken.AccessToken, map[string]any{
		"decision":              "approve",
		"threshold_at_decision": "999999999",
		"amount_at_decision":    "1",
	})
	defer authored.Body.Close()
	if authored.StatusCode != 400 {
		t.Fatalf("expected 400 - a caller must not be able to supply the approval snapshot at all - got %d", authored.StatusCode)
	}

	// (b) The same approver, deciding honestly. The snapshot must still be
	// populated - server-resolved - never left blank.
	decideResp := postJSON(t, srv, "/v1/admin/bonus/change-requests/"+filed.ID+"/decide", approverToken.AccessToken, map[string]any{
		"decision": "approve",
	})
	defer decideResp.Body.Close()
	if decideResp.StatusCode != 200 {
		var apiErr apierror.Error
		decodeBody(t, decideResp, &apiErr)
		t.Fatalf("expected 200 for a correctly-separated approval, got %d: %+v", decideResp.StatusCode, apiErr)
	}

	var threshold, amount pgtype.Numeric
	requestID := uuid.MustParse(filed.ID)
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT threshold_at_decision, amount_at_decision FROM bonus_change_approvals WHERE tenant_id = $1 AND request_id = $2`,
			tenant.ID, requestID,
		).Scan(&threshold, &amount)
	}); err != nil {
		t.Fatalf("read back the approval row: %v", err)
	}

	// No bonus_approval_policies row exists for this tenant, so
	// ResolveApprovalPolicy's own fail-closed fallback (threshold 0, 2
	// required approvals) is what must have been snapshotted - never the
	// caller's 999999999.
	if !threshold.Valid {
		t.Fatalf("threshold_at_decision was left NULL - the forensic record is blank")
	}
	if got := numericString(t, threshold); got != "0" {
		t.Fatalf("threshold_at_decision is client-authored: expected the server-resolved policy threshold %q, got %q", "0", got)
	}
	if !amount.Valid {
		t.Fatalf("amount_at_decision was left NULL - the forensic record is blank")
	}
	if got := numericString(t, amount); got != requestedAmount {
		t.Fatalf("amount_at_decision is client-authored: expected the request's own amount_at_request %q, got %q", requestedAmount, got)
	}
}

// TestDecideChangeRequest_RejectRequiresReasonCode proves a reject with no
// reason code is refused as a 400 at the handler, rather than reaching
// migration 0063's own CHECK (decision <> 'reject' OR reason_code IS NOT
// NULL) and surfacing as an opaque "decision refused".
func TestDecideChangeRequest_RejectRequiresReasonCode(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	filer := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-pw-snap-3")
	approver := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "promo-pw-snap-4")
	filerToken := mustLoginStaff(t, srv, tenant.Slug, filer.Email, "promo-pw-snap-3")
	approverToken := mustLoginStaff(t, srv, tenant.Slug, approver.Email, "promo-pw-snap-4")

	campaignID := seedBonusCampaignForHTTP(t, pool, tenant.ID, brand.ID, filer.ID)
	fileResp := postJSON(t, srv, "/v1/admin/bonus/change-requests", filerToken.AccessToken, map[string]any{
		"operation": "campaign_activate", "target_type": "bonus_campaigns", "target_id": campaignID.String(),
		"payload": map[string]string{"action": "activate"}, "reason_code": "launch",
	})
	var filed changeRequestResponse
	decodeBody(t, fileResp, &filed)

	resp := postJSON(t, srv, "/v1/admin/bonus/change-requests/"+filed.ID+"/decide", approverToken.AccessToken, map[string]any{
		"decision": "reject",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 for a reject with no reason_code, got %d", resp.StatusCode)
	}
}

func numericString(t *testing.T, n pgtype.Numeric) string {
	t.Helper()
	v, err := n.Value()
	if err != nil {
		t.Fatalf("numeric value: %v", err)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("unexpected numeric driver value %T", v)
	}
	// Normalize "500000.000" style output to a plain integer string.
	b, ok := new(big.Float).SetString(s)
	if !ok {
		return s
	}
	i, _ := b.Int(nil)
	return i.String()
}
