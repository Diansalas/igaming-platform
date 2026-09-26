//go:build integration

// Stage 4H-B1 Wave 2 Phase 7 adversarial/concurrency suite for the G-2
// casino/bonus integration (bonus_settlement.go, orchestrator.go's
// postWin/postRollback). Walks docs/architecture/08-casino-integration-
// architecture.md §16.18/§16.19's named scenario list against REAL code
// for the first time - Phases 1-6 built and froze internal/bonus's own
// seams, but nothing in internal/casino called them until this dispatch.
//
// Every fixture below simulates a hypothetical future bonus-funded
// postBet (§16.10.1's mandated case-B lock shape - NOT YET built in
// production, ADR 0025 §6: every REAL bet today is 100% player_cash-
// funded) by posting the identical ledger shape a real bonus-funded
// postBet would need to produce, directly via ledger.Post + grant
// attribution - exactly the "read the actual signatures/behavior, don't
// silently work around a drift" posture this dispatch requires, applied
// here to test setup rather than to internal/bonus's own code.
package casino

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

// --- bonus-side fixture helpers ---

type bonusCampaignOffer struct {
	campaignID, campaignVersionID, offerID, offerVersionID uuid.UUID
}

func seedBonusCampaignOffer(t *testing.T, pool *db.Pool, tenantID, brandID uuid.UUID) bonusCampaignOffer {
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
		ov, err := bonus.CreateOfferVersion(ctx, tx, bonus.OfferVersion{
			TenantID: tenantID, OfferID: o.ID, VersionNumber: 1, RewardKind: bonus.RewardFixedValue, RewardAssetCode: "EUR",
			FulfillmentDestination: bonus.FulfillmentIntoPlatformWallet, FundingSource: "operator", CreatedByActorType: bonus.ActorSystem, CreatedByActorID: staffActorID,
		})
		if err != nil {
			return err
		}
		co.offerVersionID = ov.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed bonus campaign/offer: %v", err)
	}
	return co
}

// seedActivatedGrant creates a Grant directly via bonus.CreateGrant
// (bypassing IssueGrant's eligibility/AssetAuthorization/RG/Risk gates -
// not this dispatch's concern, which is the CASINO-side settlement-credit
// seam wiring, not Grant issuance) in status GrantActivated, operator-
// funded, EUR.
func seedActivatedGrant(t *testing.T, pool *db.Pool, f casinoFixture, co bonusCampaignOffer, triggerRef string) uuid.UUID {
	t.Helper()
	var grantID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g, err := bonus.CreateGrant(ctx, tx, bonus.Grant{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			CampaignID: co.campaignID, CampaignVersionID: co.campaignVersionID, OfferID: co.offerID, OfferVersionID: co.offerVersionID,
			AssetCode: "EUR", DecimalExponent: 2,
			FundingSource: "operator", FulfillmentDestination: bonus.FulfillmentIntoPlatformWallet, FulfillmentOwner: "internal",
			TriggerReference:   triggerRef,
			CreatedByActorType: bonus.ActorSystem,
		})
		if err != nil {
			return err
		}
		grantID = g.ID
		_, err = bonus.UpdateGrantStatus(ctx, tx, f.tenantID, grantID, bonus.GrantIssued, bonus.GrantActivated, time.Now().UTC())
		return err
	})
	if err != nil {
		t.Fatalf("seed activated grant: %v", err)
	}
	return grantID
}

func setGrantTerminal(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID, newStatus bonus.GrantStatus) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := bonus.UpdateGrantStatus(ctx, tx, tenantID, grantID, bonus.GrantActivated, newStatus, time.Now().UTC())
		return err
	})
	if err != nil {
		t.Fatalf("set grant terminal (%s): %v", newStatus, err)
	}
}

func setGrantPendingSettlement(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID, resolution bonus.TerminalResolution) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := bonus.SetGrantPendingSettlement(ctx, tx, tenantID, grantID, bonus.GrantActivated, resolution, "test", time.Now().UTC(), uuid.New())
		return err
	})
	if err != nil {
		t.Fatalf("set grant pending_settlement: %v", err)
	}
}

func readGrantStatus(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID) bonus.GrantStatus {
	t.Helper()
	var status bonus.GrantStatus
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g, err := bonus.GetGrantByID(ctx, tx, grantID)
		if err != nil {
			return err
		}
		status = g.Status
		return nil
	})
	if err != nil {
		t.Fatalf("read grant status: %v", err)
	}
	return status
}

func readHeldDispositionBySettlement(t *testing.T, pool *db.Pool, tenantID, settlementTxID uuid.UUID) bonus.HeldDisposition {
	t.Helper()
	var d bonus.HeldDisposition
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = bonus.GetHeldDispositionBySettlementTransaction(ctx, tx, tenantID, settlementTxID)
		return err
	})
	if err != nil {
		t.Fatalf("read held disposition: %v", err)
	}
	return d
}

// postLockedBonusBet simulates a hypothetical future bonus-funded postBet
// (§16.10.1's mandated case-B lock shape): Dr player_bonus stake / Cr
// player_locked_bonus stake, attributed to grantID via
// grant_ledger_attributions - exactly what a real bonus-funded postBet
// would need to do at bet time.
func postLockedBonusBet(t *testing.T, pool *db.Pool, f casinoFixture, providerID, providerTxID, roundID string, grantID uuid.UUID, stake int64) uuid.UUID {
	t.Helper()
	var txID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		playerBonusAcct, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerBonus, "EUR")
		if err != nil {
			return err
		}
		lockedBonusAcct, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerLockedBonus, "EUR")
		if err != nil {
			return err
		}
		result, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxCasinoBet,
			IdempotencyKey: providerID + ":" + providerTxID,
			ProviderID:     &providerID, ProviderTxID: &providerTxID,
			CorrelationID: roundCorrelationID(f.tenantID, providerID, roundID),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: playerBonusAcct, Direction: ledger.Debit, Amount: stake},
				{LedgerAccountID: lockedBonusAcct, Direction: ledger.Credit, Amount: stake},
			},
			BonusCost: &ledger.BonusCostAttribution{Funding: ledger.FundingOperator},
		})
		if err != nil {
			return err
		}
		txID = result.TransactionID
		return bonus.AttributeGrantLedgerTransactionIdempotent(ctx, tx, f.tenantID, grantID, txID, string(ledger.TxCasinoBet))
	})
	if err != nil {
		t.Fatalf("post locked-bonus bet: %v", err)
	}
	return txID
}

// drainRoundLock posts a transaction that reduces a round's
// player_locked_bonus balance WITHOUT reversing the original bet -
// standing in for §16.5a's NOT-YET-AUTHORIZED settlement-timeout sweep
// (its own real transaction type, casino_settlement_timeout, does not
// exist in the ledger_transactions CHECK constraint - see this
// dispatch's own report). This is the exact shape LF-18's exploit
// required: a later, independent transaction under the SAME
// correlation_id that already reduced the lock to zero.
func drainRoundLock(t *testing.T, pool *db.Pool, f casinoFixture, providerID, roundID string, amount int64) {
	t.Helper()
	reason := "test: simulate settlement-timeout sweep drain (LF-18)"
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		lockedBonusAcct, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerLockedBonus, "EUR")
		if err != nil {
			return err
		}
		houseAcct, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountHouseGaming, "EUR")
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxManualAdjustment,
			IdempotencyKey: "drain-" + uuid.New().String(), CorrelationID: roundCorrelationID(f.tenantID, providerID, roundID),
			ReasonCode: &reason,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: lockedBonusAcct, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: houseAcct, Direction: ledger.Credit, Amount: amount},
			},
			BonusCost: &ledger.BonusCostAttribution{Funding: ledger.FundingOperator},
		})
		return err
	})
	if err != nil {
		t.Fatalf("drain round lock: %v", err)
	}
}

// postManualAdjustment posts an arbitrary, unrelated ledger transaction
// and returns its id - used only to satisfy
// bonus_held_dispositions.resolution_ledger_transaction_id's FK when a
// test needs to simulate an already-resolved disposition directly via
// bonus.ResolveHeldDisposition (whose own doc comment states it "performs
// NO SEP-1/four-eyes/permission enforcement... callers are responsible"),
// without exercising the full ResolveHeldDispositionAction approval
// workflow that is not this test's subject.
func postManualAdjustment(t *testing.T, pool *db.Pool, f casinoFixture, amount int64) uuid.UUID {
	t.Helper()
	reason := "test fixture"
	var txID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cashAcct, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		adjAcct, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountManualAdjustment, "EUR")
		if err != nil {
			return err
		}
		result, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxManualAdjustment,
			IdempotencyKey: "fixture-adj-" + uuid.New().String(), CorrelationID: uuid.New(), ReasonCode: &reason,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: adjAcct, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: cashAcct, Direction: ledger.Credit, Amount: amount},
			},
		})
		if err != nil {
			return err
		}
		txID = result.TransactionID
		return nil
	})
	if err != nil {
		t.Fatalf("post manual adjustment fixture: %v", err)
	}
	return txID
}

func seedSecondPlayerWallet(t *testing.T, pool *db.Pool, f casinoFixture) (playerAccountID, walletID uuid.UUID) {
	t.Helper()
	playerAccountID = uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		personID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1,$2,$3,$4,$5,'x','active')`,
			playerAccountID, f.tenantID, f.brandID, personID, playerAccountID.String()+"@example.com"); err != nil {
			return err
		}
		w, err := wallet.GetOrCreate(ctx, tx, f.tenantID, f.brandID, playerAccountID, "EUR")
		if err != nil {
			return err
		}
		walletID = w.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed second player wallet: %v", err)
	}
	return playerAccountID, walletID
}

// bonusBalances reads f's own wallet's per-account_type net balance
// (credit_total - debit_total) directly from the projection - a thin,
// test-only convenience over the same table cashBalance already reads.
func bonusBalances(t *testing.T, pool *db.Pool, f casinoFixture) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT account_type, (credit_total - debit_total)::bigint FROM wallet_balance_projection WHERE wallet_id = $1`, f.walletID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var acct string
			var bal int64
			if err := rows.Scan(&acct, &bal); err != nil {
				return err
			}
			out[acct] = bal
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read bonus balances: %v", err)
	}
	return out
}

func strPtr(s string) *string { return &s }

// --- §16.4's destination-resolution query (LF-18/LF-7/LF-8) ---

func TestResolveWinOrigin_CorrelationWalletCollision(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	_, otherWalletID := seedSecondPlayerWallet(t, pool, f)

	correlationID := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for i, wid := range []uuid.UUID{f.walletID, otherWalletID} {
			playerBonusAcct, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &wid, ledger.AccountPlayerBonus, "EUR")
			if err != nil {
				return err
			}
			lockedBonusAcct, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &wid, ledger.AccountPlayerLockedBonus, "EUR")
			if err != nil {
				return err
			}
			providerTxID := fmt.Sprintf("bet-collide-%d", i)
			if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: f.tenantID, TransactionType: ledger.TxCasinoBet,
				IdempotencyKey: "mock-casino:" + providerTxID,
				ProviderID:     strPtr("mock-casino"), ProviderTxID: &providerTxID,
				CorrelationID: correlationID,
				Entries: []ledger.EntryInput{
					{LedgerAccountID: playerBonusAcct, Direction: ledger.Debit, Amount: 100},
					{LedgerAccountID: lockedBonusAcct, Direction: ledger.Credit, Amount: 100},
				},
				BonusCost: &ledger.BonusCostAttribution{Funding: ledger.FundingOperator},
			}); err != nil {
				return err
			}
		}
		_, err := resolveWinOrigin(ctx, tx, f.tenantID, correlationID)
		return err
	})
	if !errors.Is(err, ErrCorrelationWalletCollision) {
		t.Fatalf("expected ErrCorrelationWalletCollision, got %v", err)
	}
}

func TestResolveWinOrigin_AmbiguousMultiOriginRound(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)

	correlationID := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		playerBonusAcct, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerBonus, "EUR")
		if err != nil {
			return err
		}
		lockedBonusAcct, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerLockedBonus, "EUR")
		if err != nil {
			return err
		}
		for i := 0; i < 2; i++ {
			providerTxID := fmt.Sprintf("bet-multi-%d", i)
			if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: f.tenantID, TransactionType: ledger.TxCasinoBet,
				IdempotencyKey: "mock-casino:" + providerTxID,
				ProviderID:     strPtr("mock-casino"), ProviderTxID: &providerTxID,
				CorrelationID: correlationID,
				Entries: []ledger.EntryInput{
					{LedgerAccountID: playerBonusAcct, Direction: ledger.Debit, Amount: 100},
					{LedgerAccountID: lockedBonusAcct, Direction: ledger.Credit, Amount: 100},
				},
				BonusCost: &ledger.BonusCostAttribution{Funding: ledger.FundingOperator},
			}); err != nil {
				return err
			}
		}
		_, err = resolveWinOrigin(ctx, tx, f.tenantID, correlationID)
		return err
	})
	if !errors.Is(err, ErrAmbiguousMultiOriginRound) {
		t.Fatalf("expected ErrAmbiguousMultiOriginRound, got %v", err)
	}
}

// TestResolveWinOrigin_MixedFundingUnsupported exercises outcome 4 by
// posting the exact single-instruction mixed-origin shape HR-2 exists to
// make unreachable at postBet's own layer - bypassed here by calling
// ledger.Post directly (HR-2 is an application-layer rule, not a
// ledger.Post-level one), confirming §16.4's own defense-in-depth catches
// it independently if HR-2 is ever regressed.
func TestResolveWinOrigin_MixedFundingUnsupported(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)

	correlationID := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		lockedCashAcct, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerLockedCash, "EUR")
		if err != nil {
			return err
		}
		lockedBonusAcct, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerLockedBonus, "EUR")
		if err != nil {
			return err
		}
		cashAcct, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		bonusAcct, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerBonus, "EUR")
		if err != nil {
			return err
		}
		providerTxID := "bet-mixed-1"
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxCasinoBet,
			IdempotencyKey: "mock-casino:" + providerTxID,
			ProviderID:     strPtr("mock-casino"), ProviderTxID: &providerTxID,
			CorrelationID: correlationID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: cashAcct, Direction: ledger.Debit, Amount: 50},
				{LedgerAccountID: lockedCashAcct, Direction: ledger.Credit, Amount: 50},
				{LedgerAccountID: bonusAcct, Direction: ledger.Debit, Amount: 50},
				{LedgerAccountID: lockedBonusAcct, Direction: ledger.Credit, Amount: 50},
			},
			BonusCost: &ledger.BonusCostAttribution{Funding: ledger.FundingOperator},
		}); err != nil {
			return err
		}
		_, err = resolveWinOrigin(ctx, tx, f.tenantID, correlationID)
		return err
	})
	if !errors.Is(err, ErrMixedFundingUnsupported) {
		t.Fatalf("expected ErrMixedFundingUnsupported, got %v", err)
	}
}

// TestPostWin_LockAlreadyReleased is LF-18's own exploit, closed: a
// genuine casino_bet credit leg exists, but an independent later
// transaction under the SAME correlation_id already drained the lock to
// zero (standing in for §16.5a's sweep) BEFORE a genuine late win
// arrives. Outcome 6 must abort - never release a second time.
func TestPostWin_LockAlreadyReleased(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	co := seedBonusCampaignOffer(t, pool, f.tenantID, f.brandID)
	grantID := seedActivatedGrant(t, pool, f, co, "trig-lf18")
	postLockedBonusBet(t, pool, f, "mock-casino", "bet-lf18", "round-lf18", grantID, 500)
	drainRoundLock(t, pool, f, "mock-casino", "round-lf18", 500)

	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-lf18", "", "round-lf18", "game-1", 300, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", winPayload)
		return err
	})
	if !errors.Is(err, ErrLockAlreadyReleased) {
		t.Fatalf("expected ErrLockAlreadyReleased, got %v", err)
	}

	// player_bonus stays at -500 (the bet's own original debit, never
	// released) - the property under test is that it is not ALSO credited
	// a second +500 by this aborted win.
	bal := bonusBalances(t, pool, f)
	if bal["player_bonus"] != -500 {
		t.Fatalf("the lock must never be released a second time; expected player_bonus=-500 (unchanged), got %d", bal["player_bonus"])
	}
}

// --- Ordinary (non-terminal) locked-bonus win, and terminal-grant
// unconditional hold-capture, across every trigger kind doc 08 §16.8
// bullet 1 names (expired/cancelled/forfeited/pending_settlement -
// covering the human directive's named "callback after expiry" and
// "callback after cancellation" scenarios concretely) ---

func TestPostWin_LockedBonusOrdinary_GrantNonTerminal(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	co := seedBonusCampaignOffer(t, pool, f.tenantID, f.brandID)
	grantID := seedActivatedGrant(t, pool, f, co, "trig-ordinary-1")
	postLockedBonusBet(t, pool, f, "mock-casino", "bet-ord-1", "round-ord-1", grantID, 1000)

	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-ord-1", "", "round-ord-1", "game-1", 300, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	var result ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", winPayload)
		return err
	})
	if err != nil {
		t.Fatalf("win: %v", err)
	}
	if result.Outcome != OutcomeSucceeded {
		t.Fatalf("expected success, got %+v", result)
	}

	// Bet posted Dr player_bonus 1000 / Cr player_locked_bonus 1000 at bet
	// time; the win now posts Cr player_bonus 300 (payout) + Cr
	// player_bonus 1000 (lock release) / Dr player_locked_bonus 1000 - net
	// player_bonus = -1000+300+1000 = 300.
	bal := bonusBalances(t, pool, f)
	if bal["player_bonus"] != 300 {
		t.Fatalf("expected player_bonus=300 (net of the 1000 stake debit, 1000 lock release, and 300 payout), got %v", bal)
	}
	if bal["player_locked_bonus"] != 0 {
		t.Fatalf("expected player_locked_bonus released to 0, got %v", bal)
	}
	if status := readGrantStatus(t, pool, f.tenantID, grantID); status != bonus.GrantActivated {
		t.Fatalf("expected grant to remain activated (non-terminal, ordinary case), got %s", status)
	}
	if _, err := readHeldDispositionErr(pool, f.tenantID, *result.LedgerTransactionID); !errors.Is(err, bonus.ErrNotFound) {
		t.Fatalf("expected NO held disposition for a non-terminal-grant win, got err=%v", err)
	}

	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}

func readHeldDispositionErr(pool *db.Pool, tenantID, settlementTxID uuid.UUID) (bonus.HeldDisposition, error) {
	var d bonus.HeldDisposition
	var outErr error
	_ = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = bonus.GetHeldDispositionBySettlementTransaction(ctx, tx, tenantID, settlementTxID)
		outErr = err
		return nil // never abort the read-only probe transaction
	})
	return d, outErr
}

func TestPostWin_LockedBonusTerminalGrant_CapturesUnconditionally(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID)
	}{
		{"expired", func(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID) {
			setGrantTerminal(t, pool, tenantID, grantID, bonus.GrantExpired)
		}},
		{"cancelled", func(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID) {
			setGrantTerminal(t, pool, tenantID, grantID, bonus.GrantCancelled)
		}},
		{"forfeited", func(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID) {
			setGrantTerminal(t, pool, tenantID, grantID, bonus.GrantForfeited)
		}},
		{"pending_settlement", func(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID) {
			setGrantPendingSettlement(t, pool, tenantID, grantID, bonus.TerminalResolutionForfeited)
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			f := seedCasinoFixture(t, pool)
			provider := NewMockCasinoProvider("mock-casino", "EUR")
			registerCasinoCapability(t, pool, f, provider, 100)
			orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

			co := seedBonusCampaignOffer(t, pool, f.tenantID, f.brandID)
			grantID := seedActivatedGrant(t, pool, f, co, "trig-term-"+tc.name)
			roundID := "round-term-" + tc.name
			betTxID := "bet-term-" + tc.name
			postLockedBonusBet(t, pool, f, "mock-casino", betTxID, roundID, grantID, 400)
			tc.setup(t, pool, f.tenantID, grantID)

			winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-term-"+tc.name, "", roundID, "game-1", 250, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
			var result ReceiveCallbackResult
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				result, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", winPayload)
				return err
			})
			if err != nil {
				t.Fatalf("win: %v", err)
			}
			if result.Outcome != OutcomeSucceeded {
				t.Fatalf("expected success, got %+v", result)
			}

			// player_bonus is left at -400 (the bet's own original debit,
			// never offset) because §16.5a's terminal-Grant branch credits
			// the released lock into player_bonus_held, NEVER back into
			// player_bonus (doc 08 §16.5a's destination table, confirmed
			// against the actual code below) - this fixture bypasses the
			// real INV-TG step-1 write-off (bonus.TerminateGrant, which
			// would normally zero any non-locked B(G) in the SAME
			// transaction the terminal trigger fires in); it is a fixture
			// simplification, not a claim about production behavior. The
			// property this test actually asserts is the one that matters
			// here: player_bonus receives NO PART of this terminal-Grant
			// win's value, directly or indirectly.
			bal := bonusBalances(t, pool, f)
			if bal["player_bonus"] != -400 {
				t.Fatalf("player_bonus must never receive a terminal-Grant credit directly (expected -400, the untouched bet-time debit), got %v", bal)
			}
			if bal["player_locked_bonus"] != 0 {
				t.Fatalf("expected player_locked_bonus released to 0 even for a terminal Grant, got %v", bal)
			}
			if bal["player_bonus_held"] != 650 {
				t.Fatalf("expected player_bonus_held=650 (250 payout + 400 released lock), got %v", bal)
			}

			disposition := readHeldDispositionBySettlement(t, pool, f.tenantID, *result.LedgerTransactionID)
			if disposition.Status != bonus.HeldDispositionHeld {
				t.Fatalf("expected disposition status 'held' (no G-2 action decided here), got %s", disposition.Status)
			}
			if disposition.PayoutAmount.Cmp(big.NewInt(250)) != 0 {
				t.Fatalf("payout amount mismatch: got %s", disposition.PayoutAmount)
			}
			if disposition.ReleasedLockAmount.Cmp(big.NewInt(400)) != 0 {
				t.Fatalf("released lock amount mismatch: got %s", disposition.ReleasedLockAmount)
			}
			if disposition.GrantID != grantID {
				t.Fatalf("disposition attributed to wrong grant")
			}

			debits, credits := sumDebitsCredits(t, pool, f.tenantID)
			if debits != credits {
				t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
			}
		})
	}
}

// --- §16.21 site 1: plain lock-rollback recheck ---

// TestPostRollback_PlainLockRollback_RecomputesExposure exercises §16.21
// site 1 end to end: a plain lock-rollback is a TRUE, exact inverse of
// the bet it undoes (the bet's own player_bonus debit and
// player_locked_bonus credit are both inverted, netting every attributed
// balance for this Grant back to exactly what it was before the bet -
// zero, in this fixture, since the bet was the Grant's only activity).
// AOE therefore genuinely reaches empty, and RecheckGrantExposure
// correctly finalizes the Grant to its already-recorded
// terminal_resolution - an uncontested, value-neutral case needing no G-2
// involvement, exactly as §16.10.3's own reasoning describes for this
// shape.
func TestPostRollback_PlainLockRollback_RecomputesExposure(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	co := seedBonusCampaignOffer(t, pool, f.tenantID, f.brandID)
	grantID := seedActivatedGrant(t, pool, f, co, "trig-rollback-1")
	postLockedBonusBet(t, pool, f, "mock-casino", "bet-rb-1", "round-rb-1", grantID, 700)
	setGrantPendingSettlement(t, pool, f.tenantID, grantID, bonus.TerminalResolutionExpired)

	rollbackPayload := provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-rb-1", "bet-rb-1", "round-rb-1", "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)
	var result ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", rollbackPayload)
		return err
	})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if result.Outcome != OutcomeSucceeded {
		t.Fatalf("expected success, got %+v", result)
	}

	// A bet's rollback is a TRUE inverse: player_bonus's -700 debit (bet)
	// and +700 credit (rollback) net to 0, and player_locked_bonus's +700
	// (bet) and -700 (rollback) also net to 0 - the round is left exactly
	// as if the bet never happened.
	bal := bonusBalances(t, pool, f)
	if bal["player_locked_bonus"] != 0 {
		t.Fatalf("expected player_locked_bonus back to 0 (restored, not resurrected), got %v", bal)
	}
	if bal["player_bonus"] != 0 {
		t.Fatalf("expected player_bonus net to exactly 0 (the bet's own debit and the rollback's credit cancel out), got %v", bal)
	}

	// §16.21 site 1's own wiring: the reversal is attributed to the Grant
	// (bonus_settlement.go), so ComputeAOE's Component 1 nets to zero and
	// RecheckGrantExposure correctly finalizes the Grant to its
	// already-recorded terminal_resolution.
	status := readGrantStatus(t, pool, f.tenantID, grantID)
	if status != bonus.GrantExpired {
		t.Fatalf("RecheckGrantExposure's own wiring did not fire as designed: expected the grant finalized to expired, got %s", status)
	}

	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}

// --- §16.15/§16.21 site 3: held-win rollback ---

func TestPostRollback_HeldWinRollback_VoidsDispositionAndFinalizesGrant(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	co := seedBonusCampaignOffer(t, pool, f.tenantID, f.brandID)
	grantID := seedActivatedGrant(t, pool, f, co, "trig-heldrb-1")
	postLockedBonusBet(t, pool, f, "mock-casino", "bet-heldrb-1", "round-heldrb-1", grantID, 400)
	setGrantPendingSettlement(t, pool, f.tenantID, grantID, bonus.TerminalResolutionExpired)

	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-heldrb-1", "", "round-heldrb-1", "game-1", 250, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	var winResult ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		winResult, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", winPayload)
		return err
	})
	if err != nil {
		t.Fatalf("win: %v", err)
	}
	if status := readGrantStatus(t, pool, f.tenantID, grantID); status != bonus.GrantPendingSettlement {
		t.Fatalf("expected grant to remain pending_settlement after unconditional capture (§16.21: never called from postWin's hold-capture path), got %s", status)
	}

	rollbackPayload := provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-heldrb-1", "win-heldrb-1", "round-heldrb-1", "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)
	var rbResult ReceiveCallbackResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rbResult, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", rollbackPayload)
		return err
	})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rbResult.Outcome != OutcomeSucceeded {
		t.Fatalf("expected success, got %+v", rbResult)
	}

	bal := bonusBalances(t, pool, f)
	if bal["player_bonus_held"] != 0 {
		t.Fatalf("expected player_bonus_held drained to 0, got %v", bal)
	}
	if bal["player_locked_bonus"] != 0 {
		t.Fatalf("player_locked_bonus must NEVER be resurrected by a held-win rollback, got %v", bal)
	}
	if bal["house_gaming"] != 0 {
		t.Fatalf("expected house_gaming net zero (paid W+X out at capture, took it back at rollback), got %v", bal)
	}

	disposition := readHeldDispositionBySettlement(t, pool, f.tenantID, *winResult.LedgerTransactionID)
	if disposition.Status != bonus.HeldDispositionVoidedByRollback {
		t.Fatalf("expected voided_by_rollback, got %s", disposition.Status)
	}
	if disposition.ResolutionLedgerTransactionID == nil || *disposition.ResolutionLedgerTransactionID != *rbResult.LedgerTransactionID {
		t.Fatalf("expected resolution_ledger_transaction_id to name the reversal")
	}

	if status := readGrantStatus(t, pool, f.tenantID, grantID); status != bonus.GrantExpired {
		t.Fatalf("expected the grant finalized to its recorded terminal_resolution (expired) once its only AOE component cleared, got %s", status)
	}

	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}

// TestPostRollback_HeldWinRollback_DuplicateIsIdempotent is the human
// directive's "duplicate rollback" scenario, exercised specifically
// against the NEW held-disposition-aware path.
func TestPostRollback_HeldWinRollback_DuplicateIsIdempotent(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	co := seedBonusCampaignOffer(t, pool, f.tenantID, f.brandID)
	grantID := seedActivatedGrant(t, pool, f, co, "trig-dup-1")
	postLockedBonusBet(t, pool, f, "mock-casino", "bet-dup-1", "round-dup-1", grantID, 400)
	setGrantTerminal(t, pool, f.tenantID, grantID, bonus.GrantForfeited)

	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-dup-1", "", "round-dup-1", "game-1", 250, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", winPayload)
		return err
	})
	if err != nil {
		t.Fatalf("win: %v", err)
	}

	rollbackPayload := provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-dup-1", "win-dup-1", "round-dup-1", "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)
	var first, second ReceiveCallbackResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		first, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", rollbackPayload)
		return err
	})
	if err != nil {
		t.Fatalf("first rollback: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		second, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", rollbackPayload)
		return err
	})
	if err != nil {
		t.Fatalf("second (redelivered) rollback: %v", err)
	}
	if *first.LedgerTransactionID != *second.LedgerTransactionID {
		t.Fatalf("redelivery must return the SAME reversal transaction id, got %s vs %s", first.LedgerTransactionID, second.LedgerTransactionID)
	}

	dispositions, err := listHeldDispositionsByGrant(pool, f.tenantID, grantID)
	if err != nil {
		t.Fatalf("list dispositions: %v", err)
	}
	if len(dispositions) != 1 || dispositions[0].Status != bonus.HeldDispositionVoidedByRollback {
		t.Fatalf("expected exactly one voided disposition, got %+v", dispositions)
	}

	bal := bonusBalances(t, pool, f)
	if bal["player_bonus_held"] != 0 {
		t.Fatalf("redelivery must never double-reverse, player_bonus_held=%d", bal["player_bonus_held"])
	}
}

func listHeldDispositionsByGrant(pool *db.Pool, tenantID, grantID uuid.UUID) ([]bonus.HeldDisposition, error) {
	var out []bonus.HeldDisposition
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = bonus.ListHeldDispositionsByGrant(ctx, tx, tenantID, grantID)
		return err
	})
	return out, err
}

// TestPostRollback_HeldWinRollback_ConcurrentDistinctRollbacks is the
// human directive's real-Postgres-concurrency requirement, applied to
// §16.15's own "two genuinely distinct rollback attempts naming the same
// held record" proof: two DIFFERENT rollback references race for the
// SAME held disposition. Exactly one must win. The loser is rejected
// either by the PRE-EXISTING (unmodified by this dispatch) original-
// transaction-row FOR UPDATE lock + existingReversalProviderTxID check
// (ErrAlreadyRolledBack - deterministic here, since that lock fully
// serializes the two attempts before either reaches this dispatch's own
// code) or, in principle, by this dispatch's own held-disposition CAS
// (ErrHeldDispositionAlreadyVoided) - either is a correct rejection;
// TestPostRollback_HeldWinRollback_RollbackRacesDirectResolution below
// isolates the NEW mechanism specifically, racing a rollback against a
// concurrent, non-rollback resolution attempt the pre-existing guard
// cannot see at all.
func TestPostRollback_HeldWinRollback_ConcurrentDistinctRollbacks(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	co := seedBonusCampaignOffer(t, pool, f.tenantID, f.brandID)
	grantID := seedActivatedGrant(t, pool, f, co, "trig-race-1")
	postLockedBonusBet(t, pool, f, "mock-casino", "bet-race-1", "round-race-1", grantID, 400)
	setGrantTerminal(t, pool, f.tenantID, grantID, bonus.GrantForfeited)

	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-race-1", "", "round-race-1", "game-1", 250, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", winPayload)
		return err
	})
	if err != nil {
		t.Fatalf("win: %v", err)
	}

	const attempts = 2
	results := make([]error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rbTxID := fmt.Sprintf("rollback-race-%d", i)
			payload := provider.CallbackPayload(f.tenantID, CallbackEventRollback, rbTxID, "win-race-1", "round-race-1", "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)
			results[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
				return err
			})
		}(i)
	}
	wg.Wait()

	successCount, deniedCount := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			successCount++
		case errors.Is(err, ErrHeldDispositionAlreadyVoided), errors.Is(err, ErrAlreadyRolledBack):
			deniedCount++
		default:
			t.Fatalf("unexpected error from concurrent rollback: %v", err)
		}
	}
	if successCount != 1 || deniedCount != attempts-1 {
		t.Fatalf("expected exactly 1 success and %d denial(s), got successCount=%d deniedCount=%d (%v)", attempts-1, successCount, deniedCount, results)
	}

	dispositions, err := listHeldDispositionsByGrant(pool, f.tenantID, grantID)
	if err != nil {
		t.Fatalf("list dispositions: %v", err)
	}
	if len(dispositions) != 1 || dispositions[0].Status != bonus.HeldDispositionVoidedByRollback {
		t.Fatalf("expected exactly ONE disposition, voided exactly once, got %+v", dispositions)
	}

	bal := bonusBalances(t, pool, f)
	if bal["player_bonus_held"] != 0 {
		t.Fatalf("no interleaving may leave a residual: player_bonus_held=%d", bal["player_bonus_held"])
	}
	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated under concurrency: debits=%d credits=%d", debits, credits)
	}
}

// TestPostRollback_HeldWinRollback_RollbackRacesDirectResolution
// isolates THIS dispatch's own new protection (HR-25's lock order,
// §16.15's first proof bullet: "rollback races a human resolution of the
// same held record") - a race the pre-existing
// existingReversalProviderTxID/ErrAlreadyRolledBack guard cannot see at
// all, because the competing transaction is not a rollback. Simulates
// the resolution side directly via the low-level, ungated
// bonus.ResolveHeldDisposition (the SEP-1/four-eyes workflow itself is
// not this test's subject), but composes the EXACT HR-25 lock order
// (advisory lock, then the disposition's own row lock) real staff-facing
// resolution code must use.
func TestPostRollback_HeldWinRollback_RollbackRacesDirectResolution(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	co := seedBonusCampaignOffer(t, pool, f.tenantID, f.brandID)
	grantID := seedActivatedGrant(t, pool, f, co, "trig-race2-1")
	postLockedBonusBet(t, pool, f, "mock-casino", "bet-race2-1", "round-race2-1", grantID, 400)
	setGrantTerminal(t, pool, f.tenantID, grantID, bonus.GrantForfeited)

	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-race2-1", "", "round-race2-1", "game-1", 250, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	var winResult ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		winResult, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", winPayload)
		return err
	})
	if err != nil {
		t.Fatalf("win: %v", err)
	}
	disposition := readHeldDispositionBySettlement(t, pool, f.tenantID, *winResult.LedgerTransactionID)
	resolutionTxID := postManualAdjustment(t, pool, f, 1)

	var rollbackErr, resolutionErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		rollbackPayload := provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-race2-1", "win-race2-1", "round-race2-1", "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)
		rollbackErr = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", rollbackPayload)
			return err
		})
	}()
	go func() {
		defer wg.Done()
		resolutionErr = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if err := bonus.AdvisoryLockGrant(ctx, tx, f.tenantID, grantID); err != nil {
				return err
			}
			d, err := bonus.LockHeldDispositionForUpdate(ctx, tx, disposition.ID)
			if err != nil {
				return err
			}
			if d.Status != bonus.HeldDispositionHeld {
				return errRaceLost
			}
			_, err = bonus.ResolveHeldDisposition(ctx, tx, f.tenantID, disposition.ID, bonus.HeldDispositionResolvedReforfeit, nil, nil, resolutionTxID, time.Now().UTC())
			return err
		})
	}()
	wg.Wait()

	successCount, deniedCount := 0, 0
	for _, err := range []error{rollbackErr, resolutionErr} {
		switch {
		case err == nil:
			successCount++
		case errors.Is(err, ErrHeldDispositionAlreadyVoided), errors.Is(err, ErrHeldDispositionRollbackUnsupported), errors.Is(err, errRaceLost):
			deniedCount++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successCount != 1 || deniedCount != 1 {
		t.Fatalf("expected exactly one winner: rollbackErr=%v resolutionErr=%v", rollbackErr, resolutionErr)
	}

	dispositions, err := listHeldDispositionsByGrant(pool, f.tenantID, grantID)
	if err != nil {
		t.Fatalf("list dispositions: %v", err)
	}
	if len(dispositions) != 1 {
		t.Fatalf("expected exactly one disposition, got %+v", dispositions)
	}
	if dispositions[0].Status == bonus.HeldDispositionHeld {
		t.Fatalf("expected the disposition to have moved off 'held' exactly once, got %s", dispositions[0].Status)
	}

	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated under concurrency: debits=%d credits=%d", debits, credits)
	}
}

var errRaceLost = errors.New("test: lost the disposition race")

// --- LF-10 (§16.19 scenario 8, §16.20): rollback of an ALREADY-RESOLVED
// held disposition must fail closed, never guess, never silently reuse
// §16.15's still-held mechanism. ---

func TestPostRollback_HeldWinRollback_AlreadyResolved_FailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	co := seedBonusCampaignOffer(t, pool, f.tenantID, f.brandID)
	grantID := seedActivatedGrant(t, pool, f, co, "trig-lf10-1")
	postLockedBonusBet(t, pool, f, "mock-casino", "bet-lf10-1", "round-lf10-1", grantID, 400)
	setGrantTerminal(t, pool, f.tenantID, grantID, bonus.GrantForfeited)

	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-lf10-1", "", "round-lf10-1", "game-1", 250, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	var winResult ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		winResult, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", winPayload)
		return err
	})
	if err != nil {
		t.Fatalf("win: %v", err)
	}
	disposition := readHeldDispositionBySettlement(t, pool, f.tenantID, *winResult.LedgerTransactionID)

	// Move the disposition to resolved_reforfeit directly (the low-level
	// CAS, deliberately bypassing the SEP-1/four-eyes
	// ResolveHeldDispositionAction workflow, which is not this test's
	// subject) - simulating a human having already applied G-2's answer
	// before a late rollback arrives.
	resolutionTxID := postManualAdjustment(t, pool, f, 1)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := bonus.ResolveHeldDisposition(ctx, tx, f.tenantID, disposition.ID, bonus.HeldDispositionResolvedReforfeit, nil, nil, resolutionTxID, time.Now().UTC())
		return err
	})
	if err != nil {
		t.Fatalf("simulate resolution: %v", err)
	}

	balBefore := bonusBalances(t, pool, f)

	rollbackPayload := provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-lf10-1", "win-lf10-1", "round-lf10-1", "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", rollbackPayload)
		return err
	})
	if !errors.Is(err, ErrHeldDispositionRollbackUnsupported) {
		t.Fatalf("expected ErrHeldDispositionRollbackUnsupported (LF-10, fails closed), got %v", err)
	}

	balAfter := bonusBalances(t, pool, f)
	if balAfter["player_bonus_held"] != balBefore["player_bonus_held"] || balAfter["house_gaming"] != balBefore["house_gaming"] {
		t.Fatalf("a fail-closed LF-10 rejection must post NOTHING: before=%v after=%v", balBefore, balAfter)
	}

	// The disposition itself must remain exactly as the (simulated)
	// resolution left it - never silently mutated by the failed attempt.
	after := readHeldDispositionBySettlement(t, pool, f.tenantID, *winResult.LedgerTransactionID)
	if after.Status != bonus.HeldDispositionResolvedReforfeit {
		t.Fatalf("disposition status must be untouched by a rejected rollback, got %s", after.Status)
	}
}

// --- Provider-native bonus coexistence (ADR 0033 §2.1) ---

// TestPostWin_PlainCashWin_NeverTouchesBonusMachinery confirms the
// structural property this dispatch relies on rather than special-cases:
// a win whose origin resolves to player_cash (no lock, no Grant
// attribution possible) never reaches any Grant lookup, advisory lock, or
// bonus package call at all - the exact coexistence property a
// provider-native promo (baked into the settlement amount, with no
// platform Grant behind it) depends on. Also re-confirms the pre-
// existing pure-cash regression path is byte-for-byte unaffected.
func TestPostWin_PlainCashWin_NeverTouchesBonusMachinery(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-native-1", "", "round-native-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", betPayload)
		return err
	})
	if err != nil {
		t.Fatalf("bet: %v", err)
	}

	// A provider-native promo would already be baked into this amount
	// (e.g. a free-round win multiplier) - the platform never sees it as
	// anything but an ordinary cash win, exactly as ADR 0033 §2.1 requires.
	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-native-1", "", "round-native-1", "game-1", 2500, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	var result ReceiveCallbackResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", winPayload)
		return err
	})
	if err != nil {
		t.Fatalf("win: %v", err)
	}
	if result.Outcome != OutcomeSucceeded {
		t.Fatalf("expected success, got %+v", result)
	}
	if balance := cashBalance(t, pool, f); balance != 6500 {
		t.Fatalf("expected cash balance 6500 (5000 - 1000 + 2500), got %d", balance)
	}
	if !auditActionExists(t, pool, f.tenantID, "casino_win.posted") {
		t.Fatal("expected a casino_win.posted audit record")
	}
	if auditActionExists(t, pool, f.tenantID, "casino_win.captured_pending_g2") {
		t.Fatal("a plain cash win must NEVER be captured pending G-2 - no Grant, no correlation to any bonus_held_dispositions row")
	}

	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}
