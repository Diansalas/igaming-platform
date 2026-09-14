// Package identityresolution implements Stage 4E's Person-resolution
// boundary: a provider-neutral interface answering "does this
// registration correspond to an existing, platform-wide Person" - and
// the safe registration orchestration built on top of it. It exists to
// close the gap Stage 4D-RG's own specialist review identified as a P0:
// internal/identity.RegisterPlayer previously created a brand-new,
// unlinked Person on every single registration, with no way to recognize
// that two registrations (e.g. at two different brands) belong to the
// same real person - which is exactly what a self-excluded Person needs
// in order to evade the restriction by registering again.
//
// This package does NOT implement real KYC/AML. No real identity-
// verification vendor is integrated, no production identity documents
// are collected, no biometric data is stored, and no deterministic
// matching algorithm is invented (ADR 0027 §3/§10 - matching on a single
// weak, unverified attribute like email would be worse than no matching
// at all: confidently wrong). The only implementation this stage ships
// is MockPersonResolver - a fully conformant test/development stand-in,
// mirroring internal/casino.MockCasinoProvider and
// internal/payments.MockPaymentProvider's identical role: a real vendor
// slots in behind this SAME interface later without touching any caller.
//
// See docs/decisions/0027-person-resolution-and-cross-brand-identity-
// foundation.md for the full design and docs/architecture/05-identity-
// architecture.md's updated section for how this composes with the
// existing Person/PlayerAccount/Tenant/Brand/Wallet model (no second
// identity model is introduced).
package identityresolution

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// Outcome is PersonResolver's own result taxonomy - directive §5
// explicitly requires four DISTINCT states, never collapsed: an
// UNCERTAIN result must never be silently forced into Match or NoMatch,
// and a provider failure must never be silently treated as NoMatch
// either (that would be exactly the "silently create a second Person"
// bug this package exists to prevent).
type Outcome string

const (
	// Match means the resolver has identified an existing Person this
	// registration belongs to. MatchedPersonID is set.
	Match Outcome = "match"
	// NoMatch means the resolver actively determined no existing Person
	// corresponds to this registration - a new Person is the correct
	// outcome, not a fallback/default guess.
	NoMatch Outcome = "no_match"
	// Uncertain means the resolver could not confidently decide either
	// way. This is NOT an error - the resolver ran successfully and is
	// reporting a genuine ambiguous result (e.g. a real future vendor
	// returning a low-confidence fuzzy match). Must land the registration
	// in a safe, non-gambling-capable review state (ADR 0027 §6).
	Uncertain Outcome = "uncertain"
)

// ErrResolverUnavailable is returned by a PersonResolver implementation
// when it cannot complete resolution at all (network failure, vendor
// outage, timeout, misconfiguration). Callers MUST treat this identically
// to Uncertain for the purpose of Person creation - see
// RegisterPlayerWithResolution and ADR 0027 §7's fail-safe rule: never
// silently fall back to creating an ordinary, unrestricted new Person
// just because the resolver was unreachable.
var ErrResolverUnavailable = errors.New("identityresolution: resolver unavailable")

// RawClaims is what the registration REQUEST itself asserts - entirely
// unverified, directly client-supplied, and NEVER used as a matching
// signal (ADR 0027 §10's "separate raw registration claims from verified
// identity evidence" rule). Carried through purely for audit/context
// (e.g. so an audit record can note which brand/email a resolution
// request was for) - a real PersonResolver implementation must not use
// these fields to decide Match/NoMatch/Uncertain.
type RawClaims struct {
	Email     string
	BrandSlug string
}

// VerifiedAttributes is identity evidence that has ALREADY been verified
// by a trusted source (a future KYC vendor, document verification,
// liveness check, etc.) - never populated from a bare registration
// request body today, since no such trusted source is integrated yet
// (ADR 0027 §3). Every field is optional; a real resolver is expected to
// use whichever subset it has and MUST NOT assume any single field
// uniquely identifies a Person (directive §10).
type VerifiedAttributes struct {
	LegalName             string
	DateOfBirth           string // ISO-8601 date (YYYY-MM-DD); a string, not time.Time, since this is opaque evidence handed to a resolver, never computed on
	Address               string
	Phone                 string
	Email                 string
	GovernmentIDReference string
	KYCProviderReference  string
}

// IsEmpty reports whether v carries no verified evidence at all - the
// honest, expected state of every registration today, since this stage
// integrates no real KYC source (ADR 0027 §3). MockPersonResolver's own
// default behavior keys off this.
func (v VerifiedAttributes) IsEmpty() bool {
	return v == VerifiedAttributes{}
}

// ResolutionInput is PersonResolver.Resolve's argument. TenantID/BrandID
// are always server-resolved, never client-supplied, matching every
// other identity-bearing input in this codebase.
type ResolutionInput struct {
	TenantID uuid.UUID
	BrandID  uuid.UUID
	Raw      RawClaims
	Verified VerifiedAttributes
}

// ResolutionResult is PersonResolver.Resolve's return value.
// MatchedPersonID is populated only when Outcome == Match. Reason is a
// short, machine-readable explanation (e.g. "no_verified_attributes",
// "government_id_reference_match") - logged/audited, so it must never
// contain raw identity evidence itself (ADR 0027 §11/§17).
type ResolutionResult struct {
	Outcome         Outcome
	MatchedPersonID uuid.UUID
	Reason          string
}

// PersonResolver is the provider-neutral interface every identity-
// resolution implementation satisfies - real vendor or mock alike. No
// vendor-specific type or shape appears here (ADR 0025/0022's identical
// "no provider branch" discipline, applied to identity resolution).
//
// Resolve returns ErrResolverUnavailable (never a bare, unwrapped error)
// when resolution could not be completed at all - callers distinguish
// this from a successful Uncertain result via errors.Is, and MUST treat
// both identically for Person-creation safety (ADR 0027 §7).
type PersonResolver interface {
	Resolve(ctx context.Context, input ResolutionInput) (ResolutionResult, error)
}
