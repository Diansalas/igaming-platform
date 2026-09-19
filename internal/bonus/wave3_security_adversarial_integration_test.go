//go:build integration

// Stage 4H-B1 Wave 3 Phase 6 (`security`) — the mandated re-test of the
// SEC-W15-02 / CRM-decomposition class of vectors against EVERY Bonus
// surface Phase 3 added, plus the two fixes this phase makes.
//
// The vector class, restated from its source (task-registry.md's own
// SEC-W15-02 entry and docs/architecture/34-economic-operation-identity.md
// §1.1's generalization of it) so every test below is anchored to a
// named shape rather than a vague "abuse" notion:
//
//	Decomposition        one authorized operation executed as N
//	                     individually-sub-threshold operations
//	Pagination laundering a batch exceeding an approved ceiling sent as
//	                     pages that individually do not
//	Payload substitution an approval obtained for one economic payload
//	                     then applied to a different one (different
//	                     amount, different recipient, different version)
//	Actor/subject
//	laundering           routing an operation through an actor/subject
//	                     pairing other than the one actually authorized
//	                     (self-approval, self-dealing, SEP-1)
//
// The four-eyes wrappers Phase 3 built (four_eyes_ops.go) already carry
// their own "refused without any approval" tests
// (four_eyes_ops_integration_test.go). This file deliberately does NOT
// duplicate those. It tests the harder cases: an approval that EXISTS but
// was obtained for a different payload, a different subject, or from the
// wrong person.
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

// seedStaffSharingPerson inserts a SECOND staff_users row bound to an
// EXISTING staff member's own person_id — the concrete "actor laundering"
// primitive: one human holding two principals, attempting to be both the
// requester and the approver of the same dual-controlled operation.
// staff_users.person_id carries no unique constraint (migration 0029), so
// this is a real, reachable configuration, not a contrived one.
func seedStaffSharingPerson(t *testing.T, pool *db.Pool, tenantID, existingStaffID uuid.UUID) uuid.UUID {
	t.Helper()
	newStaffID := uuid.New()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var personID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT person_id FROM staff_users WHERE id = $1`, existingStaffID).Scan(&personID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status)
			VALUES ($1, $2, $3, 'x', 'bonus_operations', $4, 'active')`,
			newStaffID, tenantID, newStaffID.String()+"@staff.example.com", personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed staff sharing person: %v", err)
	}
	return newStaffID
}

func fileChangeRequestForTest(t *testing.T, pool *db.Pool, tenantID uuid.UUID, operation ChangeOperation, targetType string, targetID uuid.UUID, payload []byte, filerID uuid.UUID) uuid.UUID {
	t.Helper()
	var requestID uuid.UUID
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		req, err := FileChangeRequest(ctx, tx, ChangeRequest{
			TenantID: tenantID, Operation: operation, TargetType: targetType, TargetID: targetID,
			Payload: payload, ReasonCode: "wave3-security-adversarial", RequestedByPrincipalID: filerID,
		})
		requestID = req.ID
		return err
	})
	if err != nil {
		t.Fatalf("file %s change request: %v", operation, err)
	}
	return requestID
}

// approveForTest records one approval and returns the error verbatim, so
// a test can assert on a DB-trigger refusal rather than t.Fatal on it.
func approveForTest(pool *db.Pool, tenantID, requestID, approverID uuid.UUID) error {
	return pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return RecordChangeApproval(ctx, tx, tenantID, requestID, approverID, "approve", nil, nil, nil)
	})
}

// --- Vector 1: actor laundering / self-approval, against ALL FOUR
// newly-wired ChangeOperations (not just the previously-reviewed
// held_disposition_resolve). Two shapes per operation: the SAME principal
// approving its own request, and a DIFFERENT principal resolving to the
// SAME PERSON approving it. Both must be refused by migration 0063's
// governance trigger, at the DB, on the approval INSERT itself. ---

func TestSEC_SelfApprovalRefused_AllFourNewlyWiredOperations(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
	twinStaff := seedStaffSharingPerson(t, pool, f.tenantID, f.staffID)
	parentOpID := mintApprovedManualGrantEOI(t, pool, f)

	// A real bonus_grants row and a real bulk_grant_jobs row must exist
	// before their respective requests can be approved at all (migration
	// 0063's SEP-1 trigger resolves the beneficiary by joining them).
	var grantID, jobID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "sec-self-approval")
		g.CreatedByActorType = ActorStaff
		grant, _, err := IssueManualGrantRequest(ctx, tx, g, parentOpID, f.jurisdictionCode, f.staffID)
		if err != nil {
			return err
		}
		grantID = grant.ID
		asset := f.assetCode
		ceiling := int32(5)
		op, err := MintRootOperation(ctx, tx, MintRootOperationParams{
			TenantID: f.tenantID, OperationType: economicop.OperationBonusBulkGrant,
			InitiatingActorType: "staff", InitiatingActorID: f.staffID,
			SubjectScope: economicop.SubjectScopeEnumeratedSet, AssetCode: &asset, RecipientCeiling: &ceiling,
			IntendedAggregateValue: big.NewInt(1_000_000), IdempotencyKey: "sec-self-approval-" + uuid.NewString(),
			CorrelationID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour), ApprovalState: economicop.ApprovalApproved,
		})
		if err != nil {
			return err
		}
		job, err := CreateBulkGrantJob(ctx, tx, BulkGrantJob{
			TenantID: f.tenantID, BrandID: f.brandID, CampaignID: co.campaignID, OfferVersionID: co.offerVersionID,
			TargetKind: TargetPlayerList, TargetPlayerList: []uuid.UUID{f.playerID},
			RequestedByPrincipalID: f.staffID, ApprovalState: BulkApprovalApproved,
			IdempotencyKey: "sec-self-approval-job-" + uuid.NewString(), ParentOperationID: &op.OperationID,
		})
		jobID = job.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed grant/bulk job: %v", err)
	}

	cases := []struct {
		operation  ChangeOperation
		targetType string
		targetID   uuid.UUID
		payload    []byte
	}{
		{ChangeOpCampaignActivate, "bonus_campaigns", co.campaignID, campaignActivatePayloadMatch},
		{ChangeOpOfferPublish, "bonus_offers", co.offerID, []byte(fmt.Sprintf(`{"offer_version_id":%q}`, co.offerVersionID))},
		{ChangeOpManualGrantIssue, "bonus_grants", grantID, manualGrantIssuePayloadMatch(f.playerID, co.offerVersionID, f.assetCode, big.NewInt(1000))},
		{ChangeOpBulkJobExecute, "bulk_grant_jobs", jobID, []byte(`{"action":"execute"}`)},
	}

	for _, c := range cases {
		t.Run(string(c.operation), func(t *testing.T) {
			requestID := fileChangeRequestForTest(t, pool, f.tenantID, c.operation, c.targetType, c.targetID, c.payload, f.staffID)

			// (a) the filer approving their own request
			if err := approveForTest(pool, f.tenantID, requestID, f.staffID); err == nil {
				t.Fatalf("%s: self-approval by the filing principal was ACCEPTED - four-eyes bypassed", c.operation)
			} else if !strings.Contains(err.Error(), "self-approval") {
				t.Fatalf("%s: expected a self-approval refusal, got %v", c.operation, err)
			}

			// (b) a DIFFERENT principal resolving to the SAME person
			if err := approveForTest(pool, f.tenantID, requestID, twinStaff); err == nil {
				t.Fatalf("%s: approval by a second principal of the SAME PERSON was ACCEPTED - four-eyes bypassed via actor laundering", c.operation)
			} else if !strings.Contains(err.Error(), "self-approval") {
				t.Fatalf("%s: expected a person-level self-approval refusal, got %v", c.operation, err)
			}

			// (c) with no valid approval on the request, the consume
			// must refuse.
			consumeErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := ConsumeApprovedChangeRequest(ctx, tx, f.tenantID, c.operation, c.targetID, c.payload, 2, f.staffID)
				return err
			})
			if !errors.Is(consumeErr, ErrChangeRequestNotApproved) {
				t.Fatalf("%s: expected the consume to refuse an un-approved request, got %v", c.operation, consumeErr)
			}
		})
	}
}

// --- Vector 2: payload substitution on bulk_job_execute.
//
// FINDING (Stage 4H-B1 Wave 3 Phase 6, `security`): before this phase's
// fix, ExecuteBulkGrantJobWithApproval's payload match was the fixed
// literal `{"action":"execute"}` - it pinned NOTHING about the economic
// payload. The per-recipient `amount` is supplied by the EXECUTING caller
// (the HTTP request body), never by the approver, so an approval obtained
// for a modest bulk grant authorized an arbitrarily larger one. This is
// the payload-substitution shape of SEC-W15-02, live on the largest new
// surface this Wave adds.
//
// This test fails against the pre-fix code (the substituted execution
// succeeds) and passes against the fix.
func TestSEC_BulkJobExecute_AmountSubstitutionRefused(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	staff3 := seedExtraStaff(t, pool, f.tenantID)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	approvedAmount := big.NewInt(1000)
	substitutedAmount := big.NewInt(9_999_999)

	var jobID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		asset := f.assetCode
		ceiling := int32(10)
		op, err := MintRootOperation(ctx, tx, MintRootOperationParams{
			TenantID: f.tenantID, OperationType: economicop.OperationBonusBulkGrant,
			InitiatingActorType: "staff", InitiatingActorID: f.staffID,
			SubjectScope: economicop.SubjectScopeEnumeratedSet, AssetCode: &asset, RecipientCeiling: &ceiling,
			IntendedAggregateValue: big.NewInt(100_000_000), IdempotencyKey: "sec-bulk-substitution-" + uuid.NewString(),
			CorrelationID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour), ApprovalState: economicop.ApprovalApproved,
		})
		if err != nil {
			return err
		}
		job, err := CreateBulkGrantJob(ctx, tx, BulkGrantJob{
			TenantID: f.tenantID, BrandID: f.brandID, CampaignID: co.campaignID, OfferVersionID: co.offerVersionID,
			TargetKind: TargetPlayerList, TargetPlayerList: []uuid.UUID{f.playerID},
			RequestedByPrincipalID: f.staffID, ApprovalState: BulkApprovalApproved,
			IdempotencyKey: "sec-bulk-substitution-job-" + uuid.NewString(), ParentOperationID: &op.OperationID,
		})
		jobID = job.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed bulk job: %v", err)
	}

	target := StaticPlayerListTarget{PlayerAccountIDs: []uuid.UUID{f.playerID}}
	approvedPayload := BulkJobExecutePayloadMatch(co.offerVersionID, f.assetCode, approvedAmount, target.PlayerAccountIDs)
	fileAndDoublyApprove(t, pool, f.tenantID, ChangeOpBulkJobExecute, "bulk_grant_jobs", jobID, approvedPayload, f.staffID, f.staff2ID, staff3)

	// Execute for a DIFFERENT per-recipient amount than the one approved.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		template := newTestOfferGrant(f, co, "")
		_, execErr := ExecuteBulkGrantJobWithApproval(ctx, tx, f.tenantID, jobID, f.staffID, target, template, f.jurisdictionCode, uuid.Nil, substitutedAmount)
		return execErr
	})
	if !errors.Is(err, ErrChangeRequestNotApproved) {
		t.Fatalf("expected an amount-substituted bulk execution to be REFUSED, got %v", err)
	}

	// No item row, no Grant, no ledger movement may exist.
	var itemCount, grantCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM bulk_grant_job_items WHERE tenant_id = $1 AND bulk_grant_job_id = $2`, f.tenantID, jobID).Scan(&itemCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM bonus_grants WHERE tenant_id = $1 AND campaign_id = $2`, f.tenantID, co.campaignID).Scan(&grantCount)
	}); err != nil {
		t.Fatal(err)
	}
	if itemCount != 0 || grantCount != 0 {
		t.Fatalf("expected no partial state after a refused substitution (items=%d grants=%d)", itemCount, grantCount)
	}

	// The SAME approval, applied to the amount it was actually granted
	// for, must still work - the fix must refuse substitution, never the
	// legitimate execution.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		template := newTestOfferGrant(f, co, "")
		_, execErr := ExecuteBulkGrantJobWithApproval(ctx, tx, f.tenantID, jobID, f.staffID, target, template, f.jurisdictionCode, uuid.Nil, approvedAmount)
		return execErr
	})
	if err != nil {
		t.Fatalf("the APPROVED amount must still execute: %v", err)
	}
}

// TestSEC_BulkJobExecute_RecipientSetSubstitutionRefused is the
// pagination-laundering half of the same fix: an approval obtained for
// one pinned recipient set must not authorize execution against a
// different (here, larger) one. bulk_grant_jobs carries no DB-level
// immutability trigger on target_player_list (migration 0060 - reported
// separately by this phase), so binding the recipient set into the
// approval payload is what actually closes this.
func TestSEC_BulkJobExecute_RecipientSetSubstitutionRefused(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	staff3 := seedExtraStaff(t, pool, f.tenantID)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
	extraPlayerID, _ := seedExtraBonusPlayer(t, pool, f)

	amount := big.NewInt(1000)
	approvedSet := []uuid.UUID{f.playerID}
	substitutedSet := []uuid.UUID{f.playerID, extraPlayerID}

	var jobID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		asset := f.assetCode
		ceiling := int32(10)
		op, err := MintRootOperation(ctx, tx, MintRootOperationParams{
			TenantID: f.tenantID, OperationType: economicop.OperationBonusBulkGrant,
			InitiatingActorType: "staff", InitiatingActorID: f.staffID,
			SubjectScope: economicop.SubjectScopeEnumeratedSet, AssetCode: &asset, RecipientCeiling: &ceiling,
			IntendedAggregateValue: big.NewInt(100_000_000), IdempotencyKey: "sec-bulk-set-" + uuid.NewString(),
			CorrelationID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour), ApprovalState: economicop.ApprovalApproved,
		})
		if err != nil {
			return err
		}
		job, err := CreateBulkGrantJob(ctx, tx, BulkGrantJob{
			TenantID: f.tenantID, BrandID: f.brandID, CampaignID: co.campaignID, OfferVersionID: co.offerVersionID,
			TargetKind: TargetPlayerList, TargetPlayerList: approvedSet,
			RequestedByPrincipalID: f.staffID, ApprovalState: BulkApprovalApproved,
			IdempotencyKey: "sec-bulk-set-job-" + uuid.NewString(), ParentOperationID: &op.OperationID,
		})
		jobID = job.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed bulk job: %v", err)
	}

	approvedPayload := BulkJobExecutePayloadMatch(co.offerVersionID, f.assetCode, amount, approvedSet)
	fileAndDoublyApprove(t, pool, f.tenantID, ChangeOpBulkJobExecute, "bulk_grant_jobs", jobID, approvedPayload, f.staffID, f.staff2ID, staff3)

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		template := newTestOfferGrant(f, co, "")
		_, execErr := ExecuteBulkGrantJobWithApproval(ctx, tx, f.tenantID, jobID, f.staffID,
			StaticPlayerListTarget{PlayerAccountIDs: substitutedSet}, template, f.jurisdictionCode, uuid.Nil, amount)
		return execErr
	})
	if !errors.Is(err, ErrChangeRequestNotApproved) {
		t.Fatalf("expected a recipient-set-substituted bulk execution to be REFUSED, got %v", err)
	}
}

// --- Vector 3: payload substitution on offer_publish. An approval to
// publish OfferVersion 1 must not publish OfferVersion 2. ---

func TestSEC_OfferPublish_VersionSubstitutionRefused(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	staff3 := seedExtraStaff(t, pool, f.tenantID)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	var secondVersionID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ov, err := CreateOfferVersion(ctx, tx, OfferVersion{
			TenantID: f.tenantID, OfferID: co.offerID, VersionNumber: 2, RewardKind: RewardFixedValue, RewardAssetCode: "USD",
			FulfillmentDestination: FulfillmentIntoPlatformWallet, FundingSource: "operator",
			CreatedByActorType: ActorStaff, CreatedByActorID: f.staffID,
		})
		secondVersionID = ov.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed second offer version: %v", err)
	}

	// Approval is obtained for version 1 only.
	approvedPayload := []byte(fmt.Sprintf(`{"offer_version_id":%q}`, co.offerVersionID))
	fileAndDoublyApprove(t, pool, f.tenantID, ChangeOpOfferPublish, "bonus_offers", co.offerID, approvedPayload, f.staffID, f.staff2ID, staff3)

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := PublishOfferVersion(ctx, tx, f.tenantID, co.offerID, secondVersionID, f.staffID)
		return err
	})
	if !errors.Is(err, ErrChangeRequestNotApproved) {
		t.Fatalf("expected publishing a DIFFERENT version than the approved one to be refused, got %v", err)
	}

	var status string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		o, err := GetOfferByID(ctx, tx, co.offerID)
		status = string(o.Status)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if status != string(OfferDraft) {
		t.Fatalf("expected the offer to remain draft after a refused publish, got %s", status)
	}
}

// --- Vector 4: campaign_activate's approval must bind to the campaign it
// names. An approval filed against campaign A must not activate campaign
// B. ---

func TestSEC_CampaignActivate_TargetSubstitutionRefused(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	staff3 := seedExtraStaff(t, pool, f.tenantID)
	coA := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
	coB := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	fileAndDoublyApprove(t, pool, f.tenantID, ChangeOpCampaignActivate, "bonus_campaigns", coA.campaignID, campaignActivatePayloadMatch, f.staffID, f.staff2ID, staff3)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ActivateCampaign(ctx, tx, f.tenantID, coB.campaignID, f.staffID)
		return err
	})
	if !errors.Is(err, ErrChangeRequestNotApproved) {
		t.Fatalf("expected campaign B's activation under campaign A's approval to be refused, got %v", err)
	}
}

// --- Vector 5: subject laundering through an EconomicOperationIdentity.
//
// FINDING (Stage 4H-B1 Wave 3 Phase 6, `security`): doc 34 §3.2 names
// subject-set containment as one of the five containment checks, and §2.2
// defines subject_ref as "For single_subject: the beneficiary's
// player_account_id". Before this phase's fix NOTHING enforced it:
// CheckEntry takes no subject argument at all, and ConsumeRootBudget only
// counted DISTINCT recipients against recipient_ceiling. A single_subject
// EOI root minted and approved to grant to player A therefore authorized
// a grant to player B just as well - laundering the operation through a
// different subject pairing than the one authorized.
//
// Fails against the pre-fix code (the substituted grant activates),
// passes against the fix.
func TestSEC_ManualGrantEOI_SubjectSubstitutionRefused(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	staff3 := seedExtraStaff(t, pool, f.tenantID)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
	otherPlayerID, otherWalletID := seedExtraBonusPlayer(t, pool, f)
	amount := big.NewInt(1000)

	// The EOI root authorizes a single subject: f.playerID.
	parentOpID := mintApprovedManualGrantEOI(t, pool, f)

	// ...but the Grant is aimed at a DIFFERENT player.
	var grantID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "sec-subject-substitution")
		g.PlayerAccountID = otherPlayerID
		g.WalletID = otherWalletID
		g.CreatedByActorType = ActorStaff
		grant, _, err := IssueManualGrantRequest(ctx, tx, g, parentOpID, f.jurisdictionCode, f.staffID)
		grantID = grant.ID
		return err
	})
	if err != nil {
		t.Fatalf("IssueManualGrantRequest: %v", err)
	}

	// A genuine, correctly-separated four-eyes approval for THIS grant -
	// the four-eyes control is deliberately satisfied here, so the only
	// thing that can refuse the activation is the EOI's own subject
	// containment.
	payloadMatch := manualGrantIssuePayloadMatch(otherPlayerID, co.offerVersionID, f.assetCode, amount)
	fileAndDoublyApprove(t, pool, f.tenantID, ChangeOpManualGrantIssue, "bonus_grants", grantID, payloadMatch, f.staffID, f.staff2ID, staff3)

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := ActivateManualGrantWithApproval(ctx, tx, f.tenantID, grantID, parentOpID, f.jurisdictionCode, f.staffID, amount)
		return err
	})
	if !errors.Is(err, economicop.ErrChildScopeExceedsParent) {
		t.Fatalf("expected a single_subject EOI root to REFUSE a grant to a different player, got %v", err)
	}

	if status := readGrantStatus(t, pool, f.tenantID, grantID); status != GrantIssued {
		t.Fatalf("expected the grant to remain issued after a refused subject substitution, got %s", status)
	}
}

// TestSEC_ManualGrantEOI_AuthorizedSubjectStillAllowed is the paired
// non-regression assertion for the fix above: the subject the root
// actually names must still be grantable.
func TestSEC_ManualGrantEOI_AuthorizedSubjectStillAllowed(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	staff3 := seedExtraStaff(t, pool, f.tenantID)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
	parentOpID := mintApprovedManualGrantEOI(t, pool, f)
	amount := big.NewInt(1000)

	var grantID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "sec-subject-authorized")
		g.CreatedByActorType = ActorStaff
		grant, _, err := IssueManualGrantRequest(ctx, tx, g, parentOpID, f.jurisdictionCode, f.staffID)
		grantID = grant.ID
		return err
	})
	if err != nil {
		t.Fatalf("IssueManualGrantRequest: %v", err)
	}
	payloadMatch := manualGrantIssuePayloadMatch(f.playerID, co.offerVersionID, f.assetCode, amount)
	fileAndDoublyApprove(t, pool, f.tenantID, ChangeOpManualGrantIssue, "bonus_grants", grantID, payloadMatch, f.staffID, f.staff2ID, staff3)

	var outcome GateOutcome
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var actErr error
		_, outcome, actErr = ActivateManualGrantWithApproval(ctx, tx, f.tenantID, grantID, parentOpID, f.jurisdictionCode, f.staffID, amount)
		return actErr
	})
	if err != nil || !outcome.Allowed {
		t.Fatalf("the EOI's own authorized subject must still be grantable: err=%v outcome=%+v", err, outcome)
	}
}

// --- Vector 6: cross-tenant. A valid change request in tenant A must not
// be consumable from tenant B's connection, and a tenant-B EOI root must
// not authorize a tenant-A grant. ---

func TestSEC_ChangeRequestNotConsumableFromAnotherTenant(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	other := seedLifecycleFixture(t, pool)
	staff3 := seedExtraStaff(t, pool, f.tenantID)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	fileAndDoublyApprove(t, pool, f.tenantID, ChangeOpCampaignActivate, "bonus_campaigns", co.campaignID, campaignActivatePayloadMatch, f.staffID, f.staff2ID, staff3)

	// Tenant B's connection, naming tenant A's own campaign and tenant A's
	// own tenant_id in the call: RLS must make the request invisible.
	err := pool.WithTenant(context.Background(), other.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ConsumeApprovedChangeRequest(ctx, tx, f.tenantID, ChangeOpCampaignActivate, co.campaignID, campaignActivatePayloadMatch, 2, other.staffID)
		return err
	})
	if !errors.Is(err, ErrChangeRequestNotApproved) {
		t.Fatalf("expected tenant B to be unable to consume tenant A's approved request, got %v", err)
	}

	// And the request must still be pending (never consumed).
	var state string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM bonus_change_requests WHERE tenant_id = $1 AND target_id = $2`, f.tenantID, co.campaignID).Scan(&state)
	}); err != nil {
		t.Fatal(err)
	}
	if state != string(ChangeRequestPending) {
		t.Fatalf("expected tenant A's request to remain pending, got %s", state)
	}
}

func TestSEC_ForeignTenantEOIRootRefused(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	other := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	// A perfectly valid, approved, open EOI root - in the WRONG tenant.
	var foreignOpID uuid.UUID
	err := pool.WithTenant(context.Background(), other.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		asset := other.assetCode
		ceiling := int32(5)
		op, err := MintRootOperation(ctx, tx, MintRootOperationParams{
			TenantID: other.tenantID, OperationType: economicop.OperationBonusManualGrant,
			InitiatingActorType: "staff", InitiatingActorID: other.staffID,
			SubjectScope: economicop.SubjectScopeSingle, SubjectRef: &other.playerID, AssetCode: &asset,
			IntendedAggregateValue: big.NewInt(1_000_000), RecipientCeiling: &ceiling,
			IdempotencyKey: "sec-foreign-eoi-" + uuid.NewString(),
			CorrelationID:  uuid.New(), ExpiresAt: time.Now().Add(time.Hour), ApprovalState: economicop.ApprovalApproved,
		})
		foreignOpID = op.OperationID
		return err
	})
	if err != nil {
		t.Fatalf("mint foreign EOI: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "sec-foreign-eoi")
		g.CreatedByActorType = ActorStaff
		_, _, err := IssueManualGrantRequest(ctx, tx, g, foreignOpID, f.jurisdictionCode, f.staffID)
		return err
	})
	if !errors.Is(err, economicop.ErrParentOperationNotFound) {
		t.Fatalf("expected a foreign-tenant EOI root to be unresolvable, got %v", err)
	}
}
