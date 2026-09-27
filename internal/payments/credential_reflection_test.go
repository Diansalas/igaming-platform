// PRH-I1 round 2, ADR 0095 §16.2 item 15 (PROV-OUTBOUND-CRED-1, "no
// credential held by an adapter", S95-C8(c)): a recursive reflection walk
// over this domain's own constructed, registered adapters/resolvers, plus a
// static check that the adapter source file never imports the secret store.
// See internal/testsupport/credentialscan for the shared implementation and
// its own scope note (a CONSTRUCTED adapter/resolver value, never a
// per-call CallContext, which is deliberately excluded - it is built fresh
// per call and never stored).
package payments

import (
	"os"
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

	for name, v := range map[string]any{
		"MockProvider":            mock,
		"MockCredentialResolver":  resolver,
		"Orchestrator (registry)": orchestrator,
	} {
		if violations := credentialscan.Scan(v); len(violations) > 0 {
			t.Errorf("%s holds a credential/secret-shaped field:\n%s", name, strings.Join(violations, "\n"))
		}
	}
}

// TestCredentialReflection_AdapterFileImportsNoSecretStore is the static
// half of the same condition: the adapter source file (mock.go - the only
// PaymentProvider implementation this stage ships) never imports the
// secret store directly. A real adapter resolves nothing itself; it
// receives an already-resolved credential through CallContext.
func TestCredentialReflection_AdapterFileImportsNoSecretStore(t *testing.T) {
	src, err := os.ReadFile("mock.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), `"github.com/Diansalas/igaming-platform/internal/secretstore"`) {
		t.Error("mock.go (the adapter file) must never import internal/secretstore - a real adapter receives an already-resolved credential, it never resolves one itself")
	}
}
