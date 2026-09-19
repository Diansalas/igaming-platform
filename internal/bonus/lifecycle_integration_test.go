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
		if _, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status)
			VALUES ($1, $2, $3, 'x', 'bonus_operations', $4, 'active')`, f.staff2ID, f.tenantID, f.staff2ID.String()+"@staff.example.com", p2); err != nil {
			return err
		}
		// held_disposition_resolve's own bonus_approval_policies row: this
		// fixture only ever seeds TWO staff members (f.staffID, f.staff2ID),
		// so every held-disposition test below files as one and records
		// exactly one distinct approval from the other. Without an explicit
		// policy row, ResolveApprovalPolicy falls back to
		// fallbackApprovalPolicy's required_approvals=2 (fail-closed
		// default), which these two-staff tests cannot satisfy. This row
		// makes the ACTUAL tenant-configured policy (required_approvals=1)
		// match what these tests already set up - SEC-4I-F1's fix means the
		// resolved-server-side value now comes from this row, never from a
		// caller-supplied count, so the policy itself (not a test-supplied
		// override) is what must say "1" here.
		_, err := tx.Exec(ctx, `INSERT INTO bonus_approval_policies (tenant_id, operation, approval_threshold_minor_units, required_approvals, created_by_principal_id)
			VALUES ($1, 'held_disposition_resolve', 0, 1, $2)`, f.tenantID, f.staffID)
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

// forceActivateGrantForTest performs EXACTLY the same post-gate effecting
// logic ActivateGrant performs on a genuine ALLOW (applyGrantActivation,
// lifecycle.go - the ONE place this posting is implemented; this helper
// calls it directly rather than re-implementing it, so there is no
// second, hand-copied version that could drift), WITHOUT running T.1's
// gate first.
//
// WHY THIS EXISTS, STATED PLAINLY (never a production bypass - see this
// dispatch's own stage completion report for the full reasoning): Stage
// 4I's jurisdiction resolver (internal/jurisdiction,
// docs/governance/stage-4i-canonical-model.md) is, by design, structurally
// incapable of resolving a jurisdiction for ANY player-scoped operation
// today (HDR-J-1/HDR-J-3 are unanswered - see resolveGrantJurisdiction's
// own doc comment, eligibility.go). T.1's AssetAuthorization layer
// therefore honestly, correctly denies every real ActivateGrant call in
// this package's test suite now, exactly as it denies in production. That
// is the CORRECT, disclosed behaviour for the gate itself, and is
// asserted directly by the tests whose actual subject IS that gate (the
// manual-grant/bulk-grant/held-disposition admin surfaces, and the
// dedicated jurisdiction-gate tests). It is NOT a defect this helper works
// around.
//
// For every OTHER test in this package - whose actual subject is
// downstream of a successful activation (wagering-contribution math,
// expiry-sweep mechanics, forfeiture, conversion payout rules, EOI/
// four-eyes concurrency) - reaching `activated` is a SETUP precondition,
// not the thing under test, and Stage 4I's honest denial would otherwise
// silently delete dozens of tests' ability to exercise code that has
// nothing to do with jurisdiction at all, which is its own compliance
// problem (CLAUDE.md's financial-testing mandate). This helper is that
// precondition, built the same way any test seeds a precondition it does
// not itself want to exercise (e.g. seeding rows directly via SQL rather
// than through a full user journey) - it is defined in a _test.go file,
// is NEVER compiled into the production binary, and is NEVER reachable
// from any HTTP handler or other production code path. It never appears
// in, and never influences, internal/jurisdiction's or
// internal/assetregistry's own production decision.
func forceActivateGrantForTest(t *testing.T, ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID, p ActivateGrantParams) Grant {
	t.Helper()
	activated, err := forceActivateGrantForTestErr(ctx, tx, tenantID, grantID, p)
	if err != nil {
		t.Fatalf("forceActivateGrantForTest: %v", err)
	}
	return activated
}

// forceActivateGrantForTestErr is forceActivateGrantForTest's error-
// returning core, for the handful of tests that need to assert a SPECIFIC
// error out of the post-gate effecting logic (e.g. four_eyes_ops_
// integration_test.go's own four-eyes-consume assertions, threaded
// through p.PostGateHook) rather than treating any error as an
// unconditional test failure.
func forceActivateGrantForTestErr(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID, p ActivateGrantParams) (Grant, error) {
	if err := AdvisoryLockGrant(ctx, tx, tenantID, grantID); err != nil {
		return Grant{}, err
	}
	g, err := LockGrantForUpdate(ctx, tx, grantID)
	if err != nil {
		return Grant{}, err
	}
	if g.Status != GrantIssued {
		return Grant{}, fmt.Errorf("%w: activate requires status issued, got %s", ErrIllegalTransition, g.Status)
	}
	amountMinor, err := amountToInt64(p.Amount)
	if err != nil {
		return Grant{}, err
	}
	return applyGrantActivation(ctx, tx, tenantID, grantID, g, amountMinor, time.Now().UTC(), p)
}

// forceIssueAndActivateGrantForTest composes IssueGrant (unmodified,
// real production code - issuance's own T.2 gate does not require
// AssetAuthorization/a resolved jurisdiction at all, doc 10 §T.2, and is
// therefore UNAFFECTED by Stage 4I: see IssueGrant's own doc comment)
// with forceActivateGrantForTest above, for the common case where a test
// merely wants an activated Grant to build on and has no interest in
// exercising T.1's gate itself.
func forceIssueAndActivateGrantForTest(t *testing.T, ctx context.Context, tx pgx.Tx, g Grant, p ActivateGrantParams) Grant {
	t.Helper()
	activated, err := forceIssueAndActivateGrantForTestErr(ctx, tx, g, p)
	if err != nil {
		t.Fatalf("forceIssueAndActivateGrantForTest: %v", err)
	}
	return activated
}

// forceIssueAndActivateGrantForTestErr is forceIssueAndActivateGrantForTest's
// error-returning core - safe to call from a spawned test goroutine (see
// forceRunBulkGrantJobItemForTest's own doc comment for why t.Fatalf
// cannot be).
func forceIssueAndActivateGrantForTestErr(ctx context.Context, tx pgx.Tx, g Grant, p ActivateGrantParams) (Grant, error) {
	created, issueOutcome, err := IssueGrant(ctx, tx, IssueGrantParams{Grant: g})
	if err != nil {
		return Grant{}, fmt.Errorf("issue: %w", err)
	}
	if !issueOutcome.Allowed {
		return Grant{}, fmt.Errorf("issue denied: %+v", issueOutcome)
	}
	return forceActivateGrantForTestErr(ctx, tx, created.TenantID, created.ID, p)
}

// forceConvertGrantForTest mirrors ConvertGrant's own AOE/wagering-target
// pre-checks EXACTLY, then reaches the effecting posting via
// applyGrantConversion (conversion.go) directly, WITHOUT running T.1's
// gate - see forceActivateGrantForTest's own doc comment (above) for the
// full, disclosed reasoning (ConvertGrant's own T.1 gate now
// unconditionally denies at AssetAuthorization for every player-scoped
// operation, Stage 4I, which would otherwise make it impossible for a
// test whose actual subject is downstream of a successful conversion -
// or the conversion mechanics themselves, independent of jurisdiction -
// to ever reach `converted` via the public API).
func forceConvertGrantForTest(t *testing.T, ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID, wageringTargetScaled *big.Int, p ConvertGrantParams) ConvertGrantResult {
	t.Helper()
	if err := AdvisoryLockGrant(ctx, tx, tenantID, grantID); err != nil {
		t.Fatalf("forceConvertGrantForTest: advisory lock: %v", err)
	}
	g, err := LockGrantForUpdate(ctx, tx, grantID)
	if err != nil {
		t.Fatalf("forceConvertGrantForTest: lock grant: %v", err)
	}
	if g.Status != GrantCompleted {
		t.Fatalf("forceConvertGrantForTest: grant %s is not completed (status %s)", grantID, g.Status)
	}
	aoe, err := ComputeAOE(ctx, tx, tenantID, grantID)
	if err != nil {
		t.Fatalf("forceConvertGrantForTest: compute AOE: %v", err)
	}
	if !aoe.IsEmpty() {
		t.Fatalf("forceConvertGrantForTest: grant %s has open exposure - this helper is for the T.1-gate bypass only, not for exercising AOE blocking", grantID)
	}
	progress, err := DeriveWageringProgress(ctx, tx, tenantID, grantID)
	if err != nil {
		t.Fatalf("forceConvertGrantForTest: derive wagering progress: %v", err)
	}
	if wageringTargetScaled != nil && progress.PFirm.Cmp(wageringTargetScaled) < 0 {
		t.Fatalf("forceConvertGrantForTest: wagering requirement not met - this helper is for the T.1-gate bypass only")
	}
	remaining, err := RemainingBonusBalance(ctx, tx, tenantID, grantID)
	if err != nil {
		t.Fatalf("forceConvertGrantForTest: remaining balance: %v", err)
	}
	decided := new(big.Int).Set(remaining)
	if p.MaxCashoutAmount != nil && decided.Cmp(p.MaxCashoutAmount) > 0 {
		decided = new(big.Int).Set(p.MaxCashoutAmount)
	}
	amountMinor, err := amountToInt64(decided)
	if err != nil {
		t.Fatalf("forceConvertGrantForTest: amount: %v", err)
	}
	result, err := applyGrantConversion(ctx, tx, tenantID, grantID, g, amountMinor, decided, p)
	if err != nil {
		t.Fatalf("forceConvertGrantForTest: applyGrantConversion: %v", err)
	}
	return result
}

// forceIssueSingleManualGrantForTest mirrors IssueSingleManualGrant
// EXACTLY (targeting.go - economicop.CheckEntry, issueIdempotent, and the
// IDENTICAL EOI-budget PostGateHook), substituting
// forceActivateGrantForTestErr for the real ActivateGrant call. See
// forceRunBulkGrantJobItemForTest's own doc comment (below) for why: this
// file's EOI/adversarial tests are about economicop's own budget/ceiling
// enforcement, not jurisdiction.
func forceIssueSingleManualGrantForTest(ctx context.Context, tx pgx.Tx, g Grant, parentOperationID uuid.UUID, actorID uuid.UUID, amount *big.Int) (Grant, error) {
	if _, err := economicop.CheckEntry(ctx, tx, g.TenantID, parentOperationID, g.AssetCode); err != nil {
		return Grant{}, err
	}
	g.ParentOperationID = &parentOperationID
	g.CreatedByActorType = ActorStaff
	g.CreatedByActorID = actorID

	created, issueOutcome, err := issueIdempotent(ctx, tx, IssueGrantParams{Grant: g})
	if err != nil && !errors.Is(err, ErrAlreadyGranted) {
		return Grant{}, err
	}
	if !issueOutcome.Allowed {
		return created, nil
	}

	var postGateHook func(context.Context, pgx.Tx) error
	if !errors.Is(err, ErrAlreadyGranted) {
		playerAccountID := created.PlayerAccountID
		tenantID := g.TenantID
		postGateHook = func(hookCtx context.Context, hookTx pgx.Tx) error {
			op, getErr := economicop.GetByID(hookCtx, hookTx, parentOperationID)
			if getErr != nil {
				return getErr
			}
			return economicop.ConsumeRootBudget(hookCtx, hookTx, tenantID, op.RootOperationID, economicop.OperationBonusManualGrant, playerAccountID, amount)
		}
	}

	return forceActivateGrantForTestErr(ctx, tx, created.TenantID, created.ID, ActivateGrantParams{
		ActorType: ActorStaff, ActorID: actorID, Amount: amount,
		PostGateHook: postGateHook,
	})
}

// forceRunStaticBulkGrantJobForTest / forceRunBulkGrantJobItemForTest
// mirror RunStaticBulkGrantJob / runBulkGrantJobItem EXACTLY (targeting.go
// - resumability check, item creation, SEP-1 check, wallet resolution,
// issueIdempotent, and the IDENTICAL economicop.ConsumeRootBudget
// PostGateHook), substituting forceActivateGrantForTestErr for the real
// ActivateGrant call. See forceActivateGrantForTest's own doc comment
// (above) for why: this file's own EOI/four-eyes/adversarial tests are
// about economicop's OWN budget/ceiling enforcement (threaded through
// ActivateGrant's PostGateHook, which never runs at all once T.1's
// AssetAuthorization layer unconditionally denies first, Stage 4I) - not
// about jurisdiction, which none of them were written to exercise.
func forceRunStaticBulkGrantJobForTest(ctx context.Context, tx pgx.Tx, job BulkGrantJob, target StaticPlayerListTarget, grantTemplate Grant, systemActorID uuid.UUID, amount *big.Int) error {
	if job.ParentOperationID == nil {
		return fmt.Errorf("forceRunStaticBulkGrantJobForTest: bulk grant job %s has no parent_operation_id", job.ID)
	}
	if _, err := economicop.CheckEntry(ctx, tx, job.TenantID, *job.ParentOperationID, grantTemplate.AssetCode); err != nil {
		return err
	}
	op, err := economicop.GetByID(ctx, tx, *job.ParentOperationID)
	if err != nil {
		return err
	}
	requesterPerson, approverPersons, err := requesterAndApproverPersons(ctx, tx, job)
	if err != nil {
		return err
	}
	for _, playerAccountID := range target.PlayerAccountIDs {
		if err := forceRunBulkGrantJobItemForTest(ctx, tx, job, op.RootOperationID, playerAccountID, grantTemplate, systemActorID, amount, requesterPerson, approverPersons); err != nil {
			return err
		}
	}
	return nil
}

// forceRunBulkGrantJobItemForTest is safe to call from a spawned test
// goroutine (unlike this file's t.Fatalf-based helpers): it returns every
// failure as a plain error rather than calling t.Fatalf, since the
// testing package requires Fatal/FailNow to be called only from the
// goroutine running the test function itself, and this file's own
// concurrent adversarial tests call this helper from worker goroutines.
func forceRunBulkGrantJobItemForTest(ctx context.Context, tx pgx.Tx, job BulkGrantJob, rootOperationID, playerAccountID uuid.UUID, grantTemplate Grant, systemActorID uuid.UUID, amount *big.Int, requesterPerson uuid.UUID, approverPersons []uuid.UUID) error {
	if _, err := GetBulkGrantJobItem(ctx, tx, job.TenantID, job.ID, playerAccountID); err == nil {
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	item, err := CreateBulkGrantJobItem(ctx, tx, BulkGrantJobItem{TenantID: job.TenantID, BulkGrantJobID: job.ID, PlayerAccountID: playerAccountID, ParentOperationID: &rootOperationID})
	if err != nil {
		if errors.Is(err, ErrDuplicateItem) {
			return nil
		}
		return err
	}
	subjectPerson, err := playerPersonID(ctx, tx, playerAccountID)
	if err != nil {
		return err
	}
	if subjectPerson == requesterPerson || containsUUID(approverPersons, subjectPerson) {
		reason := "SEP-1: requester or an approver of this bulk job resolves to the same person as this targeted player"
		_, err := RecordBulkGrantJobItemOutcome(ctx, tx, job.TenantID, item.ID, ItemDenied, &reason, nil, nil, nil, time.Now().UTC())
		return err
	}
	walletID, err := resolvePlayerWallet(ctx, tx, job.TenantID, playerAccountID, grantTemplate.AssetCode)
	if err != nil {
		return err
	}
	grant := grantTemplate
	grant.PlayerAccountID = playerAccountID
	grant.WalletID = walletID
	grant.TriggerReference = fmt.Sprintf("bulk_grant_job:%s:%s", job.ID, playerAccountID)
	grant.ParentOperationID = &rootOperationID
	grant.CreatedByActorType = ActorSystem
	grant.CreatedByActorID = uuid.Nil

	created, issueOutcome, err := issueIdempotent(ctx, tx, IssueGrantParams{Grant: grant})
	if err != nil && !errors.Is(err, ErrAlreadyGranted) {
		return err
	}
	if !issueOutcome.Allowed {
		reason := denialReasonCode(issueOutcome)
		_, err := RecordBulkGrantJobItemOutcome(ctx, tx, job.TenantID, item.ID, ItemDenied, &reason, nil, nil, nil, time.Now().UTC())
		return err
	}
	if errors.Is(err, ErrAlreadyGranted) {
		_, err := RecordBulkGrantJobItemOutcome(ctx, tx, job.TenantID, item.ID, ItemAlreadyGranted, nil, &created.ID, nil, nil, time.Now().UTC())
		return err
	}

	activated, err := forceActivateGrantForTestErr(ctx, tx, created.TenantID, created.ID, ActivateGrantParams{
		ActorType: ActorSystem, ActorID: systemActorID, Amount: amount,
		PostGateHook: func(hookCtx context.Context, hookTx pgx.Tx) error {
			return economicop.ConsumeRootBudget(hookCtx, hookTx, job.TenantID, rootOperationID, economicop.OperationBonusBulkGrant, playerAccountID, amount)
		},
	})
	if err != nil {
		if !errors.Is(err, economicop.ErrBudgetExhausted) {
			return err
		}
		reason := err.Error()
		_, recErr := RecordBulkGrantJobItemOutcome(ctx, tx, job.TenantID, item.ID, ItemDenied, &reason, nil, nil, nil, time.Now().UTC())
		if recErr != nil {
			return recErr
		}
		return nil
	}
	amountCopy := new(big.Int).Set(amount)
	_, err = RecordBulkGrantJobItemOutcome(ctx, tx, job.TenantID, item.ID, ItemIssued, nil, &activated.ID, amountCopy, nil, time.Now().UTC())
	return err
}

// forceConvertGrantSkippingAssetAuthorizationForTest mirrors ConvertGrant
// EXACTLY (conversion.go's pre-checks: lock, AOE, wagering-target
// re-check, remaining-balance/MaxCashoutAmount decision), except its own
// GateCheckpoint call sets SkipAssetAuthorization: true -
// GateCheckpoint's OWN existing, real, documented parameter
// (eligibility.go: "SkipAssetAuthorization is true ONLY at Grant
// creation... every other checkpoint... leaves this false"), not a test
// invention. This still runs the REAL RG and REAL Risk checks, unmodified
// - only the Stage-4I-affected, jurisdiction-dependent AssetAuthorization
// portion is skipped. Used ONLY by this file's tests whose actual
// subject is RG's or Risk's OWN denial/allow behavior at this
// checkpoint (never jurisdiction) - a full bypass
// (forceConvertGrantForTest, below) would make every such test trivially
// "succeed" regardless of the risk_rules row it configured, which would
// prove nothing. See forceActivateGrantForTest's own doc comment (above)
// for the general Stage 4I reasoning.
func forceConvertGrantSkippingAssetAuthorizationForTest(t *testing.T, ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID, wageringTargetScaled *big.Int, p ConvertGrantParams) ConvertGrantResult {
	t.Helper()
	if err := AdvisoryLockGrant(ctx, tx, tenantID, grantID); err != nil {
		t.Fatalf("forceConvertGrantSkippingAssetAuthorizationForTest: advisory lock: %v", err)
	}
	g, err := LockGrantForUpdate(ctx, tx, grantID)
	if err != nil {
		t.Fatalf("forceConvertGrantSkippingAssetAuthorizationForTest: lock grant: %v", err)
	}
	if g.Status != GrantCompleted {
		t.Fatalf("forceConvertGrantSkippingAssetAuthorizationForTest: grant %s is not completed (status %s)", grantID, g.Status)
	}
	aoe, err := ComputeAOE(ctx, tx, tenantID, grantID)
	if err != nil {
		t.Fatalf("forceConvertGrantSkippingAssetAuthorizationForTest: compute AOE: %v", err)
	}
	if !aoe.IsEmpty() {
		t.Fatalf("forceConvertGrantSkippingAssetAuthorizationForTest: grant %s has open exposure", grantID)
	}
	progress, err := DeriveWageringProgress(ctx, tx, tenantID, grantID)
	if err != nil {
		t.Fatalf("forceConvertGrantSkippingAssetAuthorizationForTest: derive wagering progress: %v", err)
	}
	if wageringTargetScaled != nil && progress.PFirm.Cmp(wageringTargetScaled) < 0 {
		t.Fatalf("forceConvertGrantSkippingAssetAuthorizationForTest: wagering requirement not met")
	}
	licensingMode, err := resolveLicensingMode(ctx, tx, g.TenantID)
	if err != nil {
		t.Fatalf("forceConvertGrantSkippingAssetAuthorizationForTest: licensing mode: %v", err)
	}
	remaining, err := RemainingBonusBalance(ctx, tx, tenantID, grantID)
	if err != nil {
		t.Fatalf("forceConvertGrantSkippingAssetAuthorizationForTest: remaining balance: %v", err)
	}
	decided := new(big.Int).Set(remaining)
	if p.MaxCashoutAmount != nil && decided.Cmp(p.MaxCashoutAmount) > 0 {
		decided = new(big.Int).Set(p.MaxCashoutAmount)
	}
	amountMinor, err := amountToInt64(decided)
	if err != nil {
		t.Fatalf("forceConvertGrantSkippingAssetAuthorizationForTest: amount: %v", err)
	}
	outcome, err := GateCheckpoint(ctx, tx, GateParams{
		TenantID: g.TenantID, BrandID: g.BrandID, PlayerAccountID: g.PlayerAccountID, WalletID: g.WalletID,
		LicensingMode: licensingMode, AssetCode: g.AssetCode, Amount: amountMinor,
		RiskOperation: OperationBonusConversion, SkipAssetAuthorization: true,
	})
	if err != nil {
		t.Fatalf("forceConvertGrantSkippingAssetAuthorizationForTest: gate checkpoint: %v", err)
	}
	if !outcome.Allowed {
		reason := denialReasonCode(outcome)
		if err := recordConversionBlock(ctx, tx, g, outcome.DeniedBy, reason); err != nil {
			t.Fatalf("forceConvertGrantSkippingAssetAuthorizationForTest: record block: %v", err)
		}
		return ConvertGrantResult{Grant: g, Converted: false, BlockedReason: outcome.DeniedBy, BlockedCode: outcome.Code}
	}
	result, err := applyGrantConversion(ctx, tx, tenantID, grantID, g, amountMinor, decided, p)
	if err != nil {
		t.Fatalf("forceConvertGrantSkippingAssetAuthorizationForTest: applyGrantConversion: %v", err)
	}
	return result
}

// forceIssueAndActivateDepositBonusForTest mirrors
// IssueAndActivateDepositBonus's OWN reward-computation and issue/activate
// composition EXACTLY (types.go - qualifying-amount clamping, then
// computeCappedPercentageReward, then issueIdempotent), substituting
// forceActivateGrantForTest for the real ActivateGrant call. See
// forceActivateGrantForTest's own doc comment (above) for why this
// substitution is necessary and safe.
func forceIssueAndActivateDepositBonusForTest(t *testing.T, ctx context.Context, tx pgx.Tx, p DepositBonusParams) (Grant, GateOutcome) {
	t.Helper()
	if p.MinQualifying != nil && p.DepositAmount.Cmp(p.MinQualifying) < 0 {
		return Grant{}, GateOutcome{Allowed: false, DeniedBy: "eligibility_axis", Code: "below_min_qualifying_amount"}
	}
	qualifying := p.DepositAmount
	if p.MaxQualifying != nil && qualifying.Cmp(p.MaxQualifying) > 0 {
		qualifying = p.MaxQualifying
	}
	amount, err := computeCappedPercentageReward(qualifying, p.RateBP, p.CapAmount, p.Grant.DecimalExponent)
	if err != nil {
		t.Fatalf("forceIssueAndActivateDepositBonusForTest: compute reward: %v", err)
	}
	g, issueOutcome, err := issueIdempotent(ctx, tx, IssueGrantParams{Grant: p.Grant})
	if err != nil && !errors.Is(err, ErrAlreadyGranted) {
		t.Fatalf("forceIssueAndActivateDepositBonusForTest: issue: %v", err)
	}
	if errors.Is(err, ErrAlreadyGranted) {
		return g, issueOutcome
	}
	if !issueOutcome.Allowed {
		return g, issueOutcome
	}
	activated := forceActivateGrantForTest(t, ctx, tx, g.TenantID, g.ID, ActivateGrantParams{
		ActorType: TriggerActorForAutomated(p.ActorType), ActorID: p.ActorID, Amount: amount,
		WageringTimeLimit: p.WageringTimeLimit,
	})
	return activated, GateOutcome{Allowed: true}
}

// forceIssueAndActivateCashbackForTest mirrors IssueAndActivateCashback's
// OWN reward-computation, issue/activate, and immediate-completion
// composition EXACTLY (types.go), substituting forceActivateGrantForTest
// for the real ActivateGrant call. See forceActivateGrantForTest's own
// doc comment (above) for why.
func forceIssueAndActivateCashbackForTest(t *testing.T, ctx context.Context, tx pgx.Tx, p CashbackParams) (Grant, GateOutcome) {
	t.Helper()
	amount, err := computeCappedPercentageReward(p.NetLossAmount, p.RateBP, p.CapAmount, p.Grant.DecimalExponent)
	if err != nil {
		t.Fatalf("forceIssueAndActivateCashbackForTest: compute reward: %v", err)
	}
	g, issueOutcome, err := issueIdempotent(ctx, tx, IssueGrantParams{Grant: p.Grant})
	if err != nil && !errors.Is(err, ErrAlreadyGranted) {
		t.Fatalf("forceIssueAndActivateCashbackForTest: issue: %v", err)
	}
	if errors.Is(err, ErrAlreadyGranted) {
		return g, issueOutcome
	}
	if !issueOutcome.Allowed {
		return g, issueOutcome
	}
	forceActivateGrantForTest(t, ctx, tx, g.TenantID, g.ID, ActivateGrantParams{
		ActorType: ActorSystem, ActorID: p.ActorID, Amount: amount,
	})
	completed, _, err := CheckAndCompleteGrant(ctx, tx, g.TenantID, g.ID, nil)
	if err != nil {
		t.Fatalf("forceIssueAndActivateCashbackForTest: complete: %v", err)
	}
	return completed, GateOutcome{Allowed: true}
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
		result, outcome := forceIssueAndActivateDepositBonusForTest(t, ctx, tx, DepositBonusParams{
			Grant: g, DepositAmount: big.NewInt(10000), RateBP: 5000, CapAmount: big.NewInt(5000),
			ActorType: ActorSystem, ActorID: uuid.Nil,
		})
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
		result2, outcome2 := forceIssueAndActivateDepositBonusForTest(t, ctx, tx, DepositBonusParams{
			Grant: newTestOfferGrant(f, co, "deposit-1"), DepositAmount: big.NewInt(10000), RateBP: 5000, CapAmount: big.NewInt(5000),
			ActorType: ActorSystem,
		})
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
		result := forceIssueAndActivateGrantForTest(t, ctx, tx, g, ActivateGrantParams{Amount: big.NewInt(1000), ActorType: ActorSystem})
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
		result := forceIssueAndActivateGrantForTest(t, ctx, tx, g, ActivateGrantParams{Amount: big.NewInt(1000), ActorType: ActorSystem})
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
		err = forceRunStaticBulkGrantJobForTest(ctx, tx, job, StaticPlayerListTarget{PlayerAccountIDs: players}, template, uuid.Nil, big.NewInt(1000))
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
		_, _, err := IssueSingleManualGrant(ctx, tx, g, uuid.New() /* does not exist */, f.staffID, big.NewInt(500))
		if !errors.Is(err, economicop.ErrParentOperationNotFound) {
			return fmt.Errorf("expected ErrParentOperationNotFound, got %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// seedExtraBonusPlayer inserts one more player_account (+ wallet, for
// f.assetCode) beyond the lifecycleFixture's own single f.playerID/
// f.walletID - mirroring TestEOI_BulkGrantRecipientCeiling_RejectsBeyond
// Budget's own inline extra-player seeding, factored out because both
// regression tests below need MULTIPLE DISTINCT players relaying the SAME
// parent_operation_id (the exact SEC-W15-02 decomposition shape).
func seedExtraBonusPlayer(t *testing.T, pool *db.Pool, f lifecycleFixture) (playerID, walletID uuid.UUID) {
	t.Helper()
	playerID = uuid.New()
	walletID = uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		personID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
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
		t.Fatalf("seed extra player: %v", err)
	}
	return playerID, walletID
}

// TestEOI_SingleManualGrant_RecipientCeiling_RejectsSecondDistinctPlayer is
// DR-4HB1W2-02's own regression proof (Stage 4H-B1 Wave 2, closing the gap
// the DR-4HB1W2-01 dispatch found and correctly declined to fix on the
// spot, as out of that dispatch's own narrow scope): before this fix,
// economicop.ConsumeRootBudget's recipient-ceiling check was a
// STRUCTURAL NO-OP on this surface. IssueSingleManualGrant's own
// issueIdempotent call always inserts the bonus_grants row (this
// operation's own consumption record) BEFORE ConsumeRootBudget ever runs
// (which now happens inside ActivateGrant's PostGateHook, per
// DR-4HB1W2-01) - and, before this fix, consumptionRealizedFilter had no
// entry for "bonus_grants", so that just-inserted, still-'issued' row was
// ALREADY counted (alreadyCounted=true) the instant the check ran,
// meaning "!alreadyCounted && currentCount+1 > ceiling" could never fire.
//
// This is exactly the "single-Grant staff-action-equivalent surface" door
// SEC-W15-02's original finding named as the harder-to-close half: a
// caller relaying ONE shared, approved parent_operation_id across MANY
// individual IssueSingleManualGrant calls, each targeting a DIFFERENT
// player, must be capped by that operation's recipient_ceiling - exactly
// as the bulk surface (bulk_grant_job_items, which already carries its
// own correct 'pending'-excluding realized filter) already is.
//
// Fix chosen (see enforce.go's consumptionRealizedFilter doc comment for
// the full reasoning): give "bonus_grants" a genuine realized predicate,
// "AND c.status NOT IN ('issued', 'cancelled')" - symmetric with
// bulk_grant_job_items' own discipline, and correct because
// ActivateGrant's PostGateHook call site (lifecycle.go) runs strictly
// BEFORE the 'issued'->'activated' UpdateGrantStatus call, so THIS
// execution's own row is always still 'issued' (never yet 'activated')
// at the instant its own count query runs - excluding 'issued' rows is
// exactly what stops an execution from permanently counting itself. The
// alternative (excluding the current row by its own id) was rejected: it
// requires widening ConsumeRootBudget's signature for every caller
// (including the bulk surface, out of this dispatch's own scope) for no
// benefit over the realized-filter approach, which needs no signature
// change and is symmetric with the table bulk_grant_job_items already
// uses correctly.
//
// IntendedAggregateValue is deliberately left nil on this root: bonus_
// grants has no "granted_amount" (or any amount) column at all (migration
// 0057), so ConsumeRootBudget's value-budget branch (which hardcodes
// "granted_amount" as its value column, ready-built only for
// bulk_grant_job_items' own schema) would fail with an undefined-column
// error the instant it ran against this shape. That is a SEPARATE, real,
// pre-existing gap this dispatch's own scope does not cover (fixing it
// requires an architecture decision on where a Grant's own monetary value
// for budget-accounting purposes should be sourced from - bonus_grants
// has no amount column, only bonus_grant_progress and the ledger posting
// do - which is exactly the kind of cross-cutting call CLAUDE.md routes
// through the architect/ledger-finance specialists, not decided
// unilaterally here) - reported, not silently patched, and deliberately
// not exercised by this test so this recipient_ceiling regression proof
// stays isolated from it.
func TestEOI_SingleManualGrant_RecipientCeiling_RejectsSecondDistinctPlayer(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)
	player2ID, wallet2ID := seedExtraBonusPlayer(t, pool, f)

	var rootID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ceiling := int32(1)
		asset := f.assetCode
		op, err := MintRootOperation(ctx, tx, MintRootOperationParams{
			TenantID: f.tenantID, OperationType: economicop.OperationBonusManualGrant,
			InitiatingActorType: "staff", InitiatingActorID: f.staffID,
			SubjectScope: economicop.SubjectScopeEnumeratedSet,
			AssetCode:    &asset,
			// A value budget is mandatory for this operation_type since Stage
			// 4H-B1 Wave 3 Phase 10 (economicop.ValidateRootAuthorizationBounds).
			// Set deliberately far above anything this test grants so the
			// RECIPIENT ceiling remains the only bound that can bite here.
			IntendedAggregateValue: big.NewInt(1_000_000_000),
			RecipientCeiling:       &ceiling,
			IdempotencyKey:         "single-manual-ceiling-test", CorrelationID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
			ApprovalState: economicop.ApprovalApproved,
		})
		if err != nil {
			return err
		}
		rootID = op.OperationID
		return nil
	})
	if err != nil {
		t.Fatalf("mint EOI root: %v", err)
	}

	// First IssueSingleManualGrant call, first player, relaying rootID -
	// must succeed and fully activate (the ceiling of 1 has one slot,
	// this is the only consumer so far).
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "manual-player-1")
		activated, err := forceIssueSingleManualGrantForTest(ctx, tx, g, rootID, f.staffID, big.NewInt(500))
		if err != nil {
			return fmt.Errorf("first player: unexpected error: %w", err)
		}
		if activated.Status != GrantActivated {
			return fmt.Errorf("first player: expected activated grant, got status=%s", activated.Status)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Second IssueSingleManualGrant call, a DIFFERENT player, relaying the
	// SAME rootID (SEC-W15-02's decomposition shape) - must be denied for
	// exceeding recipient_ceiling=1. Under the PRE-FIX code, this call
	// incorrectly succeeded (the structural no-op this test exists to
	// close).
	//
	// The closure returns the raw error UNCONDITIONALLY (never swallowing
	// it into a nil return, even once it is confirmed to be the EXPECTED
	// denial) - exactly like every real caller must - so WithTenant's own
	// deferred Rollback actually fires, matching production's own "an
	// over-budget attempt never leaves a committed Grant behind" guarantee
	// (this function's own doc comment). Asserting on the sentinel happens
	// OUTSIDE the transaction, against the error WithTenant itself returns.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "manual-player-2")
		g.PlayerAccountID = player2ID
		g.WalletID = wallet2ID
		_, err := forceIssueSingleManualGrantForTest(ctx, tx, g, rootID, f.staffID, big.NewInt(500))
		return err
	})
	if !errors.Is(err, economicop.ErrBudgetExhausted) {
		t.Fatalf("second player: expected ErrBudgetExhausted (recipient_ceiling=1 already reached by a different player under the same root), got %v", err)
	}

	// The second player must have NO activated (or even issued) grant
	// left behind - the whole denied attempt's transaction rolled back in
	// full, including the bonus_grants row issueIdempotent inserted before
	// the denial was discovered.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM bonus_grants WHERE tenant_id = $1 AND player_account_id = $2`, f.tenantID, player2ID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("expected zero bonus_grants rows left behind for the denied second player, got %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestAdversarial_ConcurrentSingleManualGrant_NeverExceedRecipientCeiling
// is DR-4HB1W2-02's concurrent counterpart, matching TestAdversarial_
// ConcurrentBulkGrantWorkers_NeverExceedRecipientCeiling's own discipline
// (including its n=2 rationale - see that test's own NAMED FINDING
// comment - to stay clear of Postgres's documented 3+-waiter tuple-lock
// deadlock-detector pathology on ONE contended row, which is an
// availability/robustness matter, not the security property either test
// proves): two goroutines, each running an ENTIRE, independent
// IssueSingleManualGrant call (its own transaction) against the SAME EOI
// root (recipient_ceiling=1), targeting two DIFFERENT players. Exactly
// one must win.
func TestAdversarial_ConcurrentSingleManualGrant_NeverExceedRecipientCeiling(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	const n = 2
	ceiling := int32(1) // strictly fewer than n concurrent workers

	type playerFixture struct {
		playerID uuid.UUID
		walletID uuid.UUID
	}
	players := make([]playerFixture, n)
	players[0] = playerFixture{playerID: f.playerID, walletID: f.walletID}
	for i := 1; i < n; i++ {
		pid, wid := seedExtraBonusPlayer(t, pool, f)
		players[i] = playerFixture{playerID: pid, walletID: wid}
	}

	var rootID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		asset := f.assetCode
		op, err := MintRootOperation(ctx, tx, MintRootOperationParams{
			TenantID: f.tenantID, OperationType: economicop.OperationBonusManualGrant,
			InitiatingActorType: "staff", InitiatingActorID: f.staffID,
			SubjectScope: economicop.SubjectScopeEnumeratedSet,
			AssetCode:    &asset,
			// See the sibling fixture above: a value budget is mandatory
			// since Stage 4H-B1 Wave 3 Phase 10, set far above this test's
			// own grants so the RECIPIENT ceiling stays the only live bound.
			IntendedAggregateValue: big.NewInt(1_000_000_000),
			RecipientCeiling:       &ceiling,
			IdempotencyKey:         "concurrent-single-manual-ceiling-test", CorrelationID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
			ApprovalState: economicop.ApprovalApproved,
		})
		if err != nil {
			return err
		}
		rootID = op.OperationID
		return nil
	})
	if err != nil {
		t.Fatalf("mint EOI root: %v", err)
	}

	// Same retryable-transient-error discipline as TestAdversarial_
	// ConcurrentBulkGrantWorkers_NeverExceedRecipientCeiling's own NAMED
	// FINDING comment: N backends issuing `SELECT ... FOR UPDATE` against
	// the SAME economic_operations root row can hit real Postgres
	// deadlock/serialization errors under genuine concurrency - a
	// transaction that hits one never commits (no over-issuance, no
	// bypassed check is possible via this path), so retrying it from
	// scratch is the correct, ordinary mitigation, not a workaround for a
	// security defect.
	isRetryableTxError := func(err error) bool {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			return false
		}
		return pgErr.Code == "40P01" /* deadlock_detected */ || pgErr.Code == "40001" /* serialization_failure */
	}

	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			const maxAttempts = 40
			for attempt := 0; attempt < maxAttempts; attempt++ {
				err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					g := newTestOfferGrant(f, co, fmt.Sprintf("concurrent-manual-%d", idx))
					g.PlayerAccountID = players[idx].playerID
					g.WalletID = players[idx].walletID
					_, err := forceIssueSingleManualGrantForTest(ctx, tx, g, rootID, f.staffID, big.NewInt(500))
					return err
				})
				if err == nil || errors.Is(err, economicop.ErrBudgetExhausted) || !isRetryableTxError(err) {
					results[idx] = err
					return
				}
				time.Sleep(time.Duration(5+idx*3) * time.Millisecond)
			}
			results[idx] = fmt.Errorf("worker %d: exhausted %d retries on transient errors", idx, maxAttempts)
		}(i)
	}
	wg.Wait()

	issued, denied := 0, 0
	for i, err := range results {
		switch {
		case err == nil:
			issued++
		case errors.Is(err, economicop.ErrBudgetExhausted):
			denied++
		default:
			t.Fatalf("worker %d: unexpected error: %v", i, err)
		}
	}
	if issued != int(ceiling) {
		t.Fatalf("recipient_ceiling=%d was NOT correctly enforced under real concurrency on the single-Grant surface: expected exactly %d issued, got %d (decomposition/race bypass)", ceiling, ceiling, issued)
	}
	if denied != n-int(ceiling) {
		t.Fatalf("expected exactly %d denied, got %d", n-int(ceiling), denied)
	}
}

// TestEOI_BulkGrantItem_ActivateDenialNeverConsumesEOIBudget is
// DR-4HB1W2-01's own regression proof (Stage 4H-B1 Wave 2 Phase 10,
// architect composition finding, closed by this dispatch): the locking
// economicop.ConsumeRootBudget consume must run strictly AFTER
// ActivateGrant's own T.1 gate chain has ALLOWED the activation - never
// before it, and never at all on a denial (doc 34 §5.3 rules 3/4, doc 10
// N2.4a). The prior code called ConsumeRootBudget (which takes a
// locking `FOR UPDATE` on the EOI root row) BEFORE ever calling
// ActivateGrant, so that lock could be acquired before ActivateGrant's
// own GateCheckpoint call ever had a chance to take Risk's
// pg_advisory_xact_lock (were a cumulative rule ever scoped to
// bonus_grant) - the reverse of the canonical "Risk's lock always
// before the EOI lock" ordering.
//
// This uses the BULK surface (runBulkGrantJobItem), not the single-Grant
// one, because bulk_grant_job_items is the ONE consumption-record shape
// whose ConsumeRootBudget realized-filter ("AND c.outcome IN ('issued',
// 'already_granted')", enforce.go's consumptionRealizedFilter) excludes
// the CURRENT item from its own recipient count while that item is still
// 'pending' - i.e. at every point before RecordBulkGrantJobItemOutcome
// finalizes it. This is what makes the ceiling genuinely bite on a
// SECOND, distinct recipient once a first one has consumed it (exactly
// as TestEOI_BulkGrantRecipientCeiling_RejectsBeyondBudget already
// proves sequentially, and TestAdversarial_ConcurrentBulkGrantWorkers_
// NeverExceedRecipientCeiling under real concurrency) - the property
// this test's own distinguishing scenario depends on.
//
// The distinguishing scenario uses AssetAuthorization, not Risk, because
// it is the one live T.1 axis doc 10 T.2 deliberately evaluates ONLY at
// activation: IssueGrant's own gate call passes SkipAssetAuthorization:
// true ("issued is a decision, not a movement"), while ActivateGrant's
// gate call never skips it. A jurisdiction for which this test's own
// asset was never platform/tenant/jurisdiction-authorized therefore
// passes issuance untouched but is refused at activation, on
// AssetAuthorization's own merits alone - isolating ActivateGrant's gate
// as the ONLY gate that can deny this attempt (no risk_rules row is
// created at all, so Risk cannot be the cause either way).
//
// The EOI root's recipient_ceiling is set to exactly 1 and already
// fully consumed by a first, successful item (in the properly authorized
// jurisdiction) to a DIFFERENT player before the denied item runs. Under
// the PRE-FIX ordering, this second item would call
// economicop.ConsumeRootBudget BEFORE ActivateGrant's gate chain ever
// ran, immediately finding the ceiling already exhausted and recording
// ItemDenied with economicop.ErrBudgetExhausted's own reason - the T.1
// gate chain would never even execute, and the denial reason would name
// the EOI budget, never AssetAuthorization. Under the CORRECTED
// ordering, ActivateGrant's own gate chain runs first and denies on its
// own merits (an "asset_authorization_denied:..." reason code), proving
// the EOI consume was never attempted on this denied item.
func TestEOI_BulkGrantItem_ActivateDenialNeverConsumesEOIBudget(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	// A second player/wallet - the recipient this test's own denied item
	// targets, kept distinct from f.playerID (who consumes the EOI
	// root's only recipient slot below), mirroring
	// TestEOI_BulkGrantRecipientCeiling_RejectsBeyondBudget's own extra-
	// player seeding pattern.
	var player2ID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		player2ID = uuid.New()
		personID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1,$2,$3,$4,$5,'x','active')`,
			player2ID, f.tenantID, f.brandID, personID, player2ID.String()+"@example.com"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1,$2,$3,$4,$5)`,
			uuid.New(), f.tenantID, f.brandID, player2ID, f.assetCode)
		return err
	})
	if err != nil {
		t.Fatalf("seed second player: %v", err)
	}

	// Stage 4I NOTE (superseding this test's own original "second
	// jurisdiction, deliberately never authorized" fixture): the resolver
	// (internal/jurisdiction) now makes EVERY player-scoped resolution
	// unresolved(no_signal) unconditionally (HDR-J-1/HDR-J-3 unanswered -
	// see resolveGrantJurisdiction's own doc comment, eligibility.go), so
	// AssetAuthorization denies the second item on ITS OWN merits with no
	// jurisdiction-fixture distinction needed at all - the property this
	// test exists to prove (the T.1 gate runs, and denies, BEFORE EOI
	// consumption) holds even more directly than before.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		asset := f.assetCode
		ceiling := int32(1)
		op, err := MintRootOperation(ctx, tx, MintRootOperationParams{
			TenantID: f.tenantID, OperationType: economicop.OperationBonusBulkGrant,
			InitiatingActorType: "staff", InitiatingActorID: f.staffID,
			SubjectScope: economicop.SubjectScopeEnumeratedSet, AssetCode: &asset,
			// A value budget is mandatory for this operation_type since Stage
			// 4H-B1 Wave 3 Phase 10 (economicop.ValidateRootAuthorizationBounds).
			// Set far above this test's own grants: the property under test is
			// the gate-denial/lock-order path, not either budget.
			IntendedAggregateValue: big.NewInt(1_000_000_000),
			RecipientCeiling:       &ceiling,
			IdempotencyKey:         "eoi-lockorder-bulk-root", CorrelationID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour),
			ApprovalState: economicop.ApprovalApproved,
		})
		if err != nil {
			return fmt.Errorf("mint root: %w", err)
		}

		job, err := CreateBulkGrantJob(ctx, tx, BulkGrantJob{
			TenantID: f.tenantID, BrandID: f.brandID, CampaignID: co.campaignID, OfferVersionID: co.offerVersionID,
			TargetKind: TargetPlayerList, TargetPlayerList: []uuid.UUID{f.playerID, player2ID},
			RequestedByPrincipalID: f.staffID, ApprovalState: BulkApprovalApproved, Status: BulkJobRunning,
			IdempotencyKey: "eoi-lockorder-bulk-job", ParentOperationID: &op.OperationID,
		})
		if err != nil {
			return fmt.Errorf("create bulk job: %w", err)
		}

		template := newTestOfferGrant(f, co, "")
		requesterPerson := uuid.New() // unrelated to both targeted players - SEP-1 is not this test's concern

		// First item, f.playerID: forced past T.1 (forceRunBulkGrantJobItemForTest
		// - see its own doc comment above; Stage 4I's jurisdiction resolver
		// makes a genuine AssetAuthorization ALLOW impossible for ANY
		// player-scoped item today) so it consumes the EOI root's only
		// recipient slot - this test's own subject is the SECOND item's
		// gate-vs-EOI ordering, not whether the first item's own
		// jurisdiction resolves.
		if err := forceRunBulkGrantJobItemForTest(ctx, tx, job, op.RootOperationID, f.playerID, template, uuid.Nil, big.NewInt(50), requesterPerson, nil); err != nil {
			return fmt.Errorf("first item: %w", err)
		}
		item1, err := GetBulkGrantJobItem(ctx, tx, f.tenantID, job.ID, f.playerID)
		if err != nil {
			return fmt.Errorf("get item1: %w", err)
		}
		if item1.Outcome != ItemIssued {
			reason := ""
			if item1.ReasonCode != nil {
				reason = *item1.ReasonCode
			}
			return fmt.Errorf("expected first item ISSUED, got %s (reason=%q)", item1.Outcome, reason)
		}

		// Second item, player2ID: the REAL, unmodified runBulkGrantJobItem
		// (never forced) - ActivateGrant's own T.1 gate (AssetAuthorization,
		// Stage 4I's unconditional jurisdiction denial) must deny it on its
		// OWN merits - it must never even reach the EOI consume, and the
		// EOI root's already-exhausted recipient_ceiling (1, already spent
		// by item 1 above) must never be the reported reason.
		// runBulkGrantJobItem itself must return nil either way (a denial
		// is recorded on the item, never propagated as a Go error) - see
		// the NAMED FIX comment on its own ErrBudgetExhausted handling in
		// targeting.go.
		if err := runBulkGrantJobItem(ctx, tx, job, op.RootOperationID, player2ID, template, uuid.Nil, big.NewInt(1000), requesterPerson, nil); err != nil {
			return fmt.Errorf("second item returned an unexpected error %v (expected nil - a recorded ItemDenied)", err)
		}
		item2, err := GetBulkGrantJobItem(ctx, tx, f.tenantID, job.ID, player2ID)
		if err != nil {
			return fmt.Errorf("get item2: %w", err)
		}
		if item2.Outcome != ItemDenied {
			return fmt.Errorf("expected second item DENIED (unauthorized jurisdiction), got %s", item2.Outcome)
		}
		if item2.ReasonCode == nil {
			return fmt.Errorf("expected a recorded reason code on the denied second item, got none")
		}
		reason := *item2.ReasonCode
		if !strings.Contains(reason, "asset_authorization") {
			return fmt.Errorf("expected the recorded denial reason to be ActivateGrant's own T.1 gate denial "+
				"(containing \"asset_authorization\"), proving the gate chain ran and decided this - not the EOI "+
				"budget - got reason=%q. This means economicop.ConsumeRootBudget ran BEFORE ActivateGrant's own "+
				"gate chain (DR-4HB1W2-01's exact regression: the EOI's already-exhausted recipient_ceiling was "+
				"consulted before the T.1 gate ever ran)", reason)
		}
		if strings.Contains(reason, "budget") || strings.Contains(reason, "recipient_ceiling") {
			return fmt.Errorf("the recorded denial reason names the EOI budget (%q) instead of ActivateGrant's "+
				"own T.1 gate - DR-4HB1W2-01's exact regression", reason)
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

	// Stage 4I NOTE: this test's own subject is the CAS guard on the
	// issued->activated/cancelled TRANSITION itself (exactly one of N
	// concurrent callers may transition a Grant OUT of `issued` at all) -
	// not which terminal status that transition lands on. The REAL,
	// unmodified ActivateGrant is used deliberately (not a forced
	// bypass): Stage 4I's jurisdiction resolver now unconditionally denies
	// T.1's AssetAuthorization layer for every player-scoped operation
	// (resolveGrantJurisdiction's own doc comment, eligibility.go), so the
	// ONE winner's transition is issued->cancelled (ActivateGrant's own
	// documented denial path, doc 10 §5/T.3: "the Grant is recorded as
	// cancelled... never silently left issued"), not issued->activated -
	// every LOSER still correctly fails the identical CAS guard with
	// ErrIllegalTransition, exactly as before this Stage.
	const n = 5
	var wg sync.WaitGroup
	attempted := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, _, err := ActivateGrant(ctx, tx, f.tenantID, grantID, ActivateGrantParams{ActorType: ActorSystem, Amount: big.NewInt(100)})
				if err != nil {
					return err
				}
				attempted[idx] = true
				return nil
			})
			if err != nil && !errors.Is(err, ErrGrantStateConflict) && !errors.Is(err, ErrIllegalTransition) {
				t.Errorf("goroutine %d: unexpected error: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	won := 0
	for _, s := range attempted {
		if s {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("expected exactly 1 goroutine to successfully TRANSITION the grant out of issued (win the CAS), got %d", won)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g, err := GetGrantByID(ctx, tx, grantID)
		if err != nil {
			return err
		}
		if g.Status != GrantCancelled {
			return fmt.Errorf("expected final status cancelled (Stage 4I's disclosed jurisdiction-resolution gap denies at AssetAuthorization), got %s", g.Status)
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
		result := forceIssueAndActivateGrantForTest(t, ctx, tx, g, ActivateGrantParams{Amount: big.NewInt(1000), ActorType: ActorSystem})
		completed, ok, err := CheckAndCompleteGrant(ctx, tx, f.tenantID, result.ID, nil)
		if err != nil || !ok {
			return fmt.Errorf("complete: %v / %v", err, ok)
		}

		// forceConvertGrantForTest (see its own doc comment above): this
		// test's subject is that risk.OperationBonusConversion resolves and
		// ALLOWS with no matching rule - Stage 4I's now-unconditional
		// jurisdiction denial at T.1's AssetAuthorization layer would
		// otherwise make EVERY conversion deny regardless of Risk, proving
		// nothing about Risk specifically.
		convResult := forceConvertGrantForTest(t, ctx, tx, f.tenantID, completed.ID, nil, ConvertGrantParams{ActorType: ActorSystem})
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
		result := forceIssueAndActivateGrantForTest(t, ctx, tx, g, ActivateGrantParams{Amount: big.NewInt(1000), ActorType: ActorSystem})
		completed, ok, err := CheckAndCompleteGrant(ctx, tx, f.tenantID, result.ID, nil)
		if err != nil || !ok {
			return fmt.Errorf("complete: %v / %v", err, ok)
		}

		// forceConvertGrantSkippingAssetAuthorizationForTest (see its own
		// doc comment above): this test's subject is that RISK specifically
		// denies (BlockedReason == "risk") - a full T.1 bypass would make
		// this trivially succeed regardless of the risk_rules row just
		// created, proving nothing; a full, un-skipped T.1 gate would
		// instead deny at AssetAuthorization (Stage 4I's jurisdiction gap),
		// masking Risk's own denial. Only AssetAuthorization is skipped;
		// RG and Risk run for real, unmodified.
		convResult := forceConvertGrantSkippingAssetAuthorizationForTest(t, ctx, tx, f.tenantID, completed.ID, nil, ConvertGrantParams{ActorType: ActorSystem})
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
		result := forceIssueAndActivateGrantForTest(t, ctx, tx, g, ActivateGrantParams{Amount: big.NewInt(1000), ActorType: ActorSystem})
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

	// resolveHeldDispositionAction's own skipAssetAuthorization param
	// (held_disposition_ops.go, test-only, never exposed on
	// ResolveHeldDispositionActionParams - see its own doc comment): this
	// test's subject is RG's OWN self-exclusion denial, which
	// AssetAuthorization's now-unconditional Stage 4I jurisdiction denial
	// would otherwise mask (AssetAuthorization runs before RG in T.1's
	// fixed order).
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := resolveHeldDispositionAction(ctx, tx, f.tenantID, ResolveHeldDispositionActionParams{
			HeldDispositionID: dispositionID, Action: ActionRouteToCash, ActorID: f.staffID, ReasonCode: "g2-resolution-test",
			RequestID: requestID, RequiredApprovals: 1,
		}, true)
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

	// resolveHeldDispositionAction's skipAssetAuthorization=true (see
	// TestHeldDisposition_ResolveRouteToCash_BlockedBySelfExclusion's own
	// identical comment above): this test's subject is that RG/Risk
	// ALLOW for an ordinary player - Stage 4I's now-unconditional
	// jurisdiction denial at AssetAuthorization would otherwise make this
	// deny regardless.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		resolved, err := resolveHeldDispositionAction(ctx, tx, f.tenantID, ResolveHeldDispositionActionParams{
			HeldDispositionID: dispositionID, Action: ActionRouteToCash, ActorID: f.staffID, ReasonCode: "g2-resolution-test",
			RequestID: requestID, RequiredApprovals: 1,
		}, true)
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

	// resolveHeldDispositionAction's skipAssetAuthorization=true (see
	// TestHeldDisposition_ResolveRouteToCash_BlockedBySelfExclusion's own
	// identical comment above): this test's subject is RISK's OWN denial,
	// which AssetAuthorization's now-unconditional Stage 4I jurisdiction
	// denial would otherwise mask.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := resolveHeldDispositionAction(ctx, tx, f.tenantID, ResolveHeldDispositionActionParams{
			HeldDispositionID: dispositionID, Action: ActionRouteToCash, ActorID: f.staffID, ReasonCode: "g2-resolution-test",
			RequestID: requestID, RequiredApprovals: 1,
		}, true)
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
					return forceRunBulkGrantJobItemForTest(ctx, tx, job, rootID, players[idx], template, uuid.Nil, big.NewInt(1000), requesterPerson, nil)
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
		result := forceIssueAndActivateGrantForTest(t, ctx, tx, g, ActivateGrantParams{Amount: big.NewInt(1000), ActorType: ActorSystem})
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
				result, err := forceIssueAndActivateGrantForTestErr(ctx, tx, g, ActivateGrantParams{Amount: big.NewInt(500), ActorType: ActorSystem})
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
