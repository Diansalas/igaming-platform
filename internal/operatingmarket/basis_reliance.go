package operatingmarket

// lic17 (security review F-1 of lic11): the licences a tenant RELIES ON outside its own tenants.licence_id. After lic9 / lic14 a
// tenant (or one of its brands) can operate under a licence named by an approved, active, in-force operating_bases row
// (platform_licence or partner_licence), or under a documented no-licence basis. The withdrawal restriction policy must evaluate
// every licence relied on, not only tenants.licence_id.
//
// Read-only. Asserts the tenant-scoped, non-player transaction first (a mis-scoped read would see zero rows and look like "no
// basis"). The scope is the tenant and, when given, one brand: tenant-wide rows plus that brand's rows (another brand's partner
// licence is not relied on by this brand). Country, operation and product are deliberately NOT narrowed: a withdrawal has no
// country, and every licence the tenant / brand operates under is relied on. A row's review state (overdue, needs_rereview) does
// not remove the reliance: the licence is still the one the operation stands on.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// BasisReliance is the answer of TenantBasisReliance.
type BasisReliance struct {
	// Licences are the distinct licences named by the tenant's (and brand's) approved, active, in-force licence bases.
	Licences []uuid.UUID
	// HasBasis is true when ANY approved, active, in-force basis row (any kind, tenant-wide or of the brand) exists.
	HasBasis bool
	// HasDocumentedNoLicence is true when one of those rows is a documented no-licence basis.
	HasDocumentedNoLicence bool
}

// TenantBasisReliance reads the approved (executed change request), active, in-force operating_bases rows of the tenant (tenant-wide
// or of brandID when it is not Nil) at asOf. A schema older than migration 0180 has no bases.
func TenantBasisReliance(ctx context.Context, q ReadOnlyQuerier, tenantID, brandID uuid.UUID, asOf time.Time) (BasisReliance, error) {
	if tenantID == uuid.Nil || asOf.IsZero() {
		return BasisReliance{}, fmt.Errorf("%w: tenant_id and as_of are required", ErrInvalidInput)
	}
	inScope, err := assertTenantScope(ctx, q, tenantID)
	if err != nil {
		return BasisReliance{}, err
	}
	if !inScope {
		return BasisReliance{}, fmt.Errorf("%w: transaction tenant scope does not match tenant %s", ErrTransactionScope, tenantID)
	}
	var present bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('public.operating_bases') IS NOT NULL`).Scan(&present); err != nil {
		return BasisReliance{}, fmt.Errorf("operatingmarket: probe operating_bases: %w", err)
	}
	if !present {
		return BasisReliance{}, nil
	}
	var brand *uuid.UUID
	if brandID != uuid.Nil {
		brand = &brandID
	}
	rows, err := q.Query(ctx, `
		SELECT basis_kind, licence_id FROM operating_bases
		 WHERE tenant_id = $1 AND status = 'active' AND change_request_id IS NOT NULL
		   AND (brand_id IS NULL OR brand_id = $2)
		   AND effective_from <= $3 AND (effective_to IS NULL OR effective_to > $3)
		 ORDER BY licence_id NULLS LAST`, tenantID, brand, asOf)
	if err != nil {
		return BasisReliance{}, fmt.Errorf("operatingmarket: read operating bases: %w", err)
	}
	defer rows.Close()
	var out BasisReliance
	seen := map[uuid.UUID]bool{}
	for rows.Next() {
		var kind string
		var lid *uuid.UUID
		if err := rows.Scan(&kind, &lid); err != nil {
			return BasisReliance{}, fmt.Errorf("operatingmarket: scan operating basis: %w", err)
		}
		k, err := parseBasisKind(kind)
		if err != nil {
			return BasisReliance{}, err
		}
		out.HasBasis = true
		switch k {
		case BasisNoLicenceDocumented:
			out.HasDocumentedNoLicence = true
		default:
			if lid == nil {
				return BasisReliance{}, fmt.Errorf("%w: a licence basis without a licence", ErrInvalidInput)
			}
			if !seen[*lid] {
				seen[*lid] = true
				out.Licences = append(out.Licences, *lid)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return BasisReliance{}, fmt.Errorf("operatingmarket: read operating bases: %w", err)
	}
	return out, nil
}
