//go:build integration

// K-3 remediation tests (docs/governance/stage-4i-canonical-model.md §9):
// casino's per-game jurisdiction blocklist fail-open defect fix. Covers
// the exact scenarios the dispatch's validation floor names:
//
//   - an empty-blocklist game launches normally regardless of jurisdiction
//     resolution state - the non-regression the correction in §9.1 exists
//     to prove (applying RISK's/security's own "remove the guard
//     unconditionally" recommendation literally would deny 100% of casino
//     launches in Stage 4I, since every player-scoped resolution is
//     unresolved(no_signal) today - HDR-J-3 is unanswered);
//   - a non-empty-blocklist game denies on an unresolved jurisdiction - the
//     actual fix (K3-1/K3-2), in both real and demo mode (K3-4); and
//   - a non-empty-blocklist game denies on a genuinely blocked
//     jurisdiction, proven via evaluateJurisdictionBlocklist fed a
//     GENUINELY resolved jurisdiction.Resolution obtained through the
//     resolver's own producible tenant_licence basis. No player-side
//     jurisdiction producer exists anywhere in this codebase yet (HDR-J-3
//     unanswered, canonical-model §11.3), and jurisdiction.Resolution is
//     deliberately non-forgeable (no exported constructor exists anywhere
//     to fabricate one) - so this is the only way to obtain a real
//     Resolved value to test the "blocked" branch against, never a
//     fabricated one and never a real vendor.
package casino

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
)

// seedGameWithBlocklist mirrors seedGame but arms K-3's control for the
// returned title (a non-empty jurisdiction_blocklist).
func seedGameWithBlocklist(t *testing.T, pool *db.Pool, providerID string, blocklist []string, assetCodes ...string) Game {
	t.Helper()
	var g Game
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		g, err = UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: providerID, ProviderGameID: "game-" + uuid.New().String()[:8],
			Name: "Blocklisted Test Game", GameType: "slot", SupportedAssets: assetCodes,
			Status: GameStatusActive, JurisdictionBlocklist: blocklist,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed game with blocklist: %v", err)
	}
	return g
}

// TestLaunchGame_EmptyBlocklist_NonRegression proves K3-1: a game whose
// jurisdiction_blocklist is empty launches successfully regardless of
// jurisdiction resolution state. f's own tenant has no licence configured
// at all (seedCasinoFixture never sets tenants.licence_id), so the
// resolver genuinely cannot resolve anything for this launch - and the
// launch must still succeed, proving the fix does not deny 100% of casino
// launches (canonical-model §9.1).
func TestLaunchGame_EmptyBlocklist_NonRegression(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	game := seedGame(t, pool, "mock-casino", "EUR") // no blocklist (seedGame's default)
	enableGameForTenant(t, pool, f, game.ID)
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	var result LaunchGameResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.LaunchGame(ctx, tx, LaunchGameParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if err != nil {
		t.Fatalf("LaunchGame: %v", err)
	}
	if result.Denied {
		t.Fatalf("expected an empty-blocklist game to launch regardless of jurisdiction resolution state, got denied: %+v", result)
	}
	if result.LaunchURL == "" || result.SessionID == uuid.Nil {
		t.Fatalf("expected a launch url and session id, got %+v", result)
	}
}

// TestLaunchGame_ArmedBlocklist_DeniesOnUnresolvedJurisdiction proves
// K3-1/K3-2: a game whose jurisdiction_blocklist is non-empty (the
// control is "armed") denies when the platform cannot determine the
// player's jurisdiction - the actual fix for the fail-open defect. The
// denial reports the distinguishable DenialCodeJurisdictionUnresolved,
// never DenialCodeJurisdictionBlocked (K3-2): this player's jurisdiction
// is unknown, not known-and-disallowed.
func TestLaunchGame_ArmedBlocklist_DeniesOnUnresolvedJurisdiction(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	game := seedGameWithBlocklist(t, pool, "mock-casino", []string{"ANY-CODE"}, "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	var result LaunchGameResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.LaunchGame(ctx, tx, LaunchGameParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if err != nil {
		t.Fatalf("LaunchGame: %v", err)
	}
	if !result.Denied || result.DenialCode != DenialCodeJurisdictionUnresolved {
		t.Fatalf("expected a denial with DenialCodeJurisdictionUnresolved, got %+v", result)
	}
}

// TestLaunchGame_ArmedBlocklist_DeniesInDemoModeToo proves K3-4's explicit
// demo-mode ruling (canonical-model §9.6, architect's "demo launches are
// catalogue-availability-bearing by default"): a demo launch of an armed
// game denies IDENTICALLY to a real-money one - the check is not
// conditioned on params.Mode, and this is a deliberate, tested choice
// rather than an accident of code ordering.
func TestLaunchGame_ArmedBlocklist_DeniesInDemoModeToo(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	game := seedGameWithBlocklist(t, pool, "mock-casino", []string{"ANY-CODE"}, "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	var result LaunchGameResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.LaunchGame(ctx, tx, LaunchGameParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, AssetCode: "EUR", Mode: ModeDemo,
		})
		return err
	})
	if err != nil {
		t.Fatalf("LaunchGame: %v", err)
	}
	if !result.Denied || result.DenialCode != DenialCodeJurisdictionUnresolved {
		t.Fatalf("expected a demo-mode launch of an armed game to deny identically to real-money, got %+v", result)
	}
}

// seedTenantWithLicence registers a fresh tenant, brand and a real
// jurisdictions/licences pair, and points the tenant's own licence_id at
// it - the ONE basis Stage 4I's resolver can genuinely produce
// (tenant_licence, canonical-model §3.2), for a tenant/brand-subject
// operation (never a player-scoped one). Returns the jurisdiction's own
// code so callers can assert against it.
func seedTenantWithLicence(t *testing.T, pool *db.Pool) (tenantID, brandID uuid.UUID, jurisdictionCode string) {
	t.Helper()
	tenantID = uuid.New()
	brandID = uuid.New()
	jurisdictionID := uuid.New()
	licenceID := uuid.New()
	jurisdictionCode = "K3-TEST-" + jurisdictionID.String()[:8]

	// Stage 4I Phase E-SECURITY (migration 0077): `tenants`/`jurisdictions`/
	// `licences` writes now require a genuinely platform-admin-scoped
	// transaction; rows-affected is checked explicitly on every write
	// below because a denied RLS write is a silent zero-row no-op, not an
	// error.
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Test Tenant', 'under_platform_licence')`,
			tenantID, "t-"+tenantID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		tag, err = tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'K-3 Test Jurisdiction')`,
			jurisdictionID, jurisdictionCode)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 jurisdiction row, inserted %d", tag.RowsAffected())
		}
		tag, err = tx.Exec(ctx,
			`INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', 'K3-LIC-1')`,
			licenceID, jurisdictionID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 licence row, inserted %d", tag.RowsAffected())
		}
		tag, err = tx.Exec(ctx, `UPDATE tenants SET licence_id = $2 WHERE id = $1`, tenantID, licenceID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to update 1 tenant row, updated %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed tenant with licence: %v", err)
	}

	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Test Brand')`,
			brandID, tenantID, "b-"+brandID.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed brand: %v", err)
	}
	return tenantID, brandID, jurisdictionCode
}

// TestEvaluateJurisdictionBlocklist proves K3-1/K3-2's full decision table
// directly, including the "denies on a genuinely blocked jurisdiction"
// case - fed a GENUINELY resolved jurisdiction.Resolution obtained via
// the resolver's own producible tenant_licence basis (a tenant/brand-
// subject jurisdiction.Resolve call, PlayerAccountID nil), and a
// genuinely unresolved one obtained via an ordinary player-scoped call
// (which Stage 4I's resolver always returns unresolved(no_signal) for,
// canonical-model §11.3 - HDR-J-3 is unanswered and no player-side
// jurisdiction producer exists anywhere in this codebase).
func TestEvaluateJurisdictionBlocklist(t *testing.T) {
	pool := testPool(t)
	tenantID, brandID, jurisdictionCode := seedTenantWithLicence(t, pool)

	var resolved jurisdiction.Resolution
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		resolved, err = jurisdiction.Resolve(ctx, tx, jurisdiction.Params{
			TenantID: tenantID, BrandID: &brandID, OperationClass: jurisdiction.OperationCatalogueAvailability,
			RequestedByActorType: jurisdiction.ActorSystem,
		})
		return err
	})
	if err != nil {
		t.Fatalf("resolve tenant_licence jurisdiction: %v", err)
	}
	if resolved.Outcome() != jurisdiction.Resolved {
		t.Fatalf("expected the tenant_licence basis to resolve, got %s(%s)", resolved.Outcome(), resolved.Reason())
	}
	if code, _ := resolved.Code(); code != jurisdictionCode {
		t.Fatalf("expected resolved code %q, got %q", jurisdictionCode, code)
	}

	playerID := uuid.New()
	var playerScoped jurisdiction.Resolution
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		playerScoped, err = jurisdiction.Resolve(ctx, tx, jurisdiction.Params{
			TenantID: tenantID, BrandID: &brandID, PlayerAccountID: &playerID, OperationClass: jurisdiction.OperationPlay,
			RequestedByActorType: jurisdiction.ActorPlayer, RequestedByActorID: &playerID,
		})
		return err
	})
	if err != nil {
		t.Fatalf("resolve player-scoped jurisdiction: %v", err)
	}
	if playerScoped.Outcome() != jurisdiction.Unresolved || playerScoped.Reason() != jurisdiction.ReasonNoSignal {
		t.Fatalf("expected the player-scoped resolution to be unresolved(no_signal) in Stage 4I, got %s(%s)", playerScoped.Outcome(), playerScoped.Reason())
	}

	// A genuinely REFUSED resolution (canonical-model §6.2 scenario 9,
	// "unavailable resolver"/dependency-failure family) - obtained via
	// Resolve's own zero-tenant input gate (refused(scope_mismatch)),
	// never fabricated (Resolution has no exported constructor). K3-1/K3-2
	// make no distinction between Unresolved and Refused - both fail
	// evaluateJurisdictionBlocklist's single `Outcome() != Resolved` test
	// identically - and this proves that holds for Refused specifically,
	// not merely for Unresolved: an unavailable/refused resolver must
	// collapse into the SAME DenialCodeJurisdictionUnresolved a data gap
	// does, never a distinguishable player-facing outcome (canonical-model
	// §6.3's oracle rule).
	refused, err := jurisdiction.Resolve(context.Background(), nil, jurisdiction.Params{
		TenantID: uuid.Nil, OperationClass: jurisdiction.OperationPlay, RequestedByActorType: jurisdiction.ActorSystem,
	})
	if err != nil {
		t.Fatalf("resolve refused case: %v", err)
	}
	if refused.Outcome() != jurisdiction.Refused {
		t.Fatalf("expected a Refused outcome, got %s(%s)", refused.Outcome(), refused.Reason())
	}

	tests := []struct {
		name           string
		blocklist      []string
		res            jurisdiction.Resolution
		wantDenied     bool
		wantDenialCode string
	}{
		{"K3-1: empty blocklist, unresolved -> not denied", nil, playerScoped, false, ""},
		{"K3-1: empty blocklist, resolved -> not denied", []string{}, resolved, false, ""},
		{"K3-2: armed, unresolved -> denied unresolved", []string{jurisdictionCode}, playerScoped, true, DenialCodeJurisdictionUnresolved},
		{"armed, resolved but not in blocklist -> not denied", []string{"SOME-OTHER-CODE"}, resolved, false, ""},
		{"armed, resolved and blocked -> denied blocked", []string{jurisdictionCode}, resolved, true, DenialCodeJurisdictionBlocked},
		{"K3-1: empty blocklist, refused -> not denied", nil, refused, false, ""},
		{"unavailable resolver: armed, refused -> denied unresolved (never distinguishable from a data gap)", []string{jurisdictionCode}, refused, true, DenialCodeJurisdictionUnresolved},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			denied, code, err := evaluateJurisdictionBlocklist(tc.blocklist, tc.res)
			if err != nil {
				t.Fatalf("evaluateJurisdictionBlocklist: %v", err)
			}
			if denied != tc.wantDenied || code != tc.wantDenialCode {
				t.Fatalf("expected denied=%v code=%q, got denied=%v code=%q", tc.wantDenied, tc.wantDenialCode, denied, code)
			}
		})
	}
}
