// Package geolocation defines a provider-neutral interface for obtaining a
// coarse, country-grain physical-location signal for a player - the shared
// substrate a future phase can wire a real vendor behind. Mirrors
// internal/kyc.KYCProvider's exact structural pattern (one normalized
// result type crossing the boundary, two-tier failure modeling, a Mock
// implementation with test-configuration hooks) by design (Stage 4I Phase
// B directive: "this new package must mirror internal/kyc.KYCProvider's
// exact structural pattern... do not invent a different pattern").
//
// This package makes four binding commitments:
//
//  1. SignalResult deliberately carries no coordinates, city, ISP, ASN, or
//     postal code, and no raw vendor payload - country grain only. A field
//     that does not exist cannot be persisted or logged; this is a
//     stronger enforcement of "never record raw geolocation evidence" than
//     a comment-only rule, by construction.
//
//  2. This package contains no SQL, no migration, no table, and defines no
//     persistence function of any kind. It cannot accumulate a location
//     history because it cannot write anywhere.
//
//  3. Failure is modeled two ways. A Go error return means the call itself
//     failed (network/timeout/malformed response) and callers must treat
//     the returned SignalResult as meaningless in that case. An Outcome
//     value other than SignalResolved (a successful call that could not
//     resolve, or that the vendor declined, or that the vendor reports as
//     failed) means the call succeeded but produced no usable signal.
//     CountryCode must be ignored by every caller unless
//     Outcome == SignalResolved. Callers must fail closed on all four
//     non-SignalResolved/error paths (error, SignalInconclusive,
//     SignalUnavailable, SignalError).
//
//  4. A future consumer of this package (not part of this phase) must, per
//     the platform's jurisdiction-evidence audit discipline, write exactly
//     one audit_log entry per consultation carrying
//     {provider_id, outcome, provider_reference, request_id} - and must
//     NEVER include the country code, the request's IP address, or any
//     coordinate in that audit entry. This is a contractual note for that
//     future caller, not something this package itself does: this package
//     has no dependency on internal/audit and must not import it.
//
// This package is deliberately NOT registered anywhere in
// cmd/platform-api/main.go, has no Orchestrator/provider-registry type
// (unlike internal/kyc), and is not reachable from any HTTP route. This is
// intentional for this phase (canonical-model §12.2 item 1 / §9.4 - a
// vendor on this signal path would become a hard dependency of whatever
// future consumer calls it, which needs its own decision, gated on its own
// vendor-selection decision and its own security review).
package geolocation

import (
	"context"

	"github.com/google/uuid"
)

// SignalOutcome is what a LocationProvider reports for a single
// GetSignal call - normalized, platform-owned values only, mirroring
// kyc.ProviderOutcome's identical "vendor-specific strings never leak into
// core logic" contract.
type SignalOutcome string

const (
	// SignalResolved means the vendor call succeeded and produced a usable
	// country-grain result. CountryCode is only meaningful when Outcome
	// equals this value.
	SignalResolved SignalOutcome = "resolved"
	// SignalInconclusive means the vendor answered but could not determine
	// a location.
	SignalInconclusive SignalOutcome = "inconclusive"
	// SignalUnavailable means the vendor answered but declines to answer,
	// or has no coverage for this request.
	SignalUnavailable SignalOutcome = "unavailable"
	// SignalError means the call succeeded at the transport level but the
	// vendor itself reports a failure processing the request.
	SignalError SignalOutcome = "error"
)

// SignalRequest is GetSignal's argument - carries only opaque platform
// identifiers plus the one piece of request-scoped input an IP-derived
// vendor needs.
type SignalRequest struct {
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	RequestID       string
	// IPAddress is request-scoped and MUST NOT be stored, logged, or
	// returned by any implementation of this interface. It exists only so
	// an IP-derived vendor can be adapted at all.
	IPAddress string
}

// SignalResult is GetSignal's return value - the ONE normalized shape
// every caller ever sees. See this package's doc comment, binding
// requirement 1: this type must never grow a coordinate, city, ISP, ASN,
// postal code, or raw-vendor-payload field.
type SignalResult struct {
	Outcome SignalOutcome
	// CountryCode is ISO-3166 alpha-2, or "" unless Outcome ==
	// SignalResolved. Callers must ignore this field for every other
	// Outcome value.
	CountryCode string
	// ProviderReference is an opaque vendor reference, for reconstruction.
	ProviderReference string
	// Reason is a short machine-readable code, never a vendor payload.
	Reason string
}

// Capabilities describes what a registered LocationProvider supports -
// mirrors kyc.Capabilities/casino.CasinoProvider's identical "ask the
// adapter what it can do" convention.
type Capabilities struct {
	SupportsCountry bool
	// SupportsSubdivision is false for every conceivable Phase-B provider;
	// declared so a finer-grained vendor later is a capability change, not
	// a type change.
	SupportsSubdivision  bool
	SupportsVPNDetection bool
}

// LocationProvider is the provider-neutral interface every adapter (real
// or mock) implements - mirrors kyc.KYCProvider's exact shape.
type LocationProvider interface {
	ID() string
	GetSignal(ctx context.Context, req SignalRequest) (SignalResult, error)
	GetCapabilities() Capabilities
	HealthStatus(ctx context.Context) error
}
