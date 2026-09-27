// PRH-I1 round 2, ADR 0095 §16.2 item 15 (PROV-OUTBOUND-CRED-1, "no
// credential held by an adapter", S95-C8(c)): see
// internal/payments/credential_reflection_test.go's identical doc comment
// for the shared implementation and its scope note.
package kyc

import (
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/testsupport/credentialscan"
)

func TestCredentialReflection_NoConstructedAdapterHoldsACredential(t *testing.T) {
	mock := NewMockKYCProvider()
	outboundMock := NewMockOutboundResolver()
	kindSplit := NewOutboundKindSplitResolver(map[string]KYCProvider{"credscan-mock": mock}, outboundMock, nil)
	orchestrator := NewOrchestrator(
		map[string]KYCProvider{"credscan-mock": mock},
		NewMockWebhookCredentials(mock),
	)

	for name, v := range map[string]any{
		"MockKYCProvider":           mock,
		"MockOutboundResolver":      outboundMock,
		"OutboundKindSplitResolver": kindSplit,
		"Orchestrator (registry)":   orchestrator,
	} {
		if violations := credentialscan.Scan(v); len(violations) > 0 {
			t.Errorf("%s holds a credential/secret-shaped field:\n%s", name, strings.Join(violations, "\n"))
		}
	}
}

func TestCredentialReflection_PackageImportsNoSecretStore(t *testing.T) {
	violations, err := credentialscan.CheckPackageImports(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Errorf("package internal/kyc must never import internal/secretstore directly:\n%s", strings.Join(violations, "\n"))
	}
}

func TestCredentialReflection_NoPackageLevelCredentialVars(t *testing.T) {
	violations, err := credentialscan.CheckPackageLevelVars(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Errorf("package internal/kyc must have no package-level var of a forbidden credential type:\n%s", strings.Join(violations, "\n"))
	}
}
