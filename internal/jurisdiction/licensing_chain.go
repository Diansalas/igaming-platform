package jurisdiction

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LicensingOutcome is the explicit result of the operator-side chain
// brand -> tenant -> licence -> jurisdiction. It is NOT player jurisdiction
// resolution (ADR 0042 HDR-J-1 forbids a tenant/brand fallback there): it is
// the licensing jurisdiction used to select rulebooks (KYC thresholds, RG
// floors, payout-instrument freshness, readiness). A brand carries no licence
// of its own, so the chain is read from its tenant (migration 0008).
//
// The type exists so a caller MUST decide what "no licence" means for it,
// instead of every caller inventing its own default (licensing enforcement
// slice 3, G15). The helper never judges licence status or dates (use
// EvaluateLicenceValidity for that); it states only what is bound.
type LicensingOutcome string

const (
	// LicensingResolved: the tenant has a licence bound and the licence names a
	// jurisdiction that exists. JurisdictionID/Code and LicenceID are set.
	LicensingResolved LicensingOutcome = "resolved"
	// LicensingNoLicence: the tenant row is visible and no licence is bound.
	LicensingNoLicence LicensingOutcome = "no_licence"
	// LicensingInvalid: the chain cannot be relied on (see InvalidReason). A
	// caller must treat it as fail closed.
	LicensingInvalid LicensingOutcome = "licence_invalid"
)

// LicensingInvalidReason is the closed reason set for LicensingInvalid.
type LicensingInvalidReason string

const (
	// The tenant row is not visible (unknown tenant, or not this session's tenant).
	LicensingInvalidTenantNotVisible LicensingInvalidReason = "tenant_not_visible"
	// tenants.licence_id names a licence row that cannot be read, or whose
	// jurisdiction cannot be read.
	LicensingInvalidLicenceNotFound LicensingInvalidReason = "licence_or_jurisdiction_not_found"
)

// LicensingChain is ResolveLicensingChain's result.
type LicensingChain struct {
	Outcome          LicensingOutcome
	InvalidReason    LicensingInvalidReason
	TenantID         uuid.UUID
	LicenceID        uuid.UUID
	JurisdictionID   uuid.UUID
	JurisdictionCode string
}

// ResolveLicensingChain reads tenants.licence_id -> licences.jurisdiction_id ->
// jurisdictions.code through q (read-only, no lock, no write). A genuine read
// failure is an error, never one of the three states.
func ResolveLicensingChain(ctx context.Context, q ReadOnlyQuerier, tenantID uuid.UUID) (LicensingChain, error) {
	if tenantID == uuid.Nil {
		return LicensingChain{}, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	out := LicensingChain{TenantID: tenantID}
	var licenceID *uuid.UUID
	err := q.QueryRow(ctx, `SELECT licence_id FROM tenants WHERE id = $1`, tenantID).Scan(&licenceID)
	if errors.Is(err, pgx.ErrNoRows) {
		out.Outcome, out.InvalidReason = LicensingInvalid, LicensingInvalidTenantNotVisible
		return out, nil
	}
	if err != nil {
		return LicensingChain{}, fmt.Errorf("jurisdiction: read tenant licence: %w", err)
	}
	if licenceID == nil {
		out.Outcome = LicensingNoLicence
		return out, nil
	}
	var jurID uuid.UUID
	var code string
	err = q.QueryRow(ctx, `SELECT l.jurisdiction_id, j.code FROM licences l JOIN jurisdictions j ON j.id = l.jurisdiction_id WHERE l.id = $1`,
		*licenceID).Scan(&jurID, &code)
	if errors.Is(err, pgx.ErrNoRows) {
		out.Outcome, out.InvalidReason = LicensingInvalid, LicensingInvalidLicenceNotFound
		return out, nil
	}
	if err != nil {
		return LicensingChain{}, fmt.Errorf("jurisdiction: read licence jurisdiction: %w", err)
	}
	out.Outcome, out.LicenceID, out.JurisdictionID, out.JurisdictionCode = LicensingResolved, *licenceID, jurID, code
	return out, nil
}
