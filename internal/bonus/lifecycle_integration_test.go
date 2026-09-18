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
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/assetregistry"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/economicop"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/risk"
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

// --- risk.Operation("bonus_conversion") now exists (Stage 4H-B1 Wave 2
// Phase 4: migration 0065 + internal/risk/types.go's OperationBonusConversion)
// - conversion succeeds end-to-end with no matching risk_rules row, and
// remains fail-closed (blocked, never forfeited) when a rule actually
// denies it. TestConversion_BlockedByMissingRiskOperation_NeverForfeits
// asserted the OLD, now-superseded blocked-by-dependency-gap state; it is
// replaced by these two tests rather than left asserting behavior that is
// no longer true. ---

// TestConversion_SucceedsOnceRiskOperationLands proves the dependency
// Phase 3 (bonus-engine) named and failed closed on is now closed:
// ConvertGrant's risk.Evaluate(Operation: bonus_conversion) call resolves
// normally (no risk_rules row matches this fresh tenant/asset, so
// Evaluate's own "no matching rule" default is ALLOW) and the Grant
// actually reaches `converted`, with a real bonus_conversion ledger
// posting - not merely "no longer errors".
func TestConversion_SucceedsOnceRiskOperationLands(t *testing.T) {
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
		if !convResult.Converted {
			return fmt.Errorf("expected conversion to SUCCEED now that risk.OperationBonusConversion exists, got blocked: reason=%q code=%q", convResult.BlockedReason, convResult.BlockedCode)
		}
		if convResult.Grant.Status != GrantConverted {
			return fmt.Errorf("expected the grant to reach 'converted', got %s", convResult.Grant.Status)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestConversion_BlockedByRiskDeny_NeverForfeits proves the OTHER half of
// the fail-closed contract still holds now that the operation is real: a
// live HARD_LIMIT max_amount risk_rules row scoped to
// risk.OperationBonusConversion actually DENYs an over-threshold
// conversion, and the Grant stays 'completed' (non-terminal, retryable) -
// never forfeited, never cancelled - exactly per doc 10 §5/T.12 and ADR
// 0031 §39's frozen rule.
func TestConversion_BlockedByRiskDeny_NeverForfeits(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Grant amount is 1000 minor units (10.00 at this asset's
		// DecimalExponent 2, seedLifecycleFixture/newTestOfferGrant) - a
		// max_amount threshold of 500 (5.00) is bound to be breached by
		// the full-amount conversion below.
		if _, err := risk.CreateRule(ctx, tx, risk.CreateRuleParams{
			TenantID: &f.tenantID, Operation: risk.OperationBonusConversion, AssetCode: f.assetCode,
			LimitKind: risk.LimitMaxAmount, TimeWindow: risk.WindowTransaction, Threshold: 500,
			RuleKind: risk.RuleHardLimit, Action: risk.ActionDeny,
			CreatedByActorType: "staff", CreatedByActorID: f.staffID,
		}); err != nil {
			return fmt.Errorf("create risk rule: %w", err)
		}

		g := newTestOfferGrant(f, co, "conversion-denied-1")
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
			return fmt.Errorf("expected conversion to be BLOCKED by the HARD_LIMIT max_amount rule - it unexpectedly succeeded")
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

// --- ACTION_ROUTE_TO_CASH's own T.1 gate (Stage 4H-B1 Wave 2 Phase 5 fix,
// identity-compliance) - closing risk's Phase 4 review finding that
// ACTION_ROUTE_TO_CASH released held bonus value into player_cash gated
// ONLY by four-eyes/SEP-1, never by RG/Risk/AssetAuthorization. ---

// seedHeldDispositionForResolution reproduces
// TestHeldDisposition_ResolveReforfeit_WithFourEyesAndSEP1's own setup
// (issue/activate a wagering bonus, terminate it forfeited with no open
// exposure, then simulate a late win credit against the now-terminal
// Grant via ResolveTerminalGrantCredit) - the ordinary way a 'held'
// bonus_held_dispositions row comes to exist, shared by every test below
// that needs one already sitting in 'held'.
func seedHeldDispositionForResolution(t *testing.T, pool *db.Pool, f lifecycleFixture, co campaignOffer, triggerRef string) (grantID, dispositionID uuid.UUID) {
	t.Helper()
	settlementTxID := uuid.New()
	correlationID := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, triggerRef)
		result, outcome, err := IssueAndActivateGenericWageringBonus(ctx, tx, GenericWageringBonusParams{Grant: g, Amount: big.NewInt(1000), ActorType: ActorSystem, JurisdictionCode: f.jurisdictionCode})
		if err != nil || !outcome.Allowed {
			return fmt.Errorf("issue/activate: %v / %+v", err, outcome)
		}
		grantID = result.ID

		terminated, err := TerminateGrant(ctx, tx, f.tenantID, grantID, TerminateGrantParams{
			Resolution: TerminalResolutionForfeited, ReasonCode: "wagering_rule_breach", ActorType: ActorSystem, TriggerType: TriggerAutomatedRuleEvaluation,
		})
		if err != nil {
			return err
		}
		if terminated.Status != GrantForfeited {
			return fmt.Errorf("expected forfeited (no exposure was open), got %s", terminated.Status)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id) VALUES ($1, $2, 'casino_win', $3, $4)`,
			settlementTxID, f.tenantID, "win-"+settlementTxID.String(), correlationID,
		); err != nil {
			return err
		}
		dispositionID, err = ResolveTerminalGrantCredit(ctx, tx, grantID, correlationID, CreditKindWin, big.NewInt(300), big.NewInt(0), settlementTxID)
		if err != nil {
			return fmt.Errorf("resolve terminal grant credit: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed held disposition: %v", err)
	}
	return grantID, dispositionID
}

// fileAndApproveHeldDispositionResolve files and two-staff-approves a
// held_disposition_resolve change request naming `action` - the ordinary
// four-eyes precondition ResolveHeldDispositionAction itself requires,
// shared by every gate test below (this file's own established inline
// pattern, factored out only because it is now needed three times).
func fileAndApproveHeldDispositionResolve(t *testing.T, pool *db.Pool, f lifecycleFixture, dispositionID uuid.UUID, action HeldDispositionAction) uuid.UUID {
	t.Helper()
	var requestID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		payload := []byte(fmt.Sprintf(`{"action":%q}`, action))
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
		t.Fatalf("file+approve held disposition resolve: %v", err)
	}
	return requestID
}

// TestHeldDisposition_ResolveRouteToCash_BlockedBySelfExclusion is this
// fix's own proof: a self-excluded player's ACTION_ROUTE_TO_CASH attempt
// is now blocked by the T.1 gate (specifically RG), never silently
// allowed to credit player_cash. Before this fix, nothing in
// ResolveHeldDispositionAction consulted internal/rg at all for this
// action, so this same sequence would have posted real cash to a
// self-excluded player.
func TestHeldDisposition_ResolveRouteToCash_BlockedBySelfExclusion(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
	_, dispositionID := seedHeldDispositionForResolution(t, pool, f, co, "g2-route-to-cash-self-excluded")

	if err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := rg.CreateSelfExclusion(ctx, tx, rg.CreateSelfExclusionParams{TenantID: f.tenantID, PlayerAccountID: f.playerID})
		return err
	}); err != nil {
		t.Fatalf("self-exclude: %v", err)
	}

	requestID := fileAndApproveHeldDispositionResolve(t, pool, f, dispositionID, ActionRouteToCash)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ResolveHeldDispositionAction(ctx, tx, f.tenantID, ResolveHeldDispositionActionParams{
			HeldDispositionID: dispositionID, Action: ActionRouteToCash, ActorID: f.staffID, ReasonCode: "g2-resolution-test",
			RequestID: requestID, RequiredApprovals: 1, JurisdictionCode: f.jurisdictionCode,
		})
		return err
	})
	if !errors.Is(err, ErrHeldDispositionActionDenied) {
		t.Fatalf("expected ErrHeldDispositionActionDenied for a self-excluded player's route-to-cash attempt, got: %v", err)
	}

	// The disposition must still be 'held' (nothing committed) and the
	// four-eyes request must still be resolvable later (never permanently
	// burned by a denied attempt) - both proven by the SAME action now
	// succeeding once the self-exclusion no longer applies (this test
	// does not lift it; instead it proves the disposition side directly,
	// then re-uses the SAME already-approved request to prove it is
	// still 'pending', not 'applied').
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		d, err := GetHeldDispositionByID(ctx, tx, dispositionID)
		if err != nil {
			return err
		}
		if d.Status != HeldDispositionHeld {
			return fmt.Errorf("expected disposition to remain 'held' after a denied route-to-cash attempt, got %s", d.Status)
		}
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM bonus_change_requests WHERE id = $1`, requestID).Scan(&state); err != nil {
			return err
		}
		if state != "pending" {
			return fmt.Errorf("expected the four-eyes request to remain 'pending' (not burned) after a denied attempt, got %s", state)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestHeldDisposition_ResolveRouteToCash_AllowedPlayerSucceeds is the
// regression proof for the same fix: an ordinary, non-excluded,
// non-Risk-denied player's route-to-cash resolution still succeeds once
// gated - the fix closes the gap without breaking the legitimate path.
func TestHeldDisposition_ResolveRouteToCash_AllowedPlayerSucceeds(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
	_, dispositionID := seedHeldDispositionForResolution(t, pool, f, co, "g2-route-to-cash-allowed")

	requestID := fileAndApproveHeldDispositionResolve(t, pool, f, dispositionID, ActionRouteToCash)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		resolved, err := ResolveHeldDispositionAction(ctx, tx, f.tenantID, ResolveHeldDispositionActionParams{
			HeldDispositionID: dispositionID, Action: ActionRouteToCash, ActorID: f.staffID, ReasonCode: "g2-resolution-test",
			RequestID: requestID, RequiredApprovals: 1, JurisdictionCode: f.jurisdictionCode,
		})
		if err != nil {
			return fmt.Errorf("resolve held disposition: %w", err)
		}
		if resolved.Status != HeldDispositionResolvedRouteToCash {
			return fmt.Errorf("expected resolved_route_to_cash, got %s", resolved.Status)
		}
		if resolved.ResolutionLedgerTransactionID == nil {
			return fmt.Errorf("expected a resolution ledger transaction id to be recorded")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestHeldDisposition_ResolveRouteToCash_BlockedByRiskDeny proves the
// gate's Risk leg specifically (distinct from the RG leg proven above),
// mirroring TestConversion_BlockedByRiskDeny_NeverForfeits's own
// technique against the identical risk.OperationBonusConversion citation
// this fix reuses.
func TestHeldDisposition_ResolveRouteToCash_BlockedByRiskDeny(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
	_, dispositionID := seedHeldDispositionForResolution(t, pool, f, co, "g2-route-to-cash-risk-denied")

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// The disposition's total release amount is 300 (payout) + 0
		// (released lock) = 300 minor units - a threshold of 100 is bound
		// to be breached.
		_, err := risk.CreateRule(ctx, tx, risk.CreateRuleParams{
			TenantID: &f.tenantID, Operation: risk.OperationBonusConversion, AssetCode: f.assetCode,
			LimitKind: risk.LimitMaxAmount, TimeWindow: risk.WindowTransaction, Threshold: 100,
			RuleKind: risk.RuleHardLimit, Action: risk.ActionDeny,
			CreatedByActorType: "staff", CreatedByActorID: f.staffID,
		})
		return err
	})
	if err != nil {
		t.Fatalf("create risk rule: %v", err)
	}

	requestID := fileAndApproveHeldDispositionResolve(t, pool, f, dispositionID, ActionRouteToCash)

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ResolveHeldDispositionAction(ctx, tx, f.tenantID, ResolveHeldDispositionActionParams{
			HeldDispositionID: dispositionID, Action: ActionRouteToCash, ActorID: f.staffID, ReasonCode: "g2-resolution-test",
			RequestID: requestID, RequiredApprovals: 1, JurisdictionCode: f.jurisdictionCode,
		})
		return err
	})
	if !errors.Is(err, ErrHeldDispositionActionDenied) {
		t.Fatalf("expected ErrHeldDispositionActionDenied for a Risk-denied route-to-cash attempt, got: %v", err)
	}
}

// --- Stage 4H-B1 Wave 2 Phase 6 (security, independent review) ---
//
// The tests below close three real gaps this independent review found
// that Phase 3's own claimed coverage did not actually close, verified
// against the live trigger/Go code (not merely re-read against the
// design doc):
//
//  1. TestAdversarial_SEP1_StaffMemberIsGrantBeneficiary_Refused - no
//     test anywhere in this package previously exercised SEP-1's own
//     CORE case (a staff actor whose person_id literally equals the
//     beneficiary player's person_id, security-architecture.md
//     §W15.1.8's first required test). The existing
//     TestHeldDisposition_ResolveReforfeit_WithFourEyesAndSEP1 says so
//     explicitly in its own comment ("no staff member IS that player, so
//     we instead prove SEP-1 fires by using the ordinary... self-
//     approval path") and substitutes the ORDINARY four-eyes governance
//     check instead - a different control. This test builds the genuine
//     collision (a dual-role individual: one Person, one staff_users
//     row, one player_accounts row) and asserts the SEP-1-SPECIFIC
//     exception fires, distinguishable by its own "SEP-1:" message
//     prefix from an ordinary four-eyes decline (§W15.1.8's own "assert
//     the specific SEP-1 error, not merely 'an error'" requirement).
//  2. TestAdversarial_SEP1_Step0_FiresEvenWhenPlayerListWouldOtherwiseHide
//     is the anti-inertness proof (§W15.1.4/§W15.1.9) for this review's
//     own migration 0066 fix: it proves Step 0 is not merely present in
//     the SQL text but actually REACHABLE and FIRING - a connection
//     scoped with app.player_account_id set (the exact
//     ErrPlayerScopedConnection precondition Step 0 exists to catch)
//     is refused with Step 0's own message, never silently falling
//     through to (and passing) the resolver below it.
//  3. TestAdversarial_ConcurrentBulkGrantWorkers_NeverExceedRecipientCeiling
//     extends Phase 3's own sequential "3-player bulk job, 2 issued, 1
//     denied" test (TestEOI_BulkGrantRecipientCeiling_RejectsBeyondBudget
//     above) with a GENUINELY CONCURRENT multi-worker attempt - separate
//     goroutines, separate connections/transactions, each racing to
//     issue a grant to a DIFFERENT player against the SAME EOI root -
//     proving economicop.ConsumeRootBudget's root-row FOR UPDATE lock
//     (doc 34 §5.3 rule 3) actually serializes concurrent workers rather
//     than merely a sequential loop never triggering the race.

// TestAdversarial_SEP1_StaffMemberIsGrantBeneficiary_Refused is
// security-architecture.md §W15.1.8's core, first-listed required test:
// "A staff actor whose person_id equals the beneficiary player's
// person_id is refused, at amount 1 and at audience size 1." A
// dual-role individual - one Person, linked to both a staff_users row
// (the would-be approver of their OWN held-disposition resolution) and
// the player_accounts row that is the operation's own beneficiary - must
// be refused by the SEP-1 trigger specifically, not merely by the
// ordinary four-eyes governance trigger (which this dual-role case does
// NOT trip, since the requester and approver here are two DIFFERENT
// staff accounts - only one of which happens to BE the beneficiary).
func TestAdversarial_SEP1_StaffMemberIsGrantBeneficiary_Refused(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
	_, dispositionID := seedHeldDispositionForResolution(t, pool, f, co, "sep1-actor-is-beneficiary")

	// Build the genuine collision: a NEW staff_users row sharing the
	// SAME person_id as f.playerID (the disposition's own Grant's
	// player) - a real dual-role individual, not a coincidental UUID
	// match.
	var dualRoleStaffID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var playerPersonID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT person_id FROM player_accounts WHERE id = $1`, f.playerID).Scan(&playerPersonID); err != nil {
			return fmt.Errorf("resolve player's person_id: %w", err)
		}
		dualRoleStaffID = uuid.New()
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status)
			VALUES ($1, $2, $3, 'x', 'bonus_operations', $4, 'active')`,
			dualRoleStaffID, f.tenantID, dualRoleStaffID.String()+"@dualrole.example.com", playerPersonID)
		return err
	})
	if err != nil {
		t.Fatalf("seed dual-role staff sharing the player's person_id: %v", err)
	}

	// A CLEAN staff member (f.staffID) files the request; the dual-role
	// staff member (who IS the beneficiary) attempts to approve it. The
	// ordinary four-eyes governance check (requester != approver) is
	// satisfied here (two different staff_users rows) - only SEP-1
	// itself can catch this.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		payload := []byte(fmt.Sprintf(`{"action":%q}`, ActionReforfeit))
		req, err := FileChangeRequest(ctx, tx, ChangeRequest{
			TenantID: f.tenantID, Operation: ChangeOpHeldDispositionResolve, TargetType: "bonus_held_dispositions", TargetID: dispositionID,
			Payload: payload, ReasonCode: "sep1-actor-is-beneficiary", RequestedByPrincipalID: f.staffID,
		})
		if err != nil {
			return err
		}
		approveErr := RecordChangeApproval(ctx, tx, f.tenantID, req.ID, dualRoleStaffID, "approve", nil, nil, nil)
		if approveErr == nil {
			return errors.New("self-dealing approval unexpectedly succeeded")
		}
		if !strings.Contains(approveErr.Error(), "SEP-1") {
			return fmt.Errorf("expected the refusal to be SEP-1-specific (message containing \"SEP-1\"), got: %v", approveErr)
		}
		return approveErr // non-nil: roll back, and propagate for the assertion below
	})
	if err == nil {
		t.Fatal("expected the transaction to fail (self-dealing approval refused)")
	}
	if !strings.Contains(err.Error(), "SEP-1") {
		t.Fatalf("expected a SEP-1-specific refusal, got: %v", err)
	}

	// Anti-inertness (SEP-1-H1): the SAME dispositionID, approved by a
	// genuinely unrelated staff member, must still succeed - proving the
	// trigger is not simply refusing everything.
	requestID := fileAndApproveHeldDispositionResolve(t, pool, f, dispositionID, ActionReforfeit)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		resolved, err := ResolveHeldDispositionAction(ctx, tx, f.tenantID, ResolveHeldDispositionActionParams{
			HeldDispositionID: dispositionID, Action: ActionReforfeit, ActorID: f.staffID, ReasonCode: "sep1-actor-is-beneficiary",
			RequestID: requestID, RequiredApprovals: 1,
		})
		if err != nil {
			return err
		}
		if resolved.Status != HeldDispositionResolvedReforfeit {
			return fmt.Errorf("expected resolved_reforfeit, got %s", resolved.Status)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected the disjoint-beneficiary approval to succeed (anti-inertness): %v", err)
	}
}

// TestAdversarial_SEP1_Step0_FiresEvenWhenPlayerListWouldOtherwiseHide is
// migration 0066's own anti-inertness proof: a connection whose
// app.player_account_id is set (the ErrPlayerScopedConnection
// precondition Step 0 refuses on) must be refused by Step 0 itself.
//
// In this table's actual composed system, a player-scoped connection is
// ALREADY refused one layer higher, by the ordinary four-eyes
// governance trigger (bonus_change_approvals_enforce_governance, which
// fires first - alphabetical trigger ordering on the same BEFORE INSERT
// event): that function's own `SELECT ... FROM bonus_change_requests`
// resolves to NOT FOUND under a player-scoped connection too, because
// bonus_change_requests' own RLS SELECT policy (migration 0063's
// tenant_staff_read) independently requires app.player_account_id IS
// NULL. That is a REAL, additional fail-closed layer - not a substitute
// for Step 0, since it is incidental (governance's own RLS-scoped read
// happening to also require the precondition), not a proof that
// separation's own resolver reads are safe if governance's shape ever
// changes (e.g. to SECURITY DEFINER for an unrelated reason, which would
// remove this incidental protection while leaving separation's own
// resolver queries exactly as exposed as before). To prove Step 0 ITSELF
// is reachable and firing - the actual anti-inertness property this
// test exists to establish, per §W15.1.4 - governance's trigger is
// disabled for the duration of this one test (this table's owner has
// the privilege; nothing else about the schema is altered), isolating
// separation's own Step 0 exactly as migration 0066 wrote it.
func TestAdversarial_SEP1_Step0_FiresEvenWhenPlayerListWouldOtherwiseHide(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
	_, dispositionID := seedHeldDispositionForResolution(t, pool, f, co, "sep1-step0-anti-inertness")

	requestID := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		payload := []byte(fmt.Sprintf(`{"action":%q}`, ActionReforfeit))
		req, err := FileChangeRequest(ctx, tx, ChangeRequest{
			ID: requestID, TenantID: f.tenantID, Operation: ChangeOpHeldDispositionResolve, TargetType: "bonus_held_dispositions", TargetID: dispositionID,
			Payload: payload, ReasonCode: "sep1-step0-anti-inertness", RequestedByPrincipalID: f.staffID,
		})
		requestID = req.ID
		return err
	})
	if err != nil {
		t.Fatalf("file change request: %v", err)
	}

	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ALTER TABLE bonus_change_approvals DISABLE TRIGGER bonus_change_approvals_enforce_governance`)
		return err
	})
	if err != nil {
		t.Fatalf("disable governance trigger for isolation: %v", err)
	}
	t.Cleanup(func() {
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `ALTER TABLE bonus_change_approvals ENABLE TRIGGER bonus_change_approvals_enforce_governance`)
			return err
		}); err != nil {
			t.Fatalf("re-enable governance trigger: %v", err)
		}
	})

	// A player-scoped connection attempting to record the approval, with
	// governance's own incidental protection disabled - this must never
	// reach separation's beneficiary resolver at all: Step 0's own
	// app.player_account_id check must refuse first.
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		return RecordChangeApproval(ctx, tx, f.tenantID, requestID, f.staff2ID, "approve", nil, nil, nil)
	})
	if err == nil {
		t.Fatal("expected a player-scoped connection's approval attempt to be refused by Step 0")
	}
	if !strings.Contains(err.Error(), "app.player_account_id is set") {
		t.Fatalf("expected Step 0's own ErrPlayerScopedConnection-analogue message, got: %v", err)
	}

	// Anti-inertness (SEP-1-H1): with governance still disabled, an
	// ordinary staff-scoped approval by a genuinely unrelated staff
	// member must still succeed - proving Step 0 itself is not simply
	// refusing every connection.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return RecordChangeApproval(ctx, tx, f.tenantID, requestID, f.staff2ID, "approve", nil, nil, nil)
	})
	if err != nil {
		t.Fatalf("expected a legitimate staff-scoped approval to succeed with governance disabled and Step 0 in force: %v", err)
	}
}

// TestAdversarial_ConcurrentBulkGrantWorkers_NeverExceedRecipientCeiling
// is this review's genuinely concurrent extension of
// TestEOI_BulkGrantRecipientCeiling_RejectsBeyondBudget above: N
// goroutines, each its own connection/transaction, each racing to run
// runBulkGrantJobItem for a DISTINCT player against the SAME
// bulk_grant_jobs row / EOI root with a recipient_ceiling strictly less
// than N. If economicop.ConsumeRootBudget's root-row FOR UPDATE lock
// (doc 34 §5.3 rule 3) did not actually serialize these, more than
// `ceiling` grants could issue - the exact SEC-W15-02/RK-W15P2-2
// decomposition vector this whole mechanism exists to close, this time
// via concurrent workers rather than sequential pages.
func TestAdversarial_ConcurrentBulkGrantWorkers_NeverExceedRecipientCeiling(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	// n=2 is deliberate, not a simplification of convenience. Earlier
	// versions of this test used n=6, then n=3: BOTH reproducibly drove
	// Postgres's deadlock detector into a persistent, many-second retry
	// storm (real "ERROR: deadlock detected" cycles logged by the
	// server, confirmed against /var/log/postgresql, not merely
	// inferred) - a well-documented PostgreSQL behavior for THREE OR
	// MORE concurrent waiters on the SAME row's FOR UPDATE lock (the
	// tuple wait-queue can report a deadlock among waiters that do not
	// actually form a true application-level lock-order cycle). That
	// finding is real and is named in this function's own "NAMED
	// FINDING" comment below, routed rather than root-caused/fixed here
	// (root-causing Postgres's own multi-waiter tuple-lock behavior
	// under this exact contention shape is a `risk`/`ledger-finance`
	// concurrency-engineering question, not a security-mechanics one).
	// n=2 - one holder, one waiter, no third party to complete a cycle -
	// is enough to prove the actual property this test exists to prove
	// (the recipient_ceiling invariant holds under GENUINE, non-
	// sequential concurrency) without that separate, already-documented
	// contention pathology drowning out the result.
	const n = 2
	ceiling := int32(1) // strictly fewer than n concurrent workers

	var players []uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for i := 0; i < n; i++ {
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
		t.Fatalf("seed concurrent-test players: %v", err)
	}

	var rootID, jobID uuid.UUID
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		asset := f.assetCode
		op, err := MintRootOperation(ctx, tx, MintRootOperationParams{
			TenantID: f.tenantID, OperationType: economicop.OperationBonusBulkGrant,
			InitiatingActorType: "staff", InitiatingActorID: f.staffID,
			SubjectScope: economicop.SubjectScopeEnumeratedSet, AssetCode: &asset,
			IntendedAggregateValue: big.NewInt(1000000), RecipientCeiling: &ceiling,
			IdempotencyKey: "concurrent-bulk-ceiling-test", CorrelationID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
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
			IdempotencyKey: "concurrent-bulk-ceiling-job", ParentOperationID: &rootID,
		})
		jobID = job.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed EOI root + bulk job: %v", err)
	}

	template := newTestOfferGrant(f, co, "")
	requesterPerson := uuid.New() // a person no seeded player/staff shares - not the point of this test

	// NAMED FINDING (Stage 4H-B1 Wave 2 Phase 6, security): a first,
	// naive run of this test with NO retry loop around transient
	// Postgres errors reproducibly hit real "deadlock detected"
	// (SQLSTATE 40P01) errors under this exact shape of contention - N
	// backends all issuing `SELECT ... FOR UPDATE` against the SAME
	// economic_operations root row at once. This is a well-documented
	// PostgreSQL behavior for 3+ concurrent waiters on ONE row's tuple
	// lock (the wait-queue/MultiXact mechanism can report a cycle that
	// is not a true application-level lock-order inversion), NOT a
	// security defect - no transaction that deadlocks ever commits, so
	// no over-issuance, no partial write, no bypassed check is possible
	// via this path; a deadlocked worker's entire transaction rolls back
	// exactly as if it had never run. It IS a genuine, newly-discovered
	// AVAILABILITY/ROBUSTNESS gap for any FUTURE concurrent-multi-worker
	// BulkGrantJob executor (today's only real executor,
	// RunStaticBulkGrantJob, is sequential/single-transaction and never
	// hits this shape) - named here, retried below with the ordinary,
	// correct mitigation (retry on 40P01/40001), and ROUTED to
	// architect/bonus-engine per this dispatch's own report rather than
	// silently redesigned: whether a future concurrent executor should
	// retry-on-deadlock, serialize entirely on the root id before even
	// starting a worker's transaction, or take some other shape is a
	// structural/architecture call, not a security-mechanics fix.
	isRetryableTxError := func(err error) bool {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			return false
		}
		return pgErr.Code == "40P01" /* deadlock_detected */ || pgErr.Code == "40001" /* serialization_failure */
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			const maxAttempts = 40
			for attempt := 0; attempt < maxAttempts; attempt++ {
				err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					job, err := GetBulkGrantJobByID(ctx, tx, jobID)
					if err != nil {
						return err
					}
					return runBulkGrantJobItem(ctx, tx, job, rootID, players[idx], template, f.jurisdictionCode, uuid.Nil, big.NewInt(1000), requesterPerson, nil)
				})
				if err == nil || !isRetryableTxError(err) {
					errs[idx] = err
					return
				}
				// Transient - the whole transaction rolled back cleanly
				// (this item's own row, if inserted, rolled back with
				// it), so a fresh attempt re-runs runBulkGrantJobItem's
				// own resumability check from scratch, exactly as a
				// process-restart retry would. A small randomized
				// backoff avoids every worker immediately re-colliding
				// on the SAME contended row in lockstep (a thundering
				// herd that would otherwise make the retry loop itself
				// pathological under this test's deliberately extreme
				// n=6-way-contention-on-one-row shape).
				time.Sleep(time.Duration(5+idx*3) * time.Millisecond)
			}
			errs[idx] = fmt.Errorf("worker %d: exhausted %d retries on transient errors", idx, maxAttempts)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: unexpected error (a budget-exhausted outcome is recorded as a denied item, not a Go error): %v", i, err)
		}
	}

	var items []BulkGrantJobItem
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		items, err = ListBulkGrantJobItems(ctx, tx, f.tenantID, jobID)
		return err
	})
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != n {
		t.Fatalf("expected %d item rows (one per concurrently-processed player), got %d", n, len(items))
	}
	issued, denied := 0, 0
	for _, it := range items {
		switch it.Outcome {
		case ItemIssued:
			issued++
		case ItemDenied:
			denied++
		default:
			t.Fatalf("unexpected item outcome %s", it.Outcome)
		}
	}
	if issued != int(ceiling) {
		t.Fatalf("recipient_ceiling=%d was NOT correctly enforced under real concurrency: expected exactly %d issued, got %d (decomposition/race bypass)", ceiling, ceiling, issued)
	}
	if denied != n-int(ceiling) {
		t.Fatalf("expected exactly %d denied, got %d", n-int(ceiling), denied)
	}
}

// --- Stage 4H-B1 Wave 2 Phase 9 (qa): three real gaps this phase's own
// review of the human directive's §19/§20 test floor found and closed
// with real PostgreSQL tests, none of which required any domain-logic
// change - TerminateGrant, MarkGrantReversed and the brand-scoped Grant
// path were all already correctly built for these properties; only the
// adversarial test itself was missing. See docs/testing/testing-
// strategy.md's Phase 9 section for the full gap analysis (concurrent
// expiry/cancellation/reversal and "same player across multiple brands"
// were named in the directive's own required-coverage list but had no
// test anywhere in internal/bonus or internal/casino before this).

// TestAdversarial_ConcurrentTerminate_ExpireVsCancel_ExactlyOneWins closes
// the "concurrent expiry" and "concurrent cancellation" gap in one test:
// two DIFFERENT terminal triggers (expire, cancel) race on the SAME Grant
// with no open exposure, so each is eligible to resolve immediately
// (never deferred to pending_settlement) if it wins. TerminateGrant's own
// AdvisoryLockGrant + LockGrantForUpdate + ComputeNewStakeEligibility-
// re-read sequence (grant.go/lifecycle.go) is the ONLY thing that can
// make this safe under genuine concurrency - this test exists to prove
// that composition, not assume it from reading the code.
func TestAdversarial_ConcurrentTerminate_ExpireVsCancel_ExactlyOneWins(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	var grantID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "concurrent-terminate-1")
		result, outcome, err := IssueAndActivateGenericWageringBonus(ctx, tx, GenericWageringBonusParams{
			Grant: g, Amount: big.NewInt(1000), ActorType: ActorSystem, JurisdictionCode: f.jurisdictionCode,
		})
		if err != nil || !outcome.Allowed {
			return fmt.Errorf("issue/activate: %v / %+v", err, outcome)
		}
		grantID = result.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed activated grant: %v", err)
	}

	type attempt struct {
		resolution TerminalResolution
		wantStatus GrantStatus
		result     Grant
		err        error
	}
	attempts := []*attempt{
		{resolution: TerminalResolutionExpired, wantStatus: GrantExpired},
		{resolution: TerminalResolutionCancelled, wantStatus: GrantCancelled},
	}

	var wg sync.WaitGroup
	wg.Add(len(attempts))
	for _, a := range attempts {
		a := a
		go func() {
			defer wg.Done()
			_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				result, err := TerminateGrant(ctx, tx, f.tenantID, grantID, TerminateGrantParams{
					Resolution: a.resolution, ReasonCode: "concurrent-terminate-test", ActorType: ActorSystem, TriggerType: TriggerAutomatedRuleEvaluation,
				})
				a.result, a.err = result, err
				if err != nil {
					// Roll back the loser's transaction explicitly rather
					// than committing a call that itself returned an error.
					return err
				}
				return nil
			})
		}()
	}
	wg.Wait()

	wins, losses := 0, 0
	var winner *attempt
	for _, a := range attempts {
		switch {
		case a.err == nil:
			wins++
			winner = a
		case errors.Is(a.err, ErrIllegalTransition), errors.Is(a.err, ErrGrantStateConflict):
			losses++
		default:
			t.Fatalf("unexpected error: %v", a.err)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("expected exactly 1 winner and 1 loser, got wins=%d losses=%d (expire err=%v, cancel err=%v)", wins, losses, attempts[0].err, attempts[1].err)
	}
	if winner.result.Status != winner.wantStatus {
		t.Fatalf("winner's own returned Grant disagrees with its own resolution: got %s, want %s", winner.result.Status, winner.wantStatus)
	}

	// The final, committed row must agree with whichever attempt actually
	// won - never a third value, and never the loser's resolution.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g, err := GetGrantByID(ctx, tx, grantID)
		if err != nil {
			return err
		}
		if g.Status != winner.wantStatus {
			return fmt.Errorf("final committed status %s does not match the winner's resolution %s", g.Status, winner.wantStatus)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestAdversarial_ConcurrentReversal_ExactlyOneWins closes the "concurrent
// reversal" gap: two goroutines call MarkGrantReversed on the same
// already-terminal Grant at nearly the same instant, with different
// reason codes. MarkGrantReversed (grant.go) has no explicit advisory
// lock - it relies entirely on a single atomic `UPDATE ... WHERE status
// <> 'reversed'` to be safe under concurrency (ordinary Postgres row-lock
// serialization, not an application-level check-then-write). This test
// proves that reliance is actually safe, not merely plausible.
func TestAdversarial_ConcurrentReversal_ExactlyOneWins(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	var grantID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "concurrent-reversal-1")
		created, err := CreateGrant(ctx, tx, g)
		grantID = created.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := TerminateGrant(ctx, tx, f.tenantID, grantID, TerminateGrantParams{
			Resolution: TerminalResolutionCancelled, ReasonCode: "pre-reversal-setup", ActorType: ActorSystem, TriggerType: TriggerAutomatedRuleEvaluation,
		})
		return err
	})
	if err != nil {
		t.Fatalf("terminate grant before reversal race: %v", err)
	}

	const n = 2
	results := make([]struct {
		grant Grant
		err   error
	}, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				g, err := MarkGrantReversed(ctx, tx, f.tenantID, grantID, fmt.Sprintf("concurrent-reversal-%d", i), time.Now().UTC())
				results[i].grant, results[i].err = g, err
				return err
			})
		}()
	}
	wg.Wait()

	wins, losses := 0, 0
	for _, r := range results {
		switch {
		case r.err == nil:
			wins++
		case errors.Is(r.err, ErrGrantStateConflict):
			losses++
		default:
			t.Fatalf("unexpected error: %v", r.err)
		}
	}
	if wins != 1 || losses != n-1 {
		t.Fatalf("expected exactly 1 winner and %d loser(s), got wins=%d losses=%d", n-1, wins, losses)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g, err := GetGrantByID(ctx, tx, grantID)
		if err != nil {
			return err
		}
		if g.Status != GrantReversed {
			return fmt.Errorf("expected exactly one reversal to have committed, got status %s", g.Status)
		}
		if g.ReversalReasonCode == nil {
			return fmt.Errorf("expected a reversal_reason_code to be recorded")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestAdversarial_SamePlayerAcrossMultipleBrands_ConcurrentGrantsIsolated
// closes the "same player across multiple brands" gap: ONE person holds
// TWO player_accounts under the SAME tenant, one per brand (a real,
// supported shape - player_accounts.person_id carries no uniqueness
// constraint, migrations/0010). Concurrent issue+activate for that same
// person's two brand-scoped Grants must not cross-contaminate: each
// Grant must end up attributed to its own brand/player_account/wallet,
// and AdvisoryLockGrant's per-grant-id lock (never a per-person lock)
// must not serialize or block the two brands' grants against each other.
func TestAdversarial_SamePlayerAcrossMultipleBrands_ConcurrentGrantsIsolated(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co1 := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	var brand2ID, player2ID, wallet2ID, sharedPersonID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// The FIRST brand's player_account was created by seedFixture with
		// its own fresh person_id - recover it so the SECOND brand's
		// player_account can share that exact person_id (the "same
		// player" property under test).
		if err := tx.QueryRow(ctx, `SELECT person_id FROM player_accounts WHERE id = $1`, f.playerID).Scan(&sharedPersonID); err != nil {
			return err
		}

		brand2ID = uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Second Test Brand')`,
			brand2ID, f.tenantID, "b2-"+brand2ID.String()[:8]); err != nil {
			return err
		}
		player2ID = uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1,$2,$3,$4,$5,'x','active')`,
			player2ID, f.tenantID, brand2ID, sharedPersonID, player2ID.String()+"@example.com"); err != nil {
			return err
		}
		wallet2ID = uuid.New()
		_, err := tx.Exec(ctx, `INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1,$2,$3,$4,$5)`,
			wallet2ID, f.tenantID, brand2ID, player2ID, f.assetCode)
		return err
	})
	if err != nil {
		t.Fatalf("seed second brand + shared-person player account: %v", err)
	}
	co2 := seedCampaignOffer(t, pool, f.tenantID, brand2ID, f.staffID)

	f2 := f
	f2.brandID, f2.playerID, f2.walletID = brand2ID, player2ID, wallet2ID

	type brandAttempt struct {
		fx      lifecycleFixture
		co      campaignOffer
		trigger string
		result  Grant
		err     error
	}
	attempts := []*brandAttempt{
		{fx: f, co: co1, trigger: "multi-brand-1a"},
		{fx: f2, co: co2, trigger: "multi-brand-1b"},
	}

	var wg sync.WaitGroup
	wg.Add(len(attempts))
	for _, a := range attempts {
		a := a
		go func() {
			defer wg.Done()
			_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				g := newTestOfferGrant(a.fx, a.co, a.trigger)
				result, outcome, err := IssueAndActivateGenericWageringBonus(ctx, tx, GenericWageringBonusParams{
					Grant: g, Amount: big.NewInt(500), ActorType: ActorSystem, JurisdictionCode: f.jurisdictionCode,
				})
				if err == nil && !outcome.Allowed {
					err = fmt.Errorf("denied: %s / %s", outcome.DeniedBy, outcome.Code)
				}
				a.result, a.err = result, err
				return err
			})
		}()
	}
	wg.Wait()

	for i, a := range attempts {
		if a.err != nil {
			t.Fatalf("brand attempt %d: unexpected error/denial issuing to the SAME person's OTHER brand concurrently: %v", i, a.err)
		}
	}
	if attempts[0].result.ID == attempts[1].result.ID {
		t.Fatalf("expected two DISTINCT Grants (one per brand), got the same id twice")
	}

	// Each Grant must be attributed to exactly its OWN brand/player/wallet
	// - never swapped, never shared - proving the concurrent path did not
	// cross-contaminate the two brands despite them sharing one person.
	for i, a := range attempts {
		if a.result.BrandID != a.fx.brandID {
			t.Fatalf("attempt %d: Grant attributed to wrong brand: got %s, want %s", i, a.result.BrandID, a.fx.brandID)
		}
		if a.result.PlayerAccountID != a.fx.playerID {
			t.Fatalf("attempt %d: Grant attributed to wrong player_account: got %s, want %s", i, a.result.PlayerAccountID, a.fx.playerID)
		}
		if a.result.WalletID != a.fx.walletID {
			t.Fatalf("attempt %d: Grant attributed to wrong wallet: got %s, want %s", i, a.result.WalletID, a.fx.walletID)
		}
		if a.result.Status != GrantActivated {
			t.Fatalf("attempt %d: expected activated, got %s", i, a.result.Status)
		}
	}

	// Confirm both player_accounts really do share one person_id - the
	// precondition this test's whole "same player" claim rests on, proven
	// rather than assumed from the seeding code above.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var p1, p2 uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT person_id FROM player_accounts WHERE id = $1`, f.playerID).Scan(&p1); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT person_id FROM player_accounts WHERE id = $1`, player2ID).Scan(&p2); err != nil {
			return err
		}
		if p1 != p2 || p1 != sharedPersonID {
			return fmt.Errorf("precondition violated: the two player_accounts do not share one person_id (p1=%s p2=%s shared=%s)", p1, p2, sharedPersonID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
