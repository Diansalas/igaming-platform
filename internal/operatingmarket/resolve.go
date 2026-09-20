package operatingmarket

// This file implements the resolution algorithm (ADR 0045 §3.5) - THE
// binding specification for "is this tenant/brand permitted to operate
// this operation, in this country, at this AsOf". ONE internal function,
// resolve(), implements the entire algorithm and returns both the Result
// fields and the full diagnostic chain; ResolveOperatingCountryPolicy
// discards the chain, ExplainOperatingCountryPolicy (explain.go) returns
// it. This is a binding implementation constraint, not a convenience: the
// decision logic itself must never be duplicated (PHASE-D-CR-P3-2's own
// lesson, escalated here from an accepted risk to a hard requirement).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// assertTenantScope compares the tenant this resolution is being produced
// FOR against the transaction's own app.tenant_id GUC, AND requires
// app.player_account_id to be unset - a byte-for-byte behavioural mirror
// of internal/jurisdiction's own assertTenantScope, extended with the
// leading player-scope check every staff/config table in this codebase
// carries (migration 0045's asset_authorizations shape): without it, a
// player-scoped connection reading zero rows under
// operating_country_policies' RLS (which has no player-read policy at
// all) would produce a misleading not_configured rather than a
// diagnosable ErrTransactionScope.
//
// LOAD-BEARING, NOT DEFENCE-IN-DEPTH: `tenants` and `licences` carry NO
// row-level security at all (resolver.go's own recorded finding, true
// here for the identical reason), so RLS provides ZERO tenant isolation
// on the licence-lookup leg of this resolver. Without this assertion, a
// transaction scoped to tenant A could resolve a fully `permitted` answer
// for a caller-supplied tenant B.
func assertTenantScope(ctx context.Context, q ReadOnlyQuerier, tenantID uuid.UUID) (bool, error) {
	var scopedTenant *uuid.UUID
	var scopedPlayer *uuid.UUID
	if err := q.QueryRow(ctx,
		`SELECT NULLIF(current_setting('app.tenant_id', true), '')::uuid,
		        NULLIF(current_setting('app.player_account_id', true), '')::uuid`,
	).Scan(&scopedTenant, &scopedPlayer); err != nil {
		return false, fmt.Errorf("operatingmarket: read tenant scope: %w", err)
	}
	if scopedTenant == nil {
		return false, fmt.Errorf("%w: transaction has no tenant scope (use db.Pool.WithTenant)", ErrTransactionScope)
	}
	if scopedPlayer != nil {
		return false, fmt.Errorf("%w: transaction must not be player-scoped", ErrTransactionScope)
	}
	return *scopedTenant == tenantID, nil
}

// assertPlatformScope is the platform-scoped analogue, mirroring
// jurisdiction.assertPlatformScope exactly. Real control:
// licence_country_ceilings' RLS policies enforce the identical predicate
// independently at the database, so a caller that bypassed this check
// would still fail at the INSERT/UPDATE, but with a much less diagnosable
// error.
func assertPlatformScope(ctx context.Context, tx pgx.Tx) error {
	var platformAdmin, scopedTenant, scopedPlayer *uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid,
		        NULLIF(current_setting('app.tenant_id', true), '')::uuid,
		        NULLIF(current_setting('app.player_account_id', true), '')::uuid`,
	).Scan(&platformAdmin, &scopedTenant, &scopedPlayer); err != nil {
		return fmt.Errorf("operatingmarket: read platform admin scope: %w", err)
	}
	if platformAdmin == nil || scopedTenant != nil || scopedPlayer != nil {
		return ErrTransactionScope
	}
	return nil
}

// resolution is resolve()'s complete internal result: every field needed
// to build BOTH a Result (resolve.go's own caller,
// ResolveOperatingCountryPolicy) and an Explanation (explain.go). This is
// the ONE piece of state that makes "one algorithm, two views" real: no
// field here is recomputed by explain.go.
type resolution struct {
	outcome       Outcome
	asOf          time.Time
	policyVersion string

	tenantID      uuid.UUID
	brandID       *uuid.UUID
	countryCode   string
	operationCode string
	productCode   *string

	licenceID       uuid.UUID
	licenceValidity jurisdiction.LicenceValidity
	ceilingReason   LicenceCeilingReason

	sourceScope         Scope
	sourceVersionID     uuid.UUID
	sourceEffectiveFrom time.Time

	blockingScope         Scope
	blockingVersionID     uuid.UUID
	blockingEffectiveFrom time.Time
	blockingReasonCode    string

	chain []ChainStep
}

func (r resolution) toResult() Result {
	return Result{
		outcome:               r.outcome,
		asOf:                  r.asOf,
		policyVersion:         r.policyVersion,
		tenantID:              r.tenantID,
		brandID:               r.brandID,
		countryCode:           r.countryCode,
		operationCode:         r.operationCode,
		productCode:           r.productCode,
		licenceID:             r.licenceID,
		licenceCeilingReason:  r.ceilingReason,
		sourceScope:           r.sourceScope,
		sourceVersionID:       r.sourceVersionID,
		sourceEffectiveFrom:   r.sourceEffectiveFrom,
		blockingScope:         r.blockingScope,
		blockingVersionID:     r.blockingVersionID,
		blockingEffectiveFrom: r.blockingEffectiveFrom,
	}
}

// openRow is one row read from an effective-dated policy/ceiling table,
// windowed to a specific AsOf.
type openRow struct {
	id            uuid.UUID
	state         string
	status        string
	effectiveFrom time.Time
	effectiveTo   *time.Time
	brandID       *uuid.UUID
	productCode   *string
	reasonCode    string
	authRef       string
}

// ResolveOperatingCountryPolicy recomputes the answer from the rows in
// force at p.AsOf. It NEVER persists a resolution, NEVER caches one, and
// NEVER calls time.Now(): AsOf is a parameter and no SQL on this path
// calls now(). Two calls with the same AsOf against the same row set
// return the same Outcome, always (INV-M-3).
func ResolveOperatingCountryPolicy(ctx context.Context, q ReadOnlyQuerier, p Query) (Result, error) {
	res, err := resolve(ctx, q, p)
	if err != nil {
		return Result{}, err
	}
	return res.toResult(), nil
}

// resolve implements the complete algorithm (ADR 0045 §3.5). It is the
// SOLE implementation of the resolution decision - ResolveOperatingCountryPolicy
// and ExplainOperatingCountryPolicy are both thin wrappers over this
// function; neither re-implements any part of the decision logic.
func resolve(ctx context.Context, q ReadOnlyQuerier, p Query) (resolution, error) {
	// --- Preconditions: caller bugs are errors, not Outcomes. ---
	if p.TenantID == uuid.Nil {
		return resolution{}, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	if p.AsOf.IsZero() {
		return resolution{}, fmt.Errorf("%w: AsOf is required and must not be the zero time", ErrInvalidInput)
	}
	if !validation.IsISO3166Alpha2(p.CountryCode) {
		return resolution{}, fmt.Errorf("%w: country_code must be a valid, uppercase ISO-3166-1 alpha-2 code", ErrInvalidInput)
	}
	if p.OperationCode == "" {
		return resolution{}, fmt.Errorf("%w: operation_code is required", ErrInvalidInput)
	}

	inScope, err := assertTenantScope(ctx, q, p.TenantID)
	if err != nil {
		return resolution{}, err
	}
	if !inScope {
		return resolution{}, fmt.Errorf("%w: transaction tenant scope does not match tenant %s", ErrTransactionScope, p.TenantID)
	}

	res := resolution{
		asOf: p.AsOf, policyVersion: PolicyVersion,
		tenantID: p.TenantID, brandID: p.BrandID, countryCode: p.CountryCode,
		operationCode: p.OperationCode, productCode: p.ProductCode,
	}

	// A decommissioned/unknown operation or product must fail closed, not
	// crash a caller - Outcome, not an error.
	var operationActive bool
	err = q.QueryRow(ctx, `SELECT active FROM platform_operations WHERE code = $1`, p.OperationCode).Scan(&operationActive)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !operationActive) {
		res.outcome = OutcomeInvalidConfiguration
		return res, nil
	}
	if err != nil {
		return resolution{}, fmt.Errorf("operatingmarket: read platform_operations: %w", err)
	}

	if p.ProductCode != nil {
		var productActive bool
		err = q.QueryRow(ctx, `SELECT active FROM platform_products WHERE code = $1`, *p.ProductCode).Scan(&productActive)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !productActive) {
			res.outcome = OutcomeInvalidConfiguration
			return res, nil
		}
		if err != nil {
			return resolution{}, fmt.Errorf("operatingmarket: read platform_products: %w", err)
		}
	}

	// --- STEP 1: the ceiling. Unconditional, evaluated first, always. ---
	var licenceID *uuid.UUID
	err = q.QueryRow(ctx, `SELECT licence_id FROM tenants WHERE id = $1`, p.TenantID).Scan(&licenceID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && licenceID == nil) {
		res.outcome = OutcomeLicensingUnknown
		return res, nil
	}
	if err != nil {
		return resolution{}, fmt.Errorf("operatingmarket: read tenant licence: %w", err)
	}
	res.licenceID = *licenceID

	validity, err := jurisdiction.EvaluateLicenceValidity(ctx, q, *licenceID, p.AsOf)
	if err != nil {
		return resolution{}, fmt.Errorf("operatingmarket: evaluate licence validity: %w", err)
	}
	res.licenceValidity = validity
	switch validity {
	case jurisdiction.LicenceNotFound, jurisdiction.LicenceNotBound:
		res.outcome = OutcomeLicensingUnknown
		return res, nil
	case jurisdiction.LicenceSuspended:
		res.outcome = OutcomeNotPermittedByLicence
		res.ceilingReason = CeilingReasonLicenceSuspended
		res.chain = append(res.chain, licenceChainStep(false, nil, res.ceilingReason))
		return res, nil
	case jurisdiction.LicenceStatusExpired, jurisdiction.LicenceDateExpired:
		res.outcome = OutcomeNotPermittedByLicence
		res.ceilingReason = CeilingReasonLicenceExpired
		res.chain = append(res.chain, licenceChainStep(false, nil, res.ceilingReason))
		return res, nil
	case jurisdiction.LicenceNotYetIssued:
		res.outcome = OutcomeNotPermittedByLicence
		res.ceilingReason = CeilingReasonLicenceNotYetIssued
		res.chain = append(res.chain, licenceChainStep(false, nil, res.ceilingReason))
		return res, nil
	case jurisdiction.LicenceValid:
		// fall through
	default:
		return resolution{}, fmt.Errorf("%w: unrecognized licence validity %q", ErrInvalidInput, validity)
	}

	ceilingRows, err := queryWindowedRows(ctx, q, `
		SELECT id, state, status, effective_from, effective_to, reason_code, COALESCE(authorization_reference, '')
		  FROM licence_country_ceilings
		 WHERE licence_id = $1 AND country_code = $2
		   AND effective_from <= $3 AND (effective_to IS NULL OR effective_to > $3)
		 ORDER BY effective_from DESC LIMIT 2`,
		*licenceID, p.CountryCode, p.AsOf)
	if err != nil {
		return resolution{}, fmt.Errorf("operatingmarket: read licence ceiling: %w", err)
	}
	switch len(ceilingRows) {
	case 2:
		res.outcome = OutcomeConfigurationConflict
		return res, nil
	case 0:
		res.outcome = OutcomeNotPermittedByLicence
		res.ceilingReason = CeilingReasonNoCeilingRow
		res.chain = append(res.chain, licenceChainStep(false, nil, res.ceilingReason))
		return res, nil
	}
	ceilingRow := ceilingRows[0]
	ceilingStatus, err := parseStatus(ceilingRow.status)
	if err != nil {
		res.outcome = OutcomeInvalidConfiguration
		return res, nil
	}
	if ceilingStatus == StatusWithdrawn {
		res.outcome = OutcomeNotPermittedByLicence
		res.ceilingReason = CeilingReasonCeilingWithdrawn
		res.chain = append(res.chain, licenceChainStep(true, &ceilingRow, res.ceilingReason))
		return res, nil
	}
	ceilingState, err := parseState(ceilingRow.state)
	if err != nil {
		res.outcome = OutcomeInvalidConfiguration
		return res, nil
	}
	if ceilingState == StateDisabled {
		res.outcome = OutcomeNotPermittedByLicence
		res.ceilingReason = CeilingReasonCountryDisabled
		res.chain = append(res.chain, licenceChainStep(true, &ceilingRow, res.ceilingReason))
		return res, nil
	}
	// Ceiling permits. Nothing below may exceed it, and no lower row can
	// ever short-circuit this step.
	res.chain = append(res.chain, licenceChainStep(true, &ceilingRow, CeilingReasonNone))

	// --- STEP 2: tenant rung. Absence here is TERMINAL (ruling 5). ---
	tenantRows, err := queryWindowedRows(ctx, q, `
		SELECT id, state, status, effective_from, effective_to, reason_code, COALESCE(authorization_reference, '')
		  FROM operating_country_policies
		 WHERE tenant_id = $1 AND scope_kind = 'tenant' AND country_code = $2
		   AND effective_from <= $3 AND (effective_to IS NULL OR effective_to > $3)
		 ORDER BY effective_from DESC LIMIT 2`,
		p.TenantID, p.CountryCode, p.AsOf)
	if err != nil {
		return resolution{}, fmt.Errorf("operatingmarket: read tenant policy: %w", err)
	}
	if len(tenantRows) == 2 {
		res.outcome = OutcomeConfigurationConflict
		return res, nil
	}
	if len(tenantRows) == 0 {
		outcome, err := classifyAbsence(ctx, q, `
			SELECT effective_from, effective_to FROM operating_country_policies
			 WHERE tenant_id = $1 AND scope_kind = 'tenant' AND country_code = $2
			 ORDER BY effective_from DESC`,
			[]any{p.TenantID, p.CountryCode}, p.AsOf)
		if err != nil {
			return resolution{}, err
		}
		res.outcome = outcome
		res.chain = append(res.chain, ChainStep{Scope: ScopeTenant, Present: false, Inherited: false})
		return res, nil
	}
	tenantRow := tenantRows[0]
	tenantStatus, err := parseStatus(tenantRow.status)
	if err != nil {
		res.outcome = OutcomeInvalidConfiguration
		return res, nil
	}
	if tenantStatus == StatusWithdrawn {
		res.outcome = OutcomeNotConfigured
		res.chain = append(res.chain, chainStepFromRow(ScopeTenant, tenantRow, false))
		return res, nil
	}
	tenantState, err := parseState(tenantRow.state)
	if err != nil {
		res.outcome = OutcomeInvalidConfiguration
		return res, nil
	}
	res.chain = append(res.chain, chainStepFromRow(ScopeTenant, tenantRow, false))
	if tenantState == StateDisabled {
		res.outcome = OutcomeDisabledByTenant
		res.blockingScope = ScopeTenant
		res.blockingVersionID = tenantRow.id
		res.blockingEffectiveFrom = tenantRow.effectiveFrom
		res.blockingReasonCode = tenantRow.reasonCode
		return res, nil
	}
	res.sourceScope = ScopeTenant
	res.sourceVersionID = tenantRow.id
	res.sourceEffectiveFrom = tenantRow.effectiveFrom

	// --- STEP 3: brand rung. Absence means INHERIT. Only when BrandID != nil. ---
	if p.BrandID != nil {
		brandRows, err := queryWindowedRows(ctx, q, `
			SELECT id, state, status, effective_from, effective_to, reason_code, COALESCE(authorization_reference, '')
			  FROM operating_country_policies
			 WHERE tenant_id = $1 AND scope_kind = 'brand' AND brand_id = $2 AND country_code = $3
			   AND effective_from <= $4 AND (effective_to IS NULL OR effective_to > $4)
			 ORDER BY effective_from DESC LIMIT 2`,
			p.TenantID, *p.BrandID, p.CountryCode, p.AsOf)
		if err != nil {
			return resolution{}, fmt.Errorf("operatingmarket: read brand policy: %w", err)
		}
		if len(brandRows) == 2 {
			res.outcome = OutcomeConfigurationConflict
			return res, nil
		}
		if len(brandRows) == 1 {
			brandRow := brandRows[0]
			brandStatus, err := parseStatus(brandRow.status)
			if err != nil {
				res.outcome = OutcomeInvalidConfiguration
				return res, nil
			}
			if brandStatus != StatusWithdrawn {
				brandState, err := parseState(brandRow.state)
				if err != nil {
					res.outcome = OutcomeInvalidConfiguration
					return res, nil
				}
				res.chain = append(res.chain, chainStepFromRow(ScopeBrand, brandRow, false))
				if brandState == StateDisabled {
					res.outcome = OutcomeDisabledByBrand
					res.blockingScope = ScopeBrand
					res.blockingVersionID = brandRow.id
					res.blockingEffectiveFrom = brandRow.effectiveFrom
					res.blockingReasonCode = brandRow.reasonCode
					return res, nil
				}
				res.sourceScope = ScopeBrand
				res.sourceVersionID = brandRow.id
				res.sourceEffectiveFrom = brandRow.effectiveFrom
			} else {
				res.chain = append(res.chain, chainStepFromRow(ScopeBrand, brandRow, true))
			}
		} else {
			res.chain = append(res.chain, ChainStep{Scope: ScopeBrand, Present: false, Inherited: true})
		}
	}

	// --- STEP 4: operation rung (ADR 0045 §3.5-A AMENDMENT-1). Absence
	// means INHERIT. The operation rung is a SET of up to four candidates
	// (one per brand-present x product-present combination, bounded by the
	// four partial unique indexes), evaluated with FIRST-DISABLED-WINS -
	// NOT most-specific-wins: a more-specific ENABLED row can never unmask
	// a broader, in-force, active DISABLED row for the same operation, even
	// when the broader row's narrower sibling has since been withdrawn.
	opRows, err := queryOperationCandidates(ctx, q, p)
	if err != nil {
		return resolution{}, fmt.Errorf("operatingmarket: read operation policy: %w", err)
	}

	// 4.1: duplicate-rank check over the FULL candidate set - any two
	// candidates sharing a specificity rank is a version-chain corruption
	// (the partial unique indexes should make this unreachable via any
	// sanctioned write path, but a raw-SQL bypass must still be caught).
	for i := 0; i < len(opRows); i++ {
		for j := i + 1; j < len(opRows); j++ {
			if sameRank(opRows[i], opRows[j]) {
				res.outcome = OutcomeConfigurationConflict
				return res, nil
			}
		}
	}

	// 4.2: parse check - an unparsable state/status on ANY candidate is a
	// data defect, not a caller bug.
	type parsedOpRow struct {
		row    openRow
		state  State
		status Status
	}
	parsedOpRows := make([]parsedOpRow, 0, len(opRows))
	for _, r := range opRows {
		status, err := parseStatus(r.status)
		if err != nil {
			res.outcome = OutcomeInvalidConfiguration
			return res, nil
		}
		state, err := parseState(r.state)
		if err != nil {
			res.outcome = OutcomeInvalidConfiguration
			return res, nil
		}
		parsedOpRows = append(parsedOpRows, parsedOpRow{row: r, state: state, status: status})
	}

	// Chain emission: one ChainStep per applicable candidate,
	// specificity-descending (the queried order) - Inherited:true on
	// withdrawn candidates, false on live ones. Zero candidates emits
	// exactly one absent ChainStep.
	if len(parsedOpRows) == 0 {
		res.chain = append(res.chain, ChainStep{Scope: ScopeOperation, Present: false, Inherited: true})
	} else {
		for _, pr := range parsedOpRows {
			res.chain = append(res.chain, chainStepFromRow(ScopeOperation, pr.row, pr.status == StatusWithdrawn))
		}
	}

	// 4.3: the "live" set - withdrawn candidates are tombstones: never
	// permit, never block, never mask. Order is preserved
	// (specificity-descending).
	var live []parsedOpRow
	for _, pr := range parsedOpRows {
		if pr.status == StatusActive {
			live = append(live, pr)
		}
	}

	// 4.4: if ANY live candidate is disabled, block - the blocking row is
	// the LEAST SPECIFIC such live-disabled row (iterate in reverse of the
	// queried order = ascending specificity, take the first live+disabled
	// match).
	var blocking *parsedOpRow
	for i := len(live) - 1; i >= 0; i-- {
		if live[i].state == StateDisabled {
			b := live[i]
			blocking = &b
			break
		}
	}
	if blocking != nil {
		res.outcome = OutcomeDisabledByOperation
		res.blockingScope = ScopeOperation
		res.blockingVersionID = blocking.row.id
		res.blockingEffectiveFrom = blocking.row.effectiveFrom
		res.blockingReasonCode = blocking.row.reasonCode
		return res, nil
	}

	// 4.5: else if live is non-empty (all enabled, since 4.4 already ruled
	// out any disabled live row), source = the MOST SPECIFIC live row
	// (first in queried order); else (live empty) inherit unchanged from
	// brand/tenant.
	if len(live) > 0 {
		source := live[0]
		res.sourceScope = ScopeOperation
		res.sourceVersionID = source.row.id
		res.sourceEffectiveFrom = source.row.effectiveFrom
	}

	// --- STEP 5: permitted. ---
	res.outcome = OutcomePermitted
	return res, nil
}

// queryWindowedRows runs a windowed effective-dated query and scans up to
// two rows (the callers only ever ask for LIMIT 2), in the shared
// (id, state, status, effective_from, effective_to) column shape.
func queryWindowedRows(ctx context.Context, q ReadOnlyQuerier, sql string, args ...any) ([]openRow, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []openRow
	for rows.Next() {
		var r openRow
		if err := rows.Scan(&r.id, &r.state, &r.status, &r.effectiveFrom, &r.effectiveTo, &r.reasonCode, &r.authRef); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// queryOperationCandidates implements STEP 4's ranked candidate query:
// most-specific-first (brand present DESC, product present DESC,
// effective_from DESC), returning the FULL candidate set - bounded at
// four rows by the four partial unique open-version indexes (one per
// brand-present x product-present combination), never LIMITed. The full
// set is required (ADR 0045 §3.5-A AMENDMENT-1): resolve()'s STEP 4
// evaluates every applicable candidate with first-disabled-wins, not just
// the most-specific one, so a broader in-force disable is never unmasked
// by a narrower row (live or withdrawn).
func queryOperationCandidates(ctx context.Context, q ReadOnlyQuerier, p Query) ([]openRow, error) {
	rows, err := q.Query(ctx, `
		SELECT id, state, status, effective_from, effective_to, brand_id, product_code, reason_code, COALESCE(authorization_reference, '')
		  FROM operating_country_policies
		 WHERE tenant_id = $1 AND scope_kind = 'operation' AND country_code = $2 AND operation_code = $3
		   AND (brand_id = $4 OR brand_id IS NULL)
		   AND (product_code = $5 OR product_code IS NULL)
		   AND effective_from <= $6 AND (effective_to IS NULL OR effective_to > $6)
		 ORDER BY (brand_id IS NOT NULL) DESC, (product_code IS NOT NULL) DESC, effective_from DESC`,
		p.TenantID, p.CountryCode, p.OperationCode, p.BrandID, p.ProductCode, p.AsOf)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []openRow
	for rows.Next() {
		var r openRow
		if err := rows.Scan(&r.id, &r.state, &r.status, &r.effectiveFrom, &r.effectiveTo, &r.brandID, &r.productCode, &r.reasonCode, &r.authRef); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// sameRank reports whether two operation-rung candidate rows tie for the
// same specificity rank (brand-present, product-present) - the signal
// that the underlying version chain has been corrupted (the partial
// unique indexes should make this unreachable via any sanctioned write
// path).
func sameRank(a, b openRow) bool {
	return (a.brandID != nil) == (b.brandID != nil) && (a.productCode != nil) == (b.productCode != nil)
}

// classifyAbsence implements STEP 2's absence classification: looking at
// ALL rows for the key, ignoring the AsOf window, to distinguish
// "genuinely never configured" from "configured only in the future" from
// "configured, then the version chain has a gap at AsOf".
func classifyAbsence(ctx context.Context, q ReadOnlyQuerier, sql string, args []any, asOf time.Time) (Outcome, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var any_ bool
	allFuture := true
	var latestEffectiveTo *time.Time
	first := true
	for rows.Next() {
		var from time.Time
		var to *time.Time
		if err := rows.Scan(&from, &to); err != nil {
			return "", err
		}
		any_ = true
		if !from.After(asOf) {
			allFuture = false
		}
		if first {
			latestEffectiveTo = to
			first = false
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	if !any_ {
		return OutcomeNotConfigured, nil
	}
	if allFuture {
		return OutcomePolicyNotYetEffective, nil
	}
	if latestEffectiveTo != nil && !latestEffectiveTo.After(asOf) {
		return OutcomePolicyExpired, nil
	}
	return OutcomeNotConfigured, nil
}

func chainStepFromRow(scope Scope, r openRow, inherited bool) ChainStep {
	state, _ := parseState(r.state)
	status, _ := parseStatus(r.status)
	id := r.id
	step := ChainStep{
		Scope:                  scope,
		Present:                true,
		State:                  &state,
		Status:                 &status,
		VersionID:              &id,
		EffectiveFrom:          &r.effectiveFrom,
		EffectiveTo:            r.effectiveTo,
		ReasonCode:             r.reasonCode,
		AuthorizationReference: r.authRef,
		Inherited:              inherited,
	}
	// BrandID/ProductCode are only ever populated on openRow for
	// ScopeOperation candidates (queryOperationCandidates is the only
	// query that selects those columns) - tenant/brand rung rows leave
	// r.brandID/r.productCode at their zero value, so this assignment is a
	// no-op for those scopes and satisfies the "ONLY on
	// Scope==ScopeOperation steps" contract without a scope switch.
	step.BrandID = r.brandID
	step.ProductCode = r.productCode
	return step
}

func licenceChainStep(present bool, r *openRow, reason LicenceCeilingReason) ChainStep {
	step := ChainStep{Scope: ScopeLicence, Present: present, ReasonCode: string(reason)}
	if r != nil {
		state, _ := parseState(r.state)
		status, _ := parseStatus(r.status)
		id := r.id
		step.State = &state
		step.Status = &status
		step.VersionID = &id
		step.EffectiveFrom = &r.effectiveFrom
		step.EffectiveTo = r.effectiveTo
		step.AuthorizationReference = r.authRef
	}
	return step
}
