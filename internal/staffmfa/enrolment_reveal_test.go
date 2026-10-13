package staffmfa

import (
	"fmt"
	"strings"
	"testing"
)

// S2 adds the one exported read of EnrolmentMaterial (the store tests and the S3 enrolment response need it);
// it returns exactly what the unexported accessors hold and nothing else renders the material.
func TestEnrolmentMaterial_RevealForEnrolment(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewTOTPEnrolmentMaterial("iGaming Test", "a@example.test", secret)
	if err != nil {
		t.Fatal(err)
	}
	uri, b32 := m.RevealForEnrolment()
	if uri != m.otpauthURI() || b32 != m.secretBase32() || b32 == "" || !strings.Contains(uri, "secret="+b32) {
		t.Fatalf("reveal differs from the accessors: %q %q", uri, b32)
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		if out := fmt.Sprintf(verb, m); strings.Contains(out, b32) || strings.Contains(out, "otpauth") {
			t.Fatalf("%s renders the enrolment material: %s", verb, out)
		}
	}
	if u, s := (EnrolmentMaterial{}).RevealForEnrolment(); u != "" || s != "" {
		t.Fatal("the zero material reveals nothing")
	}
}
