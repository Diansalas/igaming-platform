package operatingmarket

// lic14 (migration 0183, H-1): which jurisdiction's RULEBOOK (KYC thresholds, RG floors, payout freshness) governs an operation.
// Before lic14 every rulebook consumer read tenants.licence_id and treated "no licence" as "nothing required" (default-allow),
// so a money or wager path opened through an operating basis (notably a documented no-licence basis) ran with dormant KYC. The
// rulebook jurisdiction is now selected by the GOVERNING BASIS of the scope:
//
//   - basis row present and permitting at this step: its jurisdiction_id (no_licence_documented, mandatory field) or the
//     governing licence's jurisdiction (licence kinds);
//   - the tenant has NO approved, active, in-force basis at all: RulebookLegacy, the pre-lic14 behaviour exactly;
//   - the tenant has bases but the scope context (country) is unavailable, or the scope resolves to a row that does not permit,
//     or to no row while the tenant has no licence: RulebookUnavailable (callers treat it as deny, never as not-required).
//
// Read-only; asserts the tenant-scoped, non-player transaction first (a mis-scoped read would see zero rows and look like
// "no basis", which must never read as permission).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type RulebookState string

const (
	RulebookLegacy      RulebookState = "legacy"
	RulebookBasis       RulebookState = "basis"
	RulebookUnavailable RulebookState = "unavailable"
)

// RulebookQuery names the scope. CountryCode may be empty only when the caller has no country: then a tenant with bases is
// RulebookUnavailable.
type RulebookQuery struct {
	TenantID      uuid.UUID
	BrandID       *uuid.UUID
	CountryCode   string
	OperationCode string
	ProductCode   *string
	AsOf          time.Time
}

// RulebookJurisdiction is the answer. HasBasis is true when the tenant has any approved, active, in-force basis (a Legacy answer
// with HasBasis means "this scope has no row": a caller whose legacy licence read finds NO licence must then fail closed).
type RulebookJurisdiction struct {
	State          RulebookState
	JurisdictionID uuid.UUID
	BasisID        uuid.UUID
	Kind           BasisKind
	HasBasis       bool
	Reason         string
}

// TenantHasApprovedBasis reports whether the tenant has any active, approved (executed change request), in-force basis row.
func TenantHasApprovedBasis(ctx context.Context, q ReadOnlyQuerier, tenantID uuid.UUID, asOf time.Time) (bool, error) {
	if tenantID == uuid.Nil || asOf.IsZero() {
		return false, fmt.Errorf("%w: tenant_id and as_of are required", ErrInvalidInput)
	}
	inScope, err := assertTenantScope(ctx, q, tenantID)
	if err != nil {
		return false, err
	}
	if !inScope {
		return false, fmt.Errorf("%w: transaction tenant scope does not match tenant %s", ErrTransactionScope, tenantID)
	}
	var has bool
	if err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM operating_bases
		                WHERE tenant_id = $1 AND status = 'active' AND change_request_id IS NOT NULL
		                  AND effective_from <= $2 AND (effective_to IS NULL OR effective_to > $2))`, tenantID, asOf).Scan(&has); err != nil {
		return false, fmt.Errorf("operatingmarket: read operating bases: %w", err)
	}
	return has, nil
}

// ResolveRulebookJurisdiction implements the selection above. An error means "could not decide": callers fail closed.
func ResolveRulebookJurisdiction(ctx context.Context, q ReadOnlyQuerier, p RulebookQuery) (RulebookJurisdiction, error) {
	has, err := TenantHasApprovedBasis(ctx, q, p.TenantID, p.AsOf)
	if err != nil {
		return RulebookJurisdiction{}, err
	}
	if !has {
		return RulebookJurisdiction{State: RulebookLegacy}, nil
	}
	out := RulebookJurisdiction{HasBasis: true}
	if p.CountryCode == "" {
		out.State, out.Reason = RulebookUnavailable, "basis_scope_unavailable"
		return out, nil
	}
	if p.OperationCode == "" {
		return RulebookJurisdiction{}, fmt.Errorf("%w: operation_code is required", ErrInvalidInput)
	}
	sel, err := selectBasis(ctx, q, Query{TenantID: p.TenantID, BrandID: p.BrandID, CountryCode: p.CountryCode,
		OperationCode: p.OperationCode, ProductCode: p.ProductCode, AsOf: p.AsOf})
	if err != nil {
		return RulebookJurisdiction{}, err
	}
	switch sel.state {
	case basisSelNone:
		out.State = RulebookLegacy // the scope has no row; the caller fails closed if it has no licence either
		return out, nil
	case basisSelRow:
	default:
		out.State, out.Reason = RulebookUnavailable, "basis_not_permitting"
		return out, nil
	}
	out.BasisID, out.Kind = sel.id, sel.kind
	switch sel.kind {
	case BasisNoLicenceDocumented:
		if sel.jurisdiction == uuid.Nil {
			out.State, out.Reason = RulebookUnavailable, "basis_jurisdiction_missing"
			return out, nil
		}
		out.State, out.JurisdictionID = RulebookBasis, sel.jurisdiction
	default:
		var jid uuid.UUID
		err := q.QueryRow(ctx, `SELECT jurisdiction_id FROM licences WHERE id = $1`, sel.licenceID).Scan(&jid)
		if errors.Is(err, pgx.ErrNoRows) {
			out.State, out.Reason = RulebookUnavailable, "basis_licence_unreadable"
			return out, nil
		}
		if err != nil {
			return RulebookJurisdiction{}, fmt.Errorf("operatingmarket: read basis licence jurisdiction: %w", err)
		}
		out.State, out.JurisdictionID = RulebookBasis, jid
	}
	return out, nil
}
