package jurisdiction

// lic4 slice 4a: the READ side a Back Office needs over the platform jurisdiction / licence registry. Every function asserts
// platform scope first (the registry RLS is the independent backstop) and writes nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// GetJurisdiction returns one jurisdiction. tx MUST be platform-admin-scoped.
func GetJurisdiction(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Jurisdiction, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return Jurisdiction{}, err
	}
	if id == uuid.Nil {
		return Jurisdiction{}, fmt.Errorf("%w: jurisdiction id is required", ErrInvalidInput)
	}
	return readJurisdictionByID(ctx, tx, id)
}

// GetLicence returns one licence with its lifecycle fields. tx MUST be platform-admin-scoped.
func GetLicence(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Licence, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return Licence{}, err
	}
	if id == uuid.Nil {
		return Licence{}, fmt.Errorf("%w: licence id is required", ErrInvalidInput)
	}
	l, err := readLicence(ctx, tx, id)
	if err != nil {
		return Licence{}, err
	}
	return withListColumns(ctx, tx, l)
}

// withListColumns loads permitted_products / permitted_markets, which the lifecycle select does not carry.
func withListColumns(ctx context.Context, tx pgx.Tx, l Licence) (Licence, error) {
	var products, markets []string
	var pj, mj []byte
	if err := tx.QueryRow(ctx, `SELECT permitted_products, permitted_markets FROM licences WHERE id = $1`, l.ID).Scan(&pj, &mj); err != nil {
		return l, fmt.Errorf("jurisdiction: read licence lists: %w", err)
	}
	if err := unmarshalStrings(pj, &products); err != nil {
		return l, err
	}
	if err := unmarshalStrings(mj, &markets); err != nil {
		return l, err
	}
	l.PermittedProducts, l.PermittedMarkets = products, markets
	return l, nil
}

// LicenceBoundTenantIDs returns, per licence id, the tenants whose tenants.licence_id names it (sorted by tenant id). Licences
// with no bound tenant are absent from the map. tx MUST be platform-admin-scoped. A tenant id is not a secret to platform staff.
func LicenceBoundTenantIDs(ctx context.Context, tx pgx.Tx, licenceIDs []uuid.UUID) (map[uuid.UUID][]uuid.UUID, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return nil, err
	}
	out := map[uuid.UUID][]uuid.UUID{}
	if len(licenceIDs) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT licence_id, id FROM tenants WHERE licence_id = ANY($1) ORDER BY licence_id, id`, licenceIDs)
	if err != nil {
		return nil, fmt.Errorf("jurisdiction: list licence-bound tenants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l, t uuid.UUID
		if err := rows.Scan(&l, &t); err != nil {
			return nil, fmt.Errorf("jurisdiction: scan licence-bound tenant: %w", err)
		}
		out[l] = append(out[l], t)
	}
	return out, rows.Err()
}

// ListTenantJurisdictionConfigs returns a tenant's configuration versions (every jurisdiction, open and closed), newest first,
// with the total count. tx MUST be platform-admin-scoped (tjc_platform_read, migration 0177). An unknown tenant is ErrNotFound.
func ListTenantJurisdictionConfigs(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit, offset int) ([]TenantJurisdictionConfig, int, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return nil, 0, err
	}
	if tenantID == uuid.Nil {
		return nil, 0, fmt.Errorf("%w: tenant id is required", ErrInvalidInput)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, tenantID).Scan(&exists); err != nil {
		return nil, 0, fmt.Errorf("jurisdiction: read tenant: %w", err)
	}
	if !exists {
		return nil, 0, fmt.Errorf("%w: tenant %s", ErrNotFound, tenantID)
	}
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tenant_jurisdiction_configs WHERE tenant_id = $1`, tenantID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("jurisdiction: count configurations: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT `+tjcColumns+` FROM tenant_jurisdiction_configs WHERE tenant_id = $1
		ORDER BY effective_from DESC, id LIMIT $2 OFFSET $3`, tenantID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("jurisdiction: list configurations: %w", err)
	}
	defer rows.Close()
	var out []TenantJurisdictionConfig
	for rows.Next() {
		c, err := scanTJC(rows)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				break
			}
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

func unmarshalStrings(raw []byte, dst *[]string) error {
	*dst = []string{}
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("jurisdiction: stored list is not a string array: %w", err)
	}
	return nil
}
