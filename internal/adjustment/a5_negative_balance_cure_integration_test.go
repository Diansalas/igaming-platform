//go:build integration

// A5 / owner decision 13 ("block until cleared by a governed compensating action"): a wallet driven
// negative by a chargeback is blocked for play (wallet_negative_balance); a governed K2 credit (four-eyes,
// ADR 0100) of the shortfall clears it, and the same wallet is eligible again with no flag to reset.
package adjustment

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/testsupport/negbal"
)

func TestA5Neg_GovernedCreditClearsTheBlock(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1, authorizedAsset: true})
	ctx := context.Background()
	if err := w.pool.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := negbal.Chargeback(ctx, tx, w.Tenant, w.Wallet, w.Asset, 40)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	eligible := func() rg.Decision {
		t.Helper()
		var d rg.Decision
		if err := w.pool.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			d, err = rg.EvaluateEligibility(ctx, tx, rg.EligibilityParams{TenantID: w.Tenant, BrandID: w.Brand, PlayerAccountID: w.Player, WalletID: w.Wallet})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := eligible(); d.Allowed || d.Code != rg.CodeWalletNegativeBalance {
		t.Fatalf("a negative wallet must be blocked: %+v", d)
	}
	// The governed compensating action: a K2 credit of the shortfall, initiated by F1, approved by F2.
	r, err := w.svc.Submit(ctxFor(w.F1), w.target(), w.credit(40, ReasonOperationalErrorCorrection), Meta{RequestID: "a5-cure"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if d := eligible(); d.Allowed {
		t.Fatal("a submitted (not executed) credit must not lift the block")
	}
	out, err := w.svc.Decide(ctxFor(w.F2), w.target(), r.ID, DecisionInput{Decision: DecisionApprove, PayloadHash: r.PayloadHash, ReasonCode: "a5-cure"}, Meta{RequestID: "a5-cure"})
	if err != nil || !out.Executed {
		t.Fatalf("the governed credit must execute: %+v %v", out, err)
	}
	if d := eligible(); !d.Allowed {
		t.Fatalf("after the governed credit the wallet is eligible again: %+v", d)
	}
}
