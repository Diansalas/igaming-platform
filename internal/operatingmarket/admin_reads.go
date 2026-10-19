package operatingmarket

// lic4 slice 4a: platform-scope READ views over the licence country ceilings and a tenant's operating-country policies, for a
// Back Office. Read-only; each function asserts platform scope first. No state is written and no resolution is performed
// (these are stored versions, not a permission decision: ResolveOperatingCountryPolicy remains the only decider).

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/validation"
)

// ListLicenceCeilingVersionsPage lists a licence's ceiling versions (every country unless countryCode is set), newest first,
// with the total. currentOnly keeps only the open version of each country. tx MUST be platform-admin-scoped.
func ListLicenceCeilingVersionsPage(ctx context.Context, tx pgx.Tx, licenceID uuid.UUID, countryCode string, currentOnly bool, limit, offset int) ([]CeilingRecord, int, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return nil, 0, err
	}
	if licenceID == uuid.Nil {
		return nil, 0, fmt.Errorf("%w: licence_id is required", ErrInvalidInput)
	}
	countryCode = strings.TrimSpace(countryCode)
	if countryCode != "" && !validation.IsISO3166Alpha2(countryCode) {
		return nil, 0, fmt.Errorf("%w: country_code must be a valid, uppercase ISO-3166-1 alpha-2 code", ErrInvalidInput)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM licences WHERE id = $1)`, licenceID).Scan(&exists); err != nil {
		return nil, 0, fmt.Errorf("operatingmarket: read licence: %w", err)
	}
	if !exists {
		return nil, 0, fmt.Errorf("%w: unknown licence_id %s", ErrNotFound, licenceID)
	}
	const where = ` WHERE licence_id = $1 AND ($2 = '' OR country_code = $2) AND (NOT $3 OR effective_to IS NULL)`
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM licence_country_ceilings`+where, licenceID, countryCode, currentOnly).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("operatingmarket: count ceilings: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT id, licence_id, country_code, state, status, COALESCE(authorization_reference, ''),
		       reason_code, policy_version, effective_from, effective_to,
		       created_by_actor_type, created_by_actor_id, created_at
		  FROM licence_country_ceilings`+where+`
		 ORDER BY country_code, effective_from DESC, id LIMIT $4 OFFSET $5`,
		licenceID, countryCode, currentOnly, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("operatingmarket: list ceilings: %w", err)
	}
	defer rows.Close()
	var out []CeilingRecord
	for rows.Next() {
		var rec CeilingRecord
		var state, status string
		if err := rows.Scan(&rec.ID, &rec.LicenceID, &rec.CountryCode, &state, &status, &rec.AuthorizationReference,
			&rec.ReasonCode, &rec.PolicyVersion, &rec.EffectiveFrom, &rec.EffectiveTo, &rec.CreatedByActorType, &rec.CreatedByActorID, &rec.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("operatingmarket: scan ceiling: %w", err)
		}
		if rec.State, err = parseState(state); err != nil {
			return nil, 0, fmt.Errorf("operatingmarket: stored ceiling state is invalid: %w", err)
		}
		if rec.Status, err = parseStatus(status); err != nil {
			return nil, 0, fmt.Errorf("operatingmarket: stored ceiling status is invalid: %w", err)
		}
		out = append(out, rec)
	}
	return out, total, rows.Err()
}

// ListTenantPolicyVersionsPage lists a tenant's operating-country policy versions (all scopes), newest first, with the total.
// operating_country_policies is tenant-RLS: there is no platform read policy and none is added. This reads it the way the
// licensing change executor writes it - the platform-admin session binds the TARGET tenant (app.tenant_id) for the read only
// and clears it again; the tenant read policy is what admits the rows, so a different tenant's rows can never appear.
// currentOnly keeps only the open version of each key. tx MUST be platform-admin-scoped (no tenant GUC on entry).
func ListTenantPolicyVersionsPage(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, countryCode string, currentOnly bool, limit, offset int) ([]PolicyRecord, int, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return nil, 0, err
	}
	if tenantID == uuid.Nil {
		return nil, 0, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	countryCode = strings.TrimSpace(countryCode)
	if countryCode != "" && !validation.IsISO3166Alpha2(countryCode) {
		return nil, 0, fmt.Errorf("%w: country_code must be a valid, uppercase ISO-3166-1 alpha-2 code", ErrInvalidInput)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, tenantID).Scan(&exists); err != nil {
		return nil, 0, fmt.Errorf("operatingmarket: read tenant: %w", err)
	}
	if !exists {
		return nil, 0, fmt.Errorf("%w: unknown tenant_id %s", ErrNotFound, tenantID)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		return nil, 0, fmt.Errorf("operatingmarket: bind target tenant for the read: %w", err)
	}
	defer func() { _, _ = tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true)`) }()

	const where = ` WHERE tenant_id = $1 AND ($2 = '' OR country_code = $2) AND (NOT $3 OR effective_to IS NULL)`
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM operating_country_policies`+where, tenantID, countryCode, currentOnly).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("operatingmarket: count policies: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT id, tenant_id, scope_kind, brand_id, operation_code, product_code, country_code,
		       state, status, COALESCE(authorization_reference, ''), reason_code, policy_version,
		       effective_from, effective_to, created_by_actor_type, created_by_actor_id, created_at
		  FROM operating_country_policies`+where+`
		 ORDER BY country_code, scope_kind, effective_from DESC, id LIMIT $4 OFFSET $5`,
		tenantID, countryCode, currentOnly, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("operatingmarket: list policies: %w", err)
	}
	var out []PolicyRecord
	for rows.Next() {
		var rec PolicyRecord
		var scopeKind, state, status string
		if err := rows.Scan(&rec.ID, &rec.TenantID, &scopeKind, &rec.BrandID, &rec.OperationCode, &rec.ProductCode, &rec.CountryCode,
			&state, &status, &rec.AuthorizationReference, &rec.ReasonCode, &rec.PolicyVersion,
			&rec.EffectiveFrom, &rec.EffectiveTo, &rec.CreatedByActorType, &rec.CreatedByActorID, &rec.CreatedAt); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("operatingmarket: scan policy: %w", err)
		}
		if rec.Scope, err = parseScope(scopeKind); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("operatingmarket: stored scope_kind is invalid: %w", err)
		}
		if rec.State, err = parseState(state); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("operatingmarket: stored policy state is invalid: %w", err)
		}
		if rec.Status, err = parseStatus(status); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("operatingmarket: stored policy status is invalid: %w", err)
		}
		out = append(out, rec)
	}
	rows.Close()
	return out, total, rows.Err()
}
