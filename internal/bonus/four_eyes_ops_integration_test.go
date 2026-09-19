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
	"strings"
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

// forceActivateManualGrantWithApprovalForTest mirrors
// ActivateManualGrantWithApproval's own four-eyes-consume-then-EOI-consume
// PostGateHook EXACTLY (same resolveRequiredApprovals/
// manualGrantIssuePayloadMatch/ConsumeApprovedChangeRequest/
// economicop.ConsumeRootBudget calls), but reaches the effecting write via
// forceActivateGrantForTestErr instead of the full ActivateGrant/T.1 gate
// chain (see that helper's own doc comment, lifecycle_integration_test.go,
// for why: Stage 4I's jurisdiction resolver now unconditionally denies
// T.1's AssetAuthorization layer for every player-scoped operation, which
// would otherwise mask - never actually exercise - the four-eyes consume
// this file's own tests were written to prove). This is the ONLY
// difference from calling ActivateManualGrantWithApproval directly.
func forceActivateManualGrantWithApprovalForTest(ctx context.Context, tx pgx.Tx, tenantID, grantID, parentOperationID, actorID uuid.UUID, amount *big.Int) (Grant, error) {
	g, err := GetGrantByID(ctx, tx, grantID)
	if err != nil {
		return Grant{}, err
	}
	requiredApprovals, err := resolveRequiredApprovals(ctx, tx, tenantID, ChangeOpManualGrantIssue, &g.BrandID, &g.AssetCode)
	if err != nil {
		return Grant{}, err
	}
	payloadMatch := manualGrantIssuePayloadMatch(g.PlayerAccountID, g.OfferVersionID, g.AssetCode, amount)
	playerAccountID := g.PlayerAccountID
	postGateHook := func(hookCtx context.Context, hookTx pgx.Tx) error {
		if _, err := ConsumeApprovedChangeRequest(hookCtx, hookTx, tenantID, ChangeOpManualGrantIssue, grantID, payloadMatch, requiredApprovals, actorID); err != nil {
			return err
		}
		op, getErr := economicop.GetByID(hookCtx, hookTx, parentOperationID)
		if getErr != nil {
			return getErr
		}
		return economicop.ConsumeRootBudget(hookCtx, hookTx, tenantID, op.RootOperationID, economicop.OperationBonusManualGrant, playerAccountID, amount)
	}
	return forceActivateGrantForTestErr(ctx, tx, tenantID, grantID, ActivateGrantParams{
		ActorType: ActorStaff, ActorID: actorID, Amount: amount, PostGateHook: postGateHook,
	})
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
		// RecipientCeiling is mandatory for this operation_type since Stage
		// 4H-B1 Wave 3 Phase 10 (economicop.ValidateRootAuthorizationBounds);
		// 1 is the only value a single_subject root may declare.
		ceiling := int32(1)
		op, err := MintRootOperation(ctx, tx, MintRootOperationParams{
			TenantID: f.tenantID, OperationType: economicop.OperationBonusManualGrant,
			InitiatingActorType: "staff", InitiatingActorID: f.staffID,
			SubjectScope: economicop.SubjectScopeSingle, SubjectRef: &f.playerID, AssetCode: &asset,
			RecipientCeiling:       &ceiling,
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
		grant, _, issueErr = IssueManualGrantRequest(ctx, tx, g, parentOpID, f.staffID)
		return issueErr
	})
	if err != nil {
		t.Fatalf("IssueManualGrantRequest: %v", err)
	}
	if grant.Status != GrantIssued {
		t.Fatalf("expected issued, got %s", grant.Status)
	}

	// No bonus_change_requests row filed/approved at all - activation
	// must be refused. Uses forceActivateManualGrantWithApprovalForTest
	// (see its own doc comment above) rather than
	// ActivateManualGrantWithApproval directly: Stage 4I's jurisdiction
	// resolver now unconditionally denies T.1's AssetAuthorization layer
	// for every player-scoped operation (this file's own tests are not
	// about that gate), which would otherwise mask the four-eyes refusal
	// this test exists to prove.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := forceActivateManualGrantWithApprovalForTest(ctx, tx, f.tenantID, grant.ID, parentOpID, f.staffID, amount)
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
		grant, _, issueErr = IssueManualGrantRequest(ctx, tx, g, parentOpID, f.staffID)
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

	// Uses forceActivateManualGrantWithApprovalForTest (see its own doc
	// comment above): this test's subject is the four-eyes consume, which
	// Stage 4I's now-unconditional jurisdiction denial at T.1 would
	// otherwise mask entirely (activation would deny at
	// AssetAuthorization before the four-eyes PostGateHook ever ran).
	var activated Grant
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var actErr error
		activated, actErr = forceActivateManualGrantWithApprovalForTest(ctx, tx, f.tenantID, grant.ID, parentOpID, f.staffID, amount)
		return actErr
	})
	if err != nil {
		t.Fatalf("ActivateManualGrantWithApproval: %v", err)
	}
	if activated.Status != GrantActivated {
		t.Fatalf("expected activated, got %s", activated.Status)
	}

	// A second activation attempt against the SAME already-activated
	// Grant is illegal (ActivateGrant's own CAS guard) - proving the
	// four-eyes consume is not the only thing preventing replay.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := forceActivateManualGrantWithApprovalForTest(ctx, tx, f.tenantID, grant.ID, parentOpID, f.staffID, amount)
		return err
	})
	// A bare non-nil check here would also pass if the SECOND attempt
	// failed for a DIFFERENT reason (e.g. the four-eyes consume refusing a
	// second time because the approval was already consumed by the first
	// activation) - which would not actually prove the CAS guard this
	// test's own name and doc comment claim to exercise. Pin the specific
	// sentinel ActivateGrant's own status check returns
	// (lifecycle.go: "g.Status != GrantIssued") so a future reordering
	// that let a different error mask this one would be caught.
	if err == nil {
		t.Fatal("expected a second activation attempt against an already-activated grant to fail")
	} else if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("expected ErrIllegalTransition (the Grant-status CAS guard, not e.g. a re-consumed-approval error), got %v", err)
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
		grant, _, issueErr = IssueManualGrantRequest(ctx, tx, g, parentOpID, f.staffID)
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
		_, err := forceActivateManualGrantWithApprovalForTest(ctx, tx, f.tenantID, grant.ID, parentOpID, f.staffID, big.NewInt(999999))
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
		_, err := ExecuteBulkGrantJobWithApproval(ctx, tx, f.tenantID, jobID, f.staffID, StaticPlayerListTarget{PlayerAccountIDs: []uuid.UUID{f.playerID}}, template, uuid.Nil, big.NewInt(1000))
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

	// This test's own subject is the bulk_job_execute four-eyes consume
	// (proving it does NOT refuse once genuinely approved) - it is filed
	// and consumed at the JOB level, entirely BEFORE RunStaticBulkGrantJob
	// ever runs a single item's own ActivateGrant/T.1 gate (four_eyes_ops.go's
	// own ExecuteBulkGrantJobWithApproval), so it is UNAFFECTED by Stage
	// 4I. What IS affected is the per-recipient item's own activation,
	// which now honestly, correctly denies at T.1's AssetAuthorization
	// layer (every player-scoped jurisdiction resolution is
	// unresolved(no_signal) today - see resolveGrantJurisdiction's own doc
	// comment, eligibility.go) - so the job completes with exactly one
	// DENIED item, not an issued one, and the job's own final status
	// reflects that (BulkJobPartiallyCompleted, four_eyes_ops.go's own
	// finalStatus logic).
	var result ExecuteBulkGrantJobResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		template := newTestOfferGrant(f, co, "")
		var execErr error
		result, execErr = ExecuteBulkGrantJobWithApproval(ctx, tx, f.tenantID, jobID, f.staffID, StaticPlayerListTarget{PlayerAccountIDs: []uuid.UUID{f.playerID}}, template, uuid.Nil, big.NewInt(1000))
		return execErr
	})
	if err != nil {
		t.Fatalf("ExecuteBulkGrantJobWithApproval: %v", err)
	}
	if result.Status != BulkJobPartiallyCompleted {
		t.Fatalf("expected partially_completed (the item denied at T.1's jurisdiction-dependent AssetAuthorization layer, Stage 4I), got %s", result.Status)
	}

	items, err := listBulkJobItemsForTest(t, pool, f.tenantID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Outcome != ItemDenied {
		t.Fatalf("expected exactly one denied item (Stage 4I's disclosed jurisdiction-resolution gap, not a four-eyes refusal), got %+v", items)
	}
	if items[0].ReasonCode == nil || !strings.Contains(*items[0].ReasonCode, "asset_authorization") {
		t.Fatalf("expected the denial reason to name asset_authorization (proving the four-eyes consume itself succeeded and this is the DIFFERENT, jurisdiction-dependent gate), got %+v", items[0].ReasonCode)
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

// --- held_disposition_resolve (SEC-4I-F1) ---

// TestHeldDispositionResolve_CallerCannotLowerRequiredApprovalsBelowPolicy
// is the regression test for SEC-4I-F1 (security, Stage 4I Phase 4 design
// review): held_disposition_ops.go's own ResolveHeldDispositionAction used
// to trust a caller-supplied RequiredApprovals count UNCLAMPED (it flowed,
// via bonus_handlers.go's now-removed "required_approvals" request field,
// straight into bonus_change_consume_approved_request), letting a staff
// member lower a tenant's configured N-approver four-eyes threshold to as
// little as 1 for a real disposition resolution (ACTION_ROUTE_TO_CASH
// moves value out of player_bonus_held; ACTION_REFORFEIT is used here
// since it needs no T.1/AssetAuthorization setup, and the defect is in the
// SHARED four-eyes consume both actions go through, not in either action's
// own effecting logic).
//
// This test configures a real policy of 3 required approvals, records
// only ONE genuine approval, and calls ResolveHeldDispositionAction with a
// caller-supplied RequiredApprovals: 1 - the exact pre-fix bypass shape -
// proving the resolved-server-side value can only ever be RAISED above the
// policy (GREATEST(caller, policy)), never lowered by a caller. It then
// proves the ordinary, correctly-thresholded path (three real distinct
// approvals, RequiredApprovals left at its zero value exactly as
// bonus_handlers.go now always calls it) still succeeds - no regression to
// the legitimate flow.
func TestHeldDispositionResolve_CallerCannotLowerRequiredApprovalsBelowPolicy(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	staff3 := seedExtraStaff(t, pool, f.tenantID)
	staff4 := seedExtraStaff(t, pool, f.tenantID)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	// Override seedLifecycleFixture's own default (required_approvals=1,
	// added so the OTHER held-disposition tests in this package - which
	// only ever seed two staff members - keep working) with a
	// higher-than-default threshold: a NEW, later-effective_from row (the
	// table is insert-only, migration 0063) resolves ahead of the
	// fixture's own row at identical specificity (tenant-wide, no
	// brand/asset), per ResolveApprovalPolicy's own "... effective_from
	// DESC" ordering.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO bonus_approval_policies (tenant_id, operation, approval_threshold_minor_units, required_approvals, created_by_principal_id)
			VALUES ($1, 'held_disposition_resolve', 0, 3, $2)`, f.tenantID, f.staffID)
		return err
	})
	if err != nil {
		t.Fatalf("seed 3-approval policy override: %v", err)
	}

	var grantID, dispositionID uuid.UUID
	settlementTxID := uuid.New()
	correlationID := uuid.New()
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "sec-4i-f1-required-approvals")
		result := forceIssueAndActivateGrantForTest(t, ctx, tx, g, ActivateGrantParams{Amount: big.NewInt(1000), ActorType: ActorSystem})
		grantID = result.ID

		terminated, err := TerminateGrant(ctx, tx, f.tenantID, grantID, TerminateGrantParams{
			Resolution: TerminalResolutionForfeited, ReasonCode: "wagering_rule_breach", ActorType: ActorSystem, TriggerType: TriggerAutomatedRuleEvaluation,
		})
		if err != nil {
			return err
		}
		if terminated.Status != GrantForfeited {
			return fmt.Errorf("expected forfeited, got %s", terminated.Status)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id) VALUES ($1, $2, 'casino_win', $3, $4)`,
			settlementTxID, f.tenantID, "win-"+settlementTxID.String(), correlationID,
		); err != nil {
			return err
		}

		dispID, err := ResolveTerminalGrantCredit(ctx, tx, grantID, correlationID, CreditKindWin, big.NewInt(300), big.NewInt(0), settlementTxID)
		if err != nil {
			return fmt.Errorf("resolve terminal grant credit: %w", err)
		}
		dispositionID = dispID
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// File the request, record exactly ONE approval - insufficient against
	// the 3-approval policy just configured.
	var requestID uuid.UUID
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		payload := []byte(fmt.Sprintf(`{"action":%q}`, ActionReforfeit))
		req, err := FileChangeRequest(ctx, tx, ChangeRequest{
			TenantID: f.tenantID, Operation: ChangeOpHeldDispositionResolve, TargetType: "bonus_held_dispositions", TargetID: dispositionID,
			Payload: payload, ReasonCode: "sec-4i-f1-test", RequestedByPrincipalID: f.staffID,
		})
		if err != nil {
			return err
		}
		requestID = req.ID
		return RecordChangeApproval(ctx, tx, f.tenantID, req.ID, f.staff2ID, "approve", nil, nil, nil)
	})
	if err != nil {
		t.Fatalf("file+single-approve: %v", err)
	}

	// THE BYPASS ATTEMPT: a caller supplies RequiredApprovals: 1 (the
	// pre-fix HTTP request body's exact shape) against a real policy of 3,
	// with only ONE real approval recorded. Pre-fix, this SUCCEEDED
	// (p.RequiredApprovals flowed unclamped into
	// bonus_change_consume_approved_request). Post-fix, the
	// server-side-resolved policy value (3) can only be RAISED by a
	// caller-supplied value, never lowered, so this must be refused.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ResolveHeldDispositionAction(ctx, tx, f.tenantID, ResolveHeldDispositionActionParams{
			HeldDispositionID: dispositionID, Action: ActionReforfeit, ActorID: f.staffID, ReasonCode: "sec-4i-f1-test",
			RequestID: requestID, RequiredApprovals: 1,
		})
		return err
	})
	if !errors.Is(err, ErrChangeRequestNotApproved) {
		t.Fatalf("SEC-4I-F1 REGRESSION: expected a caller-supplied RequiredApprovals:1 to be refused against a 3-approval policy with only 1 real approval (ErrChangeRequestNotApproved), got %v", err)
	}

	// The disposition must remain 'held' - a refused four-eyes consume must
	// have no side effect.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		d, err := GetHeldDispositionByID(ctx, tx, dispositionID)
		if err != nil {
			return err
		}
		if d.Status != HeldDispositionHeld {
			return fmt.Errorf("expected disposition to remain held after a refused resolution attempt, got %s", d.Status)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Record the TWO remaining distinct approvals the real policy requires.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := RecordChangeApproval(ctx, tx, f.tenantID, requestID, staff3, "approve", nil, nil, nil); err != nil {
			return err
		}
		return RecordChangeApproval(ctx, tx, f.tenantID, requestID, staff4, "approve", nil, nil, nil)
	})
	if err != nil {
		t.Fatalf("record remaining approvals: %v", err)
	}

	// Now the ORDINARY, correctly-thresholded path: no regression to the
	// legitimate flow (three real distinct approvals against a 3-approval
	// policy, resolved entirely server-side - RequiredApprovals left at its
	// zero value here, exactly as bonus_handlers.go now always calls it).
	var resolved HeldDisposition
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var resErr error
		resolved, resErr = ResolveHeldDispositionAction(ctx, tx, f.tenantID, ResolveHeldDispositionActionParams{
			HeldDispositionID: dispositionID, Action: ActionReforfeit, ActorID: f.staffID, ReasonCode: "sec-4i-f1-test",
			RequestID: requestID,
		})
		return resErr
	})
	if err != nil {
		t.Fatalf("expected the legitimate 3-approval path to succeed, got %v", err)
	}
	if resolved.Status != HeldDispositionResolvedReforfeit {
		t.Fatalf("expected resolved_reforfeit, got %s", resolved.Status)
	}
}
