//go:build integration

package launchgov_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

// credit posts a manual-adjustment credit to the wallet's player_cash account (fixture).
func (w *world) credit(tenantID, walletID uuid.UUID, amount int64, contra ledger.AccountType) {
	w.t.Helper()
	w.creditAsset(tenantID, walletID, "EUR", amount, contra)
}

// creditAsset is credit for an explicit asset (the wallet must be a wallet of that asset).
func (w *world) creditAsset(tenantID, walletID uuid.UUID, asset string, amount int64, contra ledger.AccountType) {
	w.t.Helper()
	reason := "launchgov test funding"
	err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cash, err := ledger.GetOrCreateAccount(ctx, tx, tenantID, &walletID, ledger.AccountPlayerCash, asset)
		if err != nil {
			return err
		}
		adj, err := ledger.GetOrCreateAccount(ctx, tx, tenantID, nil, contra, asset)
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: tenantID, TransactionType: ledger.TxManualAdjustment,
			IdempotencyKey: "lgov-fund-" + uuid.NewString(), CorrelationID: uuid.New(), ReasonCode: &reason,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: adj, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: cash, Direction: ledger.Credit, Amount: amount},
			},
		})
		return err
	})
	if err != nil {
		w.t.Fatalf("fund wallet: %v", err)
	}
}

// R-PLAYER-FUNDS reports per-asset NUMERIC sums as DECIMAL STRINGS: two credits of 9e18 minor
// units sum to 1.8e19, which overflows int64 (max about 9.22e18) and is not exact as a float64.
// It also carries the stranded-funds and omissions notices.
func TestFundsCheck_PerAssetSumsAreExactDecimalStrings_AndNoticesPresent(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	b1, b2 := w.activeBrand(tid), w.activeBrand(tid)
	_, w1 := w.playerWithWallet(tid, b1)
	_, w2 := w.playerWithWallet(tid, b2)
	sub1 := launchgov.Subject{TenantID: tid, BrandID: &b1}

	// Empty: pass, omissions notice still shown, no balances.
	rd, err := w.svc.EvaluateReadiness(ctx, launchgov.EvalInput{Subject: sub1, Action: launchgov.ActionClose})
	if err != nil || len(rd.NonPassing()) != 0 || len(rd.Balances) != 0 || !containsStr(rd.Notices, launchgov.NoticeFundsOmissions) {
		t.Fatalf("empty brand: %+v err=%v", rd, err)
	}
	if containsStr(rd.Notices, launchgov.NoticeFundsStranded) {
		t.Fatal("the stranded-funds notice must not show without funds")
	}

	const big = int64(9_000_000_000_000_000_000)
	_, w1b := w.playerWithWallet(tid, b1)
	w.credit(tid, w1, big, ledger.AccountHouseGaming)
	w.credit(tid, w1b, big, ledger.AccountPSPClearing)
	w.credit(tid, w2, 7, ledger.AccountManualAdjustment)
	rd, err = w.svc.EvaluateReadiness(ctx, launchgov.EvalInput{Subject: sub1, Action: launchgov.ActionClose})
	if err != nil {
		t.Fatal(err)
	}
	if got := rd.Balances["EUR"]; got != "18000000000000000000" {
		t.Fatalf("brand 1 EUR sum must be the exact decimal string, got %q", got)
	}
	if rd.Info["players_with_non_zero_balance"] != 2 {
		t.Fatalf("the player count is kept: %+v", rd.Info)
	}
	if !containsStr(rd.Notices, launchgov.NoticeFundsStranded) || !strings.Contains(launchgov.NoticeFundsStranded, "HD-CTF-4") {
		t.Fatalf("the stranded-funds notice (HD-CTF-4) must be shown: %v", rd.Notices)
	}
	// Tenant-wide includes both brands exactly.
	rdT, err := w.svc.EvaluateReadiness(ctx, launchgov.EvalInput{Subject: launchgov.Subject{TenantID: tid}, Action: launchgov.ActionClose})
	if err != nil {
		t.Fatal(err)
	}
	if got := rdT.Balances["EUR"]; got != "18000000000000000007" {
		t.Fatalf("tenant EUR sum: %q", got)
	}
	// The hash does not depend on the live balances.
	if rd.Hash() == "" {
		t.Fatal("hash")
	}
}

// Slice-3 LOW item: TWO assets in one brand. Each asset is summed on its own (never added across
// assets), exactly, as decimal strings; a zero-sum asset is absent; an 18-exponent-sized amount
// does not overflow. The notice follows the action: closure shows the stranded text, a suspension
// shows the distinct "inaccessible, not stranded" text and never the stranded one.
func TestFundsCheck_TwoAssets_SummedSeparately_AndSuspensionNoticeIsDistinct(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	b := w.activeBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &b}
	_, eur1 := w.playerWithWallet(tid, b)
	_, eur2 := w.playerWithWallet(tid, b)
	usd1 := w.walletOf(tid, b, "USD")
	usd2 := w.walletOf(tid, b, "USD")
	usdZero := w.walletOf(tid, b, "USD")
	const big = int64(9_000_000_000_000_000_000)
	w.credit(tid, eur1, big, ledger.AccountHouseGaming)
	w.credit(tid, eur2, big, ledger.AccountPSPClearing)
	w.creditAsset(tid, usd1, "USD", 5, ledger.AccountManualAdjustment)
	w.creditAsset(tid, usd2, "USD", 11, ledger.AccountPSPClearing)
	_ = usdZero // a wallet with no movement contributes nothing

	rd, err := w.svc.EvaluateReadiness(ctx, launchgov.EvalInput{Subject: sub, Action: launchgov.ActionClose})
	if err != nil {
		t.Fatal(err)
	}
	if len(rd.Balances) != 2 || rd.Balances["EUR"] != "18000000000000000000" || rd.Balances["USD"] != "16" {
		t.Fatalf("each asset must be summed on its own as an exact decimal string: %v", rd.Balances)
	}
	if rd.Info["players_with_non_zero_balance"] != 4 {
		t.Fatalf("players with a non-zero balance across both assets: %v", rd.Info)
	}
	if !containsStr(rd.Notices, launchgov.NoticeFundsStranded) || containsStr(rd.Notices, launchgov.NoticeFundsInaccessibleOnSuspension) {
		t.Fatalf("closure shows the stranded notice only: %v", rd.Notices)
	}

	rs, err := w.svc.EvaluateReadiness(ctx, launchgov.EvalInput{Subject: sub, Action: launchgov.ActionSuspend})
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(rs.Notices, launchgov.NoticeFundsInaccessibleOnSuspension) || containsStr(rs.Notices, launchgov.NoticeFundsStranded) {
		t.Fatalf("suspension shows the inaccessible-not-stranded notice and NEVER the stranded one: %v", rs.Notices)
	}
	if !containsStr(rs.Notices, launchgov.NoticeFundsOmissions) {
		t.Fatalf("the omissions notice is shown for a suspension too: %v", rs.Notices)
	}
	if strings.Contains(launchgov.NoticeFundsInaccessibleOnSuspension, "HD-CTF-4") || !strings.Contains(launchgov.NoticeFundsInaccessibleOnSuspension, "not stranded") {
		t.Fatal("the suspension notice must say not stranded and must not engage the unclaimed-funds policy")
	}
	if launchgov.NoticeFundsInaccessibleOnSuspension == launchgov.NoticeFundsStranded {
		t.Fatal("the two notices must be distinct")
	}
}

// walletOf registers one more player in the brand and opens a wallet of the asset for it.
func (w *world) walletOf(tenantID, brandID uuid.UUID, asset string) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		br, err := identity.GetBrandByID(ctx, tx, brandID)
		if err != nil {
			return err
		}
		p, err := identity.RegisterPlayer(ctx, tx, br, "lgov-"+uuid.NewString()+"@fixture.invalid", "!x")
		if err != nil {
			return err
		}
		wl, err := wallet.GetOrCreate(ctx, tx, tenantID, brandID, p.ID, asset)
		id = wl.ID
		return err
	}); err != nil {
		w.t.Fatalf("seed %s wallet: %v", asset, err)
	}
	return id
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
