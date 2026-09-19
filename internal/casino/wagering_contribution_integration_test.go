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
	"github.com/Diansalas/igaming-platform/internal/wallet"
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

// seedActivatedWageringGrantInAsset is seedActivatedWageringGrant with the
// Grant's own asset/wallet parameterized - the DR-4HB1W3-RISK-01
// regression below needs a Grant denominated in an asset OTHER than the
// bet's.
func seedActivatedWageringGrantInAsset(t *testing.T, pool *db.Pool, f casinoFixture, co bonusCampaignOffer, triggerRef string, grantedAmount int64, assetCode string, decimalExponent int32) uuid.UUID {
	t.Helper()
	var grantID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		w, err := wallet.GetOrCreate(ctx, tx, f.tenantID, f.brandID, f.playerAccountID, assetCode)
		if err != nil {
			return err
		}
		g, err := bonus.CreateGrant(ctx, tx, bonus.Grant{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: w.ID,
			CampaignID: co.campaignID, CampaignVersionID: co.campaignVersionID, OfferID: co.offerID, OfferVersionID: co.offerVersionID,
			AssetCode: assetCode, DecimalExponent: decimalExponent,
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
		t.Fatalf("seed activated wagering grant in %s: %v", assetCode, err)
	}
	return grantID
}

// TestPostBet_CashFundedWageringContribution_CrossAssetGrantIsNeverCredited
// is the DR-4HB1W3-RISK-01 regression (Stage 4H-B1 Wave 3 Phase 4, risk).
//
// The Grant below is denominated in BTC (decimal_exponent 8); the bet is
// in EUR (decimal_exponent 2). Before the fix, postBet's
// wagering-contribution call-through selected EVERY activated/in_progress
// Grant for the player regardless of asset, recorded the EUR stake's raw
// minor-unit amount against the BTC Grant, and then compared that total
// against a target derived from the BTC Grant's own granted_amount in
// satoshi - completing the Grant outright on a single bet that had
// nothing to do with it. That is a value-authorizing comparison across
// two different decimal exponents, which ADR 0031 §34/§35 and
// internal/risk/denomination.go forbid everywhere else on this platform.
//
// Post-fix the bet still posts normally (a wagering-attribution decision
// must never gate the stake itself) and the cross-asset Grant is left
// completely untouched: no contribution row, no status change.
func TestPostBet_CashFundedWageringContribution_CrossAssetGrantIsNeverCredited(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	// 1x multiplier over a 1000-satoshi granted amount: target = 1000
	// satoshi. The EUR bet below is 1000 EUR minor units - numerically
	// identical, denominationally unrelated.
	co := seedWageringOfferVersion(t, pool, f.tenantID, f.brandID, 10000, nil)
	grantID := seedActivatedWageringGrantInAsset(t, pool, f, co, "wagering-cross-asset", 1000, "BTC", 8)

	payload := provider.CallbackPayload(CallbackEventBet, "bet-cross-asset", "", "round-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		result, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		if err != nil {
			return err
		}
		if result.Outcome != OutcomeSucceeded {
			t.Fatalf("expected the EUR bet itself to post normally, got %+v", result)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}

	if status := readGrantStatus(t, pool, f.tenantID, grantID); status != bonus.GrantActivated {
		t.Fatalf("a EUR-denominated bet must never satisfy a BTC-denominated Grant's wagering requirement (DR-4HB1W3-RISK-01); grant status is %s", status)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		progress, err := bonus.ListWageringProgressByGrant(ctx, tx, f.tenantID, grantID)
		if err != nil {
			return err
		}
		if len(progress) != 0 {
			t.Fatalf("expected zero wagering progress rows against a cross-asset Grant, got %d", len(progress))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPostBet_CashFundedWageringContribution_UnmeasurableTargetNeverCompletes
// is the DR-4HB1W3-RISK-02 regression (Stage 4H-B1 Wave 3 Phase 4, risk).
//
// bonus_grants.granted_amount is nullable by design and was only added in
// migration 0067, so a Grant activated before that migration is live today
// with a 35x wagering requirement and no granted_amount to measure it
// against. Before the fix, WageringTargetScaled returned a nil target for
// exactly that case, and CheckAndCompleteGrant reads a nil target as
// "already satisfied" - so the player's very first cash bet completed the
// Grant outright, with zero qualifying wagering performed. A missing input
// must never resolve to ALLOW.
//
// Post-fix: the Grant stays open, the skip is audited, and the bet itself
// still posts normally (a Grant-data defect must not become a betting
// outage for the player).
func TestPostBet_CashFundedWageringContribution_UnmeasurableTargetNeverCompletes(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	co := seedWageringOfferVersion(t, pool, f.tenantID, f.brandID, 350000, nil) // 35x
	// Reproduce the pre-migration-0067 state exactly: an activated Grant
	// with a real wagering requirement and NO granted_amount ever written.
	// (granted_amount cannot be nulled AFTER the fact - migration 0067's
	// own immutability trigger correctly refuses that - so this seeds the
	// Grant without ever setting it, exactly as a Grant activated before
	// that migration exists today.)
	var grantID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g, err := bonus.CreateGrant(ctx, tx, bonus.Grant{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			CampaignID: co.campaignID, CampaignVersionID: co.campaignVersionID, OfferID: co.offerID, OfferVersionID: co.offerVersionID,
			AssetCode: "EUR", DecimalExponent: 2,
			FundingSource: "operator", FulfillmentDestination: bonus.FulfillmentIntoPlatformWallet, FulfillmentOwner: "internal",
			TriggerReference: "wagering-no-granted-amount", CreatedByActorType: bonus.ActorSystem,
		})
		if err != nil {
			return err
		}
		grantID = g.ID
		_, err = bonus.UpdateGrantStatus(ctx, tx, f.tenantID, grantID, bonus.GrantIssued, bonus.GrantActivated, time.Now().UTC())
		return err
	})
	if err != nil {
		t.Fatalf("seed grant with no granted_amount: %v", err)
	}

	payload := provider.CallbackPayload(CallbackEventBet, "bet-unmeasurable", "", "round-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		result, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		if err != nil {
			return err
		}
		if result.Outcome != OutcomeSucceeded {
			t.Fatalf("expected the bet itself to post normally, got %+v", result)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}

	if status := readGrantStatus(t, pool, f.tenantID, grantID); status == bonus.GrantCompleted {
		t.Fatal("a Grant whose wagering target is unmeasurable must never be completed (DR-4HB1W3-RISK-02)")
	}
	if !auditActionExists(t, pool, f.tenantID, "bonus_wagering_contribution.unmeasurable_target_completion_skipped") {
		t.Fatal("expected the unmeasurable wagering target to be audited, not silently skipped")
	}
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
