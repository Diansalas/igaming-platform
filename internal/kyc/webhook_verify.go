package kyc

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/observability"
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

// verifyCallback is ReceiveCallback's pre-verification half (strict I1 as
// amended by ADR 0022 §3 point 9, Stage 10.3: the ONLY statement it may
// issue is the real resolver's single, lock-free, read-only,
// tenant-predicated handle SELECT in tx; a MOCK resolver issues none):
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
func (o *Orchestrator) verifyCallback(ctx context.Context, tx pgx.Tx, in webhookauth.Inbound) (KYCProvider, webhookauth.Credential, error) {
	provider, registered := o.providers[in.ProviderID]
	scheme, hasScheme := o.webhookSchemes.Lookup(in.ProviderID)
	if !registered || provider == nil || !hasScheme {
		return nil, webhookauth.Credential{}, &CallbackAuthError{Reason: webhookauth.ReasonProviderUnregistered}
	}
	m, authErr := webhookauth.ExtractInbound(scheme, in)
	if authErr != nil {
		return nil, webhookauth.Credential{}, authErr
	}
	cred, authErr := o.resolveAndVerify(ctx, tx, scheme, in, m)
	if authErr != nil {
		return nil, webhookauth.Credential{}, authErr
	}
	return provider, cred, nil
}

// resolveAndVerify is verifyCallback's credential half, split out only so
// the matched-key log can be tested with a KeyImplicit test scheme that is
// never registered (security gate W2 condition W2A-SEC-2): resolve the
// single credential binding, run the ORCHESTRATOR-ENFORCED VerifyInbound,
// and on success log which key_id verified (webhookauth.LogVerifiedKey:
// KeyImplicit schemes only; request_id, tenant_id, provider_id and key_id
// only - never the secret or its fingerprint). verifyCallback is its only
// production caller, after the registration lookup and Extract.
func (o *Orchestrator) resolveAndVerify(ctx context.Context, tx pgx.Tx, scheme webhookauth.VerificationScheme, in webhookauth.Inbound, m webhookauth.AuthMaterial) (webhookauth.Credential, *webhookauth.AuthError) {
	creds, authErr := webhookauth.ResolveCredentials(ctx, tx, scheme, o.webhookCredentialResolver, in, m)
	if authErr != nil {
		return webhookauth.Credential{}, authErr
	}
	cred, authErr := webhookauth.VerifyInbound(scheme, creds, in, m, time.Now())
	if authErr != nil {
		return webhookauth.Credential{}, authErr
	}
	webhookauth.LogVerifiedKey(ctx, o.webhookLogger, observability.RequestIDFromContext(ctx), scheme, in, cred)
	return cred, nil
}

// SetWebhookLogger sets the logger for the matched-key_id line
// (resolveAndVerify). Unset, slog.Default() is used. Call it once at
// startup, before the Orchestrator serves callbacks.
func (o *Orchestrator) SetWebhookLogger(l *slog.Logger) { o.webhookLogger = l }
