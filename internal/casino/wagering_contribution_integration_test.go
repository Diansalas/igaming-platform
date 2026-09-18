//go:build integration

// Stage 4H-B1 Wave 3 Phase 3 test suite for the cash-funded wagering-
// contribution trigger this dispatch wires (ledger-accounting-model.md
// §7.18.3, orchestrator.go's own postBet call-through into
// bonus.RecordCashFundedWageringContribution). Covers: a fully-qualifying
// bet driving a Grant all the way to `completed` via real cash play (the
// reconnaissance's own gap-list item 13, now closed for this mechanic);
// per-game-category contribution-weight exclusion; the fail-closed
// multi-Grant posture; and idempotency under a redelivered bet callback.
package casino

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// seedWageringOfferVersion creates a Campaign/Offer/OfferVersion whose
// completion mechanic is C1 (wagering-multiplier), configured with the
// given multiplier/contribution-weight-table axes - mirrors
// bonus_settlement_integration_test.go's seedBonusCampaignOffer, widened
// with the two Stage 4H-B1 Wave 3 axes this suite exercises.
func seedWageringOfferVersion(t *testing.T, pool *db.Pool, tenantID, brandID uuid.UUID, wageringMultiplierBP int32, contributionWeightTable []byte) bonusCampaignOffer {
	t.Helper()
	staffActorID := uuid.New()
	var co bonusCampaignOffer
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := bonus.CreateCampaign(ctx, tx, bonus.Campaign{TenantID: tenantID, BrandID: &brandID, CreatedByActorType: bonus.ActorSystem, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		co.campaignID = c.ID
		v, err := bonus.CreateCampaignVersion(ctx, tx, bonus.CampaignVersion{TenantID: tenantID, CampaignID: c.ID, VersionNumber: 1, Name: "V1", CreatedByActorType: bonus.ActorSystem, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		co.campaignVersionID = v.ID
		o, err := bonus.CreateOffer(ctx, tx, bonus.Offer{TenantID: tenantID, BrandID: &brandID, CampaignID: c.ID, CampaignVersionID: v.ID, GrantPolicy: bonus.GrantPolicyAutoIssue, CreatedByActorType: bonus.ActorSystem, CreatedByActorID: staffActorID})
		if err != nil {
			return err
		}
		co.offerID = o.ID
		mult := wageringMultiplierBP
		ov, err := bonus.CreateOfferVersion(ctx, tx, bonus.OfferVersion{
			TenantID: tenantID, OfferID: o.ID, VersionNumber: 1, RewardKind: bonus.RewardFixedValue, RewardAssetCode: "EUR",
			FulfillmentDestination: bonus.FulfillmentIntoPlatformWallet, FundingSource: "operator",
			WageringMultiplierBP: &mult, ContributionWeightTable: contributionWeightTable,
			CreatedByActorType: bonus.ActorSystem, CreatedByActorID: staffActorID,
		})
		if err != nil {
			return err
		}
		co.offerVersionID = ov.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed wagering offer version: %v", err)
	}
	return co
}

// seedActivatedWageringGrant seeds an Activated Grant directly (bypassing
// IssueGrant/ActivateGrant's own T.1 gates - not this suite's concern,
// which is postBet's own call-through, mirroring
// bonus_settlement_integration_test.go's identical seedActivatedGrant
// convention), with granted_amount set directly (the wagering-target
// derivation's own input, migration 0067 - ordinarily written exactly
// once by ActivateGrant itself).
func seedActivatedWageringGrant(t *testing.T, pool *db.Pool, f casinoFixture, co bonusCampaignOffer, triggerRef string, grantedAmount int64) uuid.UUID {
	t.Helper()
	var grantID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g, err := bonus.CreateGrant(ctx, tx, bonus.Grant{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			CampaignID: co.campaignID, CampaignVersionID: co.campaignVersionID, OfferID: co.offerID, OfferVersionID: co.offerVersionID,
			AssetCode: "EUR", DecimalExponent: 2,
			FundingSource: "operator", FulfillmentDestination: bonus.FulfillmentIntoPlatformWallet, FulfillmentOwner: "internal",
			TriggerReference: triggerRef, CreatedByActorType: bonus.ActorSystem,
		})
		if err != nil {
			return err
		}
		grantID = g.ID
		if _, err := tx.Exec(ctx, `UPDATE bonus_grants SET granted_amount = $3 WHERE tenant_id = $1 AND id = $2`, f.tenantID, grantID, grantedAmount); err != nil {
			return err
		}
		_, err = bonus.UpdateGrantStatus(ctx, tx, f.tenantID, grantID, bonus.GrantIssued, bonus.GrantActivated, time.Now().UTC())
		return err
	})
	if err != nil {
		t.Fatalf("seed activated wagering grant: %v", err)
	}
	return grantID
}

func TestPostBet_CashFundedWageringContribution_RecordsAndCompletesGrant(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	// 1x multiplier (10000bp), unweighted (full) contribution: a single
	// 1000-unit bet exactly satisfies a 1000-granted-amount Grant.
	co := seedWageringOfferVersion(t, pool, f.tenantID, f.brandID, 10000, nil)
	grantID := seedActivatedWageringGrant(t, pool, f, co, "wagering-1", 1000)

	payload := provider.CallbackPayload(CallbackEventBet, "bet-wager-1", "", "round-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		result, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		if err != nil {
			return err
		}
		if result.Outcome != OutcomeSucceeded {
			t.Fatalf("expected the bet to succeed, got %+v", result)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}

	if status := readGrantStatus(t, pool, f.tenantID, grantID); status != bonus.GrantCompleted {
		t.Fatalf("expected the grant to reach completed after a fully-qualifying cash bet, got %s", status)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		progress, err := bonus.ListWageringProgressByGrant(ctx, tx, f.tenantID, grantID)
		if err != nil {
			return err
		}
		if len(progress) != 1 {
			t.Fatalf("expected exactly one wagering progress row, got %d", len(progress))
		}
		if progress[0].QualifyingScaled.Cmp(big.NewInt(1000)) != 0 {
			t.Fatalf("expected QualifyingScaled 1000 (full contribution), got %s", progress[0].QualifyingScaled)
		}
		if progress[0].StakedBonusAmount.Cmp(big.NewInt(1000)) != 0 {
			t.Fatalf("expected StakedBonusAmount read from the bet's own posted player_cash debit (1000), got %s", progress[0].StakedBonusAmount)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPostBet_CashFundedWageringContribution_ExcludedGameTypeContributesNothing
// proves the per-game-category contribution-weight resolver: a game_type
// this Offer's own configuration excludes must count 0 toward wagering,
// never fall back to full contribution.
func TestPostBet_CashFundedWageringContribution_ExcludedGameTypeContributesNothing(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR") // seedGame always registers GameType "slot"
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	co := seedWageringOfferVersion(t, pool, f.tenantID, f.brandID, 10000, []byte(`{"excluded_game_types":["slot"]}`))
	grantID := seedActivatedWageringGrant(t, pool, f, co, "wagering-excluded", 1000)

	payload := provider.CallbackPayload(CallbackEventBet, "bet-excluded", "", "round-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		result, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		if err != nil {
			return err
		}
		if result.Outcome != OutcomeSucceeded {
			t.Fatalf("expected the bet to succeed (the exclusion affects wagering credit, never whether the stake itself posts), got %+v", result)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}

	if status := readGrantStatus(t, pool, f.tenantID, grantID); status != bonus.GrantActivated {
		t.Fatalf("expected the grant to remain activated (excluded game contributed nothing), got %s", status)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		progress, err := bonus.ListWageringProgressByGrant(ctx, tx, f.tenantID, grantID)
		if err != nil {
			return err
		}
		if len(progress) != 0 {
			t.Fatalf("expected zero wagering progress rows for an excluded game type, got %d", len(progress))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPostBet_CashFundedWageringContribution_AmbiguousMultiGrantSkipsBoth
// proves §7.18.3.3's own named fail-closed posture: a player holding MORE
// than one concurrently-active wagering Grant gets no contribution
// recorded for either, and the ambiguity is audited, rather than an
// arbitrary guess.
func TestPostBet_CashFundedWageringContribution_AmbiguousMultiGrantSkipsBoth(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	co := seedWageringOfferVersion(t, pool, f.tenantID, f.brandID, 10000, nil)
	grantA := seedActivatedWageringGrant(t, pool, f, co, "wagering-a", 1000)
	grantB := seedActivatedWageringGrant(t, pool, f, co, "wagering-b", 1000)

	payload := provider.CallbackPayload(CallbackEventBet, "bet-ambiguous", "", "round-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		result, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		if err != nil {
			return err
		}
		if result.Outcome != OutcomeSucceeded {
			t.Fatalf("expected the bet itself to still post (the ambiguity affects wagering credit only), got %+v", result)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}

	if status := readGrantStatus(t, pool, f.tenantID, grantA); status != bonus.GrantActivated {
		t.Fatalf("expected grant A to remain untouched (ambiguous attribution), got %s", status)
	}
	if status := readGrantStatus(t, pool, f.tenantID, grantB); status != bonus.GrantActivated {
		t.Fatalf("expected grant B to remain untouched (ambiguous attribution), got %s", status)
	}
	if !auditActionExists(t, pool, f.tenantID, "bonus_wagering_contribution.ambiguous_multi_grant_skipped") {
		t.Fatal("expected the multi-grant ambiguity to be audited")
	}
}

// TestPostBet_CashFundedWageringContribution_RedeliveredBetIsIdempotent
// proves a redelivered bet callback (the SAME provider_tx_id) never
// double-records a wagering contribution - postBet's own pre-existing
// idempotency short-circuit (findPostedBetTransaction) means the
// wagering-contribution call is only ever reached once per real bet
// (ledger-accounting-model.md §7.18.3.5's own stated finding), proven
// here against real code rather than merely asserted from the doc.
func TestPostBet_CashFundedWageringContribution_RedeliveredBetIsIdempotent(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	co := seedWageringOfferVersion(t, pool, f.tenantID, f.brandID, 100000, nil) // 10x multiplier, target 10000 - one 1000-unit bet must not complete it
	grantID := seedActivatedWageringGrant(t, pool, f, co, "wagering-redeliver", 1000)

	payload := provider.CallbackPayload(CallbackEventBet, "bet-redeliver", "", "round-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	for i := 0; i < 2; i++ {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			result, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
			if err != nil {
				return err
			}
			if result.Outcome != OutcomeSucceeded {
				t.Fatalf("delivery %d: expected the bet to succeed, got %+v", i, result)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("ReceiveCallback delivery %d: %v", i, err)
		}
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		progress, err := bonus.ListWageringProgressByGrant(ctx, tx, f.tenantID, grantID)
		if err != nil {
			return err
		}
		if len(progress) != 1 {
			t.Fatalf("expected exactly one wagering progress row after two identical bet deliveries, got %d", len(progress))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if status := readGrantStatus(t, pool, f.tenantID, grantID); status != bonus.GrantInProgress {
		t.Fatalf("expected the grant to be in_progress (1000 of 10000 target) after a redelivered bet, got %s", status)
	}
}
