package jurisdiction

import (
	"errors"
	"reflect"
	"testing"
)

// allOperationClasses mirrors validOperationClass's own exhaustive set -
// used below to iterate every value RequiredPurposes must have a map
// entry for.
var allOperationClasses = []OperationClass{
	OperationPlay, OperationCatalogueAvailability, OperationBonusIssuance, OperationBonusConversion,
}

// TestRequiredPurposes_MapKeySetIsExactlyTheFourValidClasses is the
// tripwire the ruling names: operationClassPurposeMapping's key set must
// be exactly the four values validOperationClass accepts, no more, no
// fewer - both directions checked.
func TestRequiredPurposes_MapKeySetIsExactlyTheFourValidClasses(t *testing.T) {
	if len(operationClassPurposeMapping) != len(allOperationClasses) {
		t.Fatalf("expected exactly %d map entries, got %d", len(allOperationClasses), len(operationClassPurposeMapping))
	}
	for _, oc := range allOperationClasses {
		if _, ok := operationClassPurposeMapping[oc]; !ok {
			t.Errorf("operationClassPurposeMapping is missing an entry for valid class %q", oc)
		}
	}
	for oc := range operationClassPurposeMapping {
		if !validOperationClass(oc) {
			t.Errorf("operationClassPurposeMapping has an entry for %q, which is not a valid operation class", oc)
		}
	}
}

// TestPurposeRequirement_ExactlyTwoBoolFields is the reflection tripwire
// that keeps PurposeHistoricalReporting structurally unmappable (the
// ruling's own §1.3 requirement) - mirrors EvidenceSet's existing
// three-field tripwire convention.
func TestPurposeRequirement_ExactlyTwoBoolFields(t *testing.T) {
	typ := reflect.TypeOf(PurposeRequirement{})
	if typ.NumField() != 2 {
		t.Fatalf("expected PurposeRequirement to have exactly 2 fields, got %d", typ.NumField())
	}
	wantFields := map[string]bool{"IdentityDetermination": true, "MarketAccessControl": true}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !wantFields[f.Name] {
			t.Errorf("unexpected field %q on PurposeRequirement", f.Name)
		}
		if f.Type.Kind() != reflect.Bool {
			t.Errorf("expected field %q to be a bool, got %s", f.Name, f.Type.Kind())
		}
	}
}

// TestRequiredPurposes_AllFourClassesAreUndetermined pins the "zero
// mapping content" state: every valid class returns
// ErrPurposeMappingUndetermined, and NEVER ErrInvalidInput.
func TestRequiredPurposes_AllFourClassesAreUndetermined(t *testing.T) {
	for _, oc := range allOperationClasses {
		got, err := RequiredPurposes(oc)
		if !errors.Is(err, ErrPurposeMappingUndetermined) {
			t.Errorf("%s: expected ErrPurposeMappingUndetermined, got %v", oc, err)
		}
		if errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: ErrPurposeMappingUndetermined must not also satisfy errors.Is(err, ErrInvalidInput)", oc)
		}
		if got != (PurposeRequirement{}) {
			t.Errorf("%s: expected the zero PurposeRequirement on error, got %+v", oc, got)
		}
	}
}

// TestRequiredPurposes_InvalidClassesReturnErrInvalidInput covers invalid,
// empty, whitespace, and wrong-case operation classes - all must return
// ErrInvalidInput, never ErrPurposeMappingUndetermined.
func TestRequiredPurposes_InvalidClassesReturnErrInvalidInput(t *testing.T) {
	cases := []OperationClass{
		"", " ", "PLAY", "Play", "play ", "catalogue-availability", "bonus_issuence", "unknown_class",
	}
	for _, oc := range cases {
		got, err := RequiredPurposes(oc)
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%q: expected ErrInvalidInput, got %v", oc, err)
		}
		if errors.Is(err, ErrPurposeMappingUndetermined) {
			t.Errorf("%q: must not satisfy errors.Is(err, ErrPurposeMappingUndetermined)", oc)
		}
		if got != (PurposeRequirement{}) {
			t.Errorf("%q: expected the zero PurposeRequirement on error, got %+v", oc, got)
		}
	}
}
