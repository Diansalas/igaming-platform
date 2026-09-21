//go:build integration

// Stage 9 (Production Readiness), migration 0082 section 2 (ARCH-DB-3).
//
// Proves the composite brand-pinning foreign key actually rejects a row
// that names a brand belonging to the right TENANT but not to the right
// PLAYER - the exact gap the pre-0082 (brand_id, tenant_id) -> brands FK
// left open, and the same fix Stage 6.1 made for sportsbook_bets
// (migration 0078) and Stage 7 made for casino_launch_sessions
// (migration 0079).
//
// Why this is worth a test rather than a schema review: the loose FK
// PASSES for a misattributed row. Brand B2 really does exist and really
// does belong to this tenant, so nothing at the database level objected;
// only the application deriving brand_id correctly from the player's own
// account kept it right. CLAUDE.md's rule is that this class of integrity
// is a database fact, not application discipline.
//
// withdrawal_requests is used as the representative table: all seven
// tables changed in section 2 got the identical constraint shape against
// the identical player_accounts_id_tenant_brand_key, so a failure here
// would be a failure of the pattern, not of this one table.
package withdrawal

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestMigration0082_WithdrawalRequestsBrandPinnedToPlayersOwnBrand(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)

	// A SECOND, entirely legitimate brand inside the SAME tenant. Under
	// the old (brand_id, tenant_id) -> brands FK this was an acceptable
	// brand_id for any row in this tenant, including this player's.
	otherBrandID := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Other Brand')`,
			otherBrandID, f.tenantID, "b-"+otherBrandID.String()[:8])
		return err
	}); err != nil {
		t.Fatalf("seed second brand: %v", err)
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_requests
				(tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key)
			 VALUES ($1, $2, $3, $4, 'EUR', 1000, $5)`,
			f.tenantID, otherBrandID, f.playerAccountID, f.walletID, "mig0082-wrong-brand")
		return err
	})
	if err == nil {
		t.Fatal("expected the composite (player_account_id, tenant_id, brand_id) FK to reject a withdrawal " +
			"attributed to a brand this player does not belong to, got nil error")
	}
	if !strings.Contains(err.Error(), "withdrawal_requests_player_tenant_brand_fkey") {
		t.Fatalf("expected the composite brand-pinning FK to be the constraint that fired, got: %v", err)
	}

	// The same insert with the player's OWN brand must still succeed -
	// the constraint must reject misattribution, not ordinary operation.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_requests
				(tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key)
			 VALUES ($1, $2, $3, $4, 'EUR', 1000, $5)`,
			f.tenantID, f.brandID, f.playerAccountID, f.walletID, "mig0082-right-brand")
		return err
	}); err != nil {
		t.Fatalf("expected a correctly-branded withdrawal request to remain insertable, got: %v", err)
	}
}
