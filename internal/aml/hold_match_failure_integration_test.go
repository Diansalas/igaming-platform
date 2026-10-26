//go:build integration

package aml_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

// d1 (ledger-finance LOW-3, money path): if the hold-id lookup ERRORS after the probe said "held", the verdict must be EvalFailed (a
// retryable, fail-closed deny), never a definite hold. A definite hold with ScreeningOnly unset made the payout claim REJECT the withdrawal
// (DenyForCompliance, funds returned) and the sweeper escalate. Through kyc.RetryableOnHoldEvalFailure the verdict becomes `unavailable`:
// the approved withdrawal stays approved and is retried (no rejection, no funds movement).
func TestD1_Low3_HoldLookupErrorAfterPositiveProbe_IsRetryableEvalFailure_NeverADefiniteHold(t *testing.T) {
	c := newCaseWorld(t, "under_platform_licence")
	t.Cleanup(aml.SetHoldProbeForTest(func(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, aml.Operation) (bool, error) {
		return true, nil
	}))
	t.Cleanup(aml.SetHoldMatchForTest(func(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, aml.Operation) (uuid.UUID, bool, error) {
		return uuid.Nil, false, errors.New("injected lookup fault")
	}))
	for _, alertInTx := range []bool{true, false} {
		err := c.RT.WithTenant(bg, c.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			var v aml.HoldVerdict
			var herr error
			if alertInTx {
				v, herr = aml.EvaluateHold(ctx, tx, c.Tenant, c.P.PersonID, c.P.AccountID, aml.OpWithdrawal)
			} else {
				v, herr = aml.EvaluateHoldDeferringAlert(ctx, tx, c.Tenant, c.P.PersonID, c.P.AccountID, aml.OpWithdrawal)
			}
			if herr != nil {
				t.Errorf("alertInTx=%v: %v", alertInTx, herr)
			}
			if !v.EvalFailed || v.Held || v.ScreeningOnly || !v.Denied() {
				t.Errorf("alertInTx=%v: the verdict must be EvalFailed only (denied, retryable), got %+v", alertInTx, v)
			}
			d := kyc.EnforcementDecision{Hold: v, KYCOutcome: kyc.OutcomePassed}.RetryableOnHoldEvalFailure()
			if d.TrueOutcome != kyc.OutcomeUnavailable {
				t.Errorf("alertInTx=%v: the payout claim must PARK (unavailable), not reject: %+v", alertInTx, d)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// A lookup that SUCCEEDS and finds no officer hold is still the screening-only wait (park); one that finds a hold is a definite hold.
	t.Cleanup(aml.SetHoldMatchForTest(func(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, aml.Operation) (uuid.UUID, bool, error) {
		return uuid.Nil, false, nil
	}))
	_ = c.RT.WithTenant(bg, c.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		v, _ := aml.EvaluateHold(ctx, tx, c.Tenant, c.P.PersonID, c.P.AccountID, aml.OpWithdrawal)
		if !v.Held || !v.ScreeningOnly || v.EvalFailed {
			t.Errorf("a successful lookup with no officer hold is the screening-only wait: %+v", v)
		}
		return nil
	})
}
