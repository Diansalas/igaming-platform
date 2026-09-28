package audit

import (
	"context"

	"github.com/google/uuid"
)

// Presentation controls what a tenant's platform-actions audit
// projection (ADR 0104 §5) reveals about a platform-scope action that
// concerns it. PRH-2 ships a RESOLVER WITH COMPILED-IN DEFAULTS ONLY
// (product-owner-proxy: "ACCEPT WITH CONDITIONS. Resolver-only satisfies
// HD-PRH2-5... There is a single jurisdiction today (Anjouan) and no B2B
// tenant, so the table would have no consumer") - there is deliberately
// no writable audit_presentation_policies table in this wave; that is
// registered follow-up AUDIT-PRESENTATION-POLICY-1. The record itself
// (audit_log) is NEVER altered by presentation - this only controls what
// the read-path projection includes.
type Presentation struct {
	// ActorPresentation is always "identified" in PRH-2 (HD-PRH2-5: the
	// tenant sees the identifiable platform actor). "pseudonymous" is
	// future work (ADR 0104 §5.5) and is NOT reachable today.
	ActorPresentation string
	// ShowNetworkMetadata gates ip_address/user_agent/request_id. SA-6:
	// LEGAL/PRIVACY REVIEW REQUIRED before any future policy ever sets
	// this true - carried into AUDIT-PRESENTATION-POLICY-1.
	ShowNetworkMetadata bool
	// ShowFreeFormMetadata gates any metadata key not on the per-action
	// TenantPresentation allowlist (allowlist.go).
	ShowFreeFormMetadata bool
}

// ActorPresentationIdentified is the only ActorPresentation value PRH-2
// can ever produce.
const ActorPresentationIdentified = "identified"

// restrictivePresentation is both the PRH-2 compiled-in default AND the
// fail-closed fallback for any resolver error - identified actor
// (HD-PRH2-5 still applies to WHO acted; it is network/free-form metadata
// that fails closed), no network metadata, no free-form metadata. §5.3:
// "Fail closed: a resolver error returns the most restrictive
// presentation... It never returns a wider one."
func restrictivePresentation() Presentation {
	return Presentation{ActorPresentation: ActorPresentationIdentified, ShowNetworkMetadata: false, ShowFreeFormMetadata: false}
}

// ResolvePresentation returns tenantID's platform-actions presentation
// rules. It always returns restrictivePresentation() today - the ctx and
// tenantID parameters exist so the SIGNATURE already matches what
// AUDIT-PRESENTATION-POLICY-1's real per-tenant/jurisdiction lookup (a
// platform-authored, append-only, effective-dated table) will need,
// without any caller of this function changing when that table lands.
// This function itself never errors, but every caller MUST still route
// through PresentationResolver/ResolveOrRestrictive (never call this
// directly and trust a nil error blindly) so that becomes true, and
// remains true, for free.
func ResolvePresentation(_ context.Context, _ uuid.UUID) (Presentation, error) {
	return restrictivePresentation(), nil
}

// PresentationResolver matches ResolvePresentation's signature. Callers
// (internal/httpserver's Deps.AuditPresentationResolver) carry this as a
// field, defaulting to ResolvePresentation when nil, so a test can inject
// a forced failure without touching production wiring - the ONLY
// sanctioned way to exercise §7's "a forced resolver error fails closed"
// test.
type PresentationResolver func(ctx context.Context, tenantID uuid.UUID) (Presentation, error)

// ResolvePresentationOrRestrictive calls resolver (ResolvePresentation if
// resolver is nil) and ALWAYS fails closed: any non-nil error, for any
// reason, yields restrictivePresentation() - it is never propagated to
// the caller as a wider presentation, and the caller never sees the
// error itself (SA-6/§5.3: a resolver failure must never accidentally
// widen disclosure by falling through to "no presentation restriction at
// all").
func ResolvePresentationOrRestrictive(ctx context.Context, resolver PresentationResolver, tenantID uuid.UUID) Presentation {
	if resolver == nil {
		resolver = ResolvePresentation
	}
	p, err := resolver(ctx, tenantID)
	if err != nil {
		return restrictivePresentation()
	}
	return p
}
