// Part C of docs/decisions/0083-sportsbook-jurisdiction-gating-and-
// cumulative-exposure.md (§5.4): the ONE shared "may this player bet on
// this selection" evaluation, consumed by both PlaceBet
// (enforcement point 2, orchestrator.go) and the catalogue annotators
// (enforcement point 1, catalogue.go) - INV-SB-JUR-1. There is exactly one
// call site for jurisdiction.Resolve in this package (verified by
// TestSportsbookCatalogue_AndPlaceBetShareOneEvaluation's static source
// check): ResolvePlayerJurisdiction, below.
//
// Mirrors internal/casino's K-3 remediation (evaluateJurisdictionBlocklist,
// internal/casino/orchestrator.go) byte-for-byte in behaviour:
//
//   - K3-1 (arming): no active restriction row for any of a bet's three
//     catalogue levels => the control is NOT ARMED and never denies,
//     whatever the resolution's outcome. Without this, every sportsbook
//     bet would start denying with jurisdiction_unresolved the moment
//     this code ships, because every player-scoped jurisdiction.Resolve
//     call resolves unresolved(no_signal) today (ADR 0083 §1.9 - HDR-J-7
//     is unanswered).
//   - K3-2 (armed + unresolved): armed and the resolution did not resolve
//     => deny with DenialCodeJurisdictionUnresolved, NEVER
//     DenialCodeJurisdictionBlocked. The two stay distinguishable
//     internally and in the audit record; the HTTP boundary (K3-6,
//     internal/httpserver/sportsbook_handlers.go) collapses them into one
//     opaque player-facing message.
package sportsbook

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
)

// AvailabilityContext is the server-derived context a jurisdiction
// evaluation runs in. Every field is resolved from the authenticated
// session, never from a request body, header, query or path segment
// (INV-SB-JUR-3). The ZERO VALUE means ANONYMOUS: no player exists to
// resolve a jurisdiction for, so no annotation is applied and the
// catalogue is returned exactly as it is today (§5.4.1) - both catalogue
// read endpoints (GET /v1/sportsbook/sports, GET /v1/sportsbook/events/
// {id}) are genuinely anonymous today (ADR 0081 §2.3), so this is the
// live behaviour of those routes, not a hypothetical.
type AvailabilityContext struct {
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
}

// IsAnonymous reports whether this context carries no player at all.
func (a AvailabilityContext) IsAnonymous() bool { return a.PlayerAccountID == uuid.Nil }

// catalogueScope is the three catalogue ids one selection resolves into.
// PlaceBet already has all three from getSelectionWithContext - no extra
// query is introduced on the bet path to obtain them.
type catalogueScope struct {
	EventID     uuid.UUID
	MarketID    uuid.UUID
	SelectionID uuid.UUID
}

// AvailabilityDecision is the single shared gate's output. A restriction
// is reported as a RESULT, never a Go error - identical convention to
// PlaceBetResult/casino.LaunchGameResult, for the identical reason (the
// audit record for a denial must commit in the same transaction as the
// decision, which a Go error would otherwise roll back).
type AvailabilityDecision struct {
	Available bool
	// DenialCode is one of DenialCodeJurisdictionUnresolved/
	// DenialCodeJurisdictionBlocked when Available is false, empty
	// otherwise.
	DenialCode string
}

const (
	// DenialCodeJurisdictionUnresolved is reported when at least one active
	// restriction applies to this bet's catalogue scope (the control is
	// "armed" - K3-1) but the platform could not determine the player's
	// jurisdiction (jurisdiction.Resolve did not return Resolved). This is
	// NOT "blocked in this jurisdiction" - the player's jurisdiction is
	// unknown, not known-and-disallowed - and must never be reported as
	// DenialCodeJurisdictionBlocked (K3-2).
	DenialCodeJurisdictionUnresolved = "jurisdiction_unresolved"
	// DenialCodeJurisdictionBlocked is reported when the player's resolved
	// jurisdiction code matches an active restriction on this bet's
	// catalogue scope.
	DenialCodeJurisdictionBlocked = "jurisdiction_blocked"
)

// ResolvePlayerJurisdiction is the ONE call both enforcement points make
// (INV-SB-JUR-1). opClass is jurisdiction.OperationPlay for placement and
// jurisdiction.OperationCatalogueAvailability for a catalogue read - a
// compile-time constant at each call site, never a parameter derived from
// input (canonical-model §4.4 Layer 1). It calls jurisdiction.Resolve and
// then Resolution.AssertScope (canonical-model §4.4 Layer 2 - re-asserting
// the resolution's own tenant/brand/player binding against the caller's
// OWN authenticated context before ever using it), and returns the
// Resolution unchanged; it never inspects Code() itself.
func ResolvePlayerJurisdiction(
	ctx context.Context, tx pgx.Tx,
	ac AvailabilityContext, opClass jurisdiction.OperationClass,
) (jurisdiction.Resolution, error) {
	res, err := jurisdiction.Resolve(ctx, tx, jurisdiction.Params{
		TenantID: ac.TenantID, BrandID: &ac.BrandID, PlayerAccountID: &ac.PlayerAccountID,
		OperationClass:       opClass,
		RequestedByActorType: jurisdiction.ActorPlayer, RequestedByActorID: &ac.PlayerAccountID,
	})
	if err != nil {
		return jurisdiction.Resolution{}, fmt.Errorf("sportsbook: resolve jurisdiction: %w", err)
	}
	if err := res.AssertScope(ac.TenantID, &ac.BrandID, &ac.PlayerAccountID); err != nil {
		return jurisdiction.Resolution{}, fmt.Errorf("sportsbook: jurisdiction resolution scope: %w", err)
	}
	return res, nil
}

// loadActiveRestrictions returns the DISTINCT jurisdiction codes actively
// restricted at ANY of scope's three levels, in ONE query - the three
// partial unique indexes on sb_jurisdiction_restrictions
// (idx_sb_jur_restr_event_open/_market_open/_selection_open, migration
// 0087) each cover one of the three OR'd predicates below, so Postgres can
// satisfy this via a bitmap-OR across all three rather than a sequential
// scan. Zero rows today (nothing is configured anywhere on the platform -
// ADR 0083 §2), so this is a cheap, empty-result read on every existing
// call path.
func loadActiveRestrictions(ctx context.Context, tx pgx.Tx, scope catalogueScope) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT jurisdiction_code FROM sb_jurisdiction_restrictions
		 WHERE status = 'active' AND (event_id = $1 OR market_id = $2 OR selection_id = $3)`,
		scope.EventID, scope.MarketID, scope.SelectionID,
	)
	if err != nil {
		return nil, fmt.Errorf("sportsbook: load active jurisdiction restrictions: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, fmt.Errorf("sportsbook: scan active jurisdiction restriction: %w", err)
		}
		out = append(out, code)
	}
	return out, rows.Err()
}

// evaluateJurisdictionRestriction is the byte-for-byte behavioural mirror
// of casino.evaluateJurisdictionBlocklist: K3-1 (empty restricted set =>
// not armed => never denies) and K3-2 (armed + non-Resolved => Unresolved,
// never Blocked). A pure function of its two arguments - unit-testable
// without a database, exactly as casino's is.
func evaluateJurisdictionRestriction(restricted []string, res jurisdiction.Resolution) (AvailabilityDecision, error) {
	if len(restricted) == 0 {
		return AvailabilityDecision{Available: true}, nil
	}
	if res.Outcome() != jurisdiction.Resolved {
		return AvailabilityDecision{Available: false, DenialCode: DenialCodeJurisdictionUnresolved}, nil
	}
	code, err := res.Code()
	if err != nil {
		// Unreachable: Code() is only unreachable for a non-Resolved
		// outcome (jurisdiction.ErrNotResolved's own doc comment), and the
		// check immediately above already confirmed Resolved. Kept as a
		// hard stop rather than a silent fallthrough if it is ever reached.
		return AvailabilityDecision{}, fmt.Errorf("sportsbook: resolved jurisdiction code: %w", err)
	}
	if sliceContainsString(restricted, code) {
		return AvailabilityDecision{Available: false, DenialCode: DenialCodeJurisdictionBlocked}, nil
	}
	return AvailabilityDecision{Available: true}, nil
}

// sliceContainsString is a package-local helper mirroring
// internal/casino's identical containsString - duplicated rather than
// imported because internal/sportsbook must not import internal/casino
// (see this package's existing evaluateAndAuditEligibility doc comment for
// the full package-boundary rationale).
func sliceContainsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// evaluateOperatingMarket - RUNG 2 (§5.3.3) - BLOCKED on HDR-J-7.
//
// Deliberately NOT declared. ADR 0083 §5.3.3 is explicit that "no stub is
// shipped": a stub that always returns "not determinable" would be dead
// code with a fail-open shape, which CLAUDE.md's no-fake-completion rule
// is better served by NOT shipping. The exact contract this function must
// implement, verbatim, when HDR-J-7 is answered:
//
//	// evaluateOperatingMarket consults the licence-ceiling / tenant / brand /
//	// operation-policy chain for a player whose OPERATING COUNTRY has been
//	// determined by the single canonical owner of that determination.
//	//
//	// BLOCKED on HDR-J-7. Not implemented in Stage 9.2 - see ADR 0083 §5.3.3.
//	// When implemented:
//	//   - countryCode MUST come from the canonical player-jurisdiction
//	//     determination path, never from jurisdictions.country_code (ADR 0045
//	//     INC-6 / INV-M-4) and never from a client.
//	//   - `armed` is false, and permitted is true, ONLY when no operating
//	//     country is determinable at all. It is NEVER false because a policy
//	//     is missing: operatingmarket.OutcomeNotConfigured is a DENY
//	//     (ADR 0045 INV-M-2 - the tenant-scope enabled row is a mandatory
//	//     opt-in), as is every outcome other than OutcomePermitted.
//	//   - AsOf is the caller's own placement timestamp, passed explicitly;
//	//     ResolveOperatingCountryPolicy never calls time.Now() (INV-M-3).
//	//   - OperationCode is the compile-time constant "wagering" and
//	//     ProductCode is the compile-time constant "sportsbook" - both are
//	//     already seeded vocabulary rows (migrations 0076 / 0045). Neither is
//	//     ever a wire string.
//	//   - Result.AssertScope(tenantID, &brandID) is called and refused on.
//	//   - The eleven-valued Outcome is NEVER collapsed into the player-facing
//	//     response; the player sees one opaque unavailability reason, and the
//	//     blocking rung is reachable only through the staff-only
//	//     ExplainOperatingCountryPolicy (ADR 0045 §4 property 2).
//	func evaluateOperatingMarket(
//	    ctx context.Context, tx pgx.Tx,
//	    tenantID uuid.UUID, brandID *uuid.UUID,
//	    countryCode string, asOf time.Time,
//	) (permitted bool, outcome operatingmarket.Outcome, err error)
//
// See docs/governance/task-registry.md's SB-JUR-RUNG2-1 entry for the
// launch-sequencing consequence of this rung remaining unimplemented.
