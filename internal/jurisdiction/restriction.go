package jurisdiction

import (
	"errors"
	"fmt"
	"slices"
)

// RestrictionOutcome is the closed set of outcomes a single jurisdiction
// gate can produce for a market-access decision (Stage 4I Phase C).
type RestrictionOutcome string

const (
	RestrictionAllowed    RestrictionOutcome = "allowed"
	RestrictionRestricted RestrictionOutcome = "restricted"
	RestrictionBlocked    RestrictionOutcome = "blocked"
)

// Severity reports o's relative severity: blocked=3, restricted=2,
// allowed=1. Any other value, INCLUDING the zero value "", returns an
// error - never a severity of 0 or a silent default, so an unrecognized or
// unset outcome can never be mistaken for the least-restrictive one.
func (o RestrictionOutcome) Severity() (int, error) {
	switch o {
	case RestrictionBlocked:
		return 3, nil
	case RestrictionRestricted:
		return 2, nil
	case RestrictionAllowed:
		return 1, nil
	default:
		return 0, ErrUnknownRestrictionOutcome
	}
}

// AppliedRestriction is one gate's contribution to a composed market-
// access decision.
type AppliedRestriction struct {
	Outcome RestrictionOutcome
	// Basis identifies which candidate produced this restriction.
	Basis Basis
	// Code is obtainable only via Candidate.Code() - see
	// PlayerJurisdictionCode's own doc comment for why that is
	// structurally the only way to obtain one.
	Code PlayerJurisdictionCode
	// GateID is a caller-owned opaque identifier for the record that
	// produced this restriction.
	GateID string
}

// ComposedRestriction is ComposeRestrictions' output. Fields are
// unexported - constructed only by ComposeRestrictions.
type ComposedRestriction struct {
	outcome      RestrictionOutcome
	contributors []AppliedRestriction
}

func (c ComposedRestriction) Outcome() RestrictionOutcome { return c.outcome }

// Contributors returns a DEFENSIVE COPY of the composed decision's
// contributors, never the internal backing array (PHASE-C fix round,
// architect P1-3 / security SEC-4I-C-01 - the same class of defect closed
// on PlayerJurisdictionResult.Candidates()/ConsideredEvidence(): without
// this, a caller could rewrite a contributor's Outcome in place, leaving a
// composed record whose own Outcome() and Contributors() disagree.)
func (c ComposedRestriction) Contributors() []AppliedRestriction { return slices.Clone(c.contributors) }

// String returns a REDACTED representation - AppliedRestriction.Code is an
// EXPORTED field, so the default %v/%+v verbs would otherwise print it in
// full via PlayerJurisdictionCode's own zero-arg String(); this override
// exists specifically so logging a ComposedRestriction never does that.
func (c ComposedRestriction) String() string {
	return fmt.Sprintf("ComposedRestriction{outcome=%s, contributors=%d}", c.outcome, len(c.contributors))
}

// GoString implements fmt.GoStringer - see Candidate.GoString's own doc
// comment for why %#v needs this too.
func (c ComposedRestriction) GoString() string {
	return fmt.Sprintf("jurisdiction.ComposedRestriction{outcome:%q, contributors:%d, <redacted>}", c.outcome, len(c.contributors))
}

// String returns a REDACTED representation of one contributor - Code is
// deliberately omitted; see ComposedRestriction.String's own doc comment.
func (a AppliedRestriction) String() string {
	return fmt.Sprintf("AppliedRestriction{outcome=%s, basis=%s, gate_id=%s}", a.Outcome, a.Basis, a.GateID)
}

// GoString implements fmt.GoStringer - see Candidate.GoString's own doc
// comment for why %#v needs this too.
func (a AppliedRestriction) GoString() string {
	return fmt.Sprintf("jurisdiction.AppliedRestriction{Outcome:%q, Basis:%q, GateID:%q, Code:<redacted>}", a.Outcome, a.Basis, a.GateID)
}

var (
	// ErrNoRestrictionsToCompose is returned for an empty input slice -
	// NEVER RestrictionAllowed, since "nothing to evaluate" is not the
	// same fact as "evaluated and allowed".
	ErrNoRestrictionsToCompose = errors.New("jurisdiction: no restrictions to compose")
	// ErrUnknownRestrictionOutcome is returned for the WHOLE call if any
	// element's Outcome.Severity() errors (unknown or zero-value) - such
	// an element is never silently skipped or treated as severity 0.
	ErrUnknownRestrictionOutcome = errors.New("jurisdiction: unknown restriction outcome")
)

// ComposeRestrictions returns the MAXIMUM severity among in, plus EVERY
// contributor at that severity, in input order. There is deliberately no
// pairwise MostRestrictive(a,b) function: a pairwise "winner" must
// arbitrarily break ties between two equally-severe-but-different
// restrictions; this function instead returns ALL contributors at the
// winning severity, so no tie-break policy needs to exist.
func ComposeRestrictions(in []AppliedRestriction) (ComposedRestriction, error) {
	if len(in) == 0 {
		return ComposedRestriction{}, ErrNoRestrictionsToCompose
	}

	severities := make([]int, len(in))
	maxSeverity := 0
	for i, r := range in {
		sev, err := r.Outcome.Severity()
		if err != nil {
			return ComposedRestriction{}, ErrUnknownRestrictionOutcome
		}
		severities[i] = sev
		if sev > maxSeverity {
			maxSeverity = sev
		}
	}

	// PHASE-C fix round (architect P3-1): derive the winning outcome from
	// maxSeverity via this explicit reverse mapping, never from
	// "whichever contributor happened to be seen last at the winning
	// severity" - the doc comment above already claims "no tie-break
	// policy needs to exist", and a last-wins assignment silently
	// contradicted that (harmless only by coincidence, because today's
	// three outcomes are each other's unique severity - a fourth outcome
	// sharing a severity would reintroduce an undetected arbitrary pick).
	severityToOutcome := map[int]RestrictionOutcome{3: RestrictionBlocked, 2: RestrictionRestricted, 1: RestrictionAllowed}
	winningOutcome, ok := severityToOutcome[maxSeverity]
	if !ok {
		return ComposedRestriction{}, ErrUnknownRestrictionOutcome
	}

	var contributors []AppliedRestriction
	for i, r := range in {
		if severities[i] == maxSeverity {
			contributors = append(contributors, r)
		}
	}

	return ComposedRestriction{outcome: winningOutcome, contributors: contributors}, nil
}
