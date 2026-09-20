//go:build integration

// Stage 7 §2 (pre-stage security fix): casino_launch_sessions had the
// identical brand-pinning weakness Stage 6.1 found and fixed in
// sportsbook_bets - the original (brand_id, tenant_id) -> brands FK pinned
// brand_id to "some brand in this tenant," never specifically to the
// launching player's own brand. Migration 0079 replaces it with a single
// composite (player_account_id, tenant_id, brand_id) -> player_accounts FK,
// reusing player_accounts_id_tenant_brand_key exactly as migration 0078
// did for sportsbook_bets. These tests prove the new constraint (and the
// surrounding RLS policy) actually closes the gap at the database layer,
// independent of the fact that the application never exercises it today
// (newLaunchCasinoGameHandler always derives BrandID server-side from
// account.BrandID - see casino_handlers.go).
package casino

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

// seedSiblingBrand creates a second person/brand/player/wallet inside an
// ALREADY-EXISTING tenant (created by an earlier seedCasinoFixture call),
// giving tests a second, genuinely distinct (brand, player, wallet) triple
// that shares the same tenant_id - the exact shape needed to prove
// "wrong brand in the same tenant" is rejected rather than merely
// "wrong tenant" (which the tenant_id column alone already prevented).
func seedSiblingBrand(t *testing.T, pool *db.Pool, tenantID uuid.UUID) casinoFixture {
	t.Helper()
	f := casinoFixture{tenantID: tenantID, brandID: uuid.New(), playerAccountID: uuid.New()}
	personID := uuid.New()

	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed sibling person: %v", err)
	}

	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Sibling Brand')`,
			f.brandID, tenantID, "b-"+f.brandID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			f.playerAccountID, tenantID, f.brandID, personID, f.playerAccountID.String()+"@example.com"); err != nil {
			return err
		}
		w, err := wallet.GetOrCreate(ctx, tx, tenantID, f.brandID, f.playerAccountID, "EUR")
		if err != nil {
			return err
		}
		f.walletID = w.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed sibling brand/player/wallet: %v", err)
	}
	return f
}

// TestCreateLaunchSession_CorrectPlayerTenantBrand_Succeeds is the control:
// a launch session whose (player_account_id, tenant_id, brand_id) triple
// genuinely matches the player's own row must succeed, proving the new
// constraint does not reject legitimate launches.
func TestCreateLaunchSession_CorrectPlayerTenantBrand_Succeeds(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := CreateLaunchSession(ctx, tx, CreateLaunchSessionParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, ProviderID: "mock-casino", ProviderGameID: game.ProviderGameID,
			AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if err != nil {
		t.Fatalf("expected correct player/tenant/brand triple to succeed, got: %v", err)
	}
}

// TestCreateLaunchSession_WrongBrandSameTenant_Rejected proves the actual
// gap migration 0079 closed: brand_id naming a DIFFERENT brand that is
// nonetheless valid "some brand in this tenant" must now be rejected by
// the database, because it no longer matches the player's own
// (id, tenant_id, brand_id) row.
func TestCreateLaunchSession_WrongBrandSameTenant_Rejected(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	sibling := seedSiblingBrand(t, pool, f.tenantID)
	game := seedGame(t, pool, "mock-casino", "EUR")

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := CreateLaunchSession(ctx, tx, CreateLaunchSessionParams{
			TenantID: f.tenantID, BrandID: sibling.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, ProviderID: "mock-casino", ProviderGameID: game.ProviderGameID,
			AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if !db.IsForeignKeyViolation(err) {
		t.Fatalf("expected a foreign key violation for wrong-brand-same-tenant, got: %v", err)
	}
}

// TestCreateLaunchSession_WrongTenant_Rejected proves a player_account_id
// belonging to a genuinely different tenant cannot be paired with this
// tenant_id, even though brand_id alone might coincidentally exist in
// both tenants' brand tables.
func TestCreateLaunchSession_WrongTenant_Rejected(t *testing.T) {
	pool := testPool(t)
	f1 := seedCasinoFixture(t, pool)
	f2 := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")

	err := pool.WithTenant(context.Background(), f1.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := CreateLaunchSession(ctx, tx, CreateLaunchSessionParams{
			TenantID: f1.tenantID, BrandID: f1.brandID, PlayerAccountID: f2.playerAccountID, WalletID: f1.walletID,
			GameID: game.ID, ProviderID: "mock-casino", ProviderGameID: game.ProviderGameID,
			AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if !db.IsForeignKeyViolation(err) {
		t.Fatalf("expected a foreign key violation for wrong-tenant player_account_id, got: %v", err)
	}
}

// TestCreateLaunchSession_ForgedPlayerAccountID_Rejected proves a
// player_account_id that names no row at all (a fully forged identifier,
// not merely one belonging to someone else) is rejected the same way.
func TestCreateLaunchSession_ForgedPlayerAccountID_Rejected(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := CreateLaunchSession(ctx, tx, CreateLaunchSessionParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: uuid.New(), WalletID: f.walletID,
			GameID: game.ID, ProviderID: "mock-casino", ProviderGameID: game.ProviderGameID,
			AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if !db.IsForeignKeyViolation(err) {
		t.Fatalf("expected a foreign key violation for a forged player_account_id, got: %v", err)
	}
}

// TestCreateLaunchSession_ForgedBrandID_Rejected proves a brand_id naming
// no brand anywhere (not even "some brand in this tenant") is rejected -
// the fully-forged-identifier counterpart to the wrong-brand test above.
func TestCreateLaunchSession_ForgedBrandID_Rejected(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := CreateLaunchSession(ctx, tx, CreateLaunchSessionParams{
			TenantID: f.tenantID, BrandID: uuid.New(), PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, ProviderID: "mock-casino", ProviderGameID: game.ProviderGameID,
			AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if !db.IsForeignKeyViolation(err) {
		t.Fatalf("expected a foreign key violation for a forged brand_id, got: %v", err)
	}
}

// TestCreateLaunchSession_CrossTenantAccess_RLSBlocked proves that even
// before the FK is ever consulted, casino_launch_sessions' own RLS policy
// rejects an attempt to write a row claiming a DIFFERENT tenant_id than
// the connection's own app.tenant_id setting - the row-level defense that
// stands independently of the FK fix, per CLAUDE.md's "RLS, not
// application-code discipline" rule.
func TestCreateLaunchSession_CrossTenantAccess_RLSBlocked(t *testing.T) {
	pool := testPool(t)
	f1 := seedCasinoFixture(t, pool)
	f2 := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")

	// Connected AS tenant f1, attempt to insert a row claiming to belong
	// to tenant f2 in full (tenant_id, brand_id, player_account_id all
	// self-consistent for f2) - the FK alone would happily accept this
	// triple, so only RLS's tenant_id = app.tenant_id check can catch it.
	err := pool.WithTenant(context.Background(), f1.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := CreateLaunchSession(ctx, tx, CreateLaunchSessionParams{
			TenantID: f2.tenantID, BrandID: f2.brandID, PlayerAccountID: f2.playerAccountID, WalletID: f2.walletID,
			GameID: game.ID, ProviderID: "mock-casino", ProviderGameID: game.ProviderGameID,
			AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	assertRLSViolation(t, err)
}
