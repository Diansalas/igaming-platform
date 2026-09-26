package webhookauth

import (
	"strings"
	"testing"
	"time"
)

// TestNewSchemeSet_SyntheticOnlyTheDomainsCanonicalMock is security review
// 06-gate-w1 S-1 rule 1, reproducing the review's overlay probe: a
// Synthetic scheme is accepted only when it wraps exactly the registering
// domain's canonical MOCK wire Scheme. A custom-prefix MOCK, a MOCK that
// differs only in a header name, another domain's MOCK, and any MOCK under
// an unknown domain are all refused at registration.
func TestNewSchemeSet_SyntheticOnlyTheDomainsCanonicalMock(t *testing.T) {
	canonical := map[string]Scheme{"payments": PaymentsScheme(), "kyc": KYCScheme(), "casino": CasinoScheme()}
	for domain, own := range canonical {
		if _, err := NewSchemeSet(domain, map[string]VerificationScheme{"mock-x": own.VerificationScheme()}); err != nil {
			t.Errorf("%s: its own canonical MOCK scheme must be accepted, got %v", domain, err)
		}
		for other, foreign := range canonical {
			if other == domain {
				continue
			}
			if _, err := NewSchemeSet(domain, map[string]VerificationScheme{"vendor-y": foreign.VerificationScheme()}); err == nil {
				t.Errorf("%s: the %s MOCK scheme (cross-domain) must be refused", domain, other)
			}
		}
		custom := Scheme{Prefix: "vendor.x.v1", SignatureHeader: "X-Vendor-Signature", KeyIDHeader: "X-Vendor-Key"}
		if _, err := NewSchemeSet(domain, map[string]VerificationScheme{"vendor-x": custom.VerificationScheme()}); err == nil {
			t.Errorf("%s: a custom-prefix MOCK scheme must be refused", domain)
		}
		samePrefixOtherKeyHeader := own
		samePrefixOtherKeyHeader.KeyIDHeader = "X-Vendor-Key"
		if _, err := NewSchemeSet(domain, map[string]VerificationScheme{"vendor-x": samePrefixOtherKeyHeader.VerificationScheme()}); err == nil {
			t.Errorf("%s: a MOCK scheme differing only in its key-id header must be refused", domain)
		}
		samePrefixOtherSigHeader := own
		samePrefixOtherSigHeader.SignatureHeader = "X-Vendor-Signature"
		if _, err := NewSchemeSet(domain, map[string]VerificationScheme{"vendor-x": samePrefixOtherSigHeader.VerificationScheme()}); err == nil {
			t.Errorf("%s: a MOCK scheme differing only in its signature header must be refused", domain)
		}
	}
	for _, unknown := range []string{"sportsbook", "", "Payments"} {
		if _, err := NewSchemeSet(unknown, map[string]VerificationScheme{"mock-x": PaymentsScheme().VerificationScheme()}); err == nil {
			t.Errorf("unknown domain %q must accept no Synthetic scheme", unknown)
		}
	}
}

// markedSyntheticAdapter, unmarkedAdapter and eligibleAdapter are
// SchemeSource fixtures for S-1 rule 2.
type markedSyntheticAdapter struct{ s VerificationScheme }

func (a markedSyntheticAdapter) WebhookScheme() VerificationScheme { return a.s }
func (markedSyntheticAdapter) SyntheticComponent()                 {}

type unmarkedAdapter struct{ s VerificationScheme }

func (a unmarkedAdapter) WebhookScheme() VerificationScheme { return a.s }

type eligibleAdapter struct{ s VerificationScheme }

func (a eligibleAdapter) WebhookScheme() VerificationScheme { return a.s }
func (eligibleAdapter) MarkProductionEligible()             {}

// TestNewAdapterSchemeSet_SyntheticSchemeOnlyFromSyntheticAdapter is
// security review 06-gate-w1 S-1 rule 2: the domain's own canonical MOCK
// scheme is still refused when the adapter returning it is not itself a
// synthetic component (unmarked, or claiming ProductionEligible). A
// non-synthetic, manifest-listed scheme is unaffected by this rule.
func TestNewAdapterSchemeSet_SyntheticSchemeOnlyFromSyntheticAdapter(t *testing.T) {
	mock := PaymentsScheme().VerificationScheme()
	if _, err := NewAdapterSchemeSet("payments", map[string]SchemeSource{"mock-psp": markedSyntheticAdapter{mock}}); err != nil {
		t.Fatalf("the canonical MOCK from a synthetic adapter must be accepted, got %v", err)
	}
	for name, a := range map[string]SchemeSource{"unmarked": unmarkedAdapter{mock}, "production-eligible": eligibleAdapter{mock}} {
		_, err := NewAdapterSchemeSet("payments", map[string]SchemeSource{"vendor-x": a})
		if err == nil || !strings.Contains(err.Error(), "not itself a synthetic component") {
			t.Errorf("%s adapter returning the MOCK scheme must be refused, got %v", name, err)
		}
	}
	// Rule 1 still applies on the adapter path: a synthetic adapter
	// returning another domain's MOCK, or a custom MOCK, is refused.
	if _, err := NewAdapterSchemeSet("payments", map[string]SchemeSource{"mock-psp": markedSyntheticAdapter{CasinoScheme().VerificationScheme()}}); err == nil {
		t.Fatal("a synthetic adapter returning a cross-domain MOCK scheme must be refused")
	}
	custom := Scheme{Prefix: "vendor.x.v1", SignatureHeader: "X-Vendor-Signature", KeyIDHeader: "X-Vendor-Key"}
	if _, err := NewAdapterSchemeSet("payments", map[string]SchemeSource{"mock-psp": markedSyntheticAdapter{custom.VerificationScheme()}}); err == nil {
		t.Fatal("a synthetic adapter returning a custom-prefix MOCK scheme must be refused")
	}
	if _, err := NewAdapterSchemeSet("payments", map[string]SchemeSource{"mock-psp": nil}); err == nil {
		t.Fatal("a nil adapter must be refused")
	}

	extractOK := func(Inbound) (AuthMaterial, Reason, bool) { return NewAuthMaterial("k", nil), "", true }
	verifyOK := func(c CredentialSet, _ Inbound, _ AuthMaterial, _ time.Time) (string, error) {
		return c.Active.KeyID, nil
	}
	vendor := spyScheme{name: "vendor-z-v1", props: realProps(), extract: extractOK, verify: verifyOK}
	conformanceManifest["vendor-z-v1"] = struct{}{}
	t.Cleanup(func() { delete(conformanceManifest, "vendor-z-v1") })
	if _, err := NewAdapterSchemeSet("payments", map[string]SchemeSource{"vendor-z": eligibleAdapter{vendor}}); err != nil {
		t.Fatalf("a manifest-listed real scheme from an eligible adapter must be accepted, got %v", err)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("MustAdapterSchemeSet must panic (fail startup) for a MOCK scheme on an unmarked adapter")
		}
	}()
	MustAdapterSchemeSet("kyc", map[string]SchemeSource{"vendor-x": unmarkedAdapter{KYCScheme().VerificationScheme()}})
}

// TestMockVerificationScheme_IsSyntheticComponent: the MOCK scheme itself
// carries the providerkind.Synthetic marker, so registering it with the
// production guard refuses it in production (S-1 / code review #6).
func TestMockVerificationScheme_IsSyntheticComponent(t *testing.T) {
	for _, s := range []Scheme{PaymentsScheme(), KYCScheme(), CasinoScheme()} {
		if _, ok := s.VerificationScheme().(interface{ SyntheticComponent() }); !ok {
			t.Fatalf("%s MOCK VerificationScheme must implement SyntheticComponent()", s.Prefix)
		}
	}
}
