//go:build integration

// Real-PostgreSQL behavioral tests for Stage 4H-B1 Wave 3 Phase 2's three
// schema changes (ledger-accounting-model.md §7.18; migrations
// 0068/0069/0070). Schema only, per this dispatch's own scope - no
// application/repository code is added for the two new watermark tables
// (that is Phase 3, bonus-engine's), so these tests exercise the schema
// directly with SQL, the same discipline bonus_integration_test.go
// already uses for RLS/constraint verification.
package bonus

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const pgCheckViolation = "23514"
const pgUniqueViolation = "23505"

// --- Migration 0068: bonus_ledger_sweep_watermarks ---

func TestBonusLedgerSweepWatermark_SchemaBehavior(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, "EUR")
	depositTxID := seedLedgerTransaction(t, pool, f.tenantID)

	// Insert the first watermark row for this tenant's deposit-sweep
	// consumer.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO bonus_ledger_sweep_watermarks (id, tenant_id, consumer_name, last_processed_ledger_transaction_id, last_processed_posted_at)
			 VALUES ($1, $2, 'bonus_deposit_sweep', $3, now())`,
			uuid.New(), f.tenantID, depositTxID)
		return err
	})
	if err != nil {
		t.Fatalf("insert first watermark row: %v", err)
	}

	// A second row for the SAME (tenant, consumer) violates the unique
	// constraint - the watermark is a single cursor per consumer, never a
	// history.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO bonus_ledger_sweep_watermarks (id, tenant_id, consumer_name, last_processed_ledger_transaction_id, last_processed_posted_at)
			 VALUES ($1, $2, 'bonus_deposit_sweep', $3, now())`,
			uuid.New(), f.tenantID, depositTxID)
		return err
	})
	if err == nil {
		t.Fatal("expected a second watermark row for the same (tenant, consumer) to be rejected")
	}
	assertPgErrorCode(t, err, pgUniqueViolation)

	// A DIFFERENT consumer_name for the same tenant is a distinct,
	// independent cursor - proving the per-(tenant, consumer) shape
	// actually generalizes, not just accepts one hardcoded value.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO bonus_ledger_sweep_watermarks (id, tenant_id, consumer_name, last_processed_ledger_transaction_id, last_processed_posted_at)
			 VALUES ($1, $2, 'some_future_consumer', $3, now())`,
			uuid.New(), f.tenantID, depositTxID)
		return err
	})
	if err != nil {
		t.Fatalf("a second, differently-named consumer's watermark row must be independent: %v", err)
	}

	// Both-or-neither: last_processed_ledger_transaction_id set without
	// last_processed_posted_at (or vice versa) is structurally illegal.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO bonus_ledger_sweep_watermarks (id, tenant_id, consumer_name, last_processed_ledger_transaction_id, last_processed_posted_at)
			 VALUES ($1, $2, 'partial_consumer', $3, NULL)`,
			uuid.New(), f.tenantID, depositTxID)
		return err
	})
	if err == nil {
		t.Fatal("expected a watermark row with only one of the paired columns set to be rejected")
	}
	assertPgErrorCode(t, err, pgCheckViolation)

	// Advancing the cursor (the entire point of this table) is an
	// ordinary, permitted UPDATE.
	newer := seedLedgerTransaction(t, pool, f.tenantID)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE bonus_ledger_sweep_watermarks SET last_processed_ledger_transaction_id = $1, last_processed_posted_at = now()
			 WHERE tenant_id = $2 AND consumer_name = 'bonus_deposit_sweep'`,
			newer, f.tenantID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			t.Errorf("expected exactly 1 row updated, got %d", ct.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("advance watermark cursor: %v", err)
	}

	// The row's own identity (tenant_id/consumer_name) is frozen - unlike
	// every other Bonus table's full append-only immutability, this
	// table's cursor columns must stay mutable, so this specifically
	// targets the identity trigger, not a blanket update-denial.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE bonus_ledger_sweep_watermarks SET consumer_name = 'renamed' WHERE tenant_id = $1 AND consumer_name = 'bonus_deposit_sweep'`,
			f.tenantID)
		return err
	})
	if err == nil {
		t.Fatal("expected changing consumer_name to be rejected by the immutable-identity trigger")
	}
	if !strings.Contains(err.Error(), "immutable after insert") {
		t.Fatalf("expected the immutable-identity trigger's message, got: %v", err)
	}

	// TRUNCATE is denied, mirroring every other Bonus table.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE bonus_ledger_sweep_watermarks`)
		return err
	})
	if err == nil {
		t.Fatal("expected TRUNCATE to be denied")
	}
}

func TestBonusLedgerSweepWatermark_CrossTenantIsolation(t *testing.T) {
	pool := testPool(t)
	fA := seedFixture(t, pool, "EUR")
	fB := seedFixture(t, pool, "EUR")
	txA := seedLedgerTransaction(t, pool, fA.tenantID)

	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO bonus_ledger_sweep_watermarks (id, tenant_id, consumer_name, last_processed_ledger_transaction_id, last_processed_posted_at)
			 VALUES ($1, $2, 'bonus_deposit_sweep', $3, now())`,
			uuid.New(), fA.tenantID, txA)
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant A watermark: %v", err)
	}

	// Tenant B must see nothing of tenant A's watermark row.
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM bonus_ledger_sweep_watermarks WHERE tenant_id = $1`, fA.tenantID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Errorf("expected tenant B to see 0 rows for tenant A, saw %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cross-tenant read check: %v", err)
	}

	// Tenant B's connection cannot forge a row claiming tenant A's tenant_id.
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO bonus_ledger_sweep_watermarks (id, tenant_id, consumer_name, last_processed_ledger_transaction_id, last_processed_posted_at)
			 VALUES ($1, $2, 'forged', $3, now())`,
			uuid.New(), fA.tenantID, txA)
		return err
	})
	if err == nil {
		t.Fatal("expected tenant B's connection to be unable to insert a row claiming tenant A's tenant_id")
	}
	assertPgErrorCode(t, err, pgRLSViolation)

	// A player-scoped connection must see nothing at all - staff/system
	// only, no player-facing purpose for a sweep cursor.
	err = pool.WithPlayerScope(context.Background(), fA.tenantID, fA.playerID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM bonus_ledger_sweep_watermarks`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Errorf("expected a player-scoped connection to see 0 rows, saw %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("player-scope read check: %v", err)
	}
}

// --- Migration 0069: bonus_cashback_schedule_watermarks ---

func TestBonusCashbackScheduleWatermark_SchemaBehavior(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, "EUR")
	staffActorID := uuid.New()

	var campaignID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := CreateCampaign(ctx, tx, Campaign{TenantID: f.tenantID, BrandID: &f.brandID, CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		campaignID = c.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed campaign: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO bonus_cashback_schedule_watermarks (id, tenant_id, campaign_id, player_account_id, asset_code, last_processed_window_end)
			 VALUES ($1, $2, $3, $4, 'EUR', NULL)`,
			uuid.New(), f.tenantID, campaignID, f.playerID)
		return err
	})
	if err != nil {
		t.Fatalf("insert first cashback watermark row (no window processed yet): %v", err)
	}

	// Duplicate (tenant, campaign, player, asset) is rejected.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO bonus_cashback_schedule_watermarks (id, tenant_id, campaign_id, player_account_id, asset_code, last_processed_window_end)
			 VALUES ($1, $2, $3, $4, 'EUR', NULL)`,
			uuid.New(), f.tenantID, campaignID, f.playerID)
		return err
	})
	if err == nil {
		t.Fatal("expected a duplicate (tenant, campaign, player, asset) watermark row to be rejected")
	}
	assertPgErrorCode(t, err, pgUniqueViolation)

	// Advancing the cursor to the end of a just-processed window is an
	// ordinary, permitted UPDATE.
	windowEnd := time.Now().UTC().Add(-time.Hour)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE bonus_cashback_schedule_watermarks SET last_processed_window_end = $1
			 WHERE tenant_id = $2 AND campaign_id = $3 AND player_account_id = $4 AND asset_code = 'EUR'`,
			windowEnd, f.tenantID, campaignID, f.playerID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			t.Errorf("expected exactly 1 row updated, got %d", ct.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("advance cashback watermark cursor: %v", err)
	}

	// Identity fields (campaign_id/player_account_id/asset_code) are
	// frozen after insert - only the cursor itself may move.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE bonus_cashback_schedule_watermarks SET asset_code = 'USD'
			 WHERE tenant_id = $1 AND campaign_id = $2 AND player_account_id = $3`,
			f.tenantID, campaignID, f.playerID)
		return err
	})
	if err == nil {
		t.Fatal("expected changing asset_code to be rejected by the immutable-identity trigger")
	}
	if !strings.Contains(err.Error(), "immutable after insert") {
		t.Fatalf("expected the immutable-identity trigger's message, got: %v", err)
	}

	// A non-existent campaign_id is rejected by the FK, not silently
	// accepted.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO bonus_cashback_schedule_watermarks (id, tenant_id, campaign_id, player_account_id, asset_code)
			 VALUES ($1, $2, $3, $4, 'EUR')`,
			uuid.New(), f.tenantID, uuid.New(), f.playerID)
		return err
	})
	if err == nil {
		t.Fatal("expected a non-existent campaign_id to be rejected by the FK")
	}
	assertPgErrorCode(t, err, pgForeignKeyViolation)
}

func TestBonusCashbackScheduleWatermark_CrossTenantIsolation(t *testing.T) {
	pool := testPool(t)
	fA := seedFixture(t, pool, "EUR")
	fB := seedFixture(t, pool, "EUR")
	staffActorID := uuid.New()

	var campaignID uuid.UUID
	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := CreateCampaign(ctx, tx, Campaign{TenantID: fA.tenantID, BrandID: &fA.brandID, CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		campaignID = c.ID
		_, err = tx.Exec(ctx,
			`INSERT INTO bonus_cashback_schedule_watermarks (id, tenant_id, campaign_id, player_account_id, asset_code)
			 VALUES ($1, $2, $3, $4, 'EUR')`,
			uuid.New(), fA.tenantID, campaignID, fA.playerID)
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant A campaign + watermark: %v", err)
	}

	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM bonus_cashback_schedule_watermarks WHERE tenant_id = $1`, fA.tenantID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Errorf("expected tenant B to see 0 rows for tenant A, saw %d", count)
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO bonus_cashback_schedule_watermarks (id, tenant_id, campaign_id, player_account_id, asset_code)
			 VALUES ($1, $2, $3, $4, 'EUR')`,
			uuid.New(), fA.tenantID, campaignID, fB.playerID)
		return err
	})
	if err == nil {
		t.Fatal("expected tenant B's connection to be unable to insert a row claiming tenant A's tenant_id")
	}
	assertPgErrorCode(t, err, pgRLSViolation)
}

// --- Migration 0070: bonus_grants.expires_at ---

func TestBonusGrantsExpiresAt_WriteOnceImmutableAndBoundedByCreatedAt(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, "EUR")
	staffActorID := uuid.New()

	var grantID uuid.UUID
	var createdAt time.Time
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := CreateCampaign(ctx, tx, Campaign{TenantID: f.tenantID, BrandID: &f.brandID, CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		v, err := CreateCampaignVersion(ctx, tx, CampaignVersion{TenantID: f.tenantID, CampaignID: c.ID, VersionNumber: 1, Name: "V1", CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		o, err := CreateOffer(ctx, tx, Offer{TenantID: f.tenantID, BrandID: &f.brandID, CampaignID: c.ID, CampaignVersionID: v.ID, GrantPolicy: GrantPolicyAutoIssue, CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		ov, err := CreateOfferVersion(ctx, tx, OfferVersion{
			TenantID: f.tenantID, OfferID: o.ID, VersionNumber: 1, RewardKind: RewardFixedValue, RewardAssetCode: "EUR",
			FulfillmentDestination: FulfillmentIntoPlatformWallet, FundingSource: "operator",
			CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID,
		})
		if err != nil {
			return err
		}
		g, err := CreateGrant(ctx, tx, Grant{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, WalletID: f.walletID,
			CampaignID: c.ID, CampaignVersionID: v.ID, OfferID: o.ID, OfferVersionID: ov.ID,
			AssetCode: "EUR", DecimalExponent: 2,
			FundingSource: "operator", FulfillmentDestination: FulfillmentIntoPlatformWallet, FulfillmentOwner: "internal",
			TriggerReference: "expiry-test", CreatedByActorType: ActorSystem, CreatedByActorID: uuid.Nil,
		})
		if err != nil {
			return err
		}
		grantID = g.ID
		createdAt = g.CreatedAt
		if g.CreatedAt.IsZero() {
			if err := tx.QueryRow(ctx, `SELECT created_at FROM bonus_grants WHERE id = $1`, g.ID).Scan(&createdAt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// expires_at defaults to NULL - an existing Grant is unaffected by
	// this migration until something explicitly sets it.
	var isNull bool
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT expires_at IS NULL FROM bonus_grants WHERE id = $1`, grantID).Scan(&isNull)
	})
	if err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	if !isNull {
		t.Fatal("expected expires_at to default to NULL")
	}

	// A value at or before created_at is rejected.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE bonus_grants SET expires_at = $1 WHERE id = $2`, createdAt, grantID)
		return err
	})
	if err == nil {
		t.Fatal("expected setting expires_at <= created_at to be rejected")
	}
	assertPgErrorCode(t, err, pgCheckViolation)

	// Setting it once, to a value after created_at, succeeds - the one
	// legitimate write (mirrors granted_amount's own NULL -> value
	// transition, migration 0067).
	firstValue := createdAt.Add(30 * 24 * time.Hour)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `UPDATE bonus_grants SET expires_at = $1 WHERE id = $2`, firstValue, grantID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			t.Errorf("expected exactly 1 row updated, got %d", ct.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("first expires_at write: %v", err)
	}

	// A second attempt to change an already-set expires_at is rejected -
	// write-once, immutable, exactly like granted_amount.
	secondValue := firstValue.Add(24 * time.Hour)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE bonus_grants SET expires_at = $1 WHERE id = $2`, secondValue, grantID)
		return err
	})
	if err == nil {
		t.Fatal("expected changing an already-set expires_at to be rejected")
	}
	if !strings.Contains(err.Error(), "expires_at is immutable once set") {
		t.Fatalf("expected the expires_at immutability message, got: %v", err)
	}

	// Re-setting it to the SAME value it already holds is not a change
	// (IS DISTINCT FROM is false) and must not be rejected - mirrors
	// granted_amount's own idempotent-replay tolerance.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE bonus_grants SET expires_at = $1 WHERE id = $2`, firstValue, grantID)
		return err
	})
	if err != nil {
		t.Fatalf("re-setting expires_at to its own current value must be a no-op, not an error: %v", err)
	}

	// The pre-existing immutable-fields guard (migration 0057/0067) is
	// still intact - this migration only EXTENDED the trigger function,
	// it must not have dropped any prior check.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE bonus_grants SET asset_code = 'USD' WHERE id = $1`, grantID)
		return err
	})
	if err == nil {
		t.Fatal("expected the pre-existing asset_code immutability guard to still be enforced")
	}
	if !strings.Contains(err.Error(), "identity/scope/asset/funding") {
		t.Fatalf("expected the original immutable-fields message, got: %v", err)
	}
}

// --- CreateWageringProgress idempotency hardening (§7.18.3.5) ---

// TestBonusWageringProgress_ExistingUniqueConstraintAlreadySupportsOnConflict
// concretely proves ledger-accounting-model.md §7.18.3.5's finding: the
// UNIQUE (tenant_id, grant_id, lock_ledger_transaction_id) constraint
// migration 0058 already created is, by itself, sufficient for an
// ON CONFLICT DO NOTHING clause - no migration is needed to harden
// CreateWageringProgress's idempotency, only a Go-level change to the
// INSERT statement itself (Phase 3, bonus-engine, per the dispatch's own
// item 3 instruction). This test exercises the constraint directly via
// SQL, deliberately WITHOUT touching CreateWageringProgress's Go body -
// application-code change stays out of this schema-only phase.
func TestBonusWageringProgress_ExistingUniqueConstraintAlreadySupportsOnConflict(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, "EUR")
	lockTxID := seedLedgerTransaction(t, pool, f.tenantID)
	staffActorID := uuid.New()

	var grantID, offerVersionID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := CreateCampaign(ctx, tx, Campaign{TenantID: f.tenantID, BrandID: &f.brandID, CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		v, err := CreateCampaignVersion(ctx, tx, CampaignVersion{TenantID: f.tenantID, CampaignID: c.ID, VersionNumber: 1, Name: "V1", CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		o, err := CreateOffer(ctx, tx, Offer{TenantID: f.tenantID, BrandID: &f.brandID, CampaignID: c.ID, CampaignVersionID: v.ID, GrantPolicy: GrantPolicyAutoIssue, CreatedByActorType: ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
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
			CampaignID: c.ID, CampaignVersionID: v.ID, OfferID: o.ID, OfferVersionID: ov.ID,
			AssetCode: "EUR", DecimalExponent: 2,
			FundingSource: "operator", FulfillmentDestination: FulfillmentIntoPlatformWallet, FulfillmentOwner: "internal",
			TriggerReference: "wp-onconflict-test", CreatedByActorType: ActorSystem, CreatedByActorID: uuid.Nil,
		})
		if err != nil {
			return err
		}
		grantID = g.ID
		return nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	correlationID := uuid.New()
	insertOnConflict := `
		INSERT INTO bonus_wagering_progress (
			id, tenant_id, grant_id, player_account_id, offer_version_id, lock_ledger_transaction_id, correlation_id,
			asset_code, staked_bonus_amount, contribution_weight_bp, qualifying_scaled
		) VALUES ($1,$2,$3,$4,$5,$6,$7,'EUR',1000,10000,10000000)
		ON CONFLICT (tenant_id, grant_id, lock_ledger_transaction_id) DO NOTHING`

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, insertOnConflict,
			uuid.New(), f.tenantID, grantID, f.playerID, offerVersionID, lockTxID, correlationID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			t.Errorf("expected the first insert to affect 1 row, got %d", ct.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("first ON CONFLICT insert: %v", err)
	}

	// A second attempt for the IDENTICAL (tenant_id, grant_id,
	// lock_ledger_transaction_id) tuple, with a fresh row id (exactly
	// what a redelivered/retried caller would send), must be a graceful
	// no-op - zero rows affected, NOT an error - using ONLY the
	// already-existing migration 0058 constraint, no schema change.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, insertOnConflict,
			uuid.New(), f.tenantID, grantID, f.playerID, offerVersionID, lockTxID, correlationID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 0 {
			t.Errorf("expected the redelivered insert to affect 0 rows (idempotent no-op), got %d", ct.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("second (redelivered) ON CONFLICT insert: %v", err)
	}

	// Exactly one row exists for this (grant, lock transaction) pair.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM bonus_wagering_progress WHERE tenant_id = $1 AND grant_id = $2 AND lock_ledger_transaction_id = $3`,
			f.tenantID, grantID, lockTxID).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Errorf("expected exactly 1 wagering progress row after the redelivered insert, got %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify row count: %v", err)
	}
}
