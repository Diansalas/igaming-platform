package jurisdiction

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// --- Category 6: enum vocabulary round trip ---

func TestLocationRequirement_RoundTrip(t *testing.T) {
	cases := []struct {
		dbVal string
		goVal LocationRequirement
	}{
		{"unset", LocationRequirementUnset},
		{"required", LocationRequired},
		{"advisory", LocationAdvisory},
	}
	for _, c := range cases {
		got, err := parseLocationRequirement(c.dbVal)
		if err != nil {
			t.Errorf("parseLocationRequirement(%q): unexpected error %v", c.dbVal, err)
		}
		if got != c.goVal {
			t.Errorf("parseLocationRequirement(%q) = %q, want %q", c.dbVal, got, c.goVal)
		}
		back, err := formatLocationRequirement(c.goVal)
		if err != nil {
			t.Errorf("formatLocationRequirement(%q): unexpected error %v", c.goVal, err)
		}
		if back != c.dbVal {
			t.Errorf("formatLocationRequirement(%q) = %q, want %q", c.goVal, back, c.dbVal)
		}
	}
}

func TestParseLocationRequirement_HostileInputsRejected(t *testing.T) {
	hostile := []string{"", "REQUIRED", "advisory ", " unset", "unknown", "Required", "Unset"}
	for _, s := range hostile {
		got, err := parseLocationRequirement(s)
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("parseLocationRequirement(%q): expected ErrInvalidInput, got %v", s, err)
		}
		if got != "" {
			t.Errorf("parseLocationRequirement(%q): expected zero value on error, got %q", s, got)
		}
	}
}

func TestFormatLocationRequirement_HostileInputsRejected(t *testing.T) {
	hostile := []LocationRequirement{"unknown", "REQUIRED", "Advisory", " "}
	for _, r := range hostile {
		got, err := formatLocationRequirement(r)
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("formatLocationRequirement(%q): expected ErrInvalidInput, got %v", r, err)
		}
		if got != "" {
			t.Errorf("formatLocationRequirement(%q): expected zero value on error, got %q", r, got)
		}
	}
}

// --- Category 23 (partial): structural invariants / reflection tripwires ---

// TestEvaluationPolicy_ExactlyTwoFields pins HDR-J-1's structural
// guarantee on the read path's OUTPUT type: EvaluationPolicy (precedence.go,
// zero diff this phase) must still carry exactly the two fields
// LocationSignalRequirement/MaxLocationSignalAge - neither of which is or
// can hold a jurisdiction code or id.
func TestEvaluationPolicy_ExactlyTwoFields(t *testing.T) {
	typ := reflect.TypeOf(EvaluationPolicy{})
	if typ.NumField() != 2 {
		t.Fatalf("expected EvaluationPolicy to have exactly 2 fields, got %d", typ.NumField())
	}
	want := map[string]bool{"LocationSignalRequirement": true, "MaxLocationSignalAge": true}
	for i := 0; i < typ.NumField(); i++ {
		if !want[typ.Field(i).Name] {
			t.Errorf("unexpected field %q on EvaluationPolicy", typ.Field(i).Name)
		}
	}
}

// TestEvaluationPolicyProvenance_ExactlyThreeFields pins the ruling's own
// tripwire: EvaluationPolicyProvenance must carry exactly {ConfigID,
// EffectiveFrom, PrecedencePolicyVersion} - deliberately NO
// LicensingJurisdictionID and no jurisdiction code.
func TestEvaluationPolicyProvenance_ExactlyThreeFields(t *testing.T) {
	typ := reflect.TypeOf(EvaluationPolicyProvenance{})
	if typ.NumField() != 3 {
		t.Fatalf("expected EvaluationPolicyProvenance to have exactly 3 fields, got %d", typ.NumField())
	}
	want := map[string]bool{"ConfigID": true, "EffectiveFrom": true, "PrecedencePolicyVersion": true}
	for i := 0; i < typ.NumField(); i++ {
		if !want[typ.Field(i).Name] {
			t.Errorf("unexpected field %q on EvaluationPolicyProvenance", typ.Field(i).Name)
		}
	}
}

// --- Category 7 (partial, no-DB-required cases only) ---

func TestResolveEvaluationPolicy_NilTenantIsInvalidInput(t *testing.T) {
	_, _, err := ResolveEvaluationPolicy(context.Background(), nil, EvaluationPolicyLookup{
		TenantID: uuid.Nil, OperationClass: OperationPlay, AsOf: time.Now(),
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for a nil tenant id, got %v", err)
	}
}

func TestResolveEvaluationPolicy_InvalidOperationClassIsInvalidInput(t *testing.T) {
	_, _, err := ResolveEvaluationPolicy(context.Background(), nil, EvaluationPolicyLookup{
		TenantID: uuid.New(), OperationClass: "not_a_real_class", AsOf: time.Now(),
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for an invalid operation class, got %v", err)
	}
}

func TestResolveEvaluationPolicy_ZeroAsOfIsInvalidInput(t *testing.T) {
	_, _, err := ResolveEvaluationPolicy(context.Background(), nil, EvaluationPolicyLookup{
		TenantID: uuid.New(), OperationClass: OperationPlay, AsOf: time.Time{},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for a zero AsOf, got %v", err)
	}
}

// --- Category 7 (partial): CreateEvaluationPolicyVersion input validation
// that fails BEFORE assertPlatformScope ever touches tx (Actor.validate is
// step 1) - safe to run with a nil pgx.Tx.

func TestCreateEvaluationPolicyVersion_MissingActorIDIsInvalidInput(t *testing.T) {
	_, err := CreateEvaluationPolicyVersion(context.Background(), nil, CreateEvaluationPolicyVersionParams{
		LicensingJurisdictionID: uuid.New(), OperationClass: OperationPlay, Status: EvaluationPolicyDraft,
		Actor: ActorContext{ReasonCode: "test"},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for a missing actor id, got %v", err)
	}
}

func TestCreateEvaluationPolicyVersion_MissingReasonCodeIsInvalidInput(t *testing.T) {
	_, err := CreateEvaluationPolicyVersion(context.Background(), nil, CreateEvaluationPolicyVersionParams{
		LicensingJurisdictionID: uuid.New(), OperationClass: OperationPlay, Status: EvaluationPolicyDraft,
		Actor: ActorContext{ActorID: uuid.New()},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for a missing reason code, got %v", err)
	}
}

// --- EvaluationPolicyStatus sanity ---

func TestValidEvaluationPolicyStatus(t *testing.T) {
	valid := []EvaluationPolicyStatus{EvaluationPolicyDraft, EvaluationPolicyActive, EvaluationPolicyWithdrawn}
	for _, s := range valid {
		if !validEvaluationPolicyStatus(s) {
			t.Errorf("expected %q to be valid", s)
		}
	}
	invalid := []EvaluationPolicyStatus{"", "ACTIVE", "suspended"}
	for _, s := range invalid {
		if validEvaluationPolicyStatus(s) {
			t.Errorf("expected %q to be invalid", s)
		}
	}
}
