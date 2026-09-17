// Package assetregistry is the platform's Asset/Currency Registry and the
// ONE canonical Asset Authorization decision point (ADR 0037 Parts A and
// C). Every domain - wallet, payments, casino, sportsbook, bonus,
// FX/conversion, retail - asks CheckEligibility instead of querying
// `assets`, `asset_authorizations` or `asset_operation_eligibility`
// itself. A domain reading those tables to make its own allow/deny call is
// a code-reviewer blocking finding (ADR 0037 §C.2), for the same reason
// internal/rg.EvaluateEligibility and internal/risk.Evaluate are the sole
// authorities at their own decision points.
//
// Implementation status (Stage 4H-B0-R6, Workstream A): layers 1-7 are
// IMPLEMENTED. Layer 8 (market-rate availability) is deliberately NOT
// part of this package - ADR 0037 §A.3/§A.7 place it with the
// FX/Conversion Service, evaluated live at conversion time, and no FX
// provider exists (this stage is not authorized to build one).
//
// Fail-closed is the whole design. Every absent row, every unparseable
// input, every error is a denial:
//   - absent configuration at ANY layer => ineligible (ADR 0037 §C.1)
//   - a non-nil error => ineligible, at every call site, with no fallback
//     to a previously-known-good answer (§C.2)
//   - a zero-value tenant or jurisdiction => immediate denial, never
//     "not scoped, therefore skip" (Stage 4H-B0-R5 security finding S-6a)
package assetregistry

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidInput covers caller-side mistakes - mirrors risk.ErrInvalidInput/
// rg.ErrInvalidInput's package-local sentinel convention.
var ErrInvalidInput = errors.New("assetregistry: invalid input")

// ErrNotFound covers "no such asset/request row".
var ErrNotFound = errors.New("assetregistry: not found")

// ErrTenantContextMismatch is returned when the tenant argument passed to
// CheckEligibility does not match the tenant the transaction is actually
// scoped to (app.tenant_id). This is the structural defence against a
// spoofed, request-payload-supplied tenant identifier: the authoritative
// tenant is the one the credential established, mirroring ADR 0038 §11's
// rule for provider callbacks ("tenant resolved from the credential that
// verified the callback's signature, never from a tenant/player
// identifier in the payload").
var ErrTenantContextMismatch = errors.New("assetregistry: tenant argument does not match the transaction's authenticated tenant scope")

// ErrDualControlRequired is returned when a create/activate/
// platform-authorize attempt has no independently-approved change request
// behind it. The authoritative refusal is migration 0044's trigger; this
// sentinel exists so the HTTP layer can answer 409 rather than 500.
var ErrDualControlRequired = errors.New("assetregistry: operation requires an approved change request by a different platform principal (four-eyes)")

// ErrSelfApproval is returned when a principal tries to approve its own
// change request. Mirrors withdrawal.ErrSelfApproval's role exactly: a
// clean application error in front of migration 0044's authoritative
// trigger, never a replacement for it.
var ErrSelfApproval = errors.New("assetregistry: a principal may not approve its own change request")

// ErrWidensPlatformAuthorization is returned when a tenant/brand/
// jurisdiction/eligibility write would grant more than the layer above it
// already grants (ADR 0037 §A.5/§A.6 narrow-only-never-widen).
var ErrWidensPlatformAuthorization = errors.New("assetregistry: configuration may only narrow what the layer above it authorizes, never widen it")

// Operation is one of ADR 0037 §C.2's exactly six operations. The list is
// fixed by the ADR; widening it requires an ADR amendment and a migration
// (migration 0045 carries the matching CHECK constraint).
type Operation string

const (
	OperationDeposit    Operation = "deposit"
	OperationWithdrawal Operation = "withdrawal"
	OperationWagering   Operation = "wagering"
	OperationSettlement Operation = "settlement"
	OperationConversion Operation = "conversion"
	OperationReporting  Operation = "reporting"
)

// Operations is the canonical list, for validation. Order is the ADR's.
func Operations() []Operation {
	return []Operation{
		OperationDeposit, OperationWithdrawal, OperationWagering,
		OperationSettlement, OperationConversion, OperationReporting,
	}
}

func validOperation(op Operation) bool {
	for _, known := range Operations() {
		if op == known {
			return true
		}
	}
	return false
}

// OperationScope is the (product, operation) pair a caller must name.
//
// ADR 0037 §C.2's original signature took a bare `operation`, with no
// product/vertical axis - which `sportsbook`'s Stage 4H-B0-R5 review
// found makes "BTC is wagering-eligible for casino but not for sportsbook
// in jurisdiction X" inexpressible, even though the platform already
// models product-specific licensing in licences.permitted_products
// (migration 0002, doc 15). The operation dimension is therefore
// (product, operation), carried in one parameter so the canonical
// six-parameter shape the ADR fixed is preserved. Recorded as ADR 0037
// §C.6.
//
// Product is REQUIRED on every check, deliberately: a caller that cannot
// say which product it is acting for cannot be authorized (fail-closed).
// Authorization ROWS may still carry a NULL product meaning "every
// product" - that is explicit, deliberate breadth granted by an
// administrator, not an inference made at check time.
type OperationScope struct {
	Product   string
	Operation Operation
}

// ReasonCode identifies WHICH layer denied, so a denial is never generic -
// the same "specific, distinguishable sentinel, never a generic denial"
// contract rg.Decision and risk.RiskDecision already use. Each of layers
// 1-7 has its own code, and each is produced from its own independently
// queryable fact (which is what makes ADR 0037 §C.2's promise of
// per-layer distinguishable reason codes testable - Stage 4H-B0-R5 `qa`
// finding).
type ReasonCode string

const (
	// ReasonEligible is the only non-denial value.
	ReasonEligible ReasonCode = "eligible"

	// Input/context failures - all fail closed.
	ReasonTenantContextMissing       ReasonCode = "tenant_context_missing"
	ReasonJurisdictionContextMissing ReasonCode = "jurisdiction_context_missing"
	ReasonProductContextMissing      ReasonCode = "product_context_missing"
	ReasonProductUnknown             ReasonCode = "product_unknown"
	ReasonOperationUnknown           ReasonCode = "operation_unknown"
	ReasonInternalError              ReasonCode = "internal_error"

	// Layer 1-3 (platform).
	ReasonAssetNotFound              ReasonCode = "asset_not_found"
	ReasonAssetInactive              ReasonCode = "asset_inactive"
	ReasonAssetNotPlatformAuthorized ReasonCode = "asset_not_platform_authorized"

	// Layer 4-6 (tenant/brand/jurisdiction).
	ReasonTenantNotAuthorized       ReasonCode = "tenant_not_authorized"
	ReasonBrandNotAuthorized        ReasonCode = "brand_not_authorized"
	ReasonJurisdictionNotAuthorized ReasonCode = "jurisdiction_not_authorized"

	// Layer 7 (operation/product eligibility). Split so a platform-level
	// denial and a tenant-level narrowing are distinguishable in the
	// audit trail - they call for different operational responses.
	ReasonOperationNotEligible          ReasonCode = "operation_not_eligible"
	ReasonOperationNotEligibleForTenant ReasonCode = "operation_not_eligible_for_tenant"
)

// AssetType values as constrained by migration 0003.
const (
	AssetTypeFiat   = "fiat"
	AssetTypeCrypto = "crypto"
)

// Asset is one registry row (layers 1-3).
type Asset struct {
	Code               string
	AssetType          string
	DecimalExponent    int16
	DisplayName        string
	Network            string
	Active             bool
	PlatformAuthorized bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ChangeOperation is one of the three dual-controlled layer-1-3
// operations (ADR 0037 §C.5.3). Suspend/revoke are deliberately absent:
// turning something off is single-actor by design, an incident
// kill-switch that must not wait for a second approver.
type ChangeOperation string

const (
	ChangeCreate            ChangeOperation = "create"
	ChangeActivate          ChangeOperation = "activate"
	ChangePlatformAuthorize ChangeOperation = "platform_authorize"
)

func validChangeOperation(op ChangeOperation) bool {
	switch op {
	case ChangeCreate, ChangeActivate, ChangePlatformAuthorize:
		return true
	}
	return false
}

// ChangeRequest is a pending (or decided) four-eyes request.
type ChangeRequest struct {
	ID                     uuid.UUID
	Operation              ChangeOperation
	AssetCode              string
	Payload                map[string]any
	ReasonCode             string
	RequestedByPrincipalID uuid.UUID
	RequestedAt            time.Time
	State                  string
	AppliedByPrincipalID   *uuid.UUID
	AppliedAt              *time.Time
}

// Approval is one four-eyes decision on a ChangeRequest.
type Approval struct {
	ID                  uuid.UUID
	RequestID           uuid.UUID
	ApproverPrincipalID uuid.UUID
	Decision            string
	ReasonCode          string
	DecidedAt           time.Time
}

// ScopeKind discriminates which of layers 4/5/6 an asset_authorizations
// row answers.
type ScopeKind string

const (
	ScopeTenant       ScopeKind = "tenant"
	ScopeBrand        ScopeKind = "brand"
	ScopeJurisdiction ScopeKind = "jurisdiction"
)

// Authorization is one layer-4/5/6 fact.
type Authorization struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	ScopeKind      ScopeKind
	BrandID        *uuid.UUID
	JurisdictionID *uuid.UUID
	AssetCode      string
	Product        string // "" means the row applies to every product
	Eligible       bool
	ReasonCode     string
	UpdatedAt      time.Time
}

// OperationEligibility is one layer-7 fact. TenantID nil is the
// platform-wide default (ADR 0037 §A.6).
type OperationEligibility struct {
	ID         uuid.UUID
	TenantID   *uuid.UUID
	AssetCode  string
	Product    string // "" means every product
	Operation  Operation
	Eligible   bool
	ReasonCode string
	UpdatedAt  time.Time
}
