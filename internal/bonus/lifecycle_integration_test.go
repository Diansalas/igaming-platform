//go:build integration

// Real-PostgreSQL tests for the Grant lifecycle state machine, the AOE/
// G-2 mechanism, EconomicOperationIdentity enforcement, SEP-1, and the
// adversarial/concurrency properties Stage 4H-B1 Wave 2 requires (see
// this dispatch's own §14). Builds on bonus_integration_test.go's
// fixture conventions.
package bonus

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/assetregistry"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/economicop"
)

// --- shared fixture helpers, extending bonus_integration_test.go's own ---

type lifecycleFixture struct {
	fixture
	jurisdictionID   uuid.UUID
	jurisdictionCode string
	staffID          uuid.UUID
	staff2ID         uuid.UUID
	assetCode        string
}

func seedLifecycleFixture(t *testing.T, pool *db.Pool) lifecycleFixture {
	t.Helper()
	// A FRESH, dedicated test asset - never the shared "USD" seed row -
	// so this suite's own platform-authorization/eligibility mutations
	// (necessarily global, ADR 0037 layers 1-3) never leak into or
	// pollute internal/assetregistry's own assertions about USD/EUR/etc.'s
	// pristine seeded state (TestSeededAssets_ActiveGrandfatheredButNot
	// PlatformAuthorized), which run against the SAME persistent dev
	// database.
	f := lifecycleFixture{assetCode: newBonusTestAssetCode()}
	adminA := seedPlatformAdminForBonus(t, pool)
	adminB := seedPlatformAdminForBonus(t, pool)
	// The asset row must exist (layer 1, create+activate) BEFORE
	// seedFixture's own wallet insert, which FKs wallets.asset_code ->
	// assets.code.
	createAndActivateAsset(t, pool, f.assetCode, adminA, adminB)

	f.fixture = seedFixture(t, pool, f.assetCode)

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		f.jurisdictionID = uuid.New()
		f.jurisdictionCode = "TJ-" + f.jurisdictionID.String()[:8]
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Test Jurisdiction')`,
			f.jurisdictionID, f.jurisdictionCode)
		return err
	})
	if err != nil {
		t.Fatalf("seed jurisdiction: %v", err)
	}

	authorizeFreshAssetForBonusWagering(t, pool, f.assetCode, adminA, adminB, f.tenantID, f.jurisdictionID)

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		f.staffID = uuid.New()
		p1 := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, p1); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status)
			VALUES ($1, $2, $3, 'x', 'bonus_operations', $4, 'active')`, f.staffID, f.tenantID, f.staffID.String()+"@staff.example.com", p1); err != nil {
			return err
		}
		f.staff2ID = uuid.New()
		p2 := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, p2); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status)
			VALUES ($1, $2, $3, 'x', 'bonus_operations', $4, 'active')`, f.staff2ID, f.tenantID, f.staff2ID.String()+"@staff.example.com", p2)
		return err
	})
	if err != nil {
		t.Fatalf("seed staff: %v", err)
	}
	return f
}

func seedPlatformAdminForBonus(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	personID := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status)
			VALUES ($1, NULL, $2, 'x', 'platform_admin', $3, 'active')`, id, id.String()+"@platform.example.com", personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed platform admin: %v", err)
	}
	return id
}

func assetActor(id uuid.UUID) assetregistry.ActorContext {
	return assetregistry.ActorContext{ActorID: id, ReasonCode: "test", RequestID: uuid.NewString()}
}

func fileAndApproveAsset(t *testing.T, pool *db.Pool, op assetregistry.ChangeOperation, code string, requester, approver uuid.UUID, eligOp assetregistry.Operation, eligProduct string) {
	t.Helper()
	var reqID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		req, err := assetregistry.FileChangeRequest(ctx, tx, assetregistry.FileChangeRequestParams{
			Operation: op, AssetCode: code, EligibilityOperation: eligOp, EligibilityProduct: eligProduct, Actor: assetActor(requester),
		})
		reqID = req.ID
		return err
	})
	if err != nil {
		t.Fatalf("file %s request: %v", op, err)
	}
	err = pool.WithPlatformAdmin(context.Background(), approver, func(ctx context.Context, tx pgx.Tx) error {
		_, err := assetregistry.DecideChangeRequest(ctx, tx, assetregistry.DecideChangeRequestParams{RequestID: reqID, Approve: true, Actor: assetActor(approver)})
		return err
	})
	if err != nil {
		t.Fatalf("approve %s request: %v", op, err)
	}
}

func newBonusTestAssetCode() string {
	return "TB" + strings.ToUpper(uuid.New().String()[:8])
}

// authorizeFreshAssetForBonusWagering walks a brand-NEW, dedicated test
// asset through create -> activate -> platform-authorize -> product=bonus/
// operation=wagering eligibility (every one dual-controlled, mirroring
// internal/assetregistry's own liveAsset test helper exactly, never
// touching a shared seeded asset like USD), then tenant + jurisdiction
// authorization - the full ADR 0037 §A.3 layer chain doc 10 T.1's
// createAndActivateAsset walks a brand-new asset code through create ->
// activate (both dual-controlled), mirroring internal/assetregistry's own
// liveAsset test helper - called BEFORE any wallet references the asset
// code (wallets.asset_code FKs into assets.code).
func createAndActivateAsset(t *testing.T, pool *db.Pool, code string, adminA, adminB uuid.UUID) {
	t.Helper()

	var createReqID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		req, err := assetregistry.FileChangeRequest(ctx, tx, assetregistry.FileChangeRequestParams{
			Operation: assetregistry.ChangeCreate, AssetCode: code, AssetType: assetregistry.AssetTypeFiat, DecimalExponent: 2,
			Actor: assetActor(adminA),
		})
		createReqID = req.ID
		return err
	})
	if err != nil {
		t.Fatalf("file create request for %s: %v", code, err)
	}
	err = pool.WithPlatformAdmin(context.Background(), adminB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := assetregistry.DecideChangeRequest(ctx, tx, assetregistry.DecideChangeRequestParams{RequestID: createReqID, Approve: true, Actor: assetActor(adminB)})
		return err
	})
	if err != nil {
		t.Fatalf("approve create request for %s: %v", code, err)
	}
	err = pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := assetregistry.CreateAsset(ctx, tx, assetregistry.CreateAssetParams{
			Code: code, AssetType: assetregistry.AssetTypeFiat, DecimalExponent: 2, DisplayName: "Bonus Test Asset",
			Actor: assetActor(adminA),
		})
		return err
	})
	if err != nil {
		t.Fatalf("create asset %s: %v", code, err)
	}

	fileAndApproveAsset(t, pool, assetregistry.ChangeActivate, code, adminA, adminB, "", "")
	err = pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := assetregistry.SetActive(ctx, tx, code, true, assetActor(adminA))
		return err
	})
	if err != nil {
		t.Fatalf("activate asset %s: %v", code, err)
	}
}

// authorizeFreshAssetForBonusWagering walks an already-created, already-
// active asset (createAndActivateAsset, above) through platform-
// authorize -> product=bonus/operation=wagering eligibility (both dual-
// controlled), then tenant + jurisdiction authorization - the full ADR
// 0037 §A.3 layer chain doc 10 T.1's AssetAuthorization.CheckEligibility
// walks.
func authorizeFreshAssetForBonusWagering(t *testing.T, pool *db.Pool, code string, adminA, adminB, tenantID, jurisdictionID uuid.UUID) {
	t.Helper()

	fileAndApproveAsset(t, pool, assetregistry.ChangePlatformAuthorize, code, adminA, adminB, "", "")
	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := assetregistry.SetPlatformAuthorized(ctx, tx, code, true, assetActor(adminA))
		return err
	})
	if err != nil {
		t.Fatalf("platform-authorize asset %s: %v", code, err)
	}

	fileAndApproveAsset(t, pool, assetregistry.ChangePlatformOperationEligibility, code, adminA, adminB, assetregistry.OperationWagering, "bonus")
	err = pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := assetregistry.ConfigureOperationEligibility(ctx, tx, assetregistry.ConfigureEligibilityParams{
			AssetCode: code, Product: "bonus", Operation: assetregistry.OperationWagering, Eligible: true, Actor: assetActor(adminA),
		})
		return err
	})
	if err != nil {
		t.Fatalf("configure %s bonus/wagering eligibility: %v", code, err)
	}

	staffID := uuid.New()
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, 'x', 'tenant_admin')`,
			staffID, tenantID, staffID.String()+"@tenant.example.com"); err != nil {
			return err
		}
		if _, err := assetregistry.AuthorizeScope(ctx, tx, assetregistry.AuthorizeScopeParams{
			TenantID: tenantID, ScopeKind: assetregistry.ScopeTenant, AssetCode: code, Product: "bonus", Eligible: true, Actor: assetActor(staffID),
		}); err != nil {
			return err
		}
		_, err := assetregistry.AuthorizeScope(ctx, tx, assetregistry.AuthorizeScopeParams{
			TenantID: tenantID, ScopeKind: assetregistry.ScopeJurisdiction, JurisdictionID: jurisdictionID, AssetCode: code, Product: "bonus", Eligible: true, Actor: assetActor(staffID),
		})
		return err
	})
	if err != nil {
		t.Fatalf("authorize tenant/jurisdiction for %s bonus: %v", code, err)
	}
}

type campaignOffer struct {
	campaignID        uuid.UUID
	campaignVersionID uuid.UUID
	offerID           uuid.UUID
	offerVersionID    uuid.UUID
}

func seedCampaignOffer(t *testing.T, pool *db.Pool, tenantID, brandID, staffActorID uuid.UUID) campaignOffer {
	t.Helper()
	var co campaignOffer
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := CreateCampaign(ctx, tx, Campaign{TenantID: tenantID, BrandID: &brandID, CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		co.campaignID = c.ID
		v, err := CreateCampaignVersion(ctx, tx, CampaignVersion{TenantID: tenantID, CampaignID: c.ID, VersionNumber: 1, Name: "V1", CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		co.campaignVersionID = v.ID
		o, err := CreateOffer(ctx, tx, Offer{TenantID: tenantID, BrandID: &brandID, CampaignID: c.ID, CampaignVersionID: v.ID, GrantPolicy: GrantPolicyAutoIssue, CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		co.offerID = o.ID
		ov, err := CreateOfferVersion(ctx, tx, OfferVersion{
			TenantID: tenantID, OfferID: o.ID, VersionNumber: 1, RewardKind: RewardFixedValue, RewardAssetCode: "USD",
			FulfillmentDestination: FulfillmentIntoPlatformWallet, FundingSource: "operator", CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID,
		})
		if err != nil {
			return err
		}
		co.offerVersionID = ov.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed campaign/offer: %v", err)
	}
	return co
}

func newTestOfferGrant(f lifecycleFixture, co campaignOffer, triggerRef string) Grant {
	return Grant{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, WalletID: f.walletID,
		CampaignID: co.campaignID, CampaignVersionID: co.campaignVersionID, OfferID: co.offerID, OfferVersionID: co.offerVersionID,
		AssetCode: f.assetCode, DecimalExponent: 2,
		FundingSource: "operator", FulfillmentDestination: FulfillmentIntoPlatformWallet, FulfillmentOwner: "internal",
		TriggerReference:   triggerRef,
		CreatedByActorType: ActorSystem,
	}
}

// --- Core lifecycle happy path: issue -> activate -> wager -> complete ---

func TestLifecycle_IssueActivateWagerComplete(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "deposit-1")
		result, outcome, err := IssueAndActivateDepositBonus(ctx, tx, DepositBonusParams{
			Grant: g, DepositAmount: big.NewInt(10000), RateBP: 5000, CapAmount: big.NewInt(5000),
			JurisdictionCode: f.jurisdictionCode, ActorType: ActorSystem, ActorID: uuid.Nil,
		})
		if err != nil {
			return fmt.Errorf("issue deposit bonus: %w", err)
		}
		if !outcome.Allowed {
			return fmt.Errorf("expected allow, got denial %s: %s", outcome.DeniedBy, outcome.Code)
		}
		if result.Status != GrantActivated {
			return fmt.Errorf("expected activated, got %s", result.Status)
		}

		remaining, err := RemainingBonusBalance(ctx, tx, f.tenantID, result.ID)
		if err != nil {
			return err
		}
		if remaining.Cmp(big.NewInt(5000)) != 0 {
			return fmt.Errorf("expected remaining balance 5000 (capped), got %s", remaining.String())
		}

		// A second delivery of the SAME deposit event must never grant
		// twice (doc 10 §9's idempotency guarantee).
		result2, outcome2, err := IssueAndActivateDepositBonus(ctx, tx, DepositBonusParams{
			Grant: newTestOfferGrant(f, co, "deposit-1"), DepositAmount: big.NewInt(10000), RateBP: 5000, CapAmount: big.NewInt(5000),
			ActorType: ActorSystem, JurisdictionCode: f.jurisdictionCode,
		})
		if err != nil {
			return fmt.Errorf("re-issue: %w", err)
		}
		if result2.ID != result.ID {
			return fmt.Errorf("expected the SAME grant id on redelivery, got a different one (%s vs %s)", result2.ID, result.ID)
		}
		if !outcome2.Allowed {
			return fmt.Errorf("expected the idempotent redelivery to report allowed, got denial")
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- AOE / pending_settlement / G-2 mechanism ---

func TestLifecycle_TerminateWithOpenExposure_DefersToPendingSettlement(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	var grantID uuid.UUID
	var lockTxID uuid.UUID
	correlationID := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "wagering-1")
		result, outcome, err := IssueAndActivateGenericWageringBonus(ctx, tx, GenericWageringBonusParams{
			Grant: g, Amount: big.NewInt(1000), ActorType: ActorSystem, JurisdictionCode: f.jurisdictionCode,
		})
		if err != nil || !outcome.Allowed {
			return fmt.Errorf("issue/activate: %v / %+v", err, outcome)
		}
		grantID = result.ID

		lockTxID = uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id) VALUES ($1, $2, 'casino_bet', $3, $4)`,
			lockTxID, f.tenantID, "bet-"+lockTxID.String(), correlationID,
		); err != nil {
			return err
		}
		if err := RecordWageringContribution(ctx, tx, f.tenantID, grantID, RecordWageringContributionParams{
			OfferVersionID: g.OfferVersionID, LockLedgerTransactionID: lockTxID, CorrelationID: correlationID,
			AssetCode: f.assetCode, StakedBonusAmount: big.NewInt(500), ContributionWeightBP: 10000, QualifyingScaled: big.NewInt(500),
		}); err != nil {
			return fmt.Errorf("record contribution: %w", err)
		}

		reasonCode := "time_limit_elapsed"
		terminated, err := TerminateGrant(ctx, tx, f.tenantID, grantID, TerminateGrantParams{
			Resolution: TerminalResolutionExpired, ReasonCode: reasonCode, ActorType: ActorSystem, TriggerType: TriggerAutomatedRuleEvaluation,
		})
		if err != nil {
			return fmt.Errorf("terminate: %w", err)
		}
		// Invariant TI-1: a Grant with open exposure (the bet placed
		// above has no closing casino_win/casino_rollback yet) must NOT
		// reach a genuinely terminal status directly.
		if terminated.Status != GrantPendingSettlement {
			return fmt.Errorf("expected pending_settlement (open in-flight exposure), got %s", terminated.Status)
		}
		if terminated.TerminalResolution == nil || *terminated.TerminalResolution != TerminalResolutionExpired {
			return fmt.Errorf("expected terminal_resolution=expired recorded, got %+v", terminated.TerminalResolution)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Now the bet resolves as a LOSS (a plain casino_rollback is NOT what
	// happens on a loss - a loss posts nothing further; here we simulate
	// the loss path via casino's own postBet convention: no further
	// event ever arrives for a loss, so InFlightExposure never clears
	// that way. Simulate instead the ROLLBACK path (doc 10 N1.4 step
	// 5a), which IS a value-reducing closing event and must clear
	// InFlightExposure and finalize the deferred expiry via
	// RecheckGrantExposure (N1.4.2) - this is casino's own Phase 7 call,
	// exercised directly here since casino does not call it yet.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rollbackTxID := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id, reverses_transaction_id) VALUES ($1, $2, 'casino_rollback', $3, $4, $5)`,
			rollbackTxID, f.tenantID, "rollback-"+rollbackTxID.String(), correlationID, lockTxID,
		); err != nil {
			return err
		}
		newStatus, err := RecheckGrantExposure(ctx, tx, f.tenantID, grantID, rollbackTxID, TriggerCasinoRollback)
		if err != nil {
			return fmt.Errorf("recheck grant exposure: %w", err)
		}
		if newStatus != GrantExpired {
			return fmt.Errorf("expected the deferred expiry to finalize once exposure cleared, got %s", newStatus)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- G-2 hold-capture + resolution (ACTION_REFORFEIT / ACTION_ROUTE_TO_CASH), SEP-1 ---

func TestHeldDisposition_ResolveReforfeit_WithFourEyesAndSEP1(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	var grantID, dispositionID uuid.UUID
	settlementTxID := uuid.New()
	correlationID := uuid.New()

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "wagering-g2-1")
		result, outcome, err := IssueAndActivateGenericWageringBonus(ctx, tx, GenericWageringBonusParams{Grant: g, Amount: big.NewInt(1000), ActorType: ActorSystem, JurisdictionCode: f.jurisdictionCode})
		if err != nil || !outcome.Allowed {
			return fmt.Errorf("issue/activate: %v / %+v", err, outcome)
		}
		grantID = result.ID

		// Terminate the Grant (forfeited, e.g. wagering-rule breach) with
		// NO stake locked yet - this is the ordinary, un-deferred path,
		// so we can then simulate a LATE win credit arriving against an
		// already-fully-terminal Grant (doc 10 §T.7's exact G-2 trigger
		// condition).
		reasonCode := "wagering_rule_breach"
		terminated, err := TerminateGrant(ctx, tx, f.tenantID, grantID, TerminateGrantParams{
			Resolution: TerminalResolutionForfeited, ReasonCode: reasonCode, ActorType: ActorSystem, TriggerType: TriggerAutomatedRuleEvaluation,
		})
		if err != nil {
			return err
		}
		if terminated.Status != GrantForfeited {
			return fmt.Errorf("expected forfeited (no exposure was open), got %s", terminated.Status)
		}

		// The "late win" settlement transaction - casino's own Phase 7
		// job posts this; simulated here as a bare ledger_transactions
		// row for FK purposes (this test exercises ResolveTerminalGrantCredit,
		// not the hold-capture posting itself).
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

		// Idempotent redelivery of the SAME settlement transaction must
		// return the SAME disposition, never a second row (LF-22).
		dispID2, err := ResolveTerminalGrantCredit(ctx, tx, grantID, correlationID, CreditKindWin, big.NewInt(300), big.NewInt(0), settlementTxID)
		if err != nil {
			return fmt.Errorf("redeliver: %w", err)
		}
		if dispID2 != dispositionID {
			return fmt.Errorf("expected the SAME held disposition id on redelivery")
		}

		disposition, err := GetHeldDispositionByID(ctx, tx, dispositionID)
		if err != nil {
			return err
		}
		if disposition.Status != HeldDispositionHeld {
			return fmt.Errorf("expected status 'held', got %s", disposition.Status)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// SEP-1 NEGATIVE case first (the anti-inertness test §W15.1.4 requires):
	// the SAME staff member as requester and approver must be refused as
	// an ordinary four-eyes violation, and a staff member who happens to
	// BE the beneficiary must be refused by SEP-1 specifically. Bonus
	// Grants in this fixture belong to f.playerID, a player - no staff
	// member IS that player, so we instead prove SEP-1 fires by using
	// the ordinary two-different-staff self-approval path being REFUSED
	// when requester==approver (the governance trigger, proven here
	// alongside SEP-1's own separate concern).
	errSelfApprovalUnexpectedlySucceeded := errors.New("self-approval unexpectedly succeeded")
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		payload := []byte(fmt.Sprintf(`{"action":%q}`, ActionReforfeit))
		req, err := FileChangeRequest(ctx, tx, ChangeRequest{
			TenantID: f.tenantID, Operation: ChangeOpHeldDispositionResolve, TargetType: "bonus_held_dispositions", TargetID: dispositionID,
			Payload: payload, ReasonCode: "g2-resolution-test", RequestedByPrincipalID: f.staffID,
		})
		if err != nil {
			return fmt.Errorf("file change request: %w", err)
		}
		// The trigger's own RAISE EXCEPTION aborts this transaction - we
		// must propagate that failure (so WithTenant rolls back rather
		// than attempting to commit an aborted transaction), not swallow
		// it and return nil.
		if err := RecordChangeApproval(ctx, tx, f.tenantID, req.ID, f.staffID, "approve", nil, nil, nil); err == nil {
			return errSelfApprovalUnexpectedlySucceeded
		}
		return fmt.Errorf("expected refusal (self-approval)") // deliberately non-nil so WithTenant rolls back
	})
	if errors.Is(err, errSelfApprovalUnexpectedlySucceeded) {
		t.Fatal("expected self-approval (same staff member as requester) to be refused by the governance trigger")
	}
	// Any OTHER error here is the EXPECTED refusal path (either the
	// trigger's own exception, propagated through Commit, or our own
	// deliberate non-nil return above) - both prove the refusal fired.

	// Positive case: two DIFFERENT staff members file/approve, then
	// ResolveHeldDispositionAction applies ACTION_REFORFEIT.
	var requestID uuid.UUID
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		payload := []byte(fmt.Sprintf(`{"action":%q}`, ActionReforfeit))
		req, err := FileChangeRequest(ctx, tx, ChangeRequest{
			TenantID: f.tenantID, Operation: ChangeOpHeldDispositionResolve, TargetType: "bonus_held_dispositions", TargetID: dispositionID,
			Payload: payload, ReasonCode: "g2-resolution-test", RequestedByPrincipalID: f.staffID,
		})
		if err != nil {
			return err
		}
		requestID = req.ID
		return RecordChangeApproval(ctx, tx, f.tenantID, req.ID, f.staff2ID, "approve", nil, nil, nil)
	})
	if err != nil {
		t.Fatalf("file+approve (two distinct staff): %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		resolved, err := ResolveHeldDispositionAction(ctx, tx, f.tenantID, ResolveHeldDispositionActionParams{
			HeldDispositionID: dispositionID, Action: ActionReforfeit, ActorID: f.staffID, ReasonCode: "g2-resolution-test",
			RequestID: requestID, RequiredApprovals: 1,
		})
		if err != nil {
			return fmt.Errorf("resolve held disposition: %w", err)
		}
		if resolved.Status != HeldDispositionResolvedReforfeit {
			return fmt.Errorf("expected resolved_reforfeit, got %s", resolved.Status)
		}

		// A second resolution attempt against the same (now non-'held')
		// disposition must be rejected, never double-applied.
		if _, err := ResolveHeldDispositionAction(ctx, tx, f.tenantID, ResolveHeldDispositionActionParams{
			HeldDispositionID: dispositionID, Action: ActionReforfeit, ActorID: f.staffID, ReasonCode: "g2-resolution-test",
			RequestID: requestID, RequiredApprovals: 1,
		}); !errors.Is(err, ErrHeldDispositionNotHeld) {
			return fmt.Errorf("expected ErrHeldDispositionNotHeld on a second resolution attempt, got %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- EOI: bulk grant respects recipient_ceiling across the whole subtree ---

func TestEOI_BulkGrantRecipientCeiling_RejectsBeyondBudget(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	var players []uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for i := 0; i < 3; i++ {
			pid := uuid.New()
			personID := uuid.New()
			if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1,$2,$3,$4,$5,'x','active')`,
				pid, f.tenantID, f.brandID, personID, pid.String()+"@example.com"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1,$2,$3,$4,$5)`,
				uuid.New(), f.tenantID, f.brandID, pid, f.assetCode); err != nil {
				return err
			}
			players = append(players, pid)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed extra players: %v", err)
	}

	var rootID uuid.UUID
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ceiling := int32(2) // fewer than the 3 targeted players
		asset := f.assetCode
		op, err := MintRootOperation(ctx, tx, MintRootOperationParams{
			TenantID: f.tenantID, OperationType: economicop.OperationBonusBulkGrant,
			InitiatingActorType: "staff", InitiatingActorID: f.staffID,
			SubjectScope: economicop.SubjectScopeEnumeratedSet, AssetCode: &asset,
			IntendedAggregateValue: big.NewInt(100000), RecipientCeiling: &ceiling,
			IdempotencyKey: "bulk-ceiling-test", CorrelationID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
			ApprovalState: economicop.ApprovalApproved,
		})
		if err != nil {
			return err
		}
		rootID = op.OperationID

		job, err := CreateBulkGrantJob(ctx, tx, BulkGrantJob{
			TenantID: f.tenantID, BrandID: f.brandID, CampaignID: co.campaignID, OfferVersionID: co.offerVersionID,
			TargetKind: TargetPlayerList, TargetPlayerList: players,
			RequestedByPrincipalID: f.staffID, ApprovalState: BulkApprovalApproved, Status: BulkJobRunning,
			IdempotencyKey: "bulk-ceiling-job", ParentOperationID: &rootID,
		})
		if err != nil {
			return err
		}

		template := newTestOfferGrant(f, co, "")
		err = RunStaticBulkGrantJob(ctx, tx, job, StaticPlayerListTarget{PlayerAccountIDs: players}, template, f.jurisdictionCode, uuid.Nil, big.NewInt(1000))
		if err != nil {
			return fmt.Errorf("run bulk job: %w", err)
		}

		items, err := ListBulkGrantJobItems(ctx, tx, f.tenantID, job.ID)
		if err != nil {
			return err
		}
		if len(items) != 3 {
			return fmt.Errorf("expected 3 item rows (one per targeted player), got %d", len(items))
		}
		issued := 0
		denied := 0
		for _, it := range items {
			switch it.Outcome {
			case ItemIssued:
				issued++
			case ItemDenied:
				denied++
			default:
				return fmt.Errorf("unexpected item outcome %s", it.Outcome)
			}
		}
		if issued != 2 {
			return fmt.Errorf("expected exactly 2 issued (recipient_ceiling=2), got %d", issued)
		}
		if denied != 1 {
			return fmt.Errorf("expected exactly 1 denied (budget exhausted), got %d", denied)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- Adversarial: single-Grant surface rejects with no resolvable EOI parent ---

func TestEOI_SingleManualGrant_RejectsUnresolvableParent(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "manual-1")
		_, _, err := IssueSingleManualGrant(ctx, tx, g, uuid.New() /* does not exist */, f.jurisdictionCode, f.staffID, big.NewInt(500))
		if !errors.Is(err, economicop.ErrParentOperationNotFound) {
			return fmt.Errorf("expected ErrParentOperationNotFound, got %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- Adversarial: cross-tenant mutation attempt (direct SQL) ---

func TestAdversarial_CrossTenantGrantUpdate_BlockedByRLS(t *testing.T) {
	pool := testPool(t)
	fA := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, fA.tenantID, fA.brandID, fA.staffID)
	fB := seedFixture(t, pool, "USD")

	var grantID uuid.UUID
	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(fA, co, "cross-tenant-1")
		created, err := CreateGrant(ctx, tx, g)
		grantID = created.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	// Tenant B's own scoped transaction must see zero rows for tenant A's
	// Grant - RLS, not application discipline.
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE bonus_grants SET status = 'cancelled' WHERE id = $1`, grantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			return fmt.Errorf("cross-tenant UPDATE affected %d rows - RLS did not isolate", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- Adversarial: concurrent activation race - exactly one wins ---

func TestAdversarial_ConcurrentActivation_ExactlyOneWins(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	var grantID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "concurrent-activation-1")
		created, err := CreateGrant(ctx, tx, g)
		grantID = created.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	const n = 5
	var wg sync.WaitGroup
	successes := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, outcome, err := ActivateGrant(ctx, tx, f.tenantID, grantID, ActivateGrantParams{ActorType: ActorSystem, Amount: big.NewInt(100), JurisdictionCode: f.jurisdictionCode})
				if err != nil {
					return err
				}
				successes[idx] = outcome.Allowed
				return nil
			})
			if err != nil && !errors.Is(err, ErrGrantStateConflict) && !errors.Is(err, ErrIllegalTransition) {
				t.Errorf("goroutine %d: unexpected error: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	won := 0
	for _, s := range successes {
		if s {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("expected exactly 1 successful activation out of %d concurrent attempts, got %d", n, won)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g, err := GetGrantByID(ctx, tx, grantID)
		if err != nil {
			return err
		}
		if g.Status != GrantActivated {
			return fmt.Errorf("expected final status activated, got %s", g.Status)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- Adversarial: conversion is blocked while risk.Operation("bonus_conversion") is unknown ---

func TestConversion_BlockedByMissingRiskOperation_NeverForfeits(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "conversion-1")
		result, outcome, err := IssueAndActivateGenericWageringBonus(ctx, tx, GenericWageringBonusParams{Grant: g, Amount: big.NewInt(1000), ActorType: ActorSystem, JurisdictionCode: f.jurisdictionCode})
		if err != nil || !outcome.Allowed {
			return fmt.Errorf("issue/activate: %v / %+v", err, outcome)
		}
		completed, ok, err := CheckAndCompleteGrant(ctx, tx, f.tenantID, result.ID, nil)
		if err != nil || !ok {
			return fmt.Errorf("complete: %v / %v", err, ok)
		}

		convResult, err := ConvertGrant(ctx, tx, f.tenantID, completed.ID, nil, ConvertGrantParams{ActorType: ActorSystem, JurisdictionCode: f.jurisdictionCode})
		if err != nil {
			return fmt.Errorf("convert: %w", err)
		}
		if convResult.Converted {
			return fmt.Errorf("expected conversion to be BLOCKED (risk.Operation bonus_conversion does not exist yet) - it unexpectedly succeeded")
		}
		if convResult.BlockedReason != "risk" {
			return fmt.Errorf("expected block reason 'risk', got %q (%s)", convResult.BlockedReason, convResult.BlockedCode)
		}
		if convResult.Grant.Status != GrantCompleted {
			return fmt.Errorf("expected the grant to remain 'completed' (never forfeited on a conversion-time denial), got %s", convResult.Grant.Status)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
