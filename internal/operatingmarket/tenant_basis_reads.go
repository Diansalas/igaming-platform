package operatingmarket

// c2: the TENANT-scoped, READ-ONLY views over a tenant's OWN operating bases (migration 0180, no new migration). The tenant id
// is a parameter ONLY because the caller (the HTTP handler) passes the authenticated context's tenant; every function first
// asserts that the transaction is tenant-scoped (db.Pool.WithTenant) to exactly that tenant and not player-scoped, and the
// FORCE-RLS policy operating_bases_tenant_read independently admits only that tenant's rows. A foreign row, a foreign brand and an
// unknown id are therefore the same ErrNotFound. These return stored versions, never a permission decision.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/validation"
)

func assertOwnTenantTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	inScope, err := assertTenantScope(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	if !inScope {
		return fmt.Errorf("%w: transaction tenant scope does not match tenant %s", ErrTransactionScope, tenantID)
	}
	return setReadOnly(ctx, tx)
}

// ListOwnOperatingBasesPage lists the tenant's operating basis versions (newest first within country/operation) with the total.
// brandID, when set, restricts to that brand's rows and MUST belong to the tenant: a brand that is unknown or another tenant's is
// ErrNotFound (identical). currentOnly keeps only the open version of each scope.
func ListOwnOperatingBasesPage(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, brandID *uuid.UUID, countryCode string, currentOnly bool, limit, offset int) ([]BasisRecord, int, error) {
	if err := assertOwnTenantTx(ctx, tx, tenantID); err != nil {
		return nil, 0, err
	}
	if limit < 1 || limit > 200 || offset < 0 {
		return nil, 0, fmt.Errorf("%w: limit must be 1..200 and offset >= 0", ErrInvalidInput)
	}
	countryCode = strings.TrimSpace(countryCode)
	if countryCode != "" && !validation.IsISO3166Alpha2(countryCode) {
		return nil, 0, fmt.Errorf("%w: country_code must be a valid, uppercase ISO-3166-1 alpha-2 code", ErrInvalidInput)
	}
	brand := uuid.Nil
	if brandID != nil {
		if *brandID == uuid.Nil {
			return nil, 0, fmt.Errorf("%w: brand_id must not be nil", ErrInvalidInput)
		}
		var owned bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brands WHERE id = $1 AND tenant_id = $2)`, *brandID, tenantID).Scan(&owned); err != nil {
			return nil, 0, fmt.Errorf("operatingmarket: read brand: %w", err)
		}
		if !owned {
			return nil, 0, fmt.Errorf("%w: brand", ErrNotFound)
		}
		brand = *brandID
	}
	const where = ` WHERE tenant_id = $1 AND ($2::uuid = '00000000-0000-0000-0000-000000000000'::uuid OR brand_id = $2)
		AND ($3 = '' OR country_code = $3) AND (NOT $4 OR effective_to IS NULL)`
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM operating_bases`+where, tenantID, brand, countryCode, currentOnly).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("operatingmarket: count own operating bases: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT `+basisColumns+` FROM operating_bases`+where+`
		ORDER BY country_code, operation_code, effective_from DESC, id LIMIT $5 OFFSET $6`, tenantID, brand, countryCode, currentOnly, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("operatingmarket: list own operating bases: %w", err)
	}
	defer rows.Close()
	var out []BasisRecord
	for rows.Next() {
		r, err := scanBasis(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("operatingmarket: scan operating basis: %w", err)
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// GetOwnOperatingBasis reads one version of the tenant's own bases. Unknown and foreign ids are the same ErrNotFound.
func GetOwnOperatingBasis(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (BasisRecord, error) {
	if err := assertOwnTenantTx(ctx, tx, tenantID); err != nil {
		return BasisRecord{}, err
	}
	if id == uuid.Nil {
		return BasisRecord{}, fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	rec, err := scanBasis(tx.QueryRow(ctx, `SELECT `+basisColumns+` FROM operating_bases WHERE id = $1 AND tenant_id = $2`, id, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return BasisRecord{}, fmt.Errorf("%w: operating basis", ErrNotFound)
	}
	if err != nil {
		return BasisRecord{}, fmt.Errorf("operatingmarket: read own operating basis: %w", err)
	}
	return rec, nil
}
