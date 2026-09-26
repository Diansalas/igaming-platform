package webhookauthtest

import (
	"crypto/rand"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// platformMockSchemes are the three domains' MOCK wire schemes.
func platformMockSchemes() []webhookauth.Scheme {
	return []webhookauth.Scheme{webhookauth.PaymentsScheme(), webhookauth.KYCScheme(), webhookauth.CasinoScheme()}
}

// MockSchemeFixture returns the conformance fixture for a platform MOCK
// VerificationScheme (one built by webhookauth.Scheme.VerificationScheme),
// with the other two domains' MOCK fixtures as its SC13 CrossDomain set.
// ok is false for any other scheme. MOCK only: it signs with the platform's
// own MOCK algorithm, which is never a vendor protocol.
func MockSchemeFixture(v webhookauth.VerificationScheme) (Fixture, bool) {
	s, ok := webhookauth.MockScheme(v)
	if !ok {
		return Fixture{}, false
	}
	f := mockFixture(s)
	for _, other := range platformMockSchemes() {
		if other.Prefix != s.Prefix {
			f.CrossDomain = append(f.CrossDomain, mockFixture(other))
		}
	}
	return f, true
}

func mockFixture(s webhookauth.Scheme) Fixture {
	return Fixture{
		Scheme:      s.VerificationScheme(),
		AuthHeaders: []string{s.SignatureHeader, s.KeyIDHeader},
		KeyIDHeader: s.KeyIDHeader,
		Sign: func(cred webhookauth.Credential, tenantID uuid.UUID, providerID string, body []byte, _ time.Time) http.Header {
			h := http.Header{}
			s.SetHeaders(h, cred.KeyID, s.Sign(cred.Secret, tenantID, providerID, cred.KeyID, body))
			return h
		},
		NewCredential: func(tenantID uuid.UUID, providerID, keyID string) webhookauth.Credential {
			secret := make([]byte, 32)
			if _, err := rand.Read(secret); err != nil {
				panic(err)
			}
			return webhookauth.Credential{
				TenantID: tenantID, ProviderID: providerID, KeyID: keyID,
				Secret: secret, Fingerprint: webhookauth.Fingerprint(secret),
			}
		},
	}
}
