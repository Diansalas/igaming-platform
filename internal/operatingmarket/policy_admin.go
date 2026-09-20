package operatingmarket

// This file implements the tenant/brand/operation policy write path (ADR
// 0045 §3.4, TENANT scope) - the decision that a tenant operates (or does
// not operate) in a country, within its licence ceiling. Every change is
// a NEW row via close-open-then-insert in ONE transaction; there is NO
// `ON CONFLICT DO UPDATE` anywhere in this file.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/validation"
)

// classifyTriggerError turns migration 0076's raised exceptions
// (operating_country_policies_enforce_ceiling) into this package's own
// ErrCeilingExceeded sentinel, mirroring assetregistry's own
// classifyTriggerError precedent. The trigger remains the authoritative
// refusal - this only classifies what it said.
func classifyTriggerError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	msg := pgErr.Message
	if strings.Contains(msg, "ocp_inherit_rung_close_requires_successor") {
		return fmt.Errorf("%w: %s", ErrPolicyCloseRequiresSuccessor, msg)
	}
	if strings.Contains(msg, "licence ceiling") ||
		strings.Contains(msg, "cannot be enabled") ||
		strings.Contains(msg, "no country may be enabled") {
		return fmt.Errorf("%w: %s", ErrCeilingExceeded, msg)
	}
	return err
}

// ClassifyCommitError maps an error returned by tx.Commit() on a
// transaction that used this package into one of this package's sentinels.
// It exists because ocp_inherit_rung_close_requires_successor is a DEFERRED
// constraint trigger: its refusal cannot surface from any call in this
// package, only from the caller's own Commit. Callers that commit a
// transaction in which any operating-country-policy version was written
// MUST route the commit error through this function before reporting it.
func ClassifyCommitError(err error) error {
	if err == nil {
		return nil
	}
	return classifyTriggerError(err)
}

// CreateOperatingCountryPolicyVersionParams is
// CreateOperatingCountryPolicyVersion's input.
type CreateOperatingCountryPolicyVersionParams struct {
	Scope Scope // tenant | brand | operation ONLY
	// TenantID MUST equal the transaction's app.tenant_id.
	TenantID uuid.UUID
	BrandID  *uuid.UUID // required for brand; optional for operation; forbidden for tenant
	// OperationCode is required iff Scope==ScopeOperation; forbidden
	// otherwise.
	OperationCode string
	// ProductCode is legal only when Scope==ScopeOperation; nil == every
	// product.
	ProductCode *string
	CountryCode string
	State       State
	Status      Status
	// AuthorizationReference is REQUIRED for every write that can increase
	// what is permitted:
	//   (a) State==StateEnabled && Status==StatusActive, at any Scope; and
	//   (b) Status==StatusWithdrawn at Scope==ScopeBrand or ScopeOperation,
	//       where absence INHERITS, so a withdrawal removes a block
	//       (ADR 0045 §3.5-A AMENDMENT-2).
	// It is NOT required to write a disable (StatusActive + StateDisabled)
	// at any scope, in any order, nor to withdraw at ScopeTenant (absence
	// there is terminal) - the emergency kill-switch is unchanged
	// (ADR 0037 §C.5.3 / ADR 0045 §7.1 bullet 4).
	AuthorizationReference string
	Actor                  ActorContext
}

// CreateOperatingCountryPolicyVersion appends a new version for the
// scope's key, closing the currently-open version in the SAME
// transaction, and writes a TENANT-scoped audit_log entry in that same
// transaction.
//
// tx MUST be db.Pool.WithTenant(p.TenantID, ...) and MUST NOT be
// player-scoped - asserted in-function via assertTenantScope AND
// enforced independently by RLS. p.TenantID is never taken from a request
// body; a mismatch against the connection's proven scope is
// ErrTransactionScope, not a 404.
//
// Call AT MOST ONCE per transaction per key - see
// CreateLicenceCountryCeilingVersion's identical doc-comment note
// (PHASE-D-CR-P3-5).
func CreateOperatingCountryPolicyVersion(ctx context.Context, tx pgx.Tx, p CreateOperatingCountryPolicyVersionParams) (PolicyRecord, error) {
	if err := p.Actor.validate(); err != nil {
		return PolicyRecord{}, err
	}
	if !validWritableScope(p.Scope) {
		return PolicyRecord{}, fmt.Errorf("%w: scope must be 'tenant', 'brand', or 'operation'", ErrInvalidInput)
	}
	if p.TenantID == uuid.Nil {
		return PolicyRecord{}, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}

	inScope, err := assertTenantScope(ctx, tx, p.TenantID)
	if err != nil {
		return PolicyRecord{}, err
	}
	if !inScope {
		return PolicyRecord{}, fmt.Errorf("%w: transaction tenant scope does not match tenant %s", ErrTransactionScope, p.TenantID)
	}

	switch p.Scope {
	case ScopeTenant:
		if p.BrandID != nil || p.OperationCode != "" || p.ProductCode != nil {
			return PolicyRecord{}, fmt.Errorf("%w: a tenant-scope policy carries no brand_id, operation_code, or product_code", ErrInvalidInput)
		}
	case ScopeBrand:
		if p.BrandID == nil {
			return PolicyRecord{}, fmt.Errorf("%w: brand_id is required for a brand-scope policy", ErrInvalidInput)
		}
		if p.OperationCode != "" || p.ProductCode != nil {
			return PolicyRecord{}, fmt.Errorf("%w: a brand-scope policy carries no operation_code or product_code", ErrInvalidInput)
		}
	case ScopeOperation:
		if p.OperationCode == "" {
			return PolicyRecord{}, fmt.Errorf("%w: operation_code is required for an operation-scope policy", ErrInvalidInput)
		}
	}

	countryCode := strings.TrimSpace(p.CountryCode)
	if !validation.IsISO3166Alpha2(countryCode) {
		return PolicyRecord{}, fmt.Errorf("%w: country_code must be a valid, uppercase ISO-3166-1 alpha-2 code", ErrInvalidInput)
	}
	if p.State != StateEnabled && p.State != StateDisabled {
		return PolicyRecord{}, fmt.Errorf("%w: state must be 'enabled' or 'disabled'", ErrInvalidInput)
	}
	if p.Status != StatusActive && p.Status != StatusWithdrawn {
		return PolicyRecord{}, fmt.Errorf("%w: status must be 'active' or 'withdrawn'", ErrInvalidInput)
	}
	authRef := strings.TrimSpace(p.AuthorizationReference)
	if p.Status == StatusActive && p.State == StateEnabled && authRef == "" {
		return PolicyRecord{}, fmt.Errorf("%w: authorization_reference is required to enable a country", ErrInvalidInput)
	}
	if p.Status == StatusWithdrawn && p.State != StateDisabled {
		return PolicyRecord{}, fmt.Errorf("%w: a withdrawal must carry state 'disabled'", ErrInvalidInput)
	}
	// ADR 0045 §3.5-A AMENDMENT-2 (SEC-E-REV-1). Absence at the brand and
	// operation rungs INHERITS from the rung above, so withdrawing an
	// in-force disable there is functionally an enable and carries the
	// enable direction's recorded-authorization requirement. Mirrors, and
	// is independently enforced by, the CHECK constraint
	// ocp_inherit_rung_withdrawal_requires_authorization - this is the
	// diagnosable half, that is the authoritative half.
	//
	// Deliberately NOT applied to ScopeTenant: absence at the tenant rung
	// is terminal (not_configured, resolve.go STEP 2), so a tenant-rung
	// withdrawal can only ever narrow. Writing a DISABLE (StatusActive +
	// StateDisabled) still needs no reference at any scope - the emergency
	// kill-switch is unchanged.
	if p.Status == StatusWithdrawn && (p.Scope == ScopeBrand || p.Scope == ScopeOperation) && authRef == "" {
		return PolicyRecord{}, fmt.Errorf("%w: authorization_reference is required to withdraw a %s-scope policy: absence at this rung inherits from the rung above, so a withdrawal here can itself widen what is permitted", ErrInvalidInput, p.Scope)
	}

	// Build the key's WHERE clause and lock target - one form per scope,
	// mirroring the four partial unique indexes exactly.
	var latestID *uuid.UUID
	var latestEffectiveTo *time.Time
	var lockErr error
	switch p.Scope {
	case ScopeTenant:
		lockErr = tx.QueryRow(ctx, `
			SELECT id, effective_to FROM operating_country_policies
			 WHERE tenant_id = $1 AND scope_kind = 'tenant' AND country_code = $2
			 ORDER BY effective_from DESC LIMIT 1 FOR UPDATE`,
			p.TenantID, countryCode,
		).Scan(&latestID, &latestEffectiveTo)
	case ScopeBrand:
		lockErr = tx.QueryRow(ctx, `
			SELECT id, effective_to FROM operating_country_policies
			 WHERE tenant_id = $1 AND scope_kind = 'brand' AND brand_id = $2 AND country_code = $3
			 ORDER BY effective_from DESC LIMIT 1 FOR UPDATE`,
			p.TenantID, *p.BrandID, countryCode,
		).Scan(&latestID, &latestEffectiveTo)
	case ScopeOperation:
		lockErr = tx.QueryRow(ctx, `
			SELECT id, effective_to FROM operating_country_policies
			 WHERE tenant_id = $1 AND scope_kind = 'operation' AND country_code = $2
			   AND operation_code = $3 AND brand_id IS NOT DISTINCT FROM $4
			   AND product_code IS NOT DISTINCT FROM $5
			 ORDER BY effective_from DESC LIMIT 1 FOR UPDATE`,
			p.TenantID, countryCode, p.OperationCode, p.BrandID, p.ProductCode,
		).Scan(&latestID, &latestEffectiveTo)
	}
	if lockErr != nil && !errors.Is(lockErr, pgx.ErrNoRows) {
		return PolicyRecord{}, fmt.Errorf("operatingmarket: lock latest policy version: %w", lockErr)
	}

	var beforeState *PolicyRecord
	var closedEffectiveTo *time.Time
	if latestID != nil && latestEffectiveTo == nil {
		before, err := readPolicyRecordByID(ctx, tx, *latestID)
		if err != nil {
			return PolicyRecord{}, err
		}
		beforeState = &before

		// ADR 0045 §3.5-A AMENDMENT-3 (SEC-E-REV-2). This close is legal
		// ONLY because the INSERT below is unconditional and in the SAME
		// transaction. A "bare close" - this UPDATE with no successor
		// insert - is a WIDENING act at the brand and operation rungs
		// (absence there inherits, ruling 6) and is refused at COMMIT by
		// the deferred constraint trigger
		// ocp_inherit_rung_close_requires_successor. Any future writer in
		// this package that closes a version MUST insert an authorized
		// successor at the same key in the same transaction, and MUST
		// route its tx.Commit() error through ClassifyCommitError.
		// Do NOT issue `SET CONSTRAINTS ALL IMMEDIATE` in a transaction
		// that reaches this line.
		var closed time.Time
		err = tx.QueryRow(ctx,
			`UPDATE operating_country_policies SET effective_to = now() WHERE id = $1 AND effective_to IS NULL RETURNING effective_to`,
			*latestID).Scan(&closed)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) {
				switch pgErr.Code {
				case "23514":
					return PolicyRecord{}, ErrConcurrentPolicyWrite
				case "P0001":
					// Reachable only via SET CONSTRAINTS ALL IMMEDIATE before
					// this statement (an edge case the migration comment
					// instructs against, but real) - the deferred constraint
					// trigger ocp_inherit_rung_close_requires_successor can
					// then fire at STATEMENT time instead of at commit.
					// Mirrors the INSERT path's identical P0001 arm below.
					return PolicyRecord{}, classifyTriggerError(err)
				}
			}
			if errors.Is(err, pgx.ErrNoRows) {
				return PolicyRecord{}, ErrConcurrentPolicyWrite
			}
			return PolicyRecord{}, fmt.Errorf("operatingmarket: close prior policy version: %w", err)
		}
		closedEffectiveTo = &closed
	}

	var operationCodeArg *string
	if p.OperationCode != "" {
		oc := p.OperationCode
		operationCodeArg = &oc
	}

	var (
		newID         uuid.UUID
		effectiveFrom time.Time
		createdAt     time.Time
	)
	err = tx.QueryRow(ctx, `
		INSERT INTO operating_country_policies (
			tenant_id, scope_kind, brand_id, operation_code, product_code, country_code,
			state, status, authorization_reference, reason_code, policy_version,
			created_by_actor_type, created_by_actor_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), $10, $11, 'staff', $12)
		RETURNING id, effective_from, created_at`,
		p.TenantID, string(p.Scope), p.BrandID, operationCodeArg, p.ProductCode, countryCode,
		string(p.State), string(p.Status), authRef, p.Actor.ReasonCode, PolicyVersion,
		p.Actor.ActorID,
	).Scan(&newID, &effectiveFrom, &createdAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23505":
				return PolicyRecord{}, ErrConcurrentPolicyWrite
			case "23503":
				return PolicyRecord{}, fmt.Errorf("%w: unknown tenant/brand/operation/product reference", ErrInvalidInput)
			case "23514":
				return PolicyRecord{}, fmt.Errorf("%w: operating_country_policies rejected this write: constraint %s", ErrInvalidInput, pgErr.ConstraintName)
			case "P0001":
				return PolicyRecord{}, classifyTriggerError(err)
			}
		}
		return PolicyRecord{}, fmt.Errorf("operatingmarket: insert policy version: %w", err)
	}

	newState := PolicyRecord{
		ID: newID, TenantID: p.TenantID, Scope: p.Scope, BrandID: p.BrandID,
		OperationCode: operationCodeArg, ProductCode: p.ProductCode, CountryCode: countryCode,
		State: p.State, Status: p.Status, AuthorizationReference: authRef,
		ReasonCode: p.Actor.ReasonCode, PolicyVersion: PolicyVersion,
		EffectiveFrom: effectiveFrom, CreatedByActorType: "staff",
		CreatedByActorID: p.Actor.ActorID, CreatedAt: createdAt,
	}

	var beforeMetadata any
	if beforeState != nil {
		beforeMetadata = policyRecordState(*beforeState)
	}
	var priorEffectiveTo any
	if closedEffectiveTo != nil {
		priorEffectiveTo = closedEffectiveTo.Format(time.RFC3339Nano)
	}
	var brandIDMeta any
	if p.BrandID != nil {
		brandIDMeta = p.BrandID.String()
	}
	var operationCodeMeta any
	if operationCodeArg != nil {
		operationCodeMeta = *operationCodeArg
	}
	var productCodeMeta any
	if p.ProductCode != nil {
		productCodeMeta = *p.ProductCode
	}

	// ADR 0045 §3.5-A AMENDMENT-2 / §10. A withdrawal that REMOVES a block
	// at an inherit rung is a widening event whose stored state reads
	// "state":"disabled","status":"withdrawn" - on those two fields alone,
	// indistinguishable from a harmless withdrawal. These two computed
	// fields are what an auditor filters on. Computed ONLY from beforeState
	// and this write's own parameters; resolve() is NEVER called here.
	blockedBefore := beforeState != nil && beforeState.Status == StatusActive && beforeState.State == StateDisabled
	blockedAfter := p.Status == StatusActive && p.State == StateDisabled
	rungBlockTransition := "no_block_change"
	switch {
	case blockedBefore && !blockedAfter:
		rungBlockTransition = "removes_block"
	case !blockedBefore && blockedAfter:
		rungBlockTransition = "adds_block"
	}
	wideningCapable := (rungBlockTransition == "removes_block" && p.Scope != ScopeTenant) ||
		(p.Status == StatusActive && p.State == StateEnabled)

	if err := recordOperatingMarketAudit(ctx, tx, p.TenantID, p.Actor, "operating_market.policy_version_created", "operating_country_policies", newID.String(), map[string]any{
		"before":                  beforeMetadata,
		"after":                   policyRecordState(newState),
		"scope":                   string(p.Scope),
		"country_code":            countryCode,
		"tenant_id":               p.TenantID.String(),
		"brand_id":                brandIDMeta,
		"operation_code":          operationCodeMeta,
		"product_code":            productCodeMeta,
		"state":                   string(p.State),
		"status":                  string(p.Status),
		"version_id":              newID.String(),
		"effective_from":          effectiveFrom.Format(time.RFC3339Nano),
		"prior_effective_to":      priorEffectiveTo,
		"authorization_reference": authRef,
		"policy_version":          PolicyVersion,
		"rung_block_transition":   rungBlockTransition,
		"widening_capable":        wideningCapable,
	}); err != nil {
		return PolicyRecord{}, err
	}

	return newState, nil
}

func readPolicyRecordByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (PolicyRecord, error) {
	var (
		rec       PolicyRecord
		scopeKind string
		state     string
		status    string
		actorType string
	)
	err := tx.QueryRow(ctx, `
		SELECT id, tenant_id, scope_kind, brand_id, operation_code, product_code, country_code,
		       state, status, COALESCE(authorization_reference, ''), reason_code, policy_version,
		       effective_from, effective_to, created_by_actor_type, created_by_actor_id, created_at
		  FROM operating_country_policies WHERE id = $1`, id,
	).Scan(
		&rec.ID, &rec.TenantID, &scopeKind, &rec.BrandID, &rec.OperationCode, &rec.ProductCode, &rec.CountryCode,
		&state, &status, &rec.AuthorizationReference, &rec.ReasonCode, &rec.PolicyVersion,
		&rec.EffectiveFrom, &rec.EffectiveTo, &actorType, &rec.CreatedByActorID, &rec.CreatedAt,
	)
	if err != nil {
		return PolicyRecord{}, fmt.Errorf("operatingmarket: read policy version %s: %w", id, err)
	}
	scope, err := parseScope(scopeKind)
	if err != nil {
		return PolicyRecord{}, fmt.Errorf("operatingmarket: stored scope_kind is invalid: %w", err)
	}
	parsedState, err := parseState(state)
	if err != nil {
		return PolicyRecord{}, fmt.Errorf("operatingmarket: stored policy state is invalid: %w", err)
	}
	parsedStatus, err := parseStatus(status)
	if err != nil {
		return PolicyRecord{}, fmt.Errorf("operatingmarket: stored policy status is invalid: %w", err)
	}
	rec.Scope = scope
	rec.State = parsedState
	rec.Status = parsedStatus
	rec.CreatedByActorType = actorType
	return rec, nil
}

// ListOperatingCountryPolicyVersions returns the full, append-only
// version history for one scope's key, newest first.
//
// tx MUST be a tenant-scoped, non-player-scoped transaction for
// p.TenantID - asserted in-function via assertTenantScope, as its FIRST
// statement (PHASE-D-SEC-P3-1's own lesson).
func ListOperatingCountryPolicyVersions(ctx context.Context, tx pgx.Tx, p HistoryQuery) ([]PolicyRecord, error) {
	if p.TenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	inScope, err := assertTenantScope(ctx, tx, p.TenantID)
	if err != nil {
		return nil, err
	}
	if !inScope {
		return nil, fmt.Errorf("%w: transaction tenant scope does not match tenant %s", ErrTransactionScope, p.TenantID)
	}
	if !validWritableScope(p.Scope) {
		return nil, fmt.Errorf("%w: scope must be 'tenant', 'brand', or 'operation'", ErrInvalidInput)
	}
	countryCode := strings.TrimSpace(p.CountryCode)
	if !validation.IsISO3166Alpha2(countryCode) {
		return nil, fmt.Errorf("%w: country_code must be a valid, uppercase ISO-3166-1 alpha-2 code", ErrInvalidInput)
	}

	var rows pgx.Rows
	switch p.Scope {
	case ScopeTenant:
		rows, err = tx.Query(ctx, `
			SELECT id, tenant_id, scope_kind, brand_id, operation_code, product_code, country_code,
			       state, status, COALESCE(authorization_reference, ''), reason_code, policy_version,
			       effective_from, effective_to, created_by_actor_type, created_by_actor_id, created_at
			  FROM operating_country_policies
			 WHERE tenant_id = $1 AND scope_kind = 'tenant' AND country_code = $2
			 ORDER BY effective_from DESC`,
			p.TenantID, countryCode)
	case ScopeBrand:
		if p.BrandID == nil {
			return nil, fmt.Errorf("%w: brand_id is required for a brand-scope history query", ErrInvalidInput)
		}
		rows, err = tx.Query(ctx, `
			SELECT id, tenant_id, scope_kind, brand_id, operation_code, product_code, country_code,
			       state, status, COALESCE(authorization_reference, ''), reason_code, policy_version,
			       effective_from, effective_to, created_by_actor_type, created_by_actor_id, created_at
			  FROM operating_country_policies
			 WHERE tenant_id = $1 AND scope_kind = 'brand' AND brand_id = $2 AND country_code = $3
			 ORDER BY effective_from DESC`,
			p.TenantID, *p.BrandID, countryCode)
	case ScopeOperation:
		if p.OperationCode == "" {
			return nil, fmt.Errorf("%w: operation_code is required for an operation-scope history query", ErrInvalidInput)
		}
		rows, err = tx.Query(ctx, `
			SELECT id, tenant_id, scope_kind, brand_id, operation_code, product_code, country_code,
			       state, status, COALESCE(authorization_reference, ''), reason_code, policy_version,
			       effective_from, effective_to, created_by_actor_type, created_by_actor_id, created_at
			  FROM operating_country_policies
			 WHERE tenant_id = $1 AND scope_kind = 'operation' AND country_code = $2
			   AND operation_code = $3 AND brand_id IS NOT DISTINCT FROM $4
			   AND product_code IS NOT DISTINCT FROM $5
			 ORDER BY effective_from DESC`,
			p.TenantID, countryCode, p.OperationCode, p.BrandID, p.ProductCode)
	}
	if err != nil {
		return nil, fmt.Errorf("operatingmarket: list policy versions: %w", err)
	}
	defer rows.Close()

	var out []PolicyRecord
	for rows.Next() {
		var (
			rec       PolicyRecord
			scopeKind string
			state     string
			status    string
			actorType string
		)
		if err := rows.Scan(
			&rec.ID, &rec.TenantID, &scopeKind, &rec.BrandID, &rec.OperationCode, &rec.ProductCode, &rec.CountryCode,
			&state, &status, &rec.AuthorizationReference, &rec.ReasonCode, &rec.PolicyVersion,
			&rec.EffectiveFrom, &rec.EffectiveTo, &actorType, &rec.CreatedByActorID, &rec.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("operatingmarket: scan policy version: %w", err)
		}
		scope, err := parseScope(scopeKind)
		if err != nil {
			return nil, fmt.Errorf("operatingmarket: stored scope_kind is invalid: %w", err)
		}
		parsedState, err := parseState(state)
		if err != nil {
			return nil, fmt.Errorf("operatingmarket: stored policy state is invalid: %w", err)
		}
		parsedStatus, err := parseStatus(status)
		if err != nil {
			return nil, fmt.Errorf("operatingmarket: stored policy status is invalid: %w", err)
		}
		rec.Scope = scope
		rec.State = parsedState
		rec.Status = parsedStatus
		rec.CreatedByActorType = actorType
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ListCurrentOperatingCountryPolicies returns every currently in-force
// (at asOf) operating_country_policies row for a tenant, across all
// scopes and countries - "inspect current" (as distinct from
// ListOperatingCountryPolicyVersions' "inspect history" for one key).
//
// tx MUST be a tenant-scoped, non-player-scoped transaction for tenantID -
// asserted in-function via assertTenantScope, as its FIRST statement.
func ListCurrentOperatingCountryPolicies(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, asOf time.Time) ([]PolicyRecord, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	inScope, err := assertTenantScope(ctx, tx, tenantID)
	if err != nil {
		return nil, err
	}
	if !inScope {
		return nil, fmt.Errorf("%w: transaction tenant scope does not match tenant %s", ErrTransactionScope, tenantID)
	}
	if asOf.IsZero() {
		return nil, fmt.Errorf("%w: as_of is required and must not be the zero time", ErrInvalidInput)
	}

	rows, err := tx.Query(ctx, `
		SELECT id, tenant_id, scope_kind, brand_id, operation_code, product_code, country_code,
		       state, status, COALESCE(authorization_reference, ''), reason_code, policy_version,
		       effective_from, effective_to, created_by_actor_type, created_by_actor_id, created_at
		  FROM operating_country_policies
		 WHERE tenant_id = $1 AND effective_from <= $2 AND (effective_to IS NULL OR effective_to > $2)
		 ORDER BY country_code, scope_kind, effective_from DESC`,
		tenantID, asOf)
	if err != nil {
		return nil, fmt.Errorf("operatingmarket: list current policies: %w", err)
	}
	defer rows.Close()

	var out []PolicyRecord
	for rows.Next() {
		var (
			rec       PolicyRecord
			scopeKind string
			state     string
			status    string
			actorType string
		)
		if err := rows.Scan(
			&rec.ID, &rec.TenantID, &scopeKind, &rec.BrandID, &rec.OperationCode, &rec.ProductCode, &rec.CountryCode,
			&state, &status, &rec.AuthorizationReference, &rec.ReasonCode, &rec.PolicyVersion,
			&rec.EffectiveFrom, &rec.EffectiveTo, &actorType, &rec.CreatedByActorID, &rec.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("operatingmarket: scan current policy: %w", err)
		}
		scope, err := parseScope(scopeKind)
		if err != nil {
			return nil, fmt.Errorf("operatingmarket: stored scope_kind is invalid: %w", err)
		}
		parsedState, err := parseState(state)
		if err != nil {
			return nil, fmt.Errorf("operatingmarket: stored policy state is invalid: %w", err)
		}
		parsedStatus, err := parseStatus(status)
		if err != nil {
			return nil, fmt.Errorf("operatingmarket: stored policy status is invalid: %w", err)
		}
		rec.Scope = scope
		rec.State = parsedState
		rec.Status = parsedStatus
		rec.CreatedByActorType = actorType
		out = append(out, rec)
	}
	return out, rows.Err()
}
