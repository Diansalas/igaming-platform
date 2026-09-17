package assetregistry

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// This file implements ADR 0037 §C.5.1 operations 6-9 (layers 4-7).
//
// Per §C.1's two-tier split these are TENANT-scoped mutations, gated by a
// tenant-scoped permission, and they may only ever NARROW what platform
// layers 1-3 already authorized. "Only ever narrow" is enforced three
// times over, deliberately: by RLS (a tenant transaction can only write
// its own rows), by migration 0045's narrowing triggers at write time,
// and by CheckEligibility's AND-chain at resolution time. None of the
// three is redundant - the write-time trigger stops a widened row being
// STORED (which is what `security`'s Stage 4H-B0-R5 test list requires),
// and the resolution-time chain stops a row that was legal when written
// from taking effect after the layer above it was later revoked.
//
// The ONLY exception to tenant scope is a platform-wide layer-7 default
// row (tenant_id NULL, ADR 0037 §A.6): that one is platform-admin-only
// and requires db.Pool.WithPlatformAdmin, enforced by migration 0045's
// own policy predicate - and, when GRANTING, an independently approved
// four-eyes request as well (migration 0047; see
// ConfigureOperationEligibility's doc comment).

// AuthorizeScopeParams is the input for operations 6-8 (layers 4/5/6).
//
// TenantID must be resolved server-side from authenticated context. It is
// additionally cross-checked against the transaction's app.tenant_id, so
// a tenant identifier that arrived in a request body cannot be written
// even if a handler passed one through.
//
// BrandID/JurisdictionID name WHICH brand/jurisdiction is being
// configured. These are configuration targets, not caller identity: a
// jurisdiction is not derivable from a staff credential, exactly as
// risk_rules' admin API accepts a jurisdiction_code. What is never
// accepted from a payload is the TENANT - and the composite
// (brand_id, tenant_id) foreign key plus RLS mean a named brand or
// jurisdiction still cannot belong to another tenant.
type AuthorizeScopeParams struct {
	TenantID       uuid.UUID
	ScopeKind      ScopeKind
	BrandID        uuid.UUID
	JurisdictionID uuid.UUID
	AssetCode      string
	// Product empty means the row applies to every product. A
	// product-specific row always wins over it at resolution time, so an
	// every-product grant can be narrowed per product without being
	// rewritten.
	Product  string
	Eligible bool
	Actor    ActorContext
}

// AuthorizeScope writes one layer-4/5/6 authorization fact
// (ADR 0037 §C.5.1 operations 6, 7 and 8 - which differ only in
// scope_kind, so they share one implementation rather than three
// near-identical ones).
func AuthorizeScope(ctx context.Context, tx pgx.Tx, p AuthorizeScopeParams) (Authorization, error) {
	if err := p.Actor.validate(); err != nil {
		return Authorization{}, err
	}
	if p.TenantID == uuid.Nil {
		return Authorization{}, fmt.Errorf("%w: tenant must be resolved from authenticated context", ErrInvalidInput)
	}
	if strings.TrimSpace(p.AssetCode) == "" {
		return Authorization{}, fmt.Errorf("%w: asset_code is required", ErrInvalidInput)
	}
	if err := assertTenantScope(ctx, tx, p.TenantID); err != nil {
		return Authorization{}, err
	}

	var brandID, jurisdictionID *uuid.UUID
	switch p.ScopeKind {
	case ScopeTenant:
		if p.BrandID != uuid.Nil || p.JurisdictionID != uuid.Nil {
			return Authorization{}, fmt.Errorf("%w: a tenant-scope authorization carries no brand or jurisdiction", ErrInvalidInput)
		}
	case ScopeBrand:
		if p.BrandID == uuid.Nil {
			return Authorization{}, fmt.Errorf("%w: brand_id is required for a brand-scope authorization", ErrInvalidInput)
		}
		if p.JurisdictionID != uuid.Nil {
			return Authorization{}, fmt.Errorf("%w: a brand-scope authorization carries no jurisdiction", ErrInvalidInput)
		}
		b := p.BrandID
		brandID = &b
	case ScopeJurisdiction:
		if p.JurisdictionID == uuid.Nil {
			return Authorization{}, fmt.Errorf("%w: jurisdiction_id is required for a jurisdiction-scope authorization", ErrInvalidInput)
		}
		if p.BrandID != uuid.Nil {
			return Authorization{}, fmt.Errorf("%w: a jurisdiction-scope authorization carries no brand", ErrInvalidInput)
		}
		j := p.JurisdictionID
		jurisdictionID = &j
	default:
		return Authorization{}, fmt.Errorf("%w: scope_kind must be tenant, brand or jurisdiction", ErrInvalidInput)
	}

	var product *string
	if strings.TrimSpace(p.Product) != "" {
		pr := p.Product
		product = &pr
	}

	before, beforeFound, err := readAuthorization(ctx, tx, p.TenantID, p.ScopeKind, brandID, jurisdictionID, p.AssetCode, product)
	if err != nil {
		return Authorization{}, err
	}

	var conflictTarget string
	switch p.ScopeKind {
	case ScopeTenant:
		conflictTarget = `(tenant_id, asset_code, COALESCE(product, '*')) WHERE scope_kind = 'tenant'`
	case ScopeBrand:
		conflictTarget = `(tenant_id, brand_id, asset_code, COALESCE(product, '*')) WHERE scope_kind = 'brand'`
	case ScopeJurisdiction:
		conflictTarget = `(tenant_id, jurisdiction_id, asset_code, COALESCE(product, '*')) WHERE scope_kind = 'jurisdiction'`
	}

	row := Authorization{TenantID: p.TenantID, ScopeKind: p.ScopeKind, BrandID: brandID,
		JurisdictionID: jurisdictionID, AssetCode: p.AssetCode, Product: p.Product,
		Eligible: p.Eligible, ReasonCode: p.Actor.ReasonCode}

	// Upsert rather than check-then-write: the partial unique indexes are
	// what actually guarantee one fact per (layer, target, asset,
	// product) under concurrency.
	err = tx.QueryRow(ctx, `
		INSERT INTO asset_authorizations
			(tenant_id, scope_kind, brand_id, jurisdiction_id, asset_code, product, eligible, reason_code,
			 created_by_actor_type, created_by_actor_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'staff', $9)
		ON CONFLICT `+conflictTarget+`
		DO UPDATE SET eligible = EXCLUDED.eligible, reason_code = EXCLUDED.reason_code
		RETURNING id, updated_at`,
		p.TenantID, p.ScopeKind, brandID, jurisdictionID, p.AssetCode, product, p.Eligible,
		p.Actor.ReasonCode, p.Actor.ActorID).Scan(&row.ID, &row.UpdatedAt)
	if err != nil {
		return Authorization{}, classifyTriggerError(fmt.Errorf("assetregistry: upsert %s authorization: %w", p.ScopeKind, err))
	}

	beforeState := any(nil)
	if beforeFound {
		beforeState = authorizationState(before)
	}
	if err := recordTenantAudit(ctx, tx, p.TenantID, p.Actor,
		"asset_authorization."+string(p.ScopeKind)+"_configured", "asset_authorization", row.ID.String(),
		map[string]any{"before": beforeState, "after": authorizationState(row)}); err != nil {
		return Authorization{}, err
	}
	return row, nil
}

func readAuthorization(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, kind ScopeKind,
	brandID, jurisdictionID *uuid.UUID, asset string, product *string) (Authorization, bool, error) {
	var a Authorization
	var prod *string
	err := tx.QueryRow(ctx, `
		SELECT id, tenant_id, scope_kind, brand_id, jurisdiction_id, asset_code, product, eligible,
		       COALESCE(reason_code, ''), updated_at
		  FROM asset_authorizations
		 WHERE tenant_id = $1 AND scope_kind = $2
		   AND brand_id IS NOT DISTINCT FROM $3::uuid
		   AND jurisdiction_id IS NOT DISTINCT FROM $4::uuid
		   AND asset_code = $5
		   AND product IS NOT DISTINCT FROM $6::text`,
		tenantID, kind, brandID, jurisdictionID, asset, product).
		Scan(&a.ID, &a.TenantID, &a.ScopeKind, &a.BrandID, &a.JurisdictionID, &a.AssetCode, &prod, &a.Eligible, &a.ReasonCode, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Authorization{}, false, nil
	}
	if err != nil {
		return Authorization{}, false, fmt.Errorf("assetregistry: read authorization: %w", err)
	}
	if prod != nil {
		a.Product = *prod
	}
	return a, true, nil
}

func authorizationState(a Authorization) map[string]any {
	state := map[string]any{
		"scope_kind": string(a.ScopeKind), "asset_code": a.AssetCode,
		"product": a.Product, "eligible": a.Eligible, "reason_code": a.ReasonCode,
	}
	if a.BrandID != nil {
		state["brand_id"] = a.BrandID.String()
	}
	if a.JurisdictionID != nil {
		state["jurisdiction_id"] = a.JurisdictionID.String()
	}
	return state
}

// ConfigureEligibilityParams is the input for operation 9 (layer 7).
// TenantID zero means the PLATFORM-WIDE default row - which requires a
// db.Pool.WithPlatformAdmin transaction (migration 0045's policy), not a
// tenant one, AND - when Eligible is true - an independently approved
// ChangePlatformOperationEligibility request (migration 0047).
type ConfigureEligibilityParams struct {
	TenantID  uuid.UUID
	AssetCode string
	Product   string
	Operation Operation
	Eligible  bool
	Actor     ActorContext
}

// ConfigureOperationEligibility writes one layer-7 fact. A tenant row may
// only narrow the platform-wide default; migration 0045's trigger refuses
// a widening row at write time, and CheckEligibility refuses to honour
// one at resolution time.
//
// FOUR-EYES on the platform-wide GRANT (TenantID zero, Eligible true).
// That one write flips an eligibility gate for every tenant on the
// platform at once, and ADR 0037 §C.5.1 op 9 / §C.5.3 always required
// dual control for it - but migration 0044's operation CHECK had no
// request type for it, so the requirement had no representation anywhere
// and a single platform-admin credential was enough (found independently
// by code-reviewer and security). It now routes through the SAME
// request/approve flow create/activate/platform_authorize use:
//
//  1. FileChangeRequest(ChangePlatformOperationEligibility, asset,
//     EligibilityOperation, EligibilityProduct)
//  2. DecideChangeRequest by a DIFFERENT platform person
//  3. this function, which the trigger lets through exactly once, for
//     exactly that (asset, operation, product)
//
// As with layers 2-3, the control lives in the trigger rather than here
// on purpose: migration 0047's asset_operation_eligibility_enforce_
// narrowing() consumes the approval in the same statement as the
// mutation, so no present or future call site can forget it and no
// approval can be replayed. This function's job is to surface the
// refusal as ErrDualControlRequired (409) instead of a generic 500.
//
// Deliberately NOT dual-controlled: a platform-wide REVOCATION
// (Eligible false), an idempotent re-write of an already-eligible row,
// and every tenant-scoped row. Revocation is the fail-closed direction
// and must never wait for a second approver - the same asymmetry ADR 0037
// §C.5.3 draws for suspend/revoke at layers 2-3.
func ConfigureOperationEligibility(ctx context.Context, tx pgx.Tx, p ConfigureEligibilityParams) (OperationEligibility, error) {
	if err := p.Actor.validate(); err != nil {
		return OperationEligibility{}, err
	}
	if strings.TrimSpace(p.AssetCode) == "" {
		return OperationEligibility{}, fmt.Errorf("%w: asset_code is required", ErrInvalidInput)
	}
	if !validOperation(p.Operation) {
		return OperationEligibility{}, fmt.Errorf("%w: unknown operation %q", ErrInvalidInput, p.Operation)
	}

	var tenantID *uuid.UUID
	conflictTarget := `(asset_code, operation, COALESCE(product, '*')) WHERE tenant_id IS NULL`
	if p.TenantID != uuid.Nil {
		if err := assertTenantScope(ctx, tx, p.TenantID); err != nil {
			return OperationEligibility{}, err
		}
		t := p.TenantID
		tenantID = &t
		conflictTarget = `(tenant_id, asset_code, operation, COALESCE(product, '*')) WHERE tenant_id IS NOT NULL`
	}

	var product *string
	if strings.TrimSpace(p.Product) != "" {
		pr := p.Product
		product = &pr
	}

	before, beforeFound, err := readEligibility(ctx, tx, tenantID, p.AssetCode, product, p.Operation)
	if err != nil {
		return OperationEligibility{}, err
	}

	row := OperationEligibility{TenantID: tenantID, AssetCode: p.AssetCode, Product: p.Product,
		Operation: p.Operation, Eligible: p.Eligible, ReasonCode: p.Actor.ReasonCode}
	err = tx.QueryRow(ctx, `
		INSERT INTO asset_operation_eligibility
			(tenant_id, asset_code, product, operation, eligible, reason_code, created_by_actor_type, created_by_actor_id)
		VALUES ($1, $2, $3, $4, $5, $6, 'staff', $7)
		ON CONFLICT `+conflictTarget+`
		DO UPDATE SET eligible = EXCLUDED.eligible, reason_code = EXCLUDED.reason_code
		RETURNING id, updated_at`,
		tenantID, p.AssetCode, product, p.Operation, p.Eligible, p.Actor.ReasonCode, p.Actor.ActorID).
		Scan(&row.ID, &row.UpdatedAt)
	if err != nil {
		return OperationEligibility{}, classifyTriggerError(fmt.Errorf("assetregistry: upsert operation eligibility: %w", err))
	}

	beforeState := any(nil)
	if beforeFound {
		beforeState = eligibilityState(before)
	}
	auditTenant := uuid.Nil
	if tenantID != nil {
		auditTenant = *tenantID
	}
	if err := recordTenantAudit(ctx, tx, auditTenant, p.Actor,
		"asset_operation_eligibility.configured", "asset_operation_eligibility", row.ID.String(),
		map[string]any{"before": beforeState, "after": eligibilityState(row),
			"platform_wide_default": tenantID == nil,
			// Recorded so the audit trail states whether this specific
			// write went through four-eyes, rather than leaving a reader
			// to infer it from the scope and the direction.
			"dual_controlled": tenantID == nil && p.Eligible}); err != nil {
		return OperationEligibility{}, err
	}
	return row, nil
}

func readEligibility(ctx context.Context, tx pgx.Tx, tenantID *uuid.UUID, asset string, product *string, op Operation) (OperationEligibility, bool, error) {
	var e OperationEligibility
	var prod *string
	err := tx.QueryRow(ctx, `
		SELECT id, tenant_id, asset_code, product, operation, eligible, COALESCE(reason_code, ''), updated_at
		  FROM asset_operation_eligibility
		 WHERE tenant_id IS NOT DISTINCT FROM $1::uuid
		   AND asset_code = $2
		   AND product IS NOT DISTINCT FROM $3::text
		   AND operation = $4`,
		tenantID, asset, product, op).
		Scan(&e.ID, &e.TenantID, &e.AssetCode, &prod, &e.Operation, &e.Eligible, &e.ReasonCode, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OperationEligibility{}, false, nil
	}
	if err != nil {
		return OperationEligibility{}, false, fmt.Errorf("assetregistry: read operation eligibility: %w", err)
	}
	if prod != nil {
		e.Product = *prod
	}
	return e, true, nil
}

func eligibilityState(e OperationEligibility) map[string]any {
	return map[string]any{
		"asset_code": e.AssetCode, "product": e.Product, "operation": string(e.Operation),
		"eligible": e.Eligible, "reason_code": e.ReasonCode,
	}
}

// ListAuthorizations returns every layer-4/5/6 fact visible in the
// current tenant scope, for the admin read path.
func ListAuthorizations(ctx context.Context, tx pgx.Tx, assetCode string) ([]Authorization, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, tenant_id, scope_kind, brand_id, jurisdiction_id, asset_code, product, eligible,
		       COALESCE(reason_code, ''), updated_at
		  FROM asset_authorizations
		 WHERE ($1 = '' OR asset_code = $1)
		 ORDER BY asset_code, scope_kind, COALESCE(product, '*')`, assetCode)
	if err != nil {
		return nil, fmt.Errorf("assetregistry: list authorizations: %w", err)
	}
	defer rows.Close()
	var out []Authorization
	for rows.Next() {
		var a Authorization
		var prod *string
		if err := rows.Scan(&a.ID, &a.TenantID, &a.ScopeKind, &a.BrandID, &a.JurisdictionID,
			&a.AssetCode, &prod, &a.Eligible, &a.ReasonCode, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("assetregistry: scan authorization: %w", err)
		}
		if prod != nil {
			a.Product = *prod
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// recordTenantAudit writes the mandatory audit record for a tenant-scoped
// mutation (or a platform-scoped one when tenantID is uuid.Nil).
func recordTenantAudit(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, actor ActorContext,
	action, targetType, targetID string, metadata map[string]any) error {
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["reason_code"] = actor.ReasonCode
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: actor.ActorID,
		Action: action, TargetType: targetType, TargetID: targetID,
		Outcome: audit.OutcomeSuccess, IPAddress: actor.IPAddress, UserAgent: actor.UserAgent,
		RequestID: actor.RequestID, Metadata: metadata,
	}); err != nil {
		return fmt.Errorf("assetregistry: audit %s: %w", action, err)
	}
	return nil
}
