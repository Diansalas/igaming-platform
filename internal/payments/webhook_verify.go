package payments

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/txscope"
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

// WebhookDomain is the domain tag a payments VerifiedCallback is sealed
// for; ReceiveVerifiedCallback refuses a token sealed for any other domain.
const WebhookDomain = "payments"

// VerifiedCallback is phase 1's opaque, single-use, age-bounded proof that
// a payments callback verified (ADR 0094 §4.1; webhookauth.VerifiedCallback).
type VerifiedCallback = webhookauth.VerifiedCallback

// VerifyCallback is phase 1 of a payments callback (ADR 0094 §4.1), in the
// fixed ADR 0022 §3 order, entirely before any tenant-scoped read other
// than the permitted ones (invariant I1). It must be called with NO
// transaction held - a txscope-marked ctx fails closed - and every
// tenant-scoped statement it runs is in its own short READ ONLY
// transaction on r that commits before any secret-store fetch
// (INV-POOL):
//
//	(0) overwrite TenantID/ProviderID from the route values - the single
//	    source of the tenant (the C3 copy happens in ResolveAndSeal);
//	(a) the adapter (and its validated scheme) must be registered, else
//	    ReasonProviderUnregistered;
//	(a') scheme.Extract (signature_missing / signature_invalid);
//	(b) ProviderAcceptsWebhook (I4: a disabled capability still accepts;
//	    revocation is by removing the credential), READ ONLY transaction;
//	(c) resolve the single credential binding - key selection from the
//	    scheme's Properties() only (security C3), re-checked against
//	    (tenant, provider, key id); its handle read runs in the resolver's
//	    own READ ONLY transaction;
//	(d) the ORCHESTRATOR-ENFORCED scheme.Verify over the raw bytes
//	    (webhookauth.VerifyInbound), sealed into a VerifiedCallback.
//
// Every failure is a *CallbackAuthError (or a DB error from (b)).
func (o *Orchestrator) VerifyCallback(ctx context.Context, r webhookauth.TenantReader, tenantID uuid.UUID, providerID string, in InboundCallback) (*VerifiedCallback, error) {
	if txscope.Held(ctx) {
		logVerifyWithTxHeld(o.webhookLogger, tenantID)
		return nil, &CallbackAuthError{Reason: ReasonCredentialUnavailable}
	}
	if r == nil || tenantID == uuid.Nil {
		return nil, &CallbackAuthError{Reason: ReasonCredentialUnavailable}
	}
	// in is a value copy; its body and headers are deep-copied by
	// webhookauth.ResolveAndSeal before verification (the single C3
	// chokepoint) and nothing here mutates them.
	in.TenantID = tenantID
	in.ProviderID = providerID

	provider, registered := o.providers[in.ProviderID]
	scheme, hasScheme := o.webhookSchemes.Lookup(in.ProviderID)
	if !registered || provider == nil || !hasScheme {
		return nil, &CallbackAuthError{Reason: ReasonProviderUnregistered}
	}
	m, authErr := webhookauth.ExtractInbound(scheme, in)
	if authErr != nil {
		return nil, authErr
	}

	var accepts bool
	if err := r.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		accepts, err = ProviderAcceptsWebhook(ctx, tx, in.TenantID, in.ProviderID)
		return err
	}); err != nil {
		return nil, err
	}
	if !accepts {
		return nil, &CallbackAuthError{Reason: ReasonProviderNotConfigured, KeyID: m.KeyID}
	}

	v, _, authErr := o.resolveAndVerify(ctx, r, scheme, in, m)
	if authErr != nil {
		return nil, authErr
	}
	return v, nil
}

// resolveAndVerify is VerifyCallback's credential half, split out only so
// the matched-key log can be tested with a KeyImplicit test scheme that is
// never registered (security gate W2 condition W2A-SEC-2): resolve the
// single credential binding, run the ORCHESTRATOR-ENFORCED VerifyInbound
// (webhookauth.ResolveAndSeal: copy, resolve, verify, seal), and on success log which key_id verified
// (webhookauth.LogVerifiedKey: KeyImplicit schemes only; request_id,
// tenant_id, provider_id and key_id only - never the secret or its
// fingerprint). VerifyCallback is its only production caller, after the
// registration lookup and Extract.
func (o *Orchestrator) resolveAndVerify(ctx context.Context, r webhookauth.TenantReader, scheme webhookauth.VerificationScheme, in webhookauth.Inbound, m webhookauth.AuthMaterial) (*VerifiedCallback, webhookauth.Credential, *webhookauth.AuthError) {
	// ResolveAndSeal is the single chokepoint: it copies the inbound,
	// resolves the credentials through this domain's resolver, verifies
	// the copy and seals it (security review 17, S-1/S-3).
	v, cred, authErr := webhookauth.ResolveAndSeal(ctx, r, WebhookDomain, scheme, o.webhookCredentialResolver, in, m)
	if authErr != nil {
		return nil, webhookauth.Credential{}, authErr
	}
	webhookauth.LogVerifiedKey(ctx, o.webhookLogger, observability.RequestIDFromContext(ctx), scheme, in, cred)
	return v, cred, nil
}

// logVerifyWithTxHeld is the INV-POOL guard's log line for a VerifyCallback
// called inside a transaction (a wiring bug, never request-driven).
func logVerifyWithTxHeld(l *slog.Logger, tenantID uuid.UUID) {
	if l == nil {
		l = slog.Default()
	}
	l.Error("secret_fetch_with_tx_held", "entry_point", "payments.Orchestrator.VerifyCallback", "tenant_id", tenantID.String())
}

// SetWebhookLogger sets the logger for the matched-key_id line
// (resolveAndVerify). Unset, slog.Default() is used. Call it once at
// startup, before the Orchestrator serves callbacks.
func (o *Orchestrator) SetWebhookLogger(l *slog.Logger) { o.webhookLogger = l }
