package casino

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/txscope"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// WebhookScheme implements CasinoProvider for the mock: the casino platform
// MOCK scheme exposed as a synthetic webhookauth.VerificationScheme (Stage
// 10.3 W1a, WH-VENDOR-SCHEME-1). Byte-identical to Stage 10.2; never a
// vendor protocol.
func (m *MockCasinoProvider) WebhookScheme() webhookauth.VerificationScheme {
	return casinoScheme.VerificationScheme()
}

// mustCasinoSchemeSet validates every registered adapter's WebhookScheme();
// a nil adapter/scheme or a non-permitted declaration panics, so startup
// fails instead of serving with a bad scheme. Stage 10.3-W1 fix round B:
// routed through webhookauth.MustAdapterSchemeSet, exactly like payments
// and KYC, so casino also enforces "a Synthetic scheme is accepted only
// from an adapter that implements SyntheticComponent()" in its own
// constructor, not merely by convention.
func mustCasinoSchemeSet(providers map[string]CasinoProvider) *webhookauth.SchemeSet {
	return webhookauth.MustAdapterSchemeSet("casino", providers)
}

// WebhookScheme returns the registered adapter's validated verification
// scheme for providerID (used by the shared HTTP preamble; no tenant input).
func (o *Orchestrator) WebhookScheme(providerID string) (webhookauth.VerificationScheme, bool) {
	return o.webhookSchemes.Lookup(providerID)
}

// WebhookDomain is the domain tag a casino VerifiedCallback is sealed for;
// ReceiveVerifiedCallback refuses a token sealed for any other domain.
const WebhookDomain = "casino"

// VerifiedCallback is phase 1's opaque, single-use, age-bounded proof that
// a casino callback verified (ADR 0094 §4.1; webhookauth.VerifiedCallback).
type VerifiedCallback = webhookauth.VerifiedCallback

// VerifyCallback is phase 1 of a casino callback (ADR 0094 §4.1; strict I1
// as amended by ADR 0022 §3 point 9, Stage 10.3: the ONLY tenant-scoped
// statement it may cause is the real resolver's single, lock-free,
// read-only, tenant-predicated handle SELECT, in the resolver's own READ
// ONLY transaction that commits before any secret-store fetch; a MOCK
// resolver issues none). It must be called with NO transaction held - a
// txscope-marked ctx fails closed (INV-POOL):
//
//	(0) clone the inbound (security C3) and overwrite TenantID/ProviderID
//	    from the route values - the single source of the tenant;
//	(a) the adapter (and its validated scheme) must be registered, else
//	    ReasonProviderUnregistered;
//	(a') scheme.Extract (signature_missing / signature_invalid);
//	(b) resolve the single credential binding - key selection from the
//	    scheme's Properties() only (security C3) - re-checked against
//	    (tenant, provider, key id);
//	(c) the ORCHESTRATOR-ENFORCED scheme.Verify over the raw bytes
//	    (webhookauth.VerifyInbound), sealed into a VerifiedCallback:
//	    HandleCallback is only reached through ReceiveVerifiedCallback, so
//	    an adapter cannot skip verification.
func (o *Orchestrator) VerifyCallback(ctx context.Context, r webhookauth.TenantReader, tenantID uuid.UUID, providerID string, in webhookauth.Inbound) (*VerifiedCallback, error) {
	if txscope.Held(ctx) {
		l := o.webhookLogger
		if l == nil {
			l = slog.Default()
		}
		l.Error("secret_fetch_with_tx_held", "entry_point", "casino.Orchestrator.VerifyCallback", "tenant_id", tenantID.String())
		return nil, &webhookauth.AuthError{Reason: webhookauth.ReasonCredentialUnavailable}
	}
	if tenantID == uuid.Nil {
		return nil, &webhookauth.AuthError{Reason: webhookauth.ReasonCredentialUnavailable}
	}
	in = webhookauth.CloneInbound(in)
	in.TenantID = tenantID
	in.ProviderID = providerID

	provider, registered := o.providers[in.ProviderID]
	scheme, hasScheme := o.webhookSchemes.Lookup(in.ProviderID)
	if !registered || provider == nil || !hasScheme {
		return nil, &webhookauth.AuthError{Reason: webhookauth.ReasonProviderUnregistered}
	}
	m, authErr := webhookauth.ExtractInbound(scheme, in)
	if authErr != nil {
		return nil, authErr
	}
	v, _, authErr := o.resolveAndVerify(ctx, r, scheme, in, m)
	if authErr != nil {
		return nil, authErr
	}
	return v, nil
}

// redeemVerified is phase 2's entry, the FIRST thing in the domain
// transaction (ADR 0094 §4.1/§5): the single-use, age-bounded token must be
// for exactly this (domain, tenant, provider), and the resolver's Recheck
// must confirm, in tx, that the handle that verified is still usable. Any
// failure is the uniform credential_unavailable and nothing is read or
// written.
func (o *Orchestrator) redeemVerified(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, v *VerifiedCallback) (CasinoProvider, webhookauth.Inbound, webhookauth.Credential, error) {
	in, cred, authErr := v.Redeem(ctx, tx, WebhookDomain, tenantID, providerID, o.webhookCredentialResolver)
	if authErr != nil {
		return nil, webhookauth.Inbound{}, webhookauth.Credential{}, authErr
	}
	provider, registered := o.providers[providerID]
	if !registered || provider == nil {
		return nil, webhookauth.Inbound{}, webhookauth.Credential{}, &webhookauth.AuthError{Reason: webhookauth.ReasonCredentialUnavailable, KeyID: cred.KeyID}
	}
	return provider, in, cred, nil
}

// resolveAndVerify is VerifyCallback's credential half, split out only so
// the matched-key log can be tested with a KeyImplicit test scheme that is
// never registered (security gate W2 condition W2A-SEC-2): resolve the
// single credential binding, run the ORCHESTRATOR-ENFORCED VerifyInbound
// (sealing the result), and on success log which key_id verified
// (webhookauth.LogVerifiedKey: KeyImplicit schemes only; request_id,
// tenant_id, provider_id and key_id only - never the secret or its
// fingerprint). VerifyCallback is its only production caller, after the
// registration lookup and Extract.
func (o *Orchestrator) resolveAndVerify(ctx context.Context, r webhookauth.TenantReader, scheme webhookauth.VerificationScheme, in webhookauth.Inbound, m webhookauth.AuthMaterial) (*VerifiedCallback, webhookauth.Credential, *webhookauth.AuthError) {
	creds, authErr := webhookauth.ResolveCredentials(ctx, r, scheme, o.webhookCredentialResolver, in, m)
	if authErr != nil {
		return nil, webhookauth.Credential{}, authErr
	}
	v, cred, authErr := webhookauth.VerifyAndSeal(WebhookDomain, scheme, creds, in, m, time.Now())
	if authErr != nil {
		return nil, webhookauth.Credential{}, authErr
	}
	webhookauth.LogVerifiedKey(ctx, o.webhookLogger, observability.RequestIDFromContext(ctx), scheme, in, cred)
	return v, cred, nil
}

// SetWebhookLogger sets the logger for the matched-key_id line
// (resolveAndVerify). Unset, slog.Default() is used. Call it once at
// startup, before the Orchestrator serves callbacks.
func (o *Orchestrator) SetWebhookLogger(l *slog.Logger) { o.webhookLogger = l }
