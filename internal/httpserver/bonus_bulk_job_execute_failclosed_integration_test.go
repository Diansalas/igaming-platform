//go:build integration

// Stage 4H-B1 Wave 3 Phase 6 (`security`), item 4 of the brief: the
// security confirmation of the completeness gap Phase 4's risk report
// flagged as F3.
//
// THE GAP (bonus-engine's to close, not `security`'s - documented, not
// fixed here): newCreateBulkGrantJobHandler
// (bonus_domain_ops_handlers.go) never populates
// bulk_grant_jobs.parent_operation_id, and RunStaticBulkGrantJob refuses
// outright when it is NULL ("bulk grant job %s has no
// parent_operation_id"). So POST /v1/admin/bonus/bulk-jobs/{jobID}/execute
// can never succeed for a job created through the HTTP surface, however
// correctly approved.
//
// WHAT THIS TEST ASSERTS (the security question, which is not "does it
// work" but "when it doesn't work, does anything leak"): the failure is
// FAIL-CLOSED and ATOMIC. ExecuteBulkGrantJobWithApproval consumes the
// four-eyes approval and flips the job to 'running' BEFORE
// RunStaticBulkGrantJob's parent_operation_id guard fires, so a
// non-transactional implementation would leave a burned approval and a
// permanently 'running' job behind - a real availability/governance
// hazard, since bonus_change_requests' migration-0063 immutability
// trigger forbids any transition out of 'applied' and the approval could
// never be re-obtained for that job. It does not: deps.DB.WithTenant
// rolls the whole transaction back, so after the 500 the job is still
// queued, the change request is still pending and still consumable, and
// no job item, Grant or ledger entry exists.
//
// If bonus-engine later fixes the parent_operation_id gap, this test's
// expected status code changes but its state assertions are what must
// keep holding for every OTHER mid-execution failure (a denied item, a
// transient DB error) - they are the property, not the 500.
package httpserver

import (
	"context"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

func TestBulkGrantJobExecute_HTTP_FailsClosedWithNoPartialState(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	filer := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleBonusOperations, "ops-pw-bulk-fc-1")
	approver1 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleBonusOperations, "ops-pw-bulk-fc-2")
	approver2 := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleBonusOperations, "ops-pw-bulk-fc-3")
	filerToken := mustLoginStaff(t, srv, tenant.Slug, filer.Email, "ops-pw-bulk-fc-1")
	approver1Token := mustLoginStaff(t, srv, tenant.Slug, approver1.Email, "ops-pw-bulk-fc-2")
	approver2Token := mustLoginStaff(t, srv, tenant.Slug, approver2.Email, "ops-pw-bulk-fc-3")

	playerID, _ := seedActiveBonusPlayer(t, pool, tenant.ID, brand.ID, "bulk-failclosed")
	offerID, offerVersionID := seedBonusOfferForHTTP(t, pool, tenant.ID, brand.ID, filer.ID)
	var campaignID uuid.UUID
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		o, err := bonus.GetOfferByID(ctx, tx, offerID)
		campaignID = o.CampaignID
		return err
	}); err != nil {
		t.Fatalf("resolve campaign: %v", err)
	}

	createResp := postJSON(t, srv, "/v1/admin/bonus/bulk-jobs", filerToken.AccessToken, map[string]any{
		"campaign_id": campaignID.String(), "offer_version_id": offerVersionID.String(),
		"target_kind": "player_list", "target_player_account_ids": []string{playerID.String()},
		"idempotency_key": "bulk-failclosed-" + uuid.NewString(),
	})
	defer createResp.Body.Close()
	if createResp.StatusCode != 201 {
		var apiErr apierror.Error
		decodeBody(t, createResp, &apiErr)
		t.Fatalf("expected 201 creating the bulk job, got %d: %+v", createResp.StatusCode, apiErr)
	}
	var job bulkGrantJobResponse
	decodeBody(t, createResp, &job)
	jobID := uuid.MustParse(job.ID)

	// A genuine, fully-separated, correctly-payload-bound approval.
	payload := bonus.BulkJobExecutePayloadMatch(offerVersionID, "USD", big.NewInt(1000), []uuid.UUID{playerID})
	fileResp := postJSON(t, srv, "/v1/admin/bonus/change-requests", filerToken.AccessToken, map[string]any{
		"operation": "bulk_job_execute", "target_type": "bulk_grant_jobs", "target_id": job.ID,
		"payload": json.RawMessage(payload), "reason_code": "wave3-failclosed",
	})
	var filed changeRequestResponse
	decodeBody(t, fileResp, &filed)
	if filed.ID == "" {
		t.Fatalf("filing the bulk_job_execute request failed")
	}
	for _, tok := range []string{approver1Token.AccessToken, approver2Token.AccessToken} {
		resp := postJSON(t, srv, "/v1/admin/bonus/change-requests/"+filed.ID+"/decide", tok, map[string]any{"decision": "approve"})
		if resp.StatusCode != 200 {
			var apiErr apierror.Error
			decodeBody(t, resp, &apiErr)
			resp.Body.Close()
			t.Fatalf("expected 200 recording an approval, got %d: %+v", resp.StatusCode, apiErr)
		}
		resp.Body.Close()
	}

	execResp := postJSON(t, srv, "/v1/admin/bonus/bulk-jobs/"+job.ID+"/execute", filerToken.AccessToken, map[string]any{
		"amount": "1000",
	})
	defer execResp.Body.Close()
	if execResp.StatusCode != 500 {
		t.Fatalf("F3 (bulk_grant_jobs.parent_operation_id is never populated by the create handler) appears to have been fixed - "+
			"expected 500, got %d. Re-read this file's header: the state assertions below are the property that must keep holding.",
			execResp.StatusCode)
	}

	// Nothing may have survived the failed execution.
	var jobStatus, requestState string
	var itemCount, grantCount int
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT status FROM bulk_grant_jobs WHERE tenant_id = $1 AND id = $2`, tenant.ID, jobID).Scan(&jobStatus); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT state FROM bonus_change_requests WHERE tenant_id = $1 AND id = $2`, tenant.ID, uuid.MustParse(filed.ID)).Scan(&requestState); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM bulk_grant_job_items WHERE tenant_id = $1 AND bulk_grant_job_id = $2`, tenant.ID, jobID).Scan(&itemCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM bonus_grants WHERE tenant_id = $1 AND campaign_id = $2`, tenant.ID, campaignID).Scan(&grantCount)
	}); err != nil {
		t.Fatalf("read back post-failure state: %v", err)
	}

	if jobStatus != string(bonus.BulkJobQueued) {
		t.Fatalf("the failed execution left the job in %q - it must remain queued, not stuck 'running'", jobStatus)
	}
	if requestState != string(bonus.ChangeRequestPending) {
		t.Fatalf("the failed execution BURNED the four-eyes approval (state=%q) - migration 0063 forbids any transition out of 'applied', "+
			"so the operation could never be re-approved for this job", requestState)
	}
	if itemCount != 0 || grantCount != 0 {
		t.Fatalf("the failed execution left partial state behind (items=%d grants=%d)", itemCount, grantCount)
	}
}
