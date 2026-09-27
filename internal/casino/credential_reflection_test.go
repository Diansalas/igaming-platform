// PRH-I1 round 2, ADR 0095 §16.2 item 15 (PROV-OUTBOUND-CRED-1, "no
// credential held by an adapter", S95-C8(c)): see
// internal/payments/credential_reflection_test.go's identical doc comment
// for the shared implementation and its scope note. Casino's own
// OutboundKindSplitResolver (launch.go) is scanned too, since it is a
// long-lived, constructed component the orchestrator is wired with.
package casino

import (
	"os"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/testsupport/credentialscan"
)

func TestCredentialReflection_NoConstructedAdapterHoldsACredential(t *testing.T) {
	mock := NewMockCasinoProvider("credscan-mock", "EUR")
	outboundMock := NewMockOutboundResolver()
	kindSplit := NewOutboundKindSplitResolver(map[string]CasinoProvider{"credscan-mock": mock}, outboundMock, nil)
	orchestrator := NewOrchestrator(
		map[string]CasinoProvider{"credscan-mock": mock},
		NewMockWebhookCredentials(mock),
	)

	for name, v := range map[string]any{
		"MockCasinoProvider":        mock,
		"MockOutboundResolver":      outboundMock,
		"OutboundKindSplitResolver": kindSplit,
		"Orchestrator (registry)":   orchestrator,
	} {
		if violations := credentialscan.Scan(v); len(violations) > 0 {
			t.Errorf("%s holds a credential/secret-shaped field:\n%s", name, strings.Join(violations, "\n"))
		}
	}
}

func TestCredentialReflection_AdapterFileImportsNoSecretStore(t *testing.T) {
	src, err := os.ReadFile("mock.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), `"github.com/Diansalas/igaming-platform/internal/secretstore"`) {
		t.Error("mock.go (the adapter file) must never import internal/secretstore - a real adapter receives an already-resolved credential, it never resolves one itself")
	}
}
