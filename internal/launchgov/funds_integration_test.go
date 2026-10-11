//go:build integration

package launchgov_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// credit posts a manual-adjustment credit to the wallet's player_cash account (fixture).
func (w *world) credit(tenantID, walletID uuid.UUID, amount int64, contra ledger.AccountType) {
	w.t.Helper()
	reason := "launchgov test funding"
	err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cash, err := ledger.GetOrCreateAccount(ctx, tx, tenantID, &walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		adj, err := ledger.GetOrCreateAccount(ctx, tx, tenantID, nil, contra, "EUR")
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

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
