package kyc

import (
	"context"
	"time"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// WebhookScheme implements KYCProvider for the mock: the KYC platform MOCK
// scheme exposed as a synthetic webhookauth.VerificationScheme (Stage 10.3
// W1a, WH-VENDOR-SCHEME-1). Byte-identical to Stage 10.2; never a vendor
// protocol.
func (m *MockKYCProvider) WebhookScheme() webhookauth.VerificationScheme {
	return kycMockScheme.VerificationScheme()
}

// mustKYCSchemeSet validates every registered adapter's WebhookScheme(); a
// nil adapter/scheme or a non-permitted declaration panics, so startup
// fails instead of serving with a bad scheme.
func mustKYCSchemeSet(providers map[string]KYCProvider) *webhookauth.SchemeSet {
	return webhookauth.MustAdapterSchemeSet("kyc", providers)
}

// WebhookScheme returns the registered adapter's validated verification
// scheme for providerID (used by the shared HTTP preamble; no tenant input).
func (o *Orchestrator) WebhookScheme(providerID string) (webhookauth.VerificationScheme, bool) {
	return o.webhookSchemes.Lookup(providerID)
}

// verifyCallback is ReceiveCallback's pre-verification half (strict I1: it
// issues NO database statement at all):
//
//	(a) the adapter (and its validated scheme) must be registered, else
//	    ReasonProviderUnregistered;
//	(a') scheme.Extract (signature_missing / signature_invalid);
//	(b) resolve the single credential binding - key selection from the
//	    scheme's Properties() only (security C3) - re-checked against
//	    (tenant, provider, key id);
//	(c) the ORCHESTRATOR-ENFORCED scheme.Verify over the raw bytes
//	    (webhookauth.VerifyInbound): HandleCallback is only reached once
//	    this succeeds, so an adapter cannot skip verification.
func (o *Orchestrator) verifyCallback(ctx context.Context, in webhookauth.Inbound) (KYCProvider, webhookauth.Credential, error) {
	provider, registered := o.providers[in.ProviderID]
	scheme, hasScheme := o.webhookSchemes.Lookup(in.ProviderID)
	if !registered || provider == nil || !hasScheme {
		return nil, webhookauth.Credential{}, &CallbackAuthError{Reason: webhookauth.ReasonProviderUnregistered}
	}
	m, authErr := webhookauth.ExtractInbound(scheme, in)
	if authErr != nil {
		return nil, webhookauth.Credential{}, authErr
	}
	creds, authErr := webhookauth.ResolveCredentials(ctx, scheme, o.webhookCredentialResolver, in, m)
	if authErr != nil {
		return nil, webhookauth.Credential{}, authErr
	}
	cred, authErr := webhookauth.VerifyInbound(scheme, creds, in, m, time.Now())
	if authErr != nil {
		return nil, webhookauth.Credential{}, authErr
	}
	return provider, cred, nil
}
