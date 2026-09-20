package jurisdiction

// This file implements Stage 4I Phase E's SEC-4I-F10 predicate (architect
// design ruling, "Stage 4I Phase E: Operating Market & Country Policy
// Foundation" §2.5; recorded in full at docs/decisions/0045-operating-
// market-and-country-policy-foundation.md). EvaluateLicenceValidity is
// the SINGLE technical implementation, anywhere in this codebase, of "is
// this licence currently reliable" - a second copy is forbidden (INV-M-5).
//
// SEC-4I-F10 / PHASE-A-SEC-2 SCOPE, STATED PLAINLY: this function exists
// and is consumed by internal/operatingmarket ONLY. resolver.go's own
// resolveTenantLicence does NOT call it in this phase and has ZERO diff -
// this file is an ADDITION, not a modification, to this package. The
// finding stays OPEN for the player-jurisdiction path; what changes is
// that the remaining work there is now exactly one call site plus one
// semantic choice (refused(dependency_unavailable) vs. unresolved),
// instead of a whole design.
//
// This file deliberately touches nothing else in this package: no other
// file in internal/jurisdiction has any diff from this addition.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LicenceValidity is a closed enum. Every value except LicenceValid is a
// fail-closed condition.
type LicenceValidity string

const (
	LicenceValid         LicenceValidity = "valid"
	LicenceNotBound      LicenceValidity = "not_bound"      // tenants.licence_id IS NULL
	LicenceNotFound      LicenceValidity = "not_found"      // licence_id names no row (registry integrity)
	LicenceSuspended     LicenceValidity = "suspended"      // licences.status = 'suspended'
	LicenceStatusExpired LicenceValidity = "status_expired" // licences.status = 'expired'
	LicenceDateExpired   LicenceValidity = "date_expired"   // expires_at has passed at AsOf
	LicenceNotYetIssued  LicenceValidity = "not_yet_issued" // issued_at is in the future at AsOf
)

// EvaluateLicenceValidity answers, for an explicit AsOf and with no
// wall-clock read of its own, whether a licence may be relied on. licenceID
// may be uuid.Nil, which is treated as "no licence bound"
// (LicenceNotBound) rather than a query - callers resolve tenants.
// licence_id themselves and pass the result through unchanged, including
// when it is NULL.
//
// A licence is reliable only within a HALF-OPEN interval: valid from
// issued_at INCLUSIVE to expires_at EXCLUSIVE (ADR 0045 §18, finding F4).
// Both boundary comparisons are STRICT UTC-date comparisons
// (licences.issued_at/expires_at are DATE, not TIMESTAMPTZ - INC-4): a
// licence is INVALID on its own stated expiry date, and VALID on its own
// stated issue date. This is the conservative reading of a genuinely
// ambiguous legal convention (inclusive vs. exclusive boundaries),
// deliberately not escalated as a human-decision item: it invents no legal
// grace period (the fail-closed instruction this predicate must honour),
// and its operational cost is exactly zero in this phase because no
// consumer is wired yet (docs/governance/task-registry.md item
// MKT-EXPIRY-1, widened by ADR 0045 §18 to cover both boundaries, records
// the named revisit trigger - the phase that first wires
// ResolveOperatingCountryPolicy into any consuming domain must confirm
// both boundaries with compliance before that wiring goes live).
func EvaluateLicenceValidity(ctx context.Context, q ReadOnlyQuerier, licenceID uuid.UUID, asOf time.Time) (LicenceValidity, error) {
	if licenceID == uuid.Nil {
		return LicenceNotBound, nil
	}

	var status string
	var issuedAt *time.Time
	var expiresAt *time.Time
	err := q.QueryRow(ctx, `SELECT status, issued_at, expires_at FROM licences WHERE id = $1`, licenceID).Scan(&status, &issuedAt, &expiresAt)
	if err != nil {
		// pgx.ErrNoRows and any other read error both mean "this licence
		// cannot be relied on" here - the caller (internal/operatingmarket)
		// maps LicenceNotFound to a data-integrity outcome distinct from a
		// genuine denial; a transport-level error is returned as an error,
		// not silently folded into LicenceNotFound.
		if errors.Is(err, pgx.ErrNoRows) {
			return LicenceNotFound, nil
		}
		return "", fmt.Errorf("jurisdiction: read licence %s for validity evaluation: %w", licenceID, err)
	}

	switch status {
	case "suspended":
		return LicenceSuspended, nil
	case "expired":
		return LicenceStatusExpired, nil
	case "active":
		// fall through to the expiry-date check below
	default:
		return "", fmt.Errorf("%w: unrecognized licences.status %q", ErrInvalidInput, status)
	}

	// ADR 0045 §18 (finding F4). A licence that has not been ISSUED yet
	// cannot be relied on, exactly as one that has expired cannot; the
	// original predicate read only status and expires_at, so a future
	// issued_at resolved VALID - a fail-OPEN in the ceiling's own root.
	// The interval is HALF-OPEN: valid from issued_at INCLUSIVE to
	// expires_at EXCLUSIVE. The inclusive lower bound is the natural
	// reading (a licence is in force on the day it is issued) and is
	// deliberately symmetric with the exclusive upper bound documented
	// above. licences.issued_at is DATE and NULLABLE, and NO Go code
	// writes it today (CreateLicence does not set it): a NULL issued_at is
	// therefore treated as "no issue date asserted" and falls through,
	// NOT as "not yet issued" - the latter would fail-close every existing
	// licence row. That NULL-handling choice, and whether issued_at should
	// become NOT NULL with a populating admin surface, are added to
	// MKT-EXPIRY-1's compliance-confirmation scope, which ADR 0045 §18
	// widens to cover BOTH boundaries rather than expiry alone.
	if issuedAt != nil && asOf.UTC().Truncate(24*time.Hour).Before(*issuedAt) {
		return LicenceNotYetIssued, nil
	}

	if expiresAt != nil && !asOf.UTC().Truncate(24*time.Hour).Before(*expiresAt) {
		return LicenceDateExpired, nil
	}
	return LicenceValid, nil
}
