//go:build integration

// Stage 4I item 6 of qa's test-floor dispatch: a genuine concurrent-access
// test for internal/jurisdiction.Resolve's H-2 read-only-on-evaluation-
// path constraint (canonical-model §6.4), consumed SIMULTANEOUSLY by
// casino's LaunchGame (K-3) and a Bonus admin surface (ActivateGrant,
// which JV-2's resolveGrantJurisdiction call site feeds - the same
// resolver call every one of Bonus's five admin surfaces ultimately
// reaches) for the SAME tenant and the SAME player.
//
// Resolve itself takes no lock of any kind (ReadOnlyQuerier exposes only
// Query/QueryRow - H-2 is structurally enforced, canonical-model §6.4) and
// its only reads (tenants/licences/jurisdictions) carry no RLS and no row
// locking. The two consumers' OWN locking is on entirely disjoint
// resources (casino.LaunchGame takes none directly; bonus.ActivateGrant
// takes an advisory lock keyed on the GRANT id and a FOR UPDATE on the
// grant row, neither of which casino's path ever touches), so no
// deadlock is structurally expected - this test proves that holds under
// genuine concurrent load rather than by inspection alone, and proves
// each consumer still reaches its own, correct, independent denial
// (canonical-model §11.3: every player-scoped resolution is
// unresolved(no_signal) in Stage 4I, so BOTH consumers deny, for their
// own domain-specific reasons, from the SAME underlying resolver state).
package casino

import (
	"context"
	"math/big"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/assetregistry"
	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

func TestJurisdictionResolve_ConcurrentCasinoLaunchAndBonusActivate_NoDeadlockIndependentDenial(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)

	// --- casino side: an armed-blocklist game, so LaunchGame denies
	// deterministically on the unresolved jurisdiction (K3-1/K3-2), rather
	// than merely resolving-and-ignoring it. ---
	provider := NewMockCasinoProvider("mock-casino-concurrent", "EUR")
	game := seedGameWithBlocklist(t, pool, "mock-casino-concurrent", []string{"ANY-CODE"}, "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino-concurrent": provider}, nil)

	// --- bonus side: a real Grant, issued (RG/Risk only - T.2, AssetAuthorization
	// is skipped at creation) and sitting in 'issued' status, ready for
	// ActivateGrant to run the FULL T.1 gate (AssetAuthorization -> RG ->
	// Risk) - which is where the SAME player's jurisdiction is resolved
	// again, independently, and denies. ---
	staffActorID := uuid.New()
	var grantID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		w, err := wallet.GetOrCreate(ctx, tx, f.tenantID, f.brandID, f.playerAccountID, "USD")
		if err != nil {
			return err
		}
		c, err := bonus.CreateCampaign(ctx, tx, bonus.Campaign{TenantID: f.tenantID, BrandID: &f.brandID, CreatedByActorType: bonus.ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		v, err := bonus.CreateCampaignVersion(ctx, tx, bonus.CampaignVersion{TenantID: f.tenantID, CampaignID: c.ID, VersionNumber: 1, Name: "V1", CreatedByActorType: bonus.ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		o, err := bonus.CreateOffer(ctx, tx, bonus.Offer{TenantID: f.tenantID, BrandID: &f.brandID, CampaignID: c.ID, CampaignVersionID: v.ID, GrantPolicy: bonus.GrantPolicyAutoIssue, CreatedByActorType: bonus.ActorStaff, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		ov, err := bonus.CreateOfferVersion(ctx, tx, bonus.OfferVersion{
			TenantID: f.tenantID, OfferID: o.ID, VersionNumber: 1, RewardKind: bonus.RewardFixedValue, RewardAssetCode: "USD",
			FulfillmentDestination: bonus.FulfillmentIntoPlatformWallet, FundingSource: "operator", CreatedByActorType: bonus.ActorStaff, CreatedByActorID: staffActorID,
		})
		if err != nil {
			return err
		}
		g := bonus.Grant{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: w.ID,
			CampaignID: c.ID, CampaignVersionID: v.ID, OfferID: o.ID, OfferVersionID: ov.ID,
			AssetCode: "USD", DecimalExponent: 2,
			FundingSource: "operator", FulfillmentDestination: bonus.FulfillmentIntoPlatformWallet, FulfillmentOwner: "internal",
			TriggerReference: "jurisdiction-concurrency-test", CreatedByActorType: bonus.ActorSystem,
		}
		created, issueOutcome, err := bonus.IssueGrant(ctx, tx, bonus.IssueGrantParams{Grant: g})
		if err != nil {
			return err
		}
		if !issueOutcome.Allowed {
			t.Fatalf("expected the grant to issue (RG/Risk only, AssetAuthorization skipped at T.2), got denial %s: %s", issueOutcome.DeniedBy, issueOutcome.Code)
		}
		grantID = created.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed bonus grant: %v", err)
	}

	// --- the actual race: two goroutines, two independent connections,
	// same tenant, same player, concurrently. ---
	var wg sync.WaitGroup
	wg.Add(2)

	var launchResult LaunchGameResult
	var launchErr error
	go func() {
		defer wg.Done()
		launchErr = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			launchResult, err = orch.LaunchGame(ctx, tx, LaunchGameParams{
				TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
				GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
			})
			return err
		})
	}()

	var activateOutcome bonus.GateOutcome
	var activateErr error
	go func() {
		defer wg.Done()
		activateErr = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			_, activateOutcome, err = bonus.ActivateGrant(ctx, tx, f.tenantID, grantID, bonus.ActivateGrantParams{
				ActorType: bonus.ActorSystem, ActorID: uuid.Nil, Amount: big.NewInt(1000),
			})
			return err
		})
	}()

	wg.Wait()

	// No deadlock, no transient/driver-level failure on either side - both
	// transactions must complete cleanly (a genuine deadlock would surface
	// here as a 40P01 wrapped error from one of the two WithTenant calls).
	if launchErr != nil {
		t.Fatalf("casino LaunchGame: unexpected error (possible deadlock/contention): %v", launchErr)
	}
	if activateErr != nil {
		t.Fatalf("bonus ActivateGrant: unexpected error (possible deadlock/contention): %v", activateErr)
	}

	// Each side reaches its OWN correct, independent denial - never an
	// ALLOW, and never the other side's denial code (proving they did not
	// somehow cross-contaminate or short-circuit each other).
	if !launchResult.Denied || launchResult.DenialCode != DenialCodeJurisdictionUnresolved {
		t.Fatalf("expected casino to independently deny with DenialCodeJurisdictionUnresolved, got %+v", launchResult)
	}
	if activateOutcome.Allowed {
		t.Fatal("expected bonus ActivateGrant to independently deny (Stage 4I: every player-scoped jurisdiction resolution is unresolved(no_signal))")
	}
	if activateOutcome.DeniedBy != "asset_authorization" || activateOutcome.Code != string(assetregistry.ReasonJurisdictionContextMissing) {
		t.Fatalf("expected bonus to deny at asset_authorization/jurisdiction_context_missing, got DeniedBy=%q Code=%q", activateOutcome.DeniedBy, activateOutcome.Code)
	}
}
