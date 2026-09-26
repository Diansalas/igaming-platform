package webhookauth

// ============================================================================
// MOCK. Everything in this file is a synthetic development/test double
// (CLAUDE.md "No fake completion"). The real resolver - a FORCE-RLS handle
// table plus an external, tenant-scoped secret store, keyed by (tenant_id,
// provider_id, key_id) - is NOT IMPLEMENTED. Nothing here may be wired
// outside TestSupportRoutesEnabled() deployments (ADR 0085; PAYWH-GATE-1).
// ============================================================================

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"

	"github.com/google/uuid"
)

// MockKeyID is the only key id a MockResolver ever resolves. A future
// rotation would add a second, coexisting id, never replace this one.
const MockKeyID = "mock-v1"

// MockMasterSize is the size, in bytes, of a mock master secret.
const MockMasterSize = 32

// NewMockMaster returns a fresh per-process random master secret from
// crypto/rand. It is never config, never in the repository, and never
// recoverable from outside the process. crypto/rand failing is a fatal
// platform problem: this panics rather than ever falling back to a
// predictable value.
func NewMockMaster() []byte {
	master := make([]byte, MockMasterSize)
	if _, err := rand.Read(master); err != nil {
		panic(fmt.Sprintf("webhookauth/mock: failed to generate master secret: %v", err))
	}
	return master
}

// DeriveMockKey computes the MOCK per-(tenant, provider) signing key
// HMAC-SHA256(master, label 0x00 tenant_id 0x00 provider_id). label is the
// domain's mock key label (domains.go), so equal masters still yield
// distinct keys per domain. NUL-separated: a UUID string never contains
// 0x00 and provider_id is charset-restricted.
//
// MOCK ONLY: deriving every tenant's key from one process-local master is
// acceptable only because this is a synthetic double. A real credential
// comes from an independent, tenant-scoped secret store. Panics on a master
// shorter than MockMasterSize - deriving from an empty or short master
// would yield a predictable key.
func DeriveMockKey(master []byte, label string, tenantID uuid.UUID, providerID string) []byte {
	if len(master) < MockMasterSize {
		panic("webhookauth/mock: refusing to derive a key from a missing or short master secret")
	}
	mac := hmac.New(sha256.New, master)
	_, _ = mac.Write([]byte(label))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(tenantID.String()))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(providerID))
	return mac.Sum(nil)
}

// MockResolver is the MOCK Resolver. It resolves ONLY KeyID == MockKeyID
// for its own ProviderID; any other provider id or key id - and a missing
// or short Master or an empty Label - is ErrCredentialUnavailable (fail
// closed, never a fallback).
type MockResolver struct {
	Master     []byte
	Label      string
	ProviderID string
}

// Resolve implements Resolver.
func (r MockResolver) Resolve(_ context.Context, tenantID uuid.UUID, providerID, keyID string) (Credential, error) {
	if len(r.Master) < MockMasterSize || r.Label == "" || r.ProviderID == "" {
		return Credential{}, ErrCredentialUnavailable
	}
	if providerID != r.ProviderID || keyID != MockKeyID {
		return Credential{}, ErrCredentialUnavailable
	}
	secret := DeriveMockKey(r.Master, r.Label, tenantID, providerID)
	return Credential{
		TenantID:    tenantID,
		ProviderID:  providerID,
		KeyID:       MockKeyID,
		Secret:      secret,
		Fingerprint: Fingerprint(secret),
	}, nil
}

// MultiResolver composes several per-provider resolvers behind the single
// Resolver a domain orchestrator is constructed with. A providerID absent
// from the map - or mapped to a nil entry, a construction mistake that must
// not panic on an unauthenticated request path - fails closed with
// ErrCredentialUnavailable, never falling back to any other entry.
//
// MOCK/TEST WIRING ONLY (Stage 10.1 architect review PW-6): this exists so
// a test fixture or a test-support deployment can stand up more than one
// mock provider in one process. It is NOT a template for the real
// resolver, which is a single platform component keyed by (tenant_id,
// provider_id, key_id), never a per-vendor map composed at an
// orchestrator boundary.
type MultiResolver map[string]Resolver

// Resolve implements Resolver by dispatching to the resolver registered
// for providerID.
func (m MultiResolver) Resolve(ctx context.Context, tenantID uuid.UUID, providerID, keyID string) (Credential, error) {
	r, ok := m[providerID]
	if !ok || r == nil {
		return Credential{}, ErrCredentialUnavailable
	}
	return r.Resolve(ctx, tenantID, providerID, keyID)
}
