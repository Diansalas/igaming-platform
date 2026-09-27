// PRH-I1 round 2, ADR 0095 §16.2 item 15 (PROV-OUTBOUND-CRED-1, "no
// credential held by an adapter", S95-C8(c)): a recursive reflection walk
// over this domain's own constructed, registered adapters/resolvers, plus a
// PACKAGE-WIDE static check that no file in this package imports the secret
// store, and no package-level var has a forbidden credential type. See
// internal/testsupport/credentialscan for the shared implementation and its
// own scope note (a CONSTRUCTED adapter/resolver value, never a per-call
// CallContext, which is deliberately excluded - it is built fresh per call
// and never stored).
package payments

import (
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/testsupport/credentialscan"
)

func TestCredentialReflection_NoConstructedAdapterHoldsACredential(t *testing.T) {
	mock := NewMockProvider("credscan-mock", "EUR", "USD")
	resolver := MockCredentialResolver{}
	orchestrator := NewOrchestrator(
		map[string]PaymentProvider{"credscan-mock": mock},
		MultiWebhookCredentialResolver{"credscan-mock": NewMockWebhookCredentials(mock)},
	)
	// PROV-OUTBOUND-CRED-1, phase 2 orchestrator wiring: the kind-split
	// resolver itself (holding both the mock and, in a real deployment,
	// the real providercred-backed resolver) must never carry a
	// credential/secret-shaped field either - it is a long-lived,
	// constructed value, not a per-call CallContext.
	kindSplit := NewOutboundKindSplitResolver(map[string]PaymentProvider{"credscan-mock": mock}, resolver, resolver)

	for name, v := range map[string]any{
		"MockProvider":              mock,
		"MockCredentialResolver":    resolver,
		"Orchestrator (registry)":   orchestrator,
		"OutboundKindSplitResolver": kindSplit,
	} {
		if violations := credentialscan.Scan(v); len(violations) > 0 {
			t.Errorf("%s holds a credential/secret-shaped field:\n%s", name, strings.Join(violations, "\n"))
		}
	}
}

// TestCredentialReflection_PackageImportsNoSecretStore is the static half
// of the same condition, PACKAGE-WIDE (RV-PRH-I1 security review M5):
// every non-test .go file in this package, not just mock.go - a real
// adapter resolves nothing itself; it receives an already-resolved
// credential through CallContext.
func TestCredentialReflection_PackageImportsNoSecretStore(t *testing.T) {
	violations, err := credentialscan.CheckPackageImports(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Errorf("package internal/payments must never import internal/secretstore directly:\n%s", strings.Join(violations, "\n"))
	}
}

// TestCredentialReflection_NoPackageLevelCredentialVars (M5).
func TestCredentialReflection_NoPackageLevelCredentialVars(t *testing.T) {
	violations, err := credentialscan.CheckPackageLevelVars(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Errorf("package internal/payments must have no package-level var of a forbidden credential type:\n%s", strings.Join(violations, "\n"))
	}
}
