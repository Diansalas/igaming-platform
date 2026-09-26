package kyc

import (
	"context"
	"errors"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// O4 (Stage 10.3 W2a; 01-provider-trust-analysis.md §5 O4): player
// self-service KYC no longer hard-codes Provider("mock"). The provider is
// selected from TENANT CONFIGURATION, and selection fails closed when none
// is configured.
//
// The configuration row is the tenant's own provider credential: a tenant
// is configured for KYC provider P when it holds an ACTIVE, in-window
// outbound_api handle for (kyc, P) in provider_credential_handles
// (migration 0096; four-eyes governed, versioned, tenant-owned, FORCE RLS).
// A real KYC vendor cannot be called without that credential anyway, so
// the credential IS the per-tenant selection; no second table is invented.
//
// Rules, in order:
//  1. Registered adapters the tenant holds such a handle for: exactly one ->
//     selected; more than one -> ErrKYCProviderAmbiguous (fail closed, never
//     a guess).
//  2. None: if the process registers exactly one adapter AND it is a
//     synthetic (MOCK) component, it is selected - the synthetic adapter
//     needs no credential, and a synthetic component can never run in
//     production (ADR 0085 guard). Otherwise ErrNoKYCProviderConfigured.

// ErrNoKYCProviderConfigured means the tenant has no usable KYC provider.
var ErrNoKYCProviderConfigured = errors.New("kyc: no KYC provider is configured for this tenant")

// ErrKYCProviderAmbiguous means more than one KYC provider is configured
// for the tenant; selection refuses to guess.
var ErrKYCProviderAmbiguous = errors.New("kyc: more than one KYC provider is configured for this tenant")

// configuredKYCProvidersSQL is one read-only, tenant-predicated read of the
// tenant's active outbound KYC credentials (no secret material).
const configuredKYCProvidersSQL = `SELECT DISTINCT provider_id FROM provider_credential_handles
	WHERE tenant_id = $1 AND domain = 'kyc' AND purpose = 'outbound_api' AND status = 'active'
	  AND not_before <= now() AND (not_after IS NULL OR not_after > now())`

type syntheticKYCComponent interface{ SyntheticComponent() }

// SelectProvider returns the tenant's configured KYC provider, read in tx
// (a tenant-scoped transaction for tenantID).
func (o *Orchestrator) SelectProvider(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (KYCProvider, error) {
	if o == nil || tenantID == uuid.Nil {
		return nil, ErrNoKYCProviderConfigured
	}
	rows, err := tx.Query(ctx, configuredKYCProvidersSQL, tenantID)
	if err != nil {
		return nil, err
	}
	var configured []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		if p, ok := o.providers[id]; ok && p != nil {
			configured = append(configured, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(configured)
	switch len(configured) {
	case 1:
		return o.providers[configured[0]], nil
	case 0:
	default:
		return nil, ErrKYCProviderAmbiguous
	}
	if len(o.providers) == 1 {
		for _, p := range o.providers {
			if _, synthetic := p.(syntheticKYCComponent); synthetic {
				return p, nil
			}
		}
	}
	return nil, ErrNoKYCProviderConfigured
}
