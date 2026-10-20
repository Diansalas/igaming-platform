package aml

// ADR 0116 slice S2 (A7): the vendor-agnostic sanctions / PEP SCREENING interface, its closed result vocabulary and the pure,
// FAIL-CLOSED policy decision. LABELS: the interface and the decision engine are IMPLEMENTED; the only provider is a MOCK
// (screening_mock.go); a real screening vendor is PROVIDER DEPENDENT; identity attributes (name, date of birth,
// nationality) are BLOCKED on the ADR 0116 Q6 privacy review, so a request carries only a per-tenant pseudonymous reference.
// Nothing here is a regulatory approval and no vendor semantics are invented: the types below are OUR contract, which an
// adapter must map onto.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// ListType is a screening list (ADR 0116 s4.1).
type ListType string

const (
	ListSanctions ListType = "sanctions"
	ListPEP       ListType = "pep"
)

// Valid reports whether l is one of the closed list names.
func (l ListType) Valid() bool { return l == ListSanctions || l == ListPEP }

// ScreeningOutcome is the closed result vocabulary. `error` covers every failure to obtain a COMPLETED answer (the ADR's
// `unavailable`): it is never a clear, and the orchestrator never treats it as one.
type ScreeningOutcome string

const (
	OutcomeClear          ScreeningOutcome = "clear"
	OutcomePotentialMatch ScreeningOutcome = "potential_match"
	OutcomeConfirmed      ScreeningOutcome = "confirmed"
	OutcomeError          ScreeningOutcome = "error"
)

// ScreeningErrorCode is the closed reason of an `error` outcome. No vendor text ever reaches it.
type ScreeningErrorCode string

const (
	ErrCodeTimeout         ScreeningErrorCode = "timeout"
	ErrCodeUnavailable     ScreeningErrorCode = "unavailable"
	ErrCodeInvalidResponse ScreeningErrorCode = "invalid_response"
	ErrCodeRateLimited     ScreeningErrorCode = "rate_limited"
	ErrCodeUnknown         ScreeningErrorCode = "unknown"
)

func (c ScreeningErrorCode) valid() bool {
	switch c {
	case ErrCodeTimeout, ErrCodeUnavailable, ErrCodeInvalidResponse, ErrCodeRateLimited, ErrCodeUnknown:
		return true
	}
	return false
}

// Errors a provider may return from Screen; the orchestrator maps every error (these or any other) to an `error` outcome.
var (
	ErrProviderTimeout     = errors.New("aml screening: provider timeout")
	ErrProviderUnavailable = errors.New("aml screening: provider unavailable")
	ErrProviderRateLimited = errors.New("aml screening: provider rate limited")
	// ErrIdempotencyConflict: the same idempotency key was presented with a different request.
	ErrIdempotencyConflict = errors.New("aml screening: idempotency key reused with a different request")
)

// ScreeningCapabilities is what an adapter declares it can do.
type ScreeningCapabilities struct {
	Sanctions bool
	PEP       bool
	// SupportsSubjectReference: the adapter screens by an opaque subject reference (no platform id, no attributes).
	SupportsSubjectReference bool
}

// ScreeningSubject is the KYC-vendor subject reference ONLY, until the Q6 privacy review allows attributes. It never carries
// a platform id.
type ScreeningSubject struct {
	VendorSubjectRef string
}

// ScreeningRequest is what a provider receives. SubjectRef is the per-tenant pseudonymous reference (C7): never persons.id
// and never a player_account id.
type ScreeningRequest struct {
	IdempotencyKey string
	SubjectRef     string
	Subject        ScreeningSubject
	Lists          []ListType
}

var (
	idempotencyKeyRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	subjectRefRE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	providerRefRE    = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	providerIDRE     = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)
)

// Validate checks the request shape every adapter must be able to rely on.
func (r ScreeningRequest) Validate() error {
	if !idempotencyKeyRE.MatchString(r.IdempotencyKey) {
		return fmt.Errorf("%w: idempotency key", ErrInvalidInput)
	}
	if !subjectRefRE.MatchString(r.SubjectRef) {
		return fmt.Errorf("%w: subject reference must be 64 lowercase hex characters", ErrInvalidInput)
	}
	if len(r.Lists) < 1 || len(r.Lists) > 2 {
		return fmt.Errorf("%w: one or two lists", ErrInvalidInput)
	}
	seen := map[ListType]bool{}
	for _, l := range r.Lists {
		if !l.Valid() || seen[l] {
			return fmt.Errorf("%w: list %q", ErrInvalidInput, l)
		}
		seen[l] = true
	}
	return nil
}

// ScreeningMatch is one hit. VendorMatchRef and Category are OPAQUE strings: they are never parsed and never persisted by the
// orchestrator (only the list name is).
type ScreeningMatch struct {
	List           ListType
	VendorMatchRef string
	Category       string
}

// ScreeningResult is what a provider returns. Provider-originated errors (Screen returning a non-nil error) are mapped by the
// orchestrator to an `error` outcome; a provider may also return Outcome `error` with a closed ErrorCode.
type ScreeningResult struct {
	Outcome           ScreeningOutcome
	Matches           []ScreeningMatch
	ErrorCode         ScreeningErrorCode
	ProviderReference string
	ScreenedAt        time.Time
	// matchRefs (lic23, N-2) is set by Orchestrator.callProvider for a match outcome: the sorted "list:vendor-match-reference" pairs, only
	// when EVERY match carries a reference. The automatic worker turns it into a keyed pseudonymous fingerprint immediately and never
	// stores or logs the references themselves.
	matchRefs []string
}

// ScreeningProvider is OUR vendor-agnostic contract (SumSub / Veriff / Jumio-shaped vendors are mapped onto it by an adapter).
// An implementation must be safe for concurrent use and idempotent per IdempotencyKey: the same key with the same request
// returns the same answer, the same key with a different request is ErrIdempotencyConflict.
type ScreeningProvider interface {
	ID() string
	Capabilities() ScreeningCapabilities
	Screen(ctx context.Context, in ScreeningRequest) (ScreeningResult, error)
}

// HoldAction is what a screening outcome requires of enforcement (ADR 0116 s4.3).
type HoldAction string

const (
	HoldNone             HoldAction = "none"
	HoldWithdrawalPayout HoldAction = "hold_withdrawal_and_payout"
	HoldAllValueMovement HoldAction = "hold_all_value_movement"
)

// Hold reason tokens a screening decision places (closed set; the database requires ^[a-z0-9_]{1,64}$).
const (
	ReasonSanctionsMatch     = "screening_sanctions_potential_match"
	ReasonSanctionsConfirmed = "screening_sanctions_confirmed"
	ReasonPEPMatch           = "screening_pep_potential_match"
	ReasonUnavailable        = "screening_unavailable"
)

// Policy is the effective per-tenant, per-jurisdiction screening policy (a configuration row; no default, no seed).
type Policy struct {
	Lists            []ListType
	RescreenInterval time.Duration
	BackfillGrace    time.Duration
	EffectiveFrom    time.Time
}

// Decision is the pure answer of Decide.
type Decision struct {
	Action HoldAction
	// ReasonCode is the hold reason token ("" when Action is none).
	ReasonCode string
	// OpenCase: a screening case should be opened for an officer (matches only).
	OpenCase bool
	// Retry: no completed result was obtained; the caller must retry with the SAME idempotency key. A retry-pending person
	// is NEVER treated as clear.
	Retry bool
}

// Decide is the fail-closed policy of ADR 0116 s4.3. It is pure (no I/O, no clock beyond `now`).
//
//   - A match ALWAYS acts, with or without a policy: a sanctions-list match or a confirmed outcome gates all value movement, a
//     PEP-only match gates withdrawal and payout; both open a case.
//   - An error is NEVER a clear. Without a policy screening is not mandated: no hold, retry. With a policy: before its
//     effective_from nothing is mandated (retry); a person registered at or after effective_from has no result and is held
//     (withdrawal and payout); an existing person is held once the backfill grace has elapsed.
//   - Only a COMPLETED clear is a clear.
func Decide(now time.Time, p *Policy, personRegisteredAt time.Time, outcome ScreeningOutcome, matches []ListType) Decision {
	switch outcome {
	case OutcomeClear:
		return Decision{Action: HoldNone}
	case OutcomeConfirmed:
		return Decision{Action: HoldAllValueMovement, ReasonCode: ReasonSanctionsConfirmed, OpenCase: true}
	case OutcomePotentialMatch:
		for _, l := range matches {
			if l == ListSanctions {
				return Decision{Action: HoldAllValueMovement, ReasonCode: ReasonSanctionsMatch, OpenCase: true}
			}
		}
		// Any other (PEP-only, or an ambiguous match the orchestrator has widened to the requested lists) gates the PEP scope.
		return Decision{Action: HoldWithdrawalPayout, ReasonCode: ReasonPEPMatch, OpenCase: true}
	}
	// outcome error, or ANY unknown value: never a clear.
	d := Decision{Action: HoldNone, Retry: true}
	if p == nil || now.Before(p.EffectiveFrom) {
		return d
	}
	if !personRegisteredAt.Before(p.EffectiveFrom) || !now.Before(p.EffectiveFrom.Add(p.BackfillGrace)) {
		d.Action, d.ReasonCode = HoldWithdrawalPayout, ReasonUnavailable
	}
	return d
}

// sortedLists returns the closed, sorted, de-duplicated string form of lists (the canonical form the proof binds).
func sortedLists(ls []ListType) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range ls {
		if !seen[string(l)] {
			seen[string(l)] = true
			out = append(out, string(l))
		}
	}
	sort.Strings(out)
	return out
}
