package kyc

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func sp(s string) *string { return &s }

func TestValidatePolicyInput_ShapesAndNoDefaults(t *testing.T) {
	ok := CreateEnforcementPolicyParams{LicensingJurisdictionID: uuid.New(), TriggerType: "cumulative_deposit", ThresholdMinorUnits: sp("1"),
		AssetCode: sp("EUR"), LegalReviewReference: "REF-1", ReasonCode: "r1"}
	if err := ValidatePolicyInput(ok); err != nil {
		t.Fatalf("valid deposit policy refused: %v", err)
	}
	play := CreateEnforcementPolicyParams{LicensingJurisdictionID: uuid.New(), TriggerType: "play", PlayOperation: sp("sportsbook_play"), LegalReviewReference: "REF-1", ReasonCode: "r1"}
	if err := ValidatePolicyInput(play); err != nil {
		t.Fatalf("valid play policy refused: %v", err)
	}
	bad := map[string]func(p *CreateEnforcementPolicyParams){
		"no jurisdiction":    func(p *CreateEnforcementPolicyParams) { p.LicensingJurisdictionID = uuid.Nil },
		"unwired edd_amount": func(p *CreateEnforcementPolicyParams) { p.TriggerType = "edd_amount" },
		"unwired registration": func(p *CreateEnforcementPolicyParams) {
			p.TriggerType = "registration_tier"
			p.RequiredTier = sp("basic")
		},
		"unknown trigger":     func(p *CreateEnforcementPolicyParams) { p.TriggerType = "first_withdrawal" },
		"empty threshold":     func(p *CreateEnforcementPolicyParams) { p.ThresholdMinorUnits = nil },
		"zero threshold":      func(p *CreateEnforcementPolicyParams) { p.ThresholdMinorUnits = sp("0") },
		"leading zero":        func(p *CreateEnforcementPolicyParams) { p.ThresholdMinorUnits = sp("01") },
		"decimal threshold":   func(p *CreateEnforcementPolicyParams) { p.ThresholdMinorUnits = sp("1.5") },
		"exponent threshold":  func(p *CreateEnforcementPolicyParams) { p.ThresholdMinorUnits = sp("1e5") },
		"39 digit threshold":  func(p *CreateEnforcementPolicyParams) { p.ThresholdMinorUnits = sp(strings.Repeat("9", 39)) },
		"no asset":            func(p *CreateEnforcementPolicyParams) { p.AssetCode = nil },
		"tier on a deposit":   func(p *CreateEnforcementPolicyParams) { p.RequiredTier = sp("full") },
		"no legal reference":  func(p *CreateEnforcementPolicyParams) { p.LegalReviewReference = "" },
		"padded legal ref":    func(p *CreateEnforcementPolicyParams) { p.LegalReviewReference = " x" },
		"control char in ref": func(p *CreateEnforcementPolicyParams) { p.LegalReviewReference = "a\x00b" },
		"long legal ref":      func(p *CreateEnforcementPolicyParams) { p.LegalReviewReference = strings.Repeat("a", 201) },
		"free-text reason":    func(p *CreateEnforcementPolicyParams) { p.ReasonCode = "has spaces" },
		"empty reason":        func(p *CreateEnforcementPolicyParams) { p.ReasonCode = "" },
	}
	for name, mut := range bad {
		p := ok
		mut(&p)
		if err := ValidatePolicyInput(p); !errors.Is(err, ErrPolicyInvalidInput) {
			t.Errorf("%s: %v, want ErrPolicyInvalidInput", name, err)
		}
	}
	for name, mut := range map[string]func(p *CreateEnforcementPolicyParams){
		"no play operation":      func(p *CreateEnforcementPolicyParams) { p.PlayOperation = nil },
		"unknown play operation": func(p *CreateEnforcementPolicyParams) { p.PlayOperation = sp("poker_play") },
		"threshold on play":      func(p *CreateEnforcementPolicyParams) { p.ThresholdMinorUnits = sp("5") },
		"asset on play":          func(p *CreateEnforcementPolicyParams) { p.AssetCode = sp("EUR") },
	} {
		p := play
		mut(&p)
		if err := ValidatePolicyInput(p); !errors.Is(err, ErrPolicyInvalidInput) {
			t.Errorf("%s: %v, want ErrPolicyInvalidInput", name, err)
		}
	}
}

func TestClassifyPolicyError(t *testing.T) {
	pg := func(code, msg string) error { return &pgconn.PgError{Code: code, Message: msg} }
	for _, c := range []struct {
		err  error
		want PolicyFailure
	}{
		{nil, PolicyFailureNone},
		{errors.New("plain"), PolicyFailureOther},
		{pg("P0001", "kyc_enforcement_policies: four-eyes required - the activating/withdrawing principal must differ from the row's creator"), PolicyFailureFourEyes},
		{pg("P0001", "kyc_enforcement_policies: trigger_type edd_amount is not yet evaluator-wired and may not be activated"), PolicyFailureNotWired},
		{pg("P0001", "kyc_enforcement_policies: withdrawing an ACTIVE policy requires supersession - set superseded_by_policy_id"), PolicyFailureNeedsSuccess},
		{pg("P0001", "kyc_enforcement_policies: illegal status transition withdrawn -> active"), PolicyFailureIllegalMove},
		{pg("23505", "duplicate key"), PolicyFailureDuplicate},
		{pg("23503", "fk"), PolicyFailureUnknownRef},
		{pg("23514", "check"), PolicyFailureInvalid},
		{pg("P0001", "something unexpected"), PolicyFailureOther},
	} {
		if got := ClassifyPolicyError(c.err); got != c.want {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
	}
}

// The admin surface must not ship a threshold: the validator accepts none by default and no exported default exists.
func TestWiredTriggerTypes_AreExactlyTheDatabaseActivatable(t *testing.T) {
	if len(WiredEnforcementTriggerTypes) != 2 || WiredEnforcementTriggerTypes[0] != "cumulative_deposit" || WiredEnforcementTriggerTypes[1] != "play" {
		t.Fatalf("wired trigger types drifted from migration 0100 (cumulative_deposit, play): %v", WiredEnforcementTriggerTypes)
	}
}
