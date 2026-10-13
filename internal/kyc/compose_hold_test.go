package kyc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
)

// Pure unit tests of the hold composition (ADR 0116 s5.1): the hold evaluator is the package seam, so no database is needed.

func stubHold(v aml.HoldVerdict, err error) func() {
	prev := evaluateHold
	evaluateHold = func(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, aml.Operation) (aml.HoldVerdict, error) {
		return v, err
	}
	return func() { evaluateHold = prev }
}

func composeParams() EnforcementParams {
	return EnforcementParams{TenantID: uuid.New(), PersonID: uuid.New(), PlayerAccountID: uuid.New(), Operation: EnforcementDeposit}
}

// A HARD error of the evaluator's alert raise makes the whole evaluation an error, which every caller treats as a deny (ADR 0096 s2.2).
func TestComposeHold_HardEvaluatorErrorIsAnErrorNotAnAllow(t *testing.T) {
	defer stubHold(aml.HoldVerdict{EvalFailed: true}, errors.New("alert insert failed hard"))()
	passed := EnforcementDecision{Outcome: OutcomePassed, Allowed: true, Code: "kyc_deposit:passed"}
	got, err := composeHold(context.Background(), nil, composeParams(), passed)
	if !errors.Is(err, ErrEnforcementUnavailable) {
		t.Fatalf("a hard evaluator error must surface as ErrEnforcementUnavailable, got %v", err)
	}
	if got.Allowed {
		t.Fatalf("an errored composition must not carry an allow: %+v", got)
	}
}

// A definite hold on a KYC-passed player: public pending / account_review / not allowed, no matched trigger, true outcome held.
func TestComposeHold_HeldOverridesPassedAndHidesTheTrigger(t *testing.T) {
	hid := uuid.New()
	defer stubHold(aml.HoldVerdict{Held: true, HoldID: hid}, nil)()
	passed := EnforcementDecision{Outcome: OutcomePassed, Allowed: true, Code: "kyc_deposit:passed", MatchedTrigger: "policy-trigger-1", PolicyVersion: PolicyVersion}
	got, err := composeHold(context.Background(), nil, composeParams(), passed)
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed || got.Outcome != OutcomePending || got.Code != AccountReviewCode {
		t.Fatalf("the public result of a hold is pending / account_review / denied: %+v", got)
	}
	if got.MatchedTrigger != "" {
		t.Fatalf("the KYC policy trigger must not survive onto a hold's public result: %q", got.MatchedTrigger)
	}
	if got.TrueOutcome != OutcomeHeld || got.KYCOutcome != OutcomePassed || got.Hold.HoldID != hid {
		t.Fatalf("the internal fields: %+v", got)
	}
	if got.PublicOutcome() != PublicOutcomeReview {
		t.Fatalf("public outcome %q", got.PublicOutcome())
	}
}

// An evaluator failure alone (no hold) denies exactly like a hold, with the same public result.
func TestComposeHold_EvalFailureDeniesLikeAHold(t *testing.T) {
	defer stubHold(aml.HoldVerdict{EvalFailed: true}, nil)()
	passed := EnforcementDecision{Outcome: OutcomePassed, Allowed: true, Code: "kyc_deposit:passed", MatchedTrigger: "t"}
	got, err := composeHold(context.Background(), nil, composeParams(), passed)
	if err != nil || got.Allowed || got.Outcome != OutcomePending || got.Code != AccountReviewCode || got.MatchedTrigger != "" {
		t.Fatalf("%+v %v", got, err)
	}
	if got.TrueOutcome == OutcomeHeld {
		t.Fatalf("an evaluator error is not a hold: %+v", got)
	}
}

// The conversion for the payout gate: an evaluator fault ALONE becomes the retryable unavailable outcome; nothing else does.
func TestRetryableOnHoldEvalFailure(t *testing.T) {
	cases := map[string]struct {
		in   EnforcementDecision
		want bool // converted to unavailable
	}{
		"eval fault, kyc passed":       {EnforcementDecision{Outcome: OutcomePending, KYCOutcome: OutcomePassed, Hold: aml.HoldVerdict{EvalFailed: true}}, true},
		"eval fault, kyc not required": {EnforcementDecision{Outcome: OutcomePending, KYCOutcome: OutcomeNotRequired, Hold: aml.HoldVerdict{EvalFailed: true}}, true},
		"eval fault, kyc failed":       {EnforcementDecision{Outcome: OutcomeFailed, KYCOutcome: OutcomeFailed, Hold: aml.HoldVerdict{EvalFailed: true}}, false},
		"eval fault, kyc pending":      {EnforcementDecision{Outcome: OutcomePending, KYCOutcome: OutcomePending, Hold: aml.HoldVerdict{EvalFailed: true}}, false},
		"definite hold":                {EnforcementDecision{Outcome: OutcomePending, KYCOutcome: OutcomePassed, Hold: aml.HoldVerdict{Held: true}}, false},
		"no fault":                     {EnforcementDecision{Outcome: OutcomePassed, KYCOutcome: OutcomePassed, Allowed: true}, false},
	}
	for name, c := range cases {
		got := c.in.RetryableOnHoldEvalFailure()
		if (got.Outcome == OutcomeUnavailable) != c.want {
			t.Errorf("%s: converted=%v want %v (%+v)", name, got.Outcome == OutcomeUnavailable, c.want, got)
		}
		if c.want && got.Code != "kyc_unavailable:evaluator" {
			// The code reaches the tenant-visible audit reason_code: it must be generic (no hold / AML wording).
			t.Errorf("%s: the converted decision's code must be the generic kyc_unavailable:evaluator, got %q", name, got.Code)
		}
		if c.want && (got.Allowed || !got.Hold.EvalFailed) {
			t.Errorf("%s: the converted decision must deny and keep the verdict: %+v", name, got)
		}
		if !c.want && got.Outcome != c.in.Outcome {
			t.Errorf("%s: must be returned unchanged", name)
		}
	}
}
