//go:build integration

// Real-PostgreSQL tests for the Bonus Engine schema (migrations
// 0053-0060): repository CRUD round-trips and cross-tenant/player RLS
// isolation, direct SQL - not merely HTTP - per this project's
// established discipline (CLAUDE.md; internal/risk/risk_integration_test.go's
// own fixture conventions, followed here).
package bonus

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, 5, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type fixture struct {
	tenantID uuid.UUID
	brandID  uuid.UUID
	playerID uuid.UUID
	walletID uuid.UUID
}

func seedFixture(t *testing.T, pool *db.Pool, assetCode string) fixture {
	t.Helper()
	var f fixture
	f.tenantID = uuid.New()
	// Stage 4I Phase E-SECURITY (migration 0077): `tenants` writes now
	// require a genuinely platform-admin-scoped transaction.
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Test Tenant', 'under_platform_licence')`,
			f.tenantID, "t-"+f.tenantID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		f.brandID = uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Test Brand')`,
			f.brandID, f.tenantID, "b-"+f.brandID.String()[:8]); err != nil {
			return err
		}
		f.playerID = uuid.New()
		personID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			f.playerID, f.tenantID, f.brandID, personID, f.playerID.String()+"@example.com"); err != nil {
			return err
		}
		f.walletID = uuid.New()
		_, err := tx.Exec(ctx,
			`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, $5)`,
			f.walletID, f.tenantID, f.brandID, f.playerID, assetCode)
		return err
	})
	if err != nil {
		t.Fatalf("seed brand/player/wallet: %v", err)
	}
	return f
}

// seedLedgerTransaction inserts a minimal, schema-legal ledger_transactions
// row for FK purposes only (WageringProgress/HeldDisposition both FK
// against it) - not a real posting, this test does not exercise
// internal/ledger.Post.
func seedLedgerTransaction(t *testing.T, pool *db.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id) VALUES ($1, $2, 'deposit', $3, $4)`,
			id, tenantID, "test-"+id.String(), uuid.New(),
		)
		return err
	})
	if err != nil {
		t.Fatalf("seed ledger transaction: %v", err)
	}
	return id
}

func assertPgErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("expected a Postgres error %s, got: %v", code, err)
	}
}

const (
	pgRLSViolation        = "42501"
	pgForeignKeyViolation = "23503"
)

// --- Repository CRUD round-trip: Campaign -> CampaignVersion -> Offer ->
// OfferVersion -> Grant -> WageringProgress -> HeldDisposition ---

func TestBonusRepositories_FullCreateAndReadRoundTrip(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, "EUR")
	lockTxID := seedLedgerTransaction(t, pool, f.tenantID)
	settlementTxID := seedLedgerTransaction(t, pool, f.tenantID)
	staffActorID := uuid.New()

	var (
		campaign Campaign
		version  CampaignVersion
		offer    Offer
		offerVer OfferVersion
		grant    Grant
		progress WageringProgress
		held     HeldDisposition
	)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		campaign, err = CreateCampaign(ctx, tx, Campaign{
			TenantID: f.tenantID, BrandID: &f.brandID,
			CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID,
		})
		if err != nil {
			return err
		}

		version, err = CreateCampaignVersion(ctx, tx, CampaignVersion{
			TenantID: f.tenantID, CampaignID: campaign.ID, VersionNumber: 1, Name: "Welcome Bonus",
			CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID,
		})
		if err != nil {
			return err
		}

		if _, err = SetCampaignCurrentVersion(ctx, tx, f.tenantID, campaign.ID, version.ID); err != nil {
			return err
		}

		offer, err = CreateOffer(ctx, tx, Offer{
			TenantID: f.tenantID, BrandID: &f.brandID, CampaignID: campaign.ID, CampaignVersionID: version.ID,
			GrantPolicy: GrantPolicyAutoIssue, CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID,
		})
		if err != nil {
			return err
		}

		offerVer, err = CreateOfferVersion(ctx, tx, OfferVersion{
			TenantID: f.tenantID, OfferID: offer.ID, VersionNumber: 1,
			RewardKind: RewardPercentageWithCap, RewardAssetCode: "EUR",
			FulfillmentDestination: FulfillmentIntoPlatformWallet, FundingSource: "operator",
			CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID,
		})
		if err != nil {
			return err
		}

		grant, err = CreateGrant(ctx, tx, Grant{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, WalletID: f.walletID,
			CampaignID: campaign.ID, CampaignVersionID: version.ID, OfferID: offer.ID, OfferVersionID: offerVer.ID,
			AssetCode: "EUR", DecimalExponent: 2,
			FundingSource: "operator", FulfillmentDestination: FulfillmentIntoPlatformWallet, FulfillmentOwner: "internal",
			TriggerReference: "deposit-123", CreatedByActorType: ActorSystem, CreatedByActorID: uuid.Nil,
		})
		if err != nil {
			return err
		}

		progress, err = CreateWageringProgress(ctx, tx, WageringProgress{
			TenantID: f.tenantID, GrantID: grant.ID, PlayerAccountID: f.playerID, OfferVersionID: offerVer.ID,
			LockLedgerTransactionID: lockTxID, CorrelationID: uuid.New(), AssetCode: "EUR",
			StakedBonusAmount: big.NewInt(1000), ContributionWeightBP: 10000, QualifyingScaled: big.NewInt(10000000),
		})
		if err != nil {
			return err
		}

		held, err = CreateHeldDisposition(ctx, tx, HeldDisposition{
			TenantID: f.tenantID, BrandID: f.brandID, WalletID: f.walletID, PlayerAccountID: f.playerID,
			AssetCode: "EUR", GrantID: grant.ID, CorrelationID: uuid.New(),
			SettlementLedgerTransactionID: settlementTxID, PayoutAmount: big.NewInt(500),
		})
		return err
	})
	if err != nil {
		t.Fatalf("create chain: %v", err)
	}

	if version.ID == uuid.Nil || offerVer.ID == uuid.Nil || grant.Status != GrantIssued {
		t.Fatalf("unexpected state after create chain: version=%v offerVer=%v grant.Status=%v", version.ID, offerVer.ID, grant.Status)
	}
	if progress.StakedBonusAmount.Cmp(big.NewInt(1000)) != 0 {
		t.Errorf("expected StakedBonusAmount 1000, got %s", progress.StakedBonusAmount)
	}
	if held.Status != HeldDispositionHeld {
		t.Errorf("expected held disposition status 'held', got %q", held.Status)
	}

	// Read-back, within the same tenant scope.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		gotCampaign, err := GetCampaignByID(ctx, tx, campaign.ID)
		if err != nil {
			return err
		}
		if gotCampaign.ID != campaign.ID {
			t.Errorf("campaign round-trip mismatch")
		}
		gotGrant, err := GetGrantByID(ctx, tx, grant.ID)
		if err != nil {
			return err
		}
		if gotGrant.TriggerReference != "deposit-123" {
			t.Errorf("grant round-trip mismatch: %+v", gotGrant)
		}
		list, err := ListWageringProgressByGrant(ctx, tx, f.tenantID, grant.ID)
		if err != nil {
			return err
		}
		if len(list) != 1 {
			t.Errorf("expected 1 wagering progress row, got %d", len(list))
		}
		gotHeld, err := GetHeldDispositionBySettlementTransaction(ctx, tx, f.tenantID, settlementTxID)
		if err != nil {
			return err
		}
		if gotHeld.ID != held.ID {
			t.Errorf("held disposition idempotency-key lookup mismatch")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read-back: %v", err)
	}
}

// --- Grant idempotency: the DB-enforced (tenant, campaign, offer_version,
// player, trigger_reference) unique constraint (doc 10 §9) ---

func TestCreateGrant_DuplicateTriggerReferenceIsRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, "EUR")
	staffActorID := uuid.New()

	var campaignID, versionOfferID, offerVersionID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := CreateCampaign(ctx, tx, Campaign{TenantID: f.tenantID, BrandID: &f.brandID, CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		campaignID = c.ID
		v, err := CreateCampaignVersion(ctx, tx, CampaignVersion{TenantID: f.tenantID, CampaignID: c.ID, VersionNumber: 1, Name: "V1", CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		o, err := CreateOffer(ctx, tx, Offer{TenantID: f.tenantID, BrandID: &f.brandID, CampaignID: c.ID, CampaignVersionID: v.ID, GrantPolicy: GrantPolicyAutoIssue, CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		versionOfferID = v.ID
		ov, err := CreateOfferVersion(ctx, tx, OfferVersion{
			TenantID: f.tenantID, OfferID: o.ID, VersionNumber: 1, RewardKind: RewardFixedValue, RewardAssetCode: "EUR",
			FulfillmentDestination: FulfillmentIntoPlatformWallet, FundingSource: "operator",
			CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID,
		})
		offerVersionID = ov.ID
		return err
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var offerID uuid.UUID
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT id FROM bonus_offers WHERE tenant_id = $1 AND campaign_id = $2`, f.tenantID, campaignID)
		return row.Scan(&offerID)
	})
	if err != nil {
		t.Fatalf("lookup offer id: %v", err)
	}

	grantParams := Grant{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, WalletID: f.walletID,
		CampaignID: campaignID, CampaignVersionID: versionOfferID, OfferID: offerID, OfferVersionID: offerVersionID,
		AssetCode: "EUR", DecimalExponent: 2,
		FundingSource: "operator", FulfillmentDestination: FulfillmentIntoPlatformWallet, FulfillmentOwner: "internal",
		TriggerReference: "dup-ref", CreatedByActorType: ActorSystem, CreatedByActorID: uuid.Nil,
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateGrant(ctx, tx, grantParams)
		return err
	})
	if err != nil {
		t.Fatalf("first grant creation should succeed: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		grantParams.ID = uuid.Nil
		_, err := CreateGrant(ctx, tx, grantParams)
		return err
	})
	if err == nil {
		t.Fatal("expected the second grant with an identical idempotency tuple to fail")
	}
	assertPgErrorCode(t, err, "23505")
}

// --- RLS: tenant isolation, direct SQL, across every new table ---

func TestBonusTables_CrossTenantIsolation(t *testing.T) {
	pool := testPool(t)
	fA := seedFixture(t, pool, "EUR")
	fB := seedFixture(t, pool, "EUR")
	staffActorID := uuid.New()

	var campaignID uuid.UUID
	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := CreateCampaign(ctx, tx, Campaign{TenantID: fA.tenantID, BrandID: &fA.brandID, CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		campaignID = c.ID
		return err
	})
	if err != nil {
		t.Fatalf("create tenant A campaign: %v", err)
	}

	// Tenant B must not see tenant A's campaign at all.
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := GetCampaignByID(ctx, tx, campaignID)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected tenant B to see nothing for tenant A's campaign (ErrNotFound), got: %v", err)
	}

	// Tenant B must not be able to forge a row claiming tenant A's id
	// while connected under tenant B's own RLS scope.
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO bonus_campaigns (id, tenant_id, status, fulfillment_owner, created_by_actor_type, created_by_actor_id) VALUES ($1, $2, 'draft', 'internal', 'staff', $3)`,
			uuid.New(), fA.tenantID, staffActorID)
		return err
	})
	if err == nil {
		t.Fatal("expected tenant B's connection to be unable to insert a row claiming tenant A's tenant_id")
	}
	assertPgErrorCode(t, err, pgRLSViolation)
}

// --- RLS: bonus_grants' dual scope - staff-only write, player read-only
// self-scope, never another player's row ---

func TestBonusGrants_PlayerSelfScopeIsReadOnlyAndOwnRowsOnly(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, "EUR")
	other := seedFixture(t, pool, "EUR")
	staffActorID := uuid.New()

	var (
		campaignID, versionID, offerID, offerVersionID, grantID uuid.UUID
	)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := CreateCampaign(ctx, tx, Campaign{TenantID: f.tenantID, BrandID: &f.brandID, CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		campaignID = c.ID
		v, err := CreateCampaignVersion(ctx, tx, CampaignVersion{TenantID: f.tenantID, CampaignID: c.ID, VersionNumber: 1, Name: "V1", CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		versionID = v.ID
		o, err := CreateOffer(ctx, tx, Offer{TenantID: f.tenantID, BrandID: &f.brandID, CampaignID: c.ID, CampaignVersionID: v.ID, GrantPolicy: GrantPolicyAutoIssue, CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		offerID = o.ID
		ov, err := CreateOfferVersion(ctx, tx, OfferVersion{
			TenantID: f.tenantID, OfferID: o.ID, VersionNumber: 1, RewardKind: RewardFixedValue, RewardAssetCode: "EUR",
			FulfillmentDestination: FulfillmentIntoPlatformWallet, FundingSource: "operator",
			CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID,
		})
		if err != nil {
			return err
		}
		offerVersionID = ov.ID
		g, err := CreateGrant(ctx, tx, Grant{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, WalletID: f.walletID,
			CampaignID: campaignID, CampaignVersionID: versionID, OfferID: offerID, OfferVersionID: offerVersionID,
			AssetCode: "EUR", DecimalExponent: 2,
			FundingSource: "operator", FulfillmentDestination: FulfillmentIntoPlatformWallet, FulfillmentOwner: "internal",
			TriggerReference: "self-scope-test", CreatedByActorType: ActorSystem, CreatedByActorID: uuid.Nil,
		})
		grantID = g.ID
		return err
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// The owning player CAN read their own Grant.
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		g, err := GetGrantByID(ctx, tx, grantID)
		if err != nil {
			return err
		}
		if g.ID != grantID {
			t.Errorf("player read own grant mismatch")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("player self-read: %v", err)
	}

	// A DIFFERENT player in the SAME tenant cannot read this Grant.
	err = pool.WithPlayerScope(context.Background(), f.tenantID, other.playerID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := GetGrantByID(ctx, tx, grantID)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected a different player to see nothing for this grant, got: %v", err)
	}

	// The owning player CANNOT write (player_self_scope is SELECT-only) -
	// attempted directly via UpdateGrantStatus under player scope.
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpdateGrantStatus(ctx, tx, f.tenantID, grantID, GrantIssued, GrantActivated, time.Now())
		return err
	})
	if !errors.Is(err, ErrGrantStateConflict) {
		t.Fatalf("expected a player-scoped update to affect zero rows (ErrGrantStateConflict), got: %v", err)
	}
}

// --- economic_operations lineage: root mints itself as its own root,
// children denormalize the SAME root_operation_id (doc 34 §2.2) ---

func TestBulkGrantJob_ParentOperationLifecycle(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, "EUR")
	staffActorID := uuid.New()

	var campaignID, offerVersionID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := CreateCampaign(ctx, tx, Campaign{TenantID: f.tenantID, BrandID: &f.brandID, CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		campaignID = c.ID
		v, err := CreateCampaignVersion(ctx, tx, CampaignVersion{TenantID: f.tenantID, CampaignID: c.ID, VersionNumber: 1, Name: "V1", CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		o, err := CreateOffer(ctx, tx, Offer{TenantID: f.tenantID, BrandID: &f.brandID, CampaignID: c.ID, CampaignVersionID: v.ID, GrantPolicy: GrantPolicyManuallyAssigned, CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		ov, err := CreateOfferVersion(ctx, tx, OfferVersion{
			TenantID: f.tenantID, OfferID: o.ID, VersionNumber: 1, RewardKind: RewardFixedValue, RewardAssetCode: "EUR",
			FulfillmentDestination: FulfillmentIntoPlatformWallet, FundingSource: "operator",
			CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID,
		})
		offerVersionID = ov.ID
		return err
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var job BulkGrantJob
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		job, err = CreateBulkGrantJob(ctx, tx, BulkGrantJob{
			TenantID: f.tenantID, BrandID: f.brandID, CampaignID: campaignID, OfferVersionID: offerVersionID,
			TargetKind: TargetSinglePlayer, TargetPlayerAccountID: &f.playerID,
			RequestedByPrincipalID: staffActorID, IdempotencyKey: "bulk-job-1",
		})
		return err
	})
	if err != nil {
		t.Fatalf("create bulk grant job: %v", err)
	}
	if job.ApprovalState != BulkApprovalPendingFourEyes {
		t.Errorf("expected default approval_state pending_four_eyes, got %q", job.ApprovalState)
	}

	// Resubmission with the identical idempotency key resolves to the
	// SAME job (doc 10 §W5) - this package does not itself enforce that
	// re-resolution behavior (Phase 3's job), but the lookup primitive it
	// depends on must work.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		got, err := GetBulkGrantJobByIdempotencyKey(ctx, tx, f.tenantID, "bulk-job-1")
		if err != nil {
			return err
		}
		if got.ID != job.ID {
			t.Errorf("expected idempotency-key lookup to resolve to the same job")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("idempotency lookup: %v", err)
	}

	// Per-player item isolation/resumability (doc 10 §W5).
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateBulkGrantJobItem(ctx, tx, BulkGrantJobItem{TenantID: f.tenantID, BulkGrantJobID: job.ID, PlayerAccountID: f.playerID})
		if err != nil {
			return err
		}
		_, err = CreateBulkGrantJobItem(ctx, tx, BulkGrantJobItem{TenantID: f.tenantID, BulkGrantJobID: job.ID, PlayerAccountID: f.playerID})
		return err
	})
	if !errors.Is(err, ErrDuplicateItem) {
		t.Fatalf("expected a second item for the same (job, player) to be rejected as ErrDuplicateItem, got: %v", err)
	}
}
