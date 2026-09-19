//go:build integration

// Stage 4H-B1 Wave 3 Phase 6 (`security`), continued: the cells of the
// mandated SEC-W15-02 / CRM-decomposition matrix (4 vector shapes x 5
// Bonus surfaces) that wave3_security_adversarial_integration_test.go and
// the pre-existing Wave 2 suite did NOT already cover.
//
// Already covered elsewhere, deliberately not duplicated here:
//   - decomposition, recipient axis, manual-grant + bulk surfaces:
//     lifecycle_integration_test.go's TestEOI_SingleManualGrant_
//     RecipientCeiling_RejectsSecondDistinctPlayer /
//     TestEOI_BulkGrantRecipientCeiling_RejectsBeyondBudget and their two
//     genuinely-concurrent siblings.
//   - payload substitution, all four four-eyes surfaces:
//     four_eyes_ops_integration_test.go's TestManualGrantWithApproval_
//     PayloadMismatchRefused plus wave3_security_adversarial's bulk
//     amount/recipient-set, offer-version and campaign-target cases.
//   - actor laundering (self-approval by the filer, and by a second
//     principal resolving to the SAME Person): wave3_security_adversarial's
//     TestSEC_SelfApprovalRefused_AllFourNewlyWiredOperations.
//   - EOI-mint decomposition/pagination bounds: httpserver's
//     bonus_eoi_mint_bounds_integration_test.go.
//
// This file adds the remaining cells:
//   - decomposition, VALUE axis (intended_aggregate_value), which no test
//     in this repository exercised at all - the recipient ceiling was
//     tested, the value budget never was, even though the EOI-mint fix
//     made intended_aggregate_value mandatory precisely so it would be
//     enforceable.
//   - pagination laundering / replay on campaign_activate, offer_publish
//     and bulk_job_execute: one approval must authorize exactly ONE
//     execution, never N.
//   - actor/subject laundering at the EOI-mint surface: the mint endpoint
//     applies no SEP-1 check of its own (it cannot - no Grant exists yet),
//     so this proves the COMPOSED control still refuses a staff member who
//     mints an authorization naming their own Person's player account.
package bonus

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/economicop"
)

// mintBoundedEnumeratedEOI mints an approved, enumerated_set manual-grant
// root with an EXPLICIT recipient ceiling and value budget - the shape
// newMintEconomicOperationHandler now forces every HTTP-minted root into.
func mintBoundedEnumeratedEOI(t *testing.T, pool *db.Pool, f lifecycleFixture, ceiling int32, aggregate *big.Int) uuid.UUID {
	t.Helper()
	var opID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		asset := f.assetCode
		c := ceiling
		op, err := MintRootOperation(ctx, tx, MintRootOperationParams{
			TenantID: f.tenantID, OperationType: economicop.OperationBonusManualGrant,
			InitiatingActorType: "staff", InitiatingActorID: f.staffID,
			SubjectScope: economicop.SubjectScopeEnumeratedSet, AssetCode: &asset,
			RecipientCeiling: &c, IntendedAggregateValue: aggregate,
			IdempotencyKey: "sec-matrix-eoi-" + uuid.NewString(),
			CorrelationID:  uuid.New(), ExpiresAt: time.Now().Add(time.Hour), ApprovalState: economicop.ApprovalApproved,
		})
		opID = op.OperationID
		return err
	})
	if err != nil {
		t.Fatalf("mint bounded EOI: %v", err)
	}
	return opID
}

// issueApproveActivateManualGrant runs the full two-phase manual-grant
// sequence (issue -> file+doubly-approve -> activate) for one player and
// returns activation's own error verbatim, so a test can assert on a
// budget refusal instead of failing on it.
func issueApproveActivateManualGrant(t *testing.T, pool *db.Pool, f lifecycleFixture, co campaignOffer, parentOpID, playerID, walletID uuid.UUID, approver2 uuid.UUID, amount *big.Int, triggerRef string) (uuid.UUID, error) {
	t.Helper()
	var grantID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, triggerRef)
		g.PlayerAccountID = playerID
		g.WalletID = walletID
		g.CreatedByActorType = ActorStaff
		grant, _, err := IssueManualGrantRequest(ctx, tx, g, parentOpID, f.jurisdictionCode, f.staffID)
		grantID = grant.ID
		return err
	}); err != nil {
		t.Fatalf("IssueManualGrantRequest(%s): %v", triggerRef, err)
	}

	payloadMatch := manualGrantIssuePayloadMatch(playerID, co.offerVersionID, f.assetCode, amount)
	fileAndDoublyApprove(t, pool, f.tenantID, ChangeOpManualGrantIssue, "bonus_grants", grantID, payloadMatch, f.staffID, f.staff2ID, approver2)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, actErr := ActivateManualGrantWithApproval(ctx, tx, f.tenantID, grantID, parentOpID, f.jurisdictionCode, f.staffID, amount)
		return actErr
	})
	return grantID, err
}

// --- Cell: DECOMPOSITION x manual_grant_issue, VALUE axis ---

// TestSEC_ManualGrantEOI_ValueBudgetDecompositionRefused is the canonical
// SEC-W15-02 shape ("one authorized operation executed as N individually
// sub-threshold operations") on the axis nothing in this repository
// previously tested: intended_aggregate_value.
//
// An EOI root authorized for 1500 total must not fund two 1000-unit
// grants, even though each one individually sits far below the root's own
// declared budget and each one carries its own genuine, correctly-
// separated four-eyes approval. The recipient ceiling is deliberately set
// high enough (10) that it cannot be what refuses the second grant - only
// the value budget can be.
func TestSEC_ManualGrantEOI_ValueBudgetDecompositionRefused(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	staff3 := seedExtraStaff(t, pool, f.tenantID)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
	otherPlayerID, otherWalletID := seedExtraBonusPlayer(t, pool, f)

	parentOpID := mintBoundedEnumeratedEOI(t, pool, f, 10, big.NewInt(1500))
	amount := big.NewInt(1000)

	if _, err := issueApproveActivateManualGrant(t, pool, f, co, parentOpID, f.playerID, f.walletID, staff3, amount, "sec-value-budget-1"); err != nil {
		t.Fatalf("the FIRST grant, comfortably inside the root's 1500 budget, must succeed: %v", err)
	}

	secondGrantID, err := issueApproveActivateManualGrant(t, pool, f, co, parentOpID, otherPlayerID, otherWalletID, staff3, amount, "sec-value-budget-2")
	if !errors.Is(err, economicop.ErrBudgetExhausted) {
		t.Fatalf("expected the SECOND 1000-unit grant to exhaust the root's 1500 value budget (500 remaining) and be refused with ErrBudgetExhausted, got %v", err)
	}
	if status := readGrantStatus(t, pool, f.tenantID, secondGrantID); status != GrantIssued {
		t.Fatalf("expected the over-budget grant to remain issued (never activated, never posted), got %s", status)
	}

	// The ledger must carry exactly the one authorized grant's value.
	var activatedCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM bonus_grants WHERE tenant_id = $1 AND parent_operation_id = $2 AND status NOT IN ('issued','cancelled')`,
			f.tenantID, parentOpID,
		).Scan(&activatedCount)
	}); err != nil {
		t.Fatal(err)
	}
	if activatedCount != 1 {
		t.Fatalf("expected exactly 1 activated grant under the 1500-unit root, got %d", activatedCount)
	}
}

// --- Cell: PAGINATION LAUNDERING / REPLAY x campaign_activate,
// offer_publish, bulk_job_execute ---

// TestSEC_ApprovalAuthorizesExactlyOneExecution proves one approved
// bonus_change_requests row authorizes exactly ONE execution on each of
// the three non-grant four-eyes surfaces. This is the replay/pagination
// shape of SEC-W15-02: an operator who holds a single approval must not
// be able to apply it N times (the manual-grant surface's own equivalent
// is already proven by TestManualGrantWithApproval_SucceedsWithApproval's
// second-activation assertion).
func TestSEC_ApprovalAuthorizesExactlyOneExecution(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	staff3 := seedExtraStaff(t, pool, f.tenantID)

	t.Run("campaign_activate", func(t *testing.T) {
		co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
		fileAndDoublyApprove(t, pool, f.tenantID, ChangeOpCampaignActivate, "bonus_campaigns", co.campaignID, campaignActivatePayloadMatch, f.staffID, f.staff2ID, staff3)

		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ActivateCampaign(ctx, tx, f.tenantID, co.campaignID, f.staffID)
			return err
		}); err != nil {
			t.Fatalf("the first, approved activation must succeed: %v", err)
		}
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ActivateCampaign(ctx, tx, f.tenantID, co.campaignID, f.staffID)
			return err
		})
		if !errors.Is(err, ErrChangeRequestNotApproved) {
			t.Fatalf("expected a SECOND activation under the same, already-consumed approval to be refused, got %v", err)
		}
	})

	t.Run("offer_publish", func(t *testing.T) {
		co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
		payload := []byte(`{"offer_version_id":"` + co.offerVersionID.String() + `"}`)
		fileAndDoublyApprove(t, pool, f.tenantID, ChangeOpOfferPublish, "bonus_offers", co.offerID, payload, f.staffID, f.staff2ID, staff3)

		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := PublishOfferVersion(ctx, tx, f.tenantID, co.offerID, co.offerVersionID, f.staffID)
			return err
		}); err != nil {
			t.Fatalf("the first, approved publish must succeed: %v", err)
		}
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := PublishOfferVersion(ctx, tx, f.tenantID, co.offerID, co.offerVersionID, f.staffID)
			return err
		})
		if !errors.Is(err, ErrChangeRequestNotApproved) {
			t.Fatalf("expected a SECOND publish under the same, already-consumed approval to be refused, got %v", err)
		}
	})

	t.Run("bulk_job_execute", func(t *testing.T) {
		co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
		amount := big.NewInt(1000)
		target := StaticPlayerListTarget{PlayerAccountIDs: []uuid.UUID{f.playerID}}

		var jobID uuid.UUID
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			asset := f.assetCode
			ceiling := int32(10)
			op, err := MintRootOperation(ctx, tx, MintRootOperationParams{
				TenantID: f.tenantID, OperationType: economicop.OperationBonusBulkGrant,
				InitiatingActorType: "staff", InitiatingActorID: f.staffID,
				SubjectScope: economicop.SubjectScopeEnumeratedSet, AssetCode: &asset, RecipientCeiling: &ceiling,
				IntendedAggregateValue: big.NewInt(1_000_000), IdempotencyKey: "sec-replay-bulk-" + uuid.NewString(),
				CorrelationID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour), ApprovalState: economicop.ApprovalApproved,
			})
			if err != nil {
				return err
			}
			job, err := CreateBulkGrantJob(ctx, tx, BulkGrantJob{
				TenantID: f.tenantID, BrandID: f.brandID, CampaignID: co.campaignID, OfferVersionID: co.offerVersionID,
				TargetKind: TargetPlayerList, TargetPlayerList: target.PlayerAccountIDs,
				RequestedByPrincipalID: f.staffID, ApprovalState: BulkApprovalApproved,
				IdempotencyKey: "sec-replay-bulk-job-" + uuid.NewString(), ParentOperationID: &op.OperationID,
			})
			jobID = job.ID
			return err
		}); err != nil {
			t.Fatalf("seed bulk job: %v", err)
		}

		payload := BulkJobExecutePayloadMatch(co.offerVersionID, f.assetCode, amount, target.PlayerAccountIDs)
		fileAndDoublyApprove(t, pool, f.tenantID, ChangeOpBulkJobExecute, "bulk_grant_jobs", jobID, payload, f.staffID, f.staff2ID, staff3)

		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			template := newTestOfferGrant(f, co, "")
			_, execErr := ExecuteBulkGrantJobWithApproval(ctx, tx, f.tenantID, jobID, f.staffID, target, template, f.jurisdictionCode, uuid.Nil, amount)
			return execErr
		}); err != nil {
			t.Fatalf("the first, approved execution must succeed: %v", err)
		}
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			template := newTestOfferGrant(f, co, "")
			_, execErr := ExecuteBulkGrantJobWithApproval(ctx, tx, f.tenantID, jobID, f.staffID, target, template, f.jurisdictionCode, uuid.Nil, amount)
			return execErr
		})
		if !errors.Is(err, ErrChangeRequestNotApproved) {
			t.Fatalf("expected a SECOND execution under the same, already-consumed approval to be refused, got %v", err)
		}
	})
}

// --- Cell: ACTOR/SUBJECT LAUNDERING x the EOI-minting surface ---

// TestSEC_EOIMintedForOwnPerson_RefusedByComposedSEP1 closes the one cell
// the EOI-minting endpoint cannot close by itself.
//
// POST /v1/admin/bonus/economic-operations mints an ALREADY-APPROVED root
// on a single actor's authority and applies no SEP-1 check of its own - it
// structurally cannot, because no Grant (and therefore no resolvable
// beneficiary) exists at mint time. Nothing stops a bonus_operations
// principal from minting a single_subject root naming a player account
// belonging to their OWN Person. This test proves the controls that
// actually close that vector are downstream and do hold, in series:
//
//  1. migration 0062's SEP-1 trigger on bonus_grant_progress refuses the
//     ISSUANCE outright - the acting staff principal resolves to the same
//     Person as the Grant's own player - so the self-dealt Grant never
//     even reaches 'issued', and
//  2. had it somehow been issued, migration 0063's SEP-1 trigger would
//     separately refuse to record any approval naming it (proven by
//     seeding the Grant row directly, below).
//
// The mint surface's silence about SEP-1 is therefore not a gap.
func TestSEC_EOIMintedForOwnPerson_RefusedByComposedSEP1(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	staff3 := seedExtraStaff(t, pool, f.tenantID)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	// A player account belonging to the SAME Person as the minting staff
	// member - staff_users.person_id carries no unique constraint, and
	// nothing forbids a Person from also holding a player account.
	selfPlayerID, selfWalletID := seedPlayerForStaffPerson(t, pool, f, f.staffID)

	var parentOpID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		asset := f.assetCode
		ceiling := int32(1)
		op, err := MintRootOperation(ctx, tx, MintRootOperationParams{
			TenantID: f.tenantID, OperationType: economicop.OperationBonusManualGrant,
			InitiatingActorType: "staff", InitiatingActorID: f.staffID,
			SubjectScope: economicop.SubjectScopeSingle, SubjectRef: &selfPlayerID, AssetCode: &asset,
			RecipientCeiling: &ceiling, IntendedAggregateValue: big.NewInt(1_000_000),
			IdempotencyKey: "sec-self-subject-" + uuid.NewString(),
			CorrelationID:  uuid.New(), ExpiresAt: time.Now().Add(time.Hour), ApprovalState: economicop.ApprovalApproved,
		})
		parentOpID = op.OperationID
		return err
	}); err != nil {
		t.Fatalf("mint self-subject EOI: %v", err)
	}

	amount := big.NewInt(1000)

	// (1) Issuance itself is refused by migration 0062's SEP-1 trigger.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "sec-self-subject")
		g.PlayerAccountID = selfPlayerID
		g.WalletID = selfWalletID
		g.CreatedByActorType = ActorStaff
		_, _, issueErr := IssueManualGrantRequest(ctx, tx, g, parentOpID, f.jurisdictionCode, f.staffID)
		return issueErr
	})
	if err == nil {
		t.Fatal("SEP-1 bypassed: a staff principal issued a manual grant to a player account belonging to their own Person")
	}
	if !strings.Contains(err.Error(), "SEP-1") {
		t.Fatalf("expected a SEP-1 refusal at issuance, got %v", err)
	}

	var leaked int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM bonus_grants WHERE tenant_id = $1 AND player_account_id = $2`, f.tenantID, selfPlayerID).Scan(&leaked)
	}); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("expected the refused self-dealt issuance to leave no bonus_grants row at all, found %d", leaked)
	}

	// (2) The SECOND, independent SEP-1 control: even if such a Grant row
	// somehow existed (seeded here directly, bypassing the layer-1 trigger
	// exactly as an attacker with raw DB access would have to), no
	// approval naming it can be recorded - not by the requester, and not
	// by a genuinely separate third principal either.
	grantID := seedGrantRowDirectly(t, pool, f, co, selfPlayerID, selfWalletID)
	payloadMatch := manualGrantIssuePayloadMatch(selfPlayerID, co.offerVersionID, f.assetCode, amount)
	requestID := fileChangeRequestForTest(t, pool, f.tenantID, ChangeOpManualGrantIssue, "bonus_grants", grantID, payloadMatch, f.staffID)

	approveErr := approveForTest(pool, f.tenantID, requestID, staff3)
	if approveErr == nil {
		t.Fatal("SEP-1 bypassed: an approval was recorded for a grant whose requester resolves to the beneficiary's own Person (self-dealing)")
	}
	if !strings.Contains(approveErr.Error(), "SEP-1") {
		t.Fatalf("expected a SEP-1 refusal at approval, got %v", approveErr)
	}

	// And with no recordable approval, the activation is refused outright.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, actErr := ActivateManualGrantWithApproval(ctx, tx, f.tenantID, grantID, parentOpID, f.jurisdictionCode, f.staffID, amount)
		return actErr
	})
	if !errors.Is(err, ErrChangeRequestNotApproved) {
		t.Fatalf("expected the self-dealt grant's activation to be refused, got %v", err)
	}
	if status := readGrantStatus(t, pool, f.tenantID, grantID); status != GrantIssued {
		t.Fatalf("expected the self-dealt grant to remain issued, got %s", status)
	}
}

// seedGrantRowDirectly inserts a bonus_grants row with a raw INSERT,
// deliberately bypassing IssueGrant's own Progress-trail write (and
// therefore migration 0062's SEP-1 trigger) - the ONLY way to reach the
// migration-0063 SEP-1 check in isolation for a beneficiary the layer-1
// trigger already refuses.
func seedGrantRowDirectly(t *testing.T, pool *db.Pool, f lifecycleFixture, co campaignOffer, playerID, walletID uuid.UUID) uuid.UUID {
	t.Helper()
	grantID := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO bonus_grants (
				id, tenant_id, brand_id, player_account_id, wallet_id, campaign_id, campaign_version_id,
				offer_id, offer_version_id, asset_code, decimal_exponent, funding_source,
				fulfillment_destination, fulfillment_owner, trigger_reference, status,
				created_by_actor_type, created_by_actor_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,2,'operator','into_platform_wallet','internal',$11,'issued','staff',$12)`,
			grantID, f.tenantID, f.brandID, playerID, walletID, co.campaignID, co.campaignVersionID,
			co.offerID, co.offerVersionID, f.assetCode, "sec-self-subject-seeded-"+grantID.String(), f.staffID)
		return err
	})
	if err != nil {
		t.Fatalf("seed grant row directly: %v", err)
	}
	return grantID
}

// seedPlayerForStaffPerson creates a player account bound to an EXISTING
// staff member's own person_id - the concrete "staff member who is also a
// player" configuration SEP-1 exists to refuse.
func seedPlayerForStaffPerson(t *testing.T, pool *db.Pool, f lifecycleFixture, staffID uuid.UUID) (playerID, walletID uuid.UUID) {
	t.Helper()
	playerID = uuid.New()
	walletID = uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var personID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT person_id FROM staff_users WHERE id = $1`, staffID).Scan(&personID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1,$2,$3,$4,$5,'x','active')`,
			playerID, f.tenantID, f.brandID, personID, playerID.String()+"@example.com"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1,$2,$3,$4,$5)`,
			walletID, f.tenantID, f.brandID, playerID, f.assetCode)
		return err
	})
	if err != nil {
		t.Fatalf("seed player for staff person: %v", err)
	}
	return playerID, walletID
}
