package webhookauth

import (
	"strings"
	"testing"
	"time"
)

// TestValidateScheme_KeyImplicitRefusedUntilW2a is gate 10.3-W1 code
// review #10: a manifest-listed, otherwise permitted KeyImplicit scheme is
// refused at registration (it would otherwise register and then 401 every
// callback as credential_unavailable), while the identical KeyFromHeader
// declaration registers. ValidateProperties still permits KeyImplicit, so
// the conformance suite can keep exercising it.
func TestValidateScheme_KeyImplicitRefusedUntilW2a(t *testing.T) {
	extractOK := func(Inbound) (AuthMaterial, Reason, bool) { return NewAuthMaterial("", nil), "", true }
	verifyOK := func(c CredentialSet, _ Inbound, _ AuthMaterial, _ time.Time) (string, error) {
		return c.Active.KeyID, nil
	}
	implicit := spyScheme{name: "vendor-implicit-v1", props: realProps(), extract: extractOK, verify: verifyOK}
	implicit.props.KeySelection = KeyImplicit
	conformanceManifest["vendor-implicit-v1"] = struct{}{}
	t.Cleanup(func() { delete(conformanceManifest, "vendor-implicit-v1") })

	if err := ValidateProperties(implicit.props); err != nil {
		t.Fatalf("KeyImplicit must remain a PERMITTED declaration for the conformance suite, got %v", err)
	}
	err := ValidateScheme(implicit)
	if err == nil || !strings.Contains(err.Error(), "KeyImplicit") || !strings.Contains(err.Error(), "W2a") {
		t.Fatalf("a KeyImplicit scheme must be refused at registration with a clear W2a error, got %v", err)
	}
	if _, err := NewAdapterSchemeSet("payments", map[string]SchemeSource{"vendor-implicit": eligibleAdapter{implicit}}); err == nil {
		t.Fatal("NewAdapterSchemeSet must refuse a KeyImplicit scheme")
	}

	fromHeader := implicit
	fromHeader.props.KeySelection = KeyFromHeader
	if err := ValidateScheme(fromHeader); err != nil {
		t.Fatalf("the same declaration with KeyFromHeader must register, got %v", err)
	}
}
