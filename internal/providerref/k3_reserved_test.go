package providerref

import (
	"errors"
	"strings"
	"testing"
)

// ADR 0101 C-9b: the reserved operator namespace is refused by the payments
// validators only; the shared validator is unchanged.
func TestK3_ReservedPrefix_Constant(t *testing.T) {
	if ReservedOperatorPrefix != "platform-operator-declared:" || len(ReservedOperatorPrefix) != 27 {
		t.Fatalf("reserved prefix drifted: %q (%d bytes)", ReservedOperatorPrefix, len(ReservedOperatorPrefix))
	}
}

func TestK3_ValidatePaymentReference_ReservedNamespace(t *testing.T) {
	cases := map[string]bool{ // value -> refused as reserved
		ReservedOperatorPrefix:                            true,
		ReservedOperatorPrefix + "abc":                    true,
		ReservedOperatorPrefix + strings.Repeat("x", 100): true,
		"platform-operator-declared":                      false, // no colon
		"Platform-Operator-Declared:x":                    false, // case-sensitive
		"x" + ReservedOperatorPrefix:                      false, // not a prefix
		"platform-operator-declared;x":                    false,
		"PSP-123":                                         false,
	}
	for v, refused := range cases {
		err := ValidatePaymentReference("f", v)
		var pe *Error
		isReserved := errors.As(err, &pe) && pe.Reason == ReasonReservedNamespace
		if isReserved != refused {
			t.Errorf("%q: reserved-refused=%v, want %v (err=%v)", v, isReserved, refused, err)
		}
		if refused && !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: the refusal must wrap ErrInvalid", v)
		}
		// The shared validator never refuses it as reserved (casino/sportsbook unchanged).
		if serr := Validate("f", v); serr != nil && errors.As(serr, &pe) && pe.Reason == ReasonReservedNamespace {
			t.Errorf("%q: the shared Validate must not know the reserved namespace", v)
		}
	}
}

func TestK3_ValidatePaymentReference_ErrorNeverCarriesTheValue(t *testing.T) {
	err := ValidatePaymentReference("f", ReservedOperatorPrefix+"secret-resolution-id")
	if err == nil || strings.Contains(err.Error(), "secret-resolution-id") {
		t.Fatalf("the error must not echo the value: %v", err)
	}
}

func TestK3_ValidatePaymentReferenceOptional_AndPlural(t *testing.T) {
	if err := ValidatePaymentReferenceOptional("f", ""); err != nil {
		t.Fatalf("empty optional must be accepted: %v", err)
	}
	if err := ValidatePaymentReferenceOptional("f", ReservedOperatorPrefix+"x"); err == nil {
		t.Fatal("a present reserved optional must be refused")
	}
	if err := ValidatePaymentReferences(Field{Name: "a", Value: "ok", Required: true}, Field{Name: "b", Value: "", Required: false}); err != nil {
		t.Fatalf("clean fields refused: %v", err)
	}
	for _, fs := range [][]Field{
		{{Name: "a", Value: "ok", Required: true}, {Name: "b", Value: ReservedOperatorPrefix + "1", Required: false}},
		{{Name: "a", Value: ReservedOperatorPrefix + "1", Required: true}},
		{{Name: "a", Value: "", Required: true}},
	} {
		if err := ValidatePaymentReferences(fs...); err == nil {
			t.Errorf("fields %+v must be refused", fs)
		}
	}
}
