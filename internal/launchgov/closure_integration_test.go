//go:build integration

package launchgov_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

// playerWithWallet registers a player on the (active) brand and returns its account and wallet ids.
func (w *world) playerWithWallet(tenantID, brandID uuid.UUID) (player, walletID uuid.UUID) {
	w.t.Helper()
	ctx := context.Background()
	if err := w.owner.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		br, err := identity.GetBrandByID(ctx, tx, brandID)
		if err != nil {
			return err
		}
		p, err := identity.RegisterPlayer(ctx, tx, br, "lgov-"+uuid.NewString()+"@fixture.invalid", "!x")
		if err != nil {
			return err
		}
		wl, err := wallet.GetOrCreate(ctx, tx, tenantID, brandID, p.ID, "EUR")
		if err != nil {
			return err
		}
		player, walletID = p.ID, wl.ID
		return nil
	}); err != nil {
		w.t.Fatalf("seed player + wallet: %v", err)
	}
	return player, walletID
}

// withdrawal inserts a withdrawal request in the given state (owner, tenant scope: a fixture).
func (w *world) withdrawal(tenantID, brandID, player, walletID uuid.UUID, state string) {
	w.t.Helper()
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// A LEGACY (NULL-binding) row: the fixture is about the state, not the payout binding (the
		// repo's sanctioned integration-only helper; owner role).
		return pitest.WithoutBindingGuard(ctx, tx, func() error {
			_, err := tx.Exec(ctx, `INSERT INTO withdrawal_requests (tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, state, idempotency_key)
				VALUES ($1, $2, $3, $4, 'EUR', 1000, $5, $6)`, tenantID, brandID, player, walletID, state, "lgov-"+uuid.NewString())
			return err
		})
	}); err != nil {
		w.t.Fatalf("seed withdrawal (%s): %v", state, err)
	}
}

// LA023 / H-6: no closure while a withdrawal request of the subject's players holds funds, for a
// brand and for a tenant; checked at the request and again under the subject locks at execution;
// a terminal withdrawal does not block; another brand's holds do not block this brand.
func TestClosure_HoldBearingWithdrawals_RefuseClosure_LA023(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	b1, b2 := w.activeBrand(tid), w.activeBrand(tid)
	s1 := launchgov.Subject{TenantID: tid, BrandID: &b1}
	s2 := launchgov.Subject{TenantID: tid, BrandID: &b2}
	st := launchgov.Subject{TenantID: tid}
	p1, w1 := w.playerWithWallet(tid, b1)

	// A completed withdrawal holds nothing.
	w.withdrawal(tid, b1, p1, w1, "completed")
	w.withdrawal(tid, b1, p1, w1, "cancelled")
	rd, err := w.svc.EvaluateReadiness(ctx, launchgov.EvalInput{Subject: s1, Action: launchgov.ActionClose})
	if err != nil || rd.Info["hold_bearing_withdrawals"] != 0 {
		t.Fatalf("terminal withdrawals hold nothing: %+v err=%v", rd, err)
	}

	// A pending (hold-bearing) one: closing brand 1 and the tenant is refused at the request; brand 2 is not.
	w.withdrawal(tid, b1, p1, w1, "pending_review")
	rd, _ = w.svc.EvaluateReadiness(ctx, launchgov.EvalInput{Subject: s1, Action: launchgov.ActionClose})
	if rd.Info["hold_bearing_withdrawals"] != 1 || rd.NonPassing()[0] != launchgov.CheckPlayerFunds {
		t.Fatalf("R-PLAYER-FUNDS must flag the hold: %+v", rd)
	}
	for name, sub := range map[string]launchgov.Subject{"brand 1": s1, "the tenant": st} {
		in := launchgov.RequestInput{Subject: sub, Action: launchgov.ActionClose, ReasonCode: "offboarding"}
		in.AcknowledgedCheckCodes = w.nonPassing(in)
		var pe *launchgov.PreconditionError
		if _, err := w.svc.Request(ctx, w.req, in); !errors.Is(err, launchgov.ErrWithdrawalsHolding) || !errors.As(err, &pe) || !contains(pe.Codes, launchgov.H6WithdrawalsHolding) {
			t.Fatalf("closing %s with a hold-bearing withdrawal: %v", name, err)
		}
	}
	if rows := w.audit(tid, "launch_authorisation.request_denied"); len(rows) != 2 || rows[0]["code"] != "LAUNCH_WITHDRAWALS_HOLDING_FUNDS" {
		t.Fatalf("each refusal is audited once: %+v", rows)
	}
	in2 := launchgov.RequestInput{Subject: s2, Action: launchgov.ActionClose, ReasonCode: "offboarding"}
	in2.AcknowledgedCheckCodes = w.nonPassing(in2)
	r2 := w.mustRequest(w.req, in2)
	if _, err := w.approve(w.ap1, r2); err != nil {
		t.Fatal(err)
	}
	if d, err := w.approve(w.ap2, r2); err != nil || !d.Executed {
		t.Fatalf("another brand's holds must not block this brand's closure: %+v err=%v", d, err)
	}
	if got := w.status(s2); got != "closed" {
		t.Fatalf("brand 2 status %s", got)
	}

	// Execution-time check under the locks: the request was filed while there was no hold; a hold
	// appears before the final approval -> refused (rolled back, request stays pending), then after the
	// hold clears the same request executes.
	b3 := w.activeBrand(tid)
	s3 := launchgov.Subject{TenantID: tid, BrandID: &b3}
	p3, w3 := w.playerWithWallet(tid, b3)
	in3 := launchgov.RequestInput{Subject: s3, Action: launchgov.ActionClose, ReasonCode: "offboarding"}
	r3 := w.mustRequest(w.req, in3)
	if _, err := w.approve(w.ap1, r3); err != nil {
		t.Fatal(err)
	}
	w.withdrawal(tid, b3, p3, w3, "requested")
	// The readiness the approver now sees has changed (a funds position appeared), so the seen hash is
	// the fresh one; the executor still refuses on the hold itself.
	_, err = w.approve(w.ap2, r3)
	if !errors.Is(err, launchgov.ErrWithdrawalsHolding) {
		t.Fatalf("a hold appearing before the final approval must refuse the closure: %v", err)
	}
	got, apps, _ := w.svc.GetRequest(ctx, w.ap1, tid, r3.ID)
	if got.Status != launchgov.ReqPending || len(apps) != 1 {
		t.Fatalf("a refused closure stays pending with the earlier approval only: %+v apps=%d", got, len(apps))
	}
	if st := w.status(s3); st != "active" {
		t.Fatalf("brand 3 status %s", st)
	}
	if rows := w.audit(tid, "launch_authorisation.decide_denied"); len(rows) != 1 || rows[0]["code"] != "LAUNCH_WITHDRAWALS_HOLDING_FUNDS" {
		t.Fatalf("one denied row for the refused execution: %+v", rows)
	}
}
