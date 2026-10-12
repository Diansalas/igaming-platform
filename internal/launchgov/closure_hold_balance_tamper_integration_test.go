//go:build integration

package launchgov_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// Security review C1 (ADR 0113 slice 2): wallet_balance_projection is runtime-writable, so LA025 must
// not trust it. As igaming_runtime, the hold projection row of a stranded hold is zeroed
// (debit_total = credit_total, the executed attack); the brand and the tenant closure are still
// refused LA025 (the balance is rebuilt from ledger_entries) and the Go arm still counts and refuses.
func TestLA025_ProjectionTamper_StillRefused(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	b := w.activeBrand(tid)
	_, wl := w.playerWithWallet(tid, b)
	w.strandHold(tid, wl, 900)
	var tampered int64
	if err := w.rt.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE wallet_balance_projection SET debit_total = credit_total
			WHERE wallet_id = $1 AND account_type = 'player_withdrawal_hold'`, wl)
		tampered = tag.RowsAffected()
		return err
	}); err != nil || tampered != 1 {
		t.Fatalf("precondition: the runtime role can zero the hold projection row (the attack): rows=%d err=%v", tampered, err)
	}
	wantLA025(t, "brand closure over a projection-zeroed stranded hold", launchfix.TrySetBrandStatus(ctx, tid, b, "closed"), 1)
	wantLA025(t, "tenant closure over a projection-zeroed stranded hold", launchfix.TrySetTenantStatus(ctx, tid, "closed"), 1)
	sub := launchgov.Subject{TenantID: tid, BrandID: &b}
	rd, err := w.svc.EvaluateReadiness(ctx, launchgov.EvalInput{Subject: sub, Action: launchgov.ActionClose})
	if err != nil || rd.Info["withdrawal_hold_accounts_non_zero"] != 1 {
		t.Fatalf("the Go arm must rebuild from the ledger too: %+v err=%v", rd.Info, err)
	}
	in := launchgov.RequestInput{Subject: sub, Action: launchgov.ActionClose, ReasonCode: "offboarding"}
	in.AcknowledgedCheckCodes = w.nonPassing(in)
	var pe *launchgov.PreconditionError
	if _, err := w.svc.Request(ctx, w.req, in); !errors.Is(err, launchgov.ErrWithdrawalsHolding) || !errors.As(err, &pe) ||
		!contains(pe.Codes, launchgov.H6HoldBalanceNonZero) {
		t.Fatalf("the Go arm must refuse the closure over the projection-zeroed hold: %v", err)
	}
}
