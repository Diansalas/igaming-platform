//go:build integration

package adjustment

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
)

// ADR 0116 s5.1 (LF): no manual adjustment may seize, zero or refund a held balance. ANY active hold on the wallet's player
// refuses a manual adjustment, at submission and again at execution (a hold placed in between still wins), with a generic
// "account under review" refusal that tells a tenant staff member nothing about AML.

func requestCount(t *testing.T, w *world) int {
	t.Helper()
	var n int
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_adjustment_requests WHERE tenant_id = $1`, w.Tenant).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAMLHold_RefusesManualAdjustment_AtSubmission(t *testing.T) {
	for _, scope := range []aml.HoldScope{aml.ScopeAllValueMovement, aml.ScopeWithdrawalAndPayout} {
		w := newWorld(t, worldOpts{base: 1})
		w.fund(10_000)
		_, hold := amlfix.HoldFor(t, w.pool, w.pool, w.Tenant, w.PlayerPerson, nil, scope)
		_ = hold
		before := requestCount(t, w)
		for _, in := range []SubmitInput{w.credit(2_500, ReasonOperationalErrorCorrection), w.debit(1_000, ReasonExternalInstruction)} {
			_, err := w.submit(w.F1, in)
			if !errors.Is(err, ErrAccountUnderReview) {
				t.Fatalf("scope %s: a held player's adjustment (%s) must be refused: %v", scope, in.Direction, err)
			}
			if ClassifyError(err) != ErrClassConflict {
				t.Fatalf("the refusal must be the generic conflict class, got %s", ClassifyError(err))
			}
		}
		if requestCount(t, w) != before {
			t.Fatal("a refused submission must write no request row")
		}
		if got := w.playerCash(); got != 10_000 {
			t.Fatalf("a held balance is untouched: %d", got)
		}
		// A DIFFERENT player of the same tenant is unaffected.
		p2, _, wallet2 := w.newPlayer()
		_ = p2
		in := w.credit(100, ReasonOperationalErrorCorrection)
		in.WalletID = wallet2
		if _, err := w.submit(w.F1, in); err != nil {
			t.Fatalf("an unheld player's adjustment must still be accepted: %v", err)
		}
	}
}

func TestAMLHold_PlacedAfterSubmission_RefusesAtExecution(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	w.fund(10_000)
	r, err := w.submit(w.F1, w.credit(2_500, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatalf("submit before any hold: %v", err)
	}
	q, hold := amlfix.HoldFor(t, w.pool, w.pool, w.Tenant, w.PlayerPerson, nil, aml.ScopeAllValueMovement)
	out, err := w.decide(w.F2, r, DecisionApprove)
	if err != nil {
		t.Fatalf("the approval itself is recorded: %v", err)
	}
	if out.Executed || out.Request.State != StateRefusedAtExecution || out.Request.RefusalCode == nil || *out.Request.RefusalCode != RefusalAccountReview {
		t.Fatalf("the execution must be refused with the generic token, got %+v", out.Request)
	}
	if got := w.playerCash(); got != 10_000 {
		t.Fatalf("a held balance must not move: %d", got)
	}
	w.assertInvariants()

	// Lift the hold: a fresh request executes normally.
	q.LiftHold(t, amlfix.Service(t, w.pool), hold)
	r2, err := w.submit(w.F1, w.credit(500, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatalf("submit after the lift: %v", err)
	}
	out2, err := w.decide(w.F2, r2, DecisionApprove)
	if err != nil || !out2.Executed {
		t.Fatalf("after the lift the adjustment executes: %+v %v", out2, err)
	}
	if got := w.playerCash(); got != 10_500 {
		t.Fatalf("player_cash after the lifted adjustment: %d", got)
	}
}

// ANY hold refuses the EXECUTION, including a withdrawal-only one (the execution evaluates the manual_adjustment operation, not deposit).
func TestAMLHold_WithdrawalOnlyHoldPlacedAfterSubmission_StillRefusesAtExecution(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	w.fund(10_000)
	r, err := w.submit(w.F1, w.credit(2_500, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	amlfix.HoldFor(t, w.pool, w.pool, w.Tenant, w.PlayerPerson, nil, aml.ScopeWithdrawalAndPayout)
	out, err := w.decide(w.F2, r, DecisionApprove)
	if err != nil || out.Executed || out.Request.State != StateRefusedAtExecution {
		t.Fatalf("a withdrawal-only hold must still refuse the execution: %+v %v", out, err)
	}
	if got := w.playerCash(); got != 10_000 {
		t.Fatalf("no movement: %d", got)
	}
}

// Fail closed: an evaluator failure refuses the adjustment (submission and execution) exactly like a hold.
func TestAMLHold_EvaluatorFailure_RefusesAdjustment(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	w.fund(10_000)
	r, err := w.submit(w.F1, w.credit(100, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	restore := aml.SetHoldProbeForTest(func(ctx context.Context, tx pgx.Tx, tenantID, personID, accountID uuid.UUID, op aml.Operation) (bool, error) {
		return false, errors.New("injected hold evaluator failure")
	})
	defer restore()
	if _, err := w.submit(w.F1, w.credit(200, ReasonOperationalErrorCorrection)); !errors.Is(err, ErrAccountUnderReview) {
		t.Fatalf("a failed evaluator must refuse the submission: %v", err)
	}
	out, err := w.decide(w.F2, r, DecisionApprove)
	if err != nil || out.Executed || out.Request.State != StateRefusedAtExecution {
		t.Fatalf("a failed evaluator must refuse the execution: %+v %v", out, err)
	}
	if got := w.playerCash(); got != 10_000 {
		t.Fatalf("no movement on a failed evaluation: %d", got)
	}
}

// A wallet that does not resolve gets NO invented hold verdict: it falls through to the ordinary payload refusal (not "under review").
func TestAMLHold_UnresolvedWallet_IsNotReportedAsUnderReview(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	w.fund(10_000)
	in := w.credit(100, ReasonOperationalErrorCorrection)
	in.WalletID = uuid.New()
	_, err := w.submit(w.F1, in)
	if err == nil || errors.Is(err, ErrAccountUnderReview) {
		t.Fatalf("an unknown wallet must be refused by the ordinary payload rule, not as under review: %v", err)
	}
}

// Ledger-finance C3: an evaluator failure at SUBMISSION rolls the submission back; the aml.hold_eval_failed alert must not be
// rolled back with it (it is raised in its own transaction after the rollback).
func TestAMLHold_EvaluatorFailureAtSubmission_AlertSurvivesTheRollback(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	w.fund(10_000)
	alertRows := func() int {
		var n int
		if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'aml.hold_eval_failed' AND subject_tenant_id = $1`, w.Tenant).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := requestCount(t, w)
	restore := aml.SetHoldProbeForTest(func(ctx context.Context, tx pgx.Tx, tenantID, personID, accountID uuid.UUID, op aml.Operation) (bool, error) {
		return false, errors.New("injected hold evaluator failure")
	})
	_, err := w.submit(w.F1, w.credit(200, ReasonOperationalErrorCorrection))
	restore()
	if !errors.Is(err, ErrAccountUnderReview) {
		t.Fatalf("a failed evaluator must refuse the submission: %v", err)
	}
	if requestCount(t, w) != before {
		t.Fatal("the refused submission wrote a request row")
	}
	if n := alertRows(); n != 1 {
		t.Fatalf("exactly one aml.hold_eval_failed alert must survive the rollback, got %d", n)
	}
}
