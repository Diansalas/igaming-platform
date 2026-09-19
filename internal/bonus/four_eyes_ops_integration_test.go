//go:build integration

// Stage 4H-B1 Wave 3 Phase 3 (item E) test suite for the four-eyes
// application-level wiring (four_eyes_ops.go). Per the dispatch's own
// item G instruction, this suite doubles as the adversarial re-test for
// these NEW surfaces: every wrapper is proven to REFUSE the underlying
// operation with no filed/approved bonus_change_requests row at all
// (never a silent fallback to "just do it"), matching the security doc's
// own "attempt each of the eight operations WITHOUT a matching approved
// request, proven to fail before Phase B and succeed [be correctly
// refused] after" instruction (docs/governance/wave-3-reconnaissance.md
// §5's own recommended Phase E review, exercised here directly against
// the wrapper functions rather than deferred to a later phase).
package bonus

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/economicop"
)

// seedExtraStaff mirrors seedLifecycleFixture's own staff-seeding shape,
// for a THIRD distinct staff/person - every four-eyes test in this file
// needs a filer plus TWO approvers, all three resolving to distinct
// persons (the governance trigger's own self-approval refusal, and
// fallbackApprovalPolicy's default RequiredApprovals=2, both require
// this).
func seedExtraStaff(t *testing.T, pool *db.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	staffID := uuid.New()
	personID := uuid.New()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status)
			VALUES ($1, $2, $3, 'x', 'bonus_operations', $4, 'active')`, staffID, tenantID, staffID.String()+"@staff.example.com", personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed extra staff: %v", err)
	}
	return staffID
}

// fileAndDoublyApprove files a bonus_change_requests row and records TWO
// approvals from staff2/staff3 (both distinct from filerID and from each
// other) - the shape fallbackApprovalPolicy's default RequiredApprovals=2
// needs, mirroring lifecycle_integration_test.go's own file+approve
// pattern for held_disposition_resolve.
func fileAndDoublyApprove(t *testing.T, pool *db.Pool, tenantID uuid.UUID, operation ChangeOperation, targetType string, targetID uuid.UUID, payload []byte, filerID, approver1ID, approver2ID uuid.UUID) uuid.UUID {
	t.Helper()
	var requestID uuid.UUID
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		req, err := FileChangeRequest(ctx, tx, ChangeRequest{
			TenantID: tenantID, Operation: operation, TargetType: targetType, TargetID: targetID,
			Payload: payload, ReasonCode: "wave3-four-eyes-test", RequestedByPrincipalID: filerID,
		})
		if err != nil {
			return err
		}
		requestID = req.ID
		if err := RecordChangeApproval(ctx, tx, tenantID, req.ID, approver1ID, "approve", nil, nil, nil); err != nil {
			return err
		}
		return RecordChangeApproval(ctx, tx, tenantID, req.ID, approver2ID, "approve", nil, nil, nil)
	})
	if err != nil {
		t.Fatalf("file+doubly-approve %s: %v", operation, err)
	}
	return requestID
}

// --- campaign_activate ---

func TestActivateCampaign_RefusedWithoutApproval(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	var campaignID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := CreateCampaign(ctx, tx, Campaign{TenantID: f.tenantID, BrandID: &f.brandID, CreatedByActorType: ActorStaff, CreatedByActorID: f.staffID})
		campaignID = c.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed draft campaign: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ActivateCampaign(ctx, tx, f.tenantID, campaignID, f.staffID)
		return err
	})
	if !errors.Is(err, ErrChangeRequestNotApproved) {
		t.Fatalf("expected ErrChangeRequestNotApproved with no filed/approved request, got %v", err)
	}

	// Status must remain draft - a refused four-eyes consume must not
	// have any side effect on the Campaign itself.
	var status string
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := GetCampaignByID(ctx, tx, campaignID)
		status = string(c.Status)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != string(CampaignDraft) {
		t.Fatalf("expected campaign to remain draft after a refused activation attempt, got %s", status)
	}
}

func TestActivateCampaign_SucceedsWithApproval(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	staff3 := seedExtraStaff(t, pool, f.tenantID)
	var campaignID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := CreateCampaign(ctx, tx, Campaign{TenantID: f.tenantID, BrandID: &f.brandID, CreatedByActorType: ActorStaff, CreatedByActorID: f.staffID})
		campaignID = c.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed draft campaign: %v", err)
	}

	fileAndDoublyApprove(t, pool, f.tenantID, ChangeOpCampaignActivate, "bonus_campaigns", campaignID, campaignActivatePayloadMatch, f.staffID, f.staff2ID, staff3)

	var activated Campaign
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var actErr error
		activated, actErr = ActivateCampaign(ctx, tx, f.tenantID, campaignID, f.staffID)
		return actErr
	})
	if err != nil {
		t.Fatalf("ActivateCampaign: %v", err)
	}
	if activated.Status != CampaignActive {
		t.Fatalf("expected active, got %s", activated.Status)
	}

	// Attempting to consume the SAME approval a second time must be
	// refused (the request already transitioned to 'applied' -
	// migration 0063's own single-consume guarantee).
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ActivateCampaign(ctx, tx, f.tenantID, campaignID, f.staffID)
		return err
	})
	if !errors.Is(err, ErrChangeRequestNotApproved) {
		t.Fatalf("expected a SECOND activate attempt to be refused (approval already consumed), got %v", err)
	}
}

// --- offer_publish ---

func TestPublishOfferVersion_RefusedWithoutApproval(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := PublishOfferVersion(ctx, tx, f.tenantID, co.offerID, co.offerVersionID, f.staffID)
		return err
	})
	if !errors.Is(err, ErrChangeRequestNotApproved) {
		t.Fatalf("expected ErrChangeRequestNotApproved with no filed/approved request, got %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		o, err := GetOfferByID(ctx, tx, co.offerID)
		if err != nil {
			return err
		}
		if o.Status != OfferDraft {
			return fmt.Errorf("expected offer to remain draft, got %s", o.Status)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPublishOfferVersion_SucceedsWithApproval(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	staff3 := seedExtraStaff(t, pool, f.tenantID)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	payloadMatch := []byte(fmt.Sprintf(`{"offer_version_id":%q}`, co.offerVersionID))
	fileAndDoublyApprove(t, pool, f.tenantID, ChangeOpOfferPublish, "bonus_offers", co.offerID, payloadMatch, f.staffID, f.staff2ID, staff3)

	var published Offer
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var pubErr error
		published, pubErr = PublishOfferVersion(ctx, tx, f.tenantID, co.offerID, co.offerVersionID, f.staffID)
		return pubErr
	})
	if err != nil {
		t.Fatalf("PublishOfferVersion: %v", err)
	}
	if published.Status != OfferActive {
		t.Fatalf("expected active, got %s", published.Status)
	}
	if published.CurrentVersionID == nil || *published.CurrentVersionID != co.offerVersionID {
		t.Fatalf("expected current_version_id pinned to %s, got %v", co.offerVersionID, published.CurrentVersionID)
	}
}

// --- manual_grant_issue ---

func mintApprovedManualGrantEOI(t *testing.T, pool *db.Pool, f lifecycleFixture) uuid.UUID {
	t.Helper()
	var opID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		asset := f.assetCode
		op, err := MintRootOperation(ctx, tx, MintRootOperationParams{
			TenantID: f.tenantID, OperationType: economicop.OperationBonusManualGrant,
			InitiatingActorType: "staff", InitiatingActorID: f.staffID,
			SubjectScope: economicop.SubjectScopeSingle, SubjectRef: &f.playerID, AssetCode: &asset,
			IntendedAggregateValue: big.NewInt(1_000_000), IdempotencyKey: "manual-grant-eoi-" + uuid.NewString(),
			CorrelationID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour), ApprovalState: economicop.ApprovalApproved,
		})
		opID = op.OperationID
		return err
	})
	if err != nil {
		t.Fatalf("mint manual grant EOI: %v", err)
	}
	return opID
}

func TestManualGrantWithApproval_RefusedWithoutApproval(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
	parentOpID := mintApprovedManualGrantEOI(t, pool, f)
	amount := big.NewInt(1000)

	var grant Grant
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "manual-grant-refused")
		g.CreatedByActorType = ActorStaff
		var issueErr error
		grant, _, issueErr = IssueManualGrantRequest(ctx, tx, g, parentOpID, f.jurisdictionCode, f.staffID)
		return issueErr
	})
	if err != nil {
		t.Fatalf("IssueManualGrantRequest: %v", err)
	}
	if grant.Status != GrantIssued {
		t.Fatalf("expected issued, got %s", grant.Status)
	}

	// No bonus_change_requests row filed/approved at all - activation
	// must be refused.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := ActivateManualGrantWithApproval(ctx, tx, f.tenantID, grant.ID, parentOpID, f.jurisdictionCode, f.staffID, amount)
		return err
	})
	if !errors.Is(err, ErrChangeRequestNotApproved) {
		t.Fatalf("expected ErrChangeRequestNotApproved with no filed/approved request, got %v", err)
	}

	if status := readGrantStatus(t, pool, f.tenantID, grant.ID); status != GrantIssued {
		t.Fatalf("expected the grant to remain issued (never activated) after a refused attempt, got %s", status)
	}
}

func TestManualGrantWithApproval_SucceedsWithApproval(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	staff3 := seedExtraStaff(t, pool, f.tenantID)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
	parentOpID := mintApprovedManualGrantEOI(t, pool, f)
	amount := big.NewInt(1000)

	var grant Grant
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "manual-grant-approved")
		g.CreatedByActorType = ActorStaff
		var issueErr error
		grant, _, issueErr = IssueManualGrantRequest(ctx, tx, g, parentOpID, f.jurisdictionCode, f.staffID)
		return issueErr
	})
	if err != nil {
		t.Fatalf("IssueManualGrantRequest: %v", err)
	}

	// File+approve AFTER issuance - the SEP-1 trigger (migration 0063)
	// requires a real bonus_grants row to already exist at the moment an
	// approval is recorded (see ActivateManualGrantWithApproval's own
	// doc comment for the full citation).
	payloadMatch := manualGrantIssuePayloadMatch(grant.PlayerAccountID, grant.OfferVersionID, grant.AssetCode, amount)
	fileAndDoublyApprove(t, pool, f.tenantID, ChangeOpManualGrantIssue, "bonus_grants", grant.ID, payloadMatch, f.staffID, f.staff2ID, staff3)

	var activated Grant
	var outcome GateOutcome
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var actErr error
		activated, outcome, actErr = ActivateManualGrantWithApproval(ctx, tx, f.tenantID, grant.ID, parentOpID, f.jurisdictionCode, f.staffID, amount)
		return actErr
	})
	if err != nil {
		t.Fatalf("ActivateManualGrantWithApproval: %v", err)
	}
	if !outcome.Allowed {
		t.Fatalf("expected the gate chain to allow, got %+v", outcome)
	}
	if activated.Status != GrantActivated {
		t.Fatalf("expected activated, got %s", activated.Status)
	}

	// A second activation attempt against the SAME already-activated
	// Grant is illegal (ActivateGrant's own CAS guard) - proving the
	// four-eyes consume is not the only thing preventing replay.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := ActivateManualGrantWithApproval(ctx, tx, f.tenantID, grant.ID, parentOpID, f.jurisdictionCode, f.staffID, amount)
		return err
	})
	if err == nil {
		t.Fatal("expected a second activation attempt against an already-activated grant to fail")
	}
}

// TestManualGrantWithApproval_PayloadMismatchRefused proves the
// SEC-W15-02/CRM-decomposition-class actor/subject-substitution vector
// cannot succeed against this NEW surface: an approval filed for one
// amount must not authorize activating the SAME grant for a DIFFERENT
// amount.
func TestManualGrantWithApproval_PayloadMismatchRefused(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	staff3 := seedExtraStaff(t, pool, f.tenantID)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
	parentOpID := mintApprovedManualGrantEOI(t, pool, f)
	approvedAmount := big.NewInt(1000)

	var grant Grant
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "manual-grant-mismatch")
		g.CreatedByActorType = ActorStaff
		var issueErr error
		grant, _, issueErr = IssueManualGrantRequest(ctx, tx, g, parentOpID, f.jurisdictionCode, f.staffID)
		return issueErr
	})
	if err != nil {
		t.Fatalf("IssueManualGrantRequest: %v", err)
	}

	payloadMatch := manualGrantIssuePayloadMatch(grant.PlayerAccountID, grant.OfferVersionID, grant.AssetCode, approvedAmount)
	fileAndDoublyApprove(t, pool, f.tenantID, ChangeOpManualGrantIssue, "bonus_grants", grant.ID, payloadMatch, f.staffID, f.staff2ID, staff3)

	// Attempt to activate the SAME grant for a DIFFERENT amount than what
	// was approved.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := ActivateManualGrantWithApproval(ctx, tx, f.tenantID, grant.ID, parentOpID, f.jurisdictionCode, f.staffID, big.NewInt(999999))
		return err
	})
	if !errors.Is(err, ErrChangeRequestNotApproved) {
		t.Fatalf("expected a payload-mismatched (different amount) activation to be refused, got %v", err)
	}
}

// --- bulk_job_execute ---

// --- bulk_job_execute ---

func TestExecuteBulkGrantJobWithApproval_RefusedWithoutApproval(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	var jobID uuid.UUID
	var rootID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		asset := f.assetCode
		ceiling := int32(10)
		op, err := MintRootOperation(ctx, tx, MintRootOperationParams{
			TenantID: f.tenantID, OperationType: economicop.OperationBonusBulkGrant,
			InitiatingActorType: "staff", InitiatingActorID: f.staffID,
			SubjectScope: economicop.SubjectScopeEnumeratedSet, AssetCode: &asset, RecipientCeiling: &ceiling,
			IntendedAggregateValue: big.NewInt(1_000_000), IdempotencyKey: "bulk-refused-" + uuid.NewString(),
			CorrelationID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour), ApprovalState: economicop.ApprovalApproved,
		})
		if err != nil {
			return err
		}
		rootID = op.OperationID
		job, err := CreateBulkGrantJob(ctx, tx, BulkGrantJob{
			TenantID: f.tenantID, BrandID: f.brandID, CampaignID: co.campaignID, OfferVersionID: co.offerVersionID,
			TargetKind: TargetPlayerList, TargetPlayerList: []uuid.UUID{f.playerID},
			RequestedByPrincipalID: f.staffID, ApprovalState: BulkApprovalApproved,
			IdempotencyKey: "bulk-refused-job-" + uuid.NewString(), ParentOperationID: &rootID,
		})
		jobID = job.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed bulk job: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		template := newTestOfferGrant(f, co, "")
		_, err := ExecuteBulkGrantJobWithApproval(ctx, tx, f.tenantID, jobID, f.staffID, StaticPlayerListTarget{PlayerAccountIDs: []uuid.UUID{f.playerID}}, template, f.jurisdictionCode, uuid.Nil, big.NewInt(1000))
		return err
	})
	if !errors.Is(err, ErrChangeRequestNotApproved) {
		t.Fatalf("expected ErrChangeRequestNotApproved with no filed/approved request, got %v", err)
	}

	var itemCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM bulk_grant_job_items WHERE tenant_id = $1 AND bulk_grant_job_id = $2`, f.tenantID, jobID).Scan(&itemCount)
	})
	if err != nil {
		t.Fatal(err)
	}
	if itemCount != 0 {
		t.Fatalf("expected zero item rows for a refused bulk job execution, got %d", itemCount)
	}
}

func TestExecuteBulkGrantJobWithApproval_SucceedsWithApproval(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	staff3 := seedExtraStaff(t, pool, f.tenantID)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	var jobID, rootID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		asset := f.assetCode
		ceiling := int32(10)
		op, err := MintRootOperation(ctx, tx, MintRootOperationParams{
			TenantID: f.tenantID, OperationType: economicop.OperationBonusBulkGrant,
			InitiatingActorType: "staff", InitiatingActorID: f.staffID,
			SubjectScope: economicop.SubjectScopeEnumeratedSet, AssetCode: &asset, RecipientCeiling: &ceiling,
			IntendedAggregateValue: big.NewInt(1_000_000), IdempotencyKey: "bulk-approved-" + uuid.NewString(),
			CorrelationID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour), ApprovalState: economicop.ApprovalApproved,
		})
		if err != nil {
			return err
		}
		rootID = op.OperationID
		job, err := CreateBulkGrantJob(ctx, tx, BulkGrantJob{
			TenantID: f.tenantID, BrandID: f.brandID, CampaignID: co.campaignID, OfferVersionID: co.offerVersionID,
			TargetKind: TargetPlayerList, TargetPlayerList: []uuid.UUID{f.playerID},
			RequestedByPrincipalID: f.staffID, ApprovalState: BulkApprovalApproved,
			IdempotencyKey: "bulk-approved-job-" + uuid.NewString(), ParentOperationID: &rootID,
		})
		jobID = job.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed bulk job: %v", err)
	}

	// Stage 4H-B1 Wave 3 Phase 6 (`security`): the approval payload now
	// pins the ECONOMIC payload (offer version, asset, per-recipient
	// amount, recipient set), not just the bare action - see
	// BulkJobExecutePayloadMatch's own doc comment.
	payloadMatch := BulkJobExecutePayloadMatch(co.offerVersionID, f.assetCode, big.NewInt(1000), []uuid.UUID{f.playerID})
	fileAndDoublyApprove(t, pool, f.tenantID, ChangeOpBulkJobExecute, "bulk_grant_jobs", jobID, payloadMatch, f.staffID, f.staff2ID, staff3)

	var result ExecuteBulkGrantJobResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		template := newTestOfferGrant(f, co, "")
		var execErr error
		result, execErr = ExecuteBulkGrantJobWithApproval(ctx, tx, f.tenantID, jobID, f.staffID, StaticPlayerListTarget{PlayerAccountIDs: []uuid.UUID{f.playerID}}, template, f.jurisdictionCode, uuid.Nil, big.NewInt(1000))
		return execErr
	})
	if err != nil {
		t.Fatalf("ExecuteBulkGrantJobWithApproval: %v", err)
	}
	if result.Status != BulkJobCompleted {
		t.Fatalf("expected completed, got %s", result.Status)
	}

	items, err := listBulkJobItemsForTest(t, pool, f.tenantID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Outcome != ItemIssued {
		t.Fatalf("expected exactly one issued item, got %+v", items)
	}
}

func listBulkJobItemsForTest(t *testing.T, pool *db.Pool, tenantID, jobID uuid.UUID) ([]BulkGrantJobItem, error) {
	t.Helper()
	var items []BulkGrantJobItem
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		items, err = ListBulkGrantJobItems(ctx, tx, tenantID, jobID)
		return err
	})
	return items, err
}
