package payments

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// mustPaymentsSchemeSet validates every registered adapter's WebhookScheme()
// (Stage 10.3 W1a, WH-VENDOR-SCHEME-1). A nil adapter, a nil scheme or a
// non-permitted Properties() declaration panics: NewOrchestrator runs at
// process start, so startup fails instead of serving with a bad scheme.
func mustPaymentsSchemeSet(providers map[string]PaymentProvider) *webhookauth.SchemeSet {
	return webhookauth.MustAdapterSchemeSet("payments", providers)
}

// WebhookScheme returns the registered adapter's validated verification
// scheme for providerID - what the shared HTTP webhook preamble uses to
// format-check the request BEFORE any tenant lookup. No tenant input.
func (o *Orchestrator) WebhookScheme(providerID string) (webhookauth.VerificationScheme, bool) {
	return o.webhookSchemes.Lookup(providerID)
}

// verifyCallback is ReceiveCallback's pre-verification half, in the fixed
// ADR 0022 §3 order, entirely before any tenant-scoped read other than the
// one permitted EXISTS (invariant I1):
//
//	(a) the adapter (and its validated scheme) must be registered, else
//	    ReasonProviderUnregistered;
//	(a') scheme.Extract (signature_missing / signature_invalid);
//	(b) ProviderAcceptsWebhook - the only statement before verification
//	    (I4: a disabled capability still accepts; revocation is by removing
//	    the credential);
//	(c) resolve the single credential binding - key selection from the
//	    scheme's Properties() only (security C3), re-checked against
//	    (tenant, provider, key id);
//	(d) the ORCHESTRATOR-ENFORCED scheme.Verify over the raw bytes
//	    (webhookauth.VerifyInbound) - the adapter's HandleCallback is only
//	    reached once this succeeds, so an adapter cannot skip verification.
//
// in.TenantID/in.ProviderID are already the route-resolved values. Every
// failure is a *CallbackAuthError (or a DB error from (b)).
func (o *Orchestrator) verifyCallback(ctx context.Context, tx pgx.Tx, in InboundCallback) (PaymentProvider, WebhookCredential, error) {
	provider, registered := o.providers[in.ProviderID]
	scheme, hasScheme := o.webhookSchemes.Lookup(in.ProviderID)
	if !registered || provider == nil || !hasScheme {
		return nil, WebhookCredential{}, &CallbackAuthError{Reason: ReasonProviderUnregistered}
	}
	m, authErr := webhookauth.ExtractInbound(scheme, in)
	if authErr != nil {
		return nil, WebhookCredential{}, authErr
	}

	accepts, err := ProviderAcceptsWebhook(ctx, tx, in.TenantID, in.ProviderID)
	if err != nil {
		return nil, WebhookCredential{}, err
	}
	if !accepts {
		return nil, WebhookCredential{}, &CallbackAuthError{Reason: ReasonProviderNotConfigured, KeyID: m.KeyID}
	}

	cred, authErr := o.resolveAndVerify(ctx, tx, scheme, in, m)
	if authErr != nil {
		return nil, WebhookCredential{}, authErr
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
