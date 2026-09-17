package assetregistry

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AssetAuthorization is ADR 0037 §C.2's one canonical authorization
// service. It holds no state: every answer is computed from the database
// inside the caller's own transaction, so an authorization decision can
// never be served from a cache that has since gone stale (the same reason
// CLAUDE.md forbids a cached authoritative balance on the bet path).
type AssetAuthorization struct{}

// CheckEligibility walks ADR 0037 §A.3's layer chain in order -
// existence -> active -> platform-authorized -> tenant -> brand ->
// jurisdiction -> operation/product eligibility - short-circuiting at the
// first failing layer and returning that layer's own ReasonCode.
//
// Contract, in full (each clause is load-bearing and tested):
//
//   - eligible is true ONLY when every layer passed. Any other outcome is
//     (false, <the failing layer's ReasonCode>, err-or-nil).
//   - eligible is NEVER true when err != nil. That is the direction of
//     the invariant, and the only direction of it: a denial is free to
//     carry a nil error, and in fact every ordinary layer denial does -
//     (false, <ReasonCode>, nil) is the normal shape of "configuration
//     says no", which is not an error condition. A non-nil error means
//     the question could not be answered (bad input, no tenant scope, a
//     failed query) and is likewise always a denial, with no exception
//     and no fallback to a previously-known-good answer (ADR 0037 §C.2,
//     mirroring risk.Evaluate's identical rule). An earlier version of
//     this comment stated the converse ("a non-nil error ALWAYS
//     accompanies eligible == false"), which is backwards and was wrong
//     about this function's actual behaviour (code-reviewer finding F8):
//     a caller that believed it would have been entitled to treat
//     (false, reason, nil) as a non-answer and retry or ignore it,
//     turning every fail-closed denial into a fail-open one.
//   - tenant, brand and jurisdiction MUST be resolved server-side from
//     authenticated context by the caller. tenant is additionally
//     cross-checked against the transaction's own app.tenant_id GUC, so a
//     tenant identifier that arrived in a request payload cannot be
//     evaluated even if a handler mistakenly passed one through
//     (ErrTenantContextMismatch).
//   - A zero-value tenant or jurisdiction is an immediate denial. It is
//     never treated as "this check is not scoped to a jurisdiction,
//     therefore skip layer 6" - that reading is exactly Stage 4H-B0-R5
//     security finding S-6a's warning, and it matters today because no
//     per-player jurisdiction resolver exists anywhere in this codebase
//     yet (Stage 4G-FINAL Part C), so a zero jurisdiction is the
//     REALISTIC input, not a theoretical one.
//   - A zero-value brand is permitted (ADR 0037 §C.2: "may be zero-value
//     where an operation is not brand-scoped"), and means layer 5 is not
//     evaluated. Layer 5 can only ever narrow layer 4, so skipping it
//     cannot widen anything.
//
// tx must already be scoped to tenant via db.Pool.WithTenant (staff,
// system and provider-callback paths) or db.Pool.WithPlayerScope (a
// player-initiated operation - migration 0045's player_read policies make
// layers 4-7 READABLE, never writable, on that path, specifically so the
// eligibility read can happen in the same transaction as the financial
// write it authorizes, per CLAUDE.md's same-transaction rule). Layers 4-7
// are RLS-isolated either way, so a wrongly-scoped transaction sees no
// rows and every layer fails closed on its own.
func (a AssetAuthorization) CheckEligibility(
	ctx context.Context,
	tx pgx.Tx,
	tenant uuid.UUID,
	brand uuid.UUID,
	jurisdiction uuid.UUID,
	asset string,
	scope OperationScope,
) (bool, ReasonCode, error) {
	// --- Input/context gates. Every one of these is a denial, not a
	// "skip this layer". ---
	if tenant == uuid.Nil {
		return false, ReasonTenantContextMissing, fmt.Errorf("%w: tenant must be resolved from authenticated context", ErrInvalidInput)
	}
	if jurisdiction == uuid.Nil {
		return false, ReasonJurisdictionContextMissing, fmt.Errorf("%w: jurisdiction must be resolved from authenticated context; a zero jurisdiction is a denial, never an unscoped check", ErrInvalidInput)
	}
	if asset == "" {
		return false, ReasonAssetNotFound, fmt.Errorf("%w: asset code is required", ErrInvalidInput)
	}
	if scope.Product == "" {
		return false, ReasonProductContextMissing, fmt.Errorf("%w: product is required - a caller that cannot name its product cannot be authorized", ErrInvalidInput)
	}
	if !validOperation(scope.Operation) {
		return false, ReasonOperationUnknown, fmt.Errorf("%w: unknown operation %q", ErrInvalidInput, scope.Operation)
	}

	// The tenant argument must be the tenant this transaction is actually
	// authenticated as. Without this, a handler that read a tenant id out
	// of a request body would produce a decision about a tenant the
	// caller never proved it was - the exact spoofing shape ADR 0038 §11
	// forbids for provider callbacks.
	if err := assertTenantScope(ctx, tx, tenant); err != nil {
		return false, ReasonInternalError, err
	}

	// The product must exist and be active in the registry: an unknown or
	// retired product string can never authorize anything.
	var productActive bool
	err := tx.QueryRow(ctx, `SELECT active FROM platform_products WHERE code = $1`, scope.Product).Scan(&productActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ReasonProductUnknown, nil
	}
	if err != nil {
		return false, ReasonInternalError, fmt.Errorf("assetregistry: read product: %w", err)
	}
	if !productActive {
		return false, ReasonProductUnknown, nil
	}

	// --- Layers 1-3: the platform-wide facts on the `assets` row. ---
	var active, platformAuthorized bool
	err = tx.QueryRow(ctx, `SELECT active, platform_authorized FROM assets WHERE code = $1`, asset).Scan(&active, &platformAuthorized)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ReasonAssetNotFound, nil
	}
	if err != nil {
		return false, ReasonInternalError, fmt.Errorf("assetregistry: read asset: %w", err)
	}
	if !active {
		return false, ReasonAssetInactive, nil
	}
	if !platformAuthorized {
		return false, ReasonAssetNotPlatformAuthorized, nil
	}

	// --- Layer 4: tenant authorization. Its own row, its own reason
	// code - not derived from the same row layer 6 uses. ---
	tenantEligible, found, err := resolveScopeFact(ctx, tx, scopeQuery{
		tenantID: tenant, scopeKind: ScopeTenant, asset: asset, product: scope.Product,
	})
	if err != nil {
		return false, ReasonInternalError, err
	}
	if !found || !tenantEligible {
		return false, ReasonTenantNotAuthorized, nil
	}

	// --- Layer 5: brand authorization. Absent row = inherit the tenant
	// answer (ADR 0037 §A.5's nullable-narrowing pattern); a present row
	// can only ever deny. ---
	if brand != uuid.Nil {
		brandEligible, brandFound, err := resolveScopeFact(ctx, tx, scopeQuery{
			tenantID: tenant, scopeKind: ScopeBrand, brandID: &brand, asset: asset, product: scope.Product,
		})
		if err != nil {
			return false, ReasonInternalError, err
		}
		if brandFound && !brandEligible {
			return false, ReasonBrandNotAuthorized, nil
		}
	}

	// --- Layer 6: jurisdiction authorization. An independent fact, and
	// fail-closed on absence: no row for this (tenant, jurisdiction,
	// asset[, product]) means not authorized, never "unrestricted". ---
	jurisdictionEligible, found, err := resolveScopeFact(ctx, tx, scopeQuery{
		tenantID: tenant, scopeKind: ScopeJurisdiction, jurisdictionID: &jurisdiction, asset: asset, product: scope.Product,
	})
	if err != nil {
		return false, ReasonInternalError, err
	}
	if !found || !jurisdictionEligible {
		return false, ReasonJurisdictionNotAuthorized, nil
	}

	// --- Layer 7: per-product operation eligibility. The platform-wide
	// default must exist and be eligible; a tenant row may then only
	// narrow it. ---
	platformEligible, found, err := resolveEligibility(ctx, tx, nil, asset, scope)
	if err != nil {
		return false, ReasonInternalError, err
	}
	if !found || !platformEligible {
		return false, ReasonOperationNotEligible, nil
	}
	tenantOverride, found, err := resolveEligibility(ctx, tx, &tenant, asset, scope)
	if err != nil {
		return false, ReasonInternalError, err
	}
	if found && !tenantOverride {
		return false, ReasonOperationNotEligibleForTenant, nil
	}

	return true, ReasonEligible, nil
}

// assertTenantScope compares tenant against the transaction's own
// app.tenant_id. A player-scoped transaction (WithPlayerScope) sets the
// same GUC, so a player-initiated operation is covered identically.
func assertTenantScope(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
	var scoped *uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT NULLIF(current_setting('app.tenant_id', true), '')::uuid`).Scan(&scoped); err != nil {
		return fmt.Errorf("assetregistry: read tenant scope: %w", err)
	}
	if scoped == nil {
		return fmt.Errorf("%w: transaction has no tenant scope (use db.Pool.WithTenant)", ErrTenantContextMismatch)
	}
	if *scoped != tenant {
		return fmt.Errorf("%w: argument %s, transaction scope %s", ErrTenantContextMismatch, tenant, *scoped)
	}
	return nil
}

type scopeQuery struct {
	tenantID       uuid.UUID
	scopeKind      ScopeKind
	brandID        *uuid.UUID
	jurisdictionID *uuid.UUID
	asset          string
	product        string
}

// resolveScopeFact reads one layer-4/5/6 fact with most-specific-product
// precedence: a row naming this exact product wins over a row applying to
// every product (product IS NULL). This is what makes "authorized for
// casino" not silently authorize sportsbook, while still allowing an
// administrator to grant every product deliberately with one row.
func resolveScopeFact(ctx context.Context, tx pgx.Tx, q scopeQuery) (eligible bool, found bool, err error) {
	const sql = `
		SELECT eligible
		  FROM asset_authorizations
		 WHERE tenant_id = $1
		   AND scope_kind = $2
		   AND asset_code = $3
		   AND (brand_id IS NOT DISTINCT FROM $4::uuid)
		   AND (jurisdiction_id IS NOT DISTINCT FROM $5::uuid)
		   AND (product = $6 OR product IS NULL)
		 ORDER BY (product IS NOT NULL) DESC
		 LIMIT 1`
	err = tx.QueryRow(ctx, sql, q.tenantID, q.scopeKind, q.asset, q.brandID, q.jurisdictionID, q.product).Scan(&eligible)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("assetregistry: read %s authorization: %w", q.scopeKind, err)
	}
	return eligible, true, nil
}

// resolveEligibility reads one layer-7 fact at either the platform scope
// (tenantID nil) or a tenant scope, with the same most-specific-product
// precedence.
func resolveEligibility(ctx context.Context, tx pgx.Tx, tenantID *uuid.UUID, asset string, scope OperationScope) (eligible bool, found bool, err error) {
	const sql = `
		SELECT eligible
		  FROM asset_operation_eligibility
		 WHERE tenant_id IS NOT DISTINCT FROM $1::uuid
		   AND asset_code = $2
		   AND operation = $3
		   AND (product = $4 OR product IS NULL)
		 ORDER BY (product IS NOT NULL) DESC
		 LIMIT 1`
	err = tx.QueryRow(ctx, sql, tenantID, asset, scope.Operation, scope.Product).Scan(&eligible)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("assetregistry: read operation eligibility: %w", err)
	}
	return eligible, true, nil
}

// GetAsset reads one registry row. This is a REGISTRY read (e.g. to look
// up decimal_exponent), never an authorization decision - an eligibility
// question must always go through CheckEligibility (ADR 0037 §C.2).
func GetAsset(ctx context.Context, tx pgx.Tx, code string) (Asset, error) {
	var a Asset
	var network *string
	err := tx.QueryRow(ctx,
		`SELECT code, asset_type, decimal_exponent, display_name, network, active, platform_authorized, created_at, updated_at
		   FROM assets WHERE code = $1`, code).
		Scan(&a.Code, &a.AssetType, &a.DecimalExponent, &a.DisplayName, &network, &a.Active, &a.PlatformAuthorized, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	if err != nil {
		return Asset{}, fmt.Errorf("assetregistry: read asset: %w", err)
	}
	if network != nil {
		a.Network = *network
	}
	return a, nil
}

// ListAssets returns the whole registry, ordered by code.
func ListAssets(ctx context.Context, tx pgx.Tx) ([]Asset, error) {
	rows, err := tx.Query(ctx,
		`SELECT code, asset_type, decimal_exponent, display_name, network, active, platform_authorized, created_at, updated_at
		   FROM assets ORDER BY code`)
	if err != nil {
		return nil, fmt.Errorf("assetregistry: list assets: %w", err)
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		var a Asset
		var network *string
		if err := rows.Scan(&a.Code, &a.AssetType, &a.DecimalExponent, &a.DisplayName, &network, &a.Active, &a.PlatformAuthorized, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("assetregistry: scan asset: %w", err)
		}
		if network != nil {
			a.Network = *network
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
