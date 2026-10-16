//go:build integration

// A5 / PAYOUT-AMOUNT-DISPUTE-1. (1) A provider reporting a payout amount OR asset that differs from the
// requested one never posts, on every evidence path that carries an amount (the verified callback and
// the QueryStatus poll): the attempt is parked with its reason, the hold is kept, one audit row and one
// P1 alert are written. (2) The governed staff resolution (migration 0163): the existing M2 force-
// resolution admits both mismatch reasons, ONLY with basis provider_confirmed_out_of_band, for both kinds;
// reconciliation_exhausted is refused (MR010) and nothing moves.
package payments

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// Migration 0163's down refuses (MR099) while an M2 resolution of an amount/asset-mismatch park exists
// (the up/down/up snapshot is in internal/withdrawal's migration test).
func TestA5_Migration0163_DownRefusedWhileAMismatchResolutionExists(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.disputedPayout(300, "amount_asset_mismatch")
	w.executeM2(a.ID, ResolutionM2DeclareNotPaid)
	// 0163 is no longer the newest migration: step down through every later one (their downs succeed on
	// this scratch DB) so the call reaches 0163's own refusal.
	dir := realMigrationsDir(t)
	steps := 1
	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		var v int
		if _, scanErr := fmt.Sscanf(filepath.Base(f), "%d_", &v); scanErr == nil && v > 163 {
			steps++
		}
	}
	// F4 (CHANGED FIXTURE): every E7 since migration 0169 records a withdrawal_gate_decisions row, and 0169's down
	// refuses (WD095) while one exists, so the chain would stop there. On this THROWAWAY scratch database the owner
	// removes them (append-only trigger lifted for that one statement, FORCE RLS lifted for the owner) so the down
	// chain reaches 0163's own refusal, which is what this test pins.
	if err := w.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []string{
			`ALTER TABLE withdrawal_gate_decisions DISABLE TRIGGER withdrawal_gate_decisions_immutable`,
			`ALTER TABLE withdrawal_gate_decisions NO FORCE ROW LEVEL SECURITY`,
			`DELETE FROM withdrawal_gate_decisions`,
			`ALTER TABLE withdrawal_gate_decisions FORCE ROW LEVEL SECURITY`,
			`ALTER TABLE withdrawal_gate_decisions ENABLE TRIGGER withdrawal_gate_decisions_immutable`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("scratch owner: clear the 0169 decision rows: %v", err)
	}
	_, err = w.pool.MigrateDown(context.Background(), dir, steps)
	if err == nil || !strings.Contains(err.Error(), "MR099") {
		t.Fatalf("0163 down must refuse MR099, got %v", err)
	}
}

func TestA5_AmountDispute_EveryPath_ParksNeverPosts(t *testing.T) {
	pool := depositV2ScratchPool(t)
	for i, c := range []struct {
		name   string
		path   string // callback | poll
		amount int64
		asset  string
		reason string
		audit  string
	}{
		{"callback amount", "callback", 499, "EUR", TerminalReasonCallbackAmountAssetMismatch, auditActionPayoutCallbackDispute},
		{"callback asset", "callback", 500, "USD", TerminalReasonCallbackAmountAssetMismatch, auditActionPayoutCallbackDispute},
		{"poll amount", "poll", 499, "EUR", "amount_asset_mismatch", "payments.payout_amount_asset_mismatch"},
		{"poll asset", "poll", 500, "USD", "amount_asset_mismatch", "payments.payout_amount_asset_mismatch"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pid := "mock-a5-aad-" + string(rune('a'+i))
			f, orch, _ := fpOrch(t, pool, pid)
			e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
			ref := "a5-aad-ref-" + string(rune('a'+i))
			wr, a := e.seedBound(t, "a5-aad-"+string(rune('a'+i)), ref)
			before := e.b12Snapshot(t, wr)
			cashBefore := e.a5Cash(t)
			switch c.path {
			case "callback":
				if err := e.r5Callback(a, ref, c.amount, c.asset); err != nil {
					t.Fatalf("callback: %v", err)
				}
			case "poll":
				e.r8StatusSuccess(t, wr, a, ref, c.amount, c.asset)
			}
			got := mustGetAttempt(t, pool, f.tenantID, a.ID)
			if got.State != AttemptDisputed || got.TerminalReason == nil || *got.TerminalReason != c.reason {
				t.Fatalf("want parked %s, got %s %v", c.reason, got.State, got.TerminalReason)
			}
			e.b12AssertNoMoneyMoved(t, b12Out{wr: wr, a: a, before: before})
			if e.a5Cash(t) != cashBefore {
				t.Fatal("a mismatch must never touch the player's cash")
			}
			if n := e.b12AuditCount(t, c.audit, a.ID); n != 1 {
				t.Fatalf("audit %s = %d, want 1", c.audit, n)
			}
			e.b12AssertOneAlert(t, a.ID, c.reason, "USD")
		})
	}
}

func TestA5_AmountDispute_M2_OnlyWithOutOfBandBasis(t *testing.T) {
	for _, reason := range []string{"amount_asset_mismatch", TerminalReasonCallbackAmountAssetMismatch} {
		for _, kind := range []ResolutionKind{ResolutionM2DeclarePaid, ResolutionM2DeclareNotPaid} {
			t.Run(reason+"/"+string(kind), func(t *testing.T) {
				w := newK3World(t, k3Opts{base: 1})
				wr, a := w.disputedPayout(300, reason)
				if a.ProviderReference == nil {
					t.Fatalf("setup: the payout needs a held reference for declare paid (D-3)")
				}
				holdBefore := w.walletBalance("player_withdrawal_hold")
				in := w.m2In(a.ID, kind)
				in.BasisCode = "reconciliation_exhausted"
				_, err := w.request(w.f1, in)
				k3RequireCode(t, err, "MR010")
				if w.withdrawalOf(wr.ID).State != withdrawal.StateSubmitted || w.walletBalance("player_withdrawal_hold") != holdBefore {
					t.Fatal("a refused request must move nothing")
				}
				res := w.executeM2(a.ID, kind) // m2In's default basis: provider_confirmed_out_of_band
				want, wantWR := AttemptSucceeded, withdrawal.StateCompleted
				if kind == ResolutionM2DeclareNotPaid {
					want, wantWR = AttemptDeclined, withdrawal.StateFailed
				}
				after := w.attempt(a.ID)
				if after.State != want || w.withdrawalOf(wr.ID).State != wantWR || res.State != ResolutionExecuted {
					t.Fatalf("attempt=%s withdrawal=%s resolution=%s", after.State, w.withdrawalOf(wr.ID).State, res.State)
				}
				if after.TerminalReason == nil || *after.TerminalReason != reason {
					t.Fatalf("the park reason stays on the attempt (R-7), got %v", after.TerminalReason)
				}
				if w.walletBalance("player_withdrawal_hold") != holdBefore-300 {
					t.Fatalf("the hold is closed exactly once")
				}
				w.assertInvariants()
			})
		}
	}
}
